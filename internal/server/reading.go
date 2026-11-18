package server

import (
	"database/sql"
	"errors"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/0xD34DC0D3/voodoo/voodooauth"

	"github.com/0xD34DC0D3/scry/internal/db"
	"github.com/0xD34DC0D3/scry/internal/web"
)

// Reading: signed-in users subscribe to feeds and read their items with the
// full sanitized HTML, on the home stream (Unread at /, Following) and on a
// subscribed feed's page. Everything here is personal, so it is never
// stored by a cache (private, no-store).

const (
	// The home stream's three views, one switch: subscribed unread items,
	// every subscribed item in the window, and every feed (excerpts).
	homePath       = "/"
	followingPath  = "/following"
	everythingPath = "/everything"
	// readingPageSize is far smaller than the public lists': each entry
	// carries a whole article (20 real ones measured about 117,000 px).
	readingPageSize = 10
)

func (s *server) readingRoutes(mux *http.ServeMux) {
	user := func(h http.HandlerFunc) http.Handler { return s.auth.RequireUser(h) }
	mux.Handle("GET "+followingPath, user(s.following))
	mux.Handle("POST /mark-all-read", user(s.markAllRead))
	mux.Handle("POST /items/{id}/read", user(s.markItem(true)))
	mux.Handle("POST /items/{id}/unread", user(s.markItem(false)))
	mux.Handle("POST /feeds/{id}/subscribe", user(s.subscription(true)))
	mux.Handle("POST /feeds/{id}/unsubscribe", user(s.subscription(false)))
}

// subView is one subscription with its unread count.
type subView struct {
	ID     int64
	Name   string
	Paused bool
	Unread int64
	Link   string // this feed's page, keeping the view
}

// seg is one link of a segmented view switch.
type seg struct {
	Label, Href string
	Current     bool
	Count       bool // carries the unread count (#unread-total)
}

// viewSwitch is the segmented control that narrows a post list: Unread |
// Following | Everything on the home stream, Unread | All on a feed's page.
// The unread count for its scope sits in the Unread label.
type viewSwitch struct {
	Segs  []seg
	Total int64
	Focus bool // after "mark all read": focus the current segment
}

// homeSwitch is the home stream's switch with current marked.
func homeSwitch(current string, total int64) *viewSwitch {
	return &viewSwitch{Total: total, Segs: []seg{
		{Label: "Unread", Href: homePath, Current: current == homePath, Count: true},
		{Label: "Following", Href: followingPath, Current: current == followingPath},
		{Label: "Everything", Href: everythingPath, Current: current == everythingPath},
	}}
}

// readingItem is one entry. HTML comes only from item_contents.html, which
// bluemonday's UGC policy sanitized at ingest; nothing else may become
// template.HTML.
type readingItem struct {
	itemView
	HTML      template.HTML
	Unread    bool
	Marked    bool   // has an item_reads row that "mark unread" can remove
	Back      string // where a plain (no-JS) post returns to
	ScopeFeed int64  // the page's feed filter, so an action's count matches it
	Focus     bool   // htmx moves focus here after the swap
}

type readingData struct {
	Header  *feedView // the feed page's header; nil on the home stream
	Switch  *viewSwitch
	Subs    []subView
	Total   int64    // unread in the page's scope: every feed, or the page's feed
	Feed    *subView // the page's feed, nil on the home stream
	All     bool     // show read items too
	Items   []readingItem
	Older   string
	UptoID  int64  // the newest item id at render, for "mark all read"
	Self    string // this list's URL without the cursor
	AllLink string // the same scope with read items too (the caught-up link)
}

// entryData is what an item action answers htmx with: the entry, plus the
// fresh counts swapped out of band into the toolbar (for the page's scope)
// and feed list.
type entryData struct {
	Item  readingItem
	Total int64
	Feed  *subView
}

func userID(r *http.Request) string { return voodooauth.UserFrom(r.Context()).ID }

// personal marks a response no cache may keep. Vary matches the public
// pages' so nothing downstream mixes them up.
func personal(w http.ResponseWriter) {
	h := w.Header()
	h.Add("Vary", "Cookie")
	h.Add("Vary", "Authorization")
	h.Set("Cache-Control", privateCache)
}

// readingItemFrom builds an entry the way the public lists build an item:
// posted time as relative text (ago), the link's domain unless it is the
// feed's own.
func readingItemFrom(it db.ListReadingItemsRow, now time.Time) readingItem {
	posted := time.Unix(it.PostedAt, 0).UTC()
	return readingItem{
		itemView: itemView{
			ID: it.ID, FeedID: it.FeedID, URL: it.Url, Title: it.Title,
			Domain: linkDomain(it.Url, it.FeedUrl), Author: it.Author, CommentsURL: it.CommentsUrl,
			FeedName: feedName(it.FeedTitle, it.FeedUrl),
			Posted:   posted, PostedAgo: ago(posted, now, toMinute),
		},
		HTML:   template.HTML(it.Html), // sanitized at ingest (see readingItem)
		Unread: it.Unread != 0,
		Marked: it.Marked != 0,
	}
}

// readingQuery is the list's scope: the home stream (feed 0) or one
// feed's page, unread only or all. Item forms post it back as feed and view.
type readingQuery struct {
	feed int64
	all  bool
}

func parseReadingQuery(v url.Values) (readingQuery, bool) {
	q := readingQuery{all: v.Get("view") == "all"}
	if f := v.Get("feed"); f != "" {
		id, err := strconv.ParseInt(f, 10, 64)
		if err != nil || id <= 0 {
			return q, false
		}
		q.feed = id
	}
	return q, true
}

func (q readingQuery) url() string {
	switch {
	case q.feed != 0 && q.all:
		return fmt.Sprintf("/feeds/%d?view=all", q.feed)
	case q.feed != 0:
		return fmt.Sprintf("/feeds/%d", q.feed)
	case q.all:
		return followingPath
	}
	return homePath
}

// home is Unread for a signed-in user with a subscription, else Everything.
func (s *server) home(w http.ResponseWriter, r *http.Request) {
	if voodooauth.UserFrom(r.Context()) == nil {
		s.everything(w, r)
		return
	}
	at, id, ok := cursor(r)
	if !ok {
		s.badRequest(w, r, "That page link is malformed.")
		return
	}
	d, _, err := s.readingData(r, readingQuery{}, at, id)
	if err != nil {
		s.internalError(w, r, "reading", err)
		return
	}
	if len(d.Subs) == 0 {
		s.everything(w, r)
		return
	}
	s.renderReading(w, r, d, "Unread")
}

// following is every subscribed item in the window, read ones too.
func (s *server) following(w http.ResponseWriter, r *http.Request) {
	at, id, ok := cursor(r)
	if !ok {
		s.badRequest(w, r, "That page link is malformed.")
		return
	}
	d, _, err := s.readingData(r, readingQuery{all: true}, at, id)
	if err != nil {
		s.internalError(w, r, "reading", err)
		return
	}
	s.renderReading(w, r, d, "Following")
}

// renderReading answers with a reading list: the page, or on htmx the
// Older page's entries.
func (s *server) renderReading(w http.ResponseWriter, r *http.Request, d readingData, title string) {
	personal(w)
	setReadingCSP(w)
	v := web.View{Page: "reading", Fragment: "reading-page", Title: title, Data: d, Nav: "home", NavSub: d.Self != homePath}
	if d.Header != nil {
		v.Nav, v.NavSub = "feeds", true
	}
	s.web.Render(w, r, v)
}

// setReadingCSP lets article images load from other https sites; every
// other page keeps img-src at 'self'. Images are the one embedded resource
// the UGC policy keeps (no iframes, video, or style), so nothing else is
// loosened.
func setReadingCSP(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", readingContentSecurityPolicy)
}

// readingData loads one page of the list. found is false when q names a
// feed the user is not subscribed to.
func (s *server) readingData(r *http.Request, q readingQuery, at, id int64) (readingData, bool, error) {
	ctx := r.Context()
	uid := userID(r)
	// The watermark is read before the list, so every item on the page has
	// an id at or below it.
	upto, err := s.r.MaxItemID(ctx)
	if err != nil {
		return readingData{}, false, err
	}
	subRows, err := s.r.ListSubscriptions(ctx, db.ListSubscriptionsParams{UserID: uid, Since: since()})
	if err != nil {
		return readingData{}, false, err
	}
	d := readingData{All: q.all, UptoID: upto, Self: q.url(), AllLink: readingQuery{feed: q.feed, all: true}.url()}
	var all int64
	d.Subs = make([]subView, len(subRows))
	for i, sr := range subRows {
		d.Subs[i] = subView{
			ID: sr.ID, Name: feedName(sr.Title, sr.Url), Paused: sr.DisabledAt.Valid, Unread: sr.Unread,
			Link: readingQuery{feed: sr.ID, all: q.all}.url(),
		}
		all += sr.Unread
		if sr.ID == q.feed {
			d.Feed = &d.Subs[i]
		}
	}
	if q.feed != 0 && d.Feed == nil {
		return d, false, nil
	}
	d.Total = all
	if d.Feed != nil {
		d.Total = d.Feed.Unread
	}
	if d.Feed != nil {
		d.Switch = &viewSwitch{Total: d.Total, Segs: []seg{
			{Label: "Unread", Href: readingQuery{feed: q.feed}.url(), Current: !q.all, Count: true},
			{Label: "All", Href: d.AllLink, Current: q.all},
		}}
	} else {
		d.Switch = homeSwitch(d.Self, d.Total)
	}
	if len(d.Subs) == 0 {
		return d, true, nil
	}
	unreadOnly := int64(1)
	if q.all {
		unreadOnly = 0
	}
	rows, err := s.r.ListReadingItems(ctx, db.ListReadingItemsParams{
		UserID: uid, FeedID: q.feed, Since: since(), BeforeAt: at, BeforeID: id,
		UnreadOnly: unreadOnly, Lim: readingPageSize + 1,
	})
	if err != nil {
		return d, false, err
	}
	now := time.Now()
	items := make([]readingItem, len(rows))
	for i, row := range rows {
		items[i] = readingItemFrom(row, now)
	}
	sep := "?"
	if strings.Contains(d.Self, "?") {
		sep = "&"
	}
	if len(items) > readingPageSize {
		last := items[readingPageSize-1]
		d.Older = fmt.Sprintf("%s%sbefore=%d-%d", d.Self, sep, last.Posted.Unix(), last.ID)
		items = items[:readingPageSize]
	}
	// A plain post returns to the same page of the list (its cursor
	// included), scrolled to where the reader was: the entry itself in the
	// all view, where it stays (collapsed); the next one in the unread view,
	// where it disappears.
	page := d.Self
	if at != math.MaxInt64 {
		page = fmt.Sprintf("%s%sbefore=%d-%d", d.Self, sep, at, id)
	}
	for i := range items {
		anchor := items[i].ID
		if !q.all {
			anchor = 0
			if i+1 < len(items) {
				anchor = items[i+1].ID
			}
		}
		items[i].Back = page
		items[i].ScopeFeed = q.feed
		if anchor != 0 {
			items[i].Back += fmt.Sprintf("#item-%d", anchor)
		}
	}
	// The first entry of an Older page takes focus (see reading.html).
	if isHTMX(r) && r.URL.Query().Get("before") != "" && len(items) > 0 {
		items[0].Focus = true
	}
	d.Items = items
	return d, true, nil
}

// localBack is the form's "back" value when it is a path on this site,
// else def: a plain post must never redirect off-site.
func localBack(r *http.Request, def string) string {
	b := r.PostFormValue("back")
	if !strings.HasPrefix(b, "/") || strings.HasPrefix(b, "//") || strings.ContainsAny(b, "\\\r\n") {
		return def
	}
	u, err := url.Parse(b)
	if err != nil || u.Scheme != "" || u.Host != "" {
		return def
	}
	return b
}

// parseForm reads a small action form, answering 413 or 400 itself.
func (s *server) parseForm(w http.ResponseWriter, r *http.Request) bool {
	if err := r.ParseForm(); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			s.web.Error(w, r, http.StatusRequestEntityTooLarge, "That request was too large.")
			return false
		}
		s.badRequest(w, r, "That form could not be read.")
		return false
	}
	return true
}

func isHTMX(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }

// markItem marks one item read (or unread again). htmx gets the entry in
// its new state plus fresh counts; a plain post goes back to the list.
func (s *server) markItem(read bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			s.notFound(w, r)
			return
		}
		if !s.parseForm(w, r) {
			return
		}
		ctx, uid := r.Context(), userID(r)
		if read {
			err = s.w.MarkItemRead(ctx, db.MarkItemReadParams{UserID: uid, ItemID: id, Now: time.Now().Unix()})
		} else {
			err = s.w.MarkItemUnread(ctx, db.MarkItemUnreadParams{UserID: uid, ItemID: id})
		}
		if err != nil {
			s.internalError(w, r, "mark item", err)
			return
		}
		row, err := s.r.GetReadingItem(ctx, db.GetReadingItemParams{UserID: uid, ItemID: id})
		if errors.Is(err, sql.ErrNoRows) {
			// No such item, or not in a subscribed feed.
			s.notFound(w, r)
			return
		}
		if err != nil {
			s.internalError(w, r, "get reading item", err)
			return
		}
		back := localBack(r, homePath)
		if !isHTMX(r) {
			http.Redirect(w, r, back, http.StatusSeeOther)
			return
		}
		subs, err := s.r.ListSubscriptions(ctx, db.ListSubscriptionsParams{UserID: uid, Since: since()})
		if err != nil {
			s.internalError(w, r, "list subscriptions", err)
			return
		}
		// The total out of band is the page's: the filter's feed when the
		// form says the page was filtered (an unparsable value means none).
		scope, _ := strconv.ParseInt(r.PostFormValue("feed"), 10, 64)
		item := readingItemFrom(db.ListReadingItemsRow(row), time.Now())
		item.Back, item.ScopeFeed = back, scope
		// The clicked button is gone with the swap; focus stays on the entry.
		item.Focus = true
		e := entryData{Item: item}
		for _, sr := range subs {
			if scope == 0 || sr.ID == scope {
				e.Total += sr.Unread
			}
			if sr.ID == row.FeedID {
				e.Feed = &subView{ID: sr.ID, Unread: sr.Unread}
			}
		}
		personal(w)
		s.web.Render(w, r, web.View{Page: "entry-action", Fragment: "entry-action", Data: e})
	}
}

// markAllRead moves read_before_at up to what the page showed: upto is the
// newest item id when it was rendered, so items that arrived since stay
// unread. feed limits it to one subscription.
func (s *server) markAllRead(w http.ResponseWriter, r *http.Request) {
	if !s.parseForm(w, r) {
		return
	}
	q, ok := parseReadingQuery(r.PostForm)
	upto, err := strconv.ParseInt(r.PostFormValue("upto"), 10, 64)
	if !ok || err != nil || upto < 0 {
		s.badRequest(w, r, "That form is missing what it needs. Reload the page and try again.")
		return
	}
	ctx, uid := r.Context(), userID(r)
	if err := s.w.MarkAllRead(ctx, db.MarkAllReadParams{UserID: uid, FeedID: q.feed, UptoID: upto}); err != nil {
		s.internalError(w, r, "mark all read", err)
		return
	}
	if !isHTMX(r) {
		http.Redirect(w, r, q.url(), http.StatusSeeOther)
		return
	}
	// htmx swaps the whole panel, first page. A WAL reader started after
	// the commit sees it, so the read pool is fine here.
	d, found, err := s.readingData(r, q, math.MaxInt64, math.MaxInt64)
	if err == nil && !found {
		q.feed = 0
		d, _, err = s.readingData(r, q, math.MaxInt64, math.MaxInt64)
	}
	if err != nil {
		s.internalError(w, r, "reading", err)
		return
	}
	d.Switch.Focus = true // the button is gone with the swap
	personal(w)
	setReadingCSP(w)
	s.web.Render(w, r, web.View{Page: "reading-panel", Fragment: "reading-panel", Data: d})
}

// subscription subscribes (or unsubscribes) the user to a catalog feed.
// htmx gets the refreshed control; a plain post goes back where it came
// from.
func (s *server) subscription(sub bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		fid, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			s.notFound(w, r)
			return
		}
		if !s.parseForm(w, r) {
			return
		}
		ctx, uid := r.Context(), userID(r)
		f, err := s.r.GetCatalogFeed(ctx, fid)
		if errors.Is(err, sql.ErrNoRows) {
			s.notFound(w, r)
			return
		}
		if err != nil {
			s.internalError(w, r, "get feed", err)
			return
		}
		if sub {
			err = s.w.Subscribe(ctx, db.SubscribeParams{UserID: uid, FeedID: fid, SubscribedAt: time.Now().Unix()})
		} else {
			err = s.w.Unsubscribe(ctx, db.UnsubscribeParams{UserID: uid, FeedID: fid})
		}
		if err != nil {
			s.internalError(w, r, "subscription", err)
			return
		}
		back := localBack(r, fmt.Sprintf("/feeds/%d", fid))
		if !isHTMX(r) {
			http.Redirect(w, r, back, http.StatusSeeOther)
			return
		}
		// Focus the button that replaced the clicked one.
		c := subControl{FeedID: fid, Name: feedName(f.Title, f.Url), Subscribed: sub, Back: back, Focus: true}
		personal(w)
		s.web.Render(w, r, web.View{Page: "sub-control", Fragment: "sub-control", Data: c})
	}
}

// subControl is the subscribe/unsubscribe button and its state, on the
// catalog and a feed's page.
type subControl struct {
	FeedID     int64
	Name       string
	Subscribed bool
	Unread     int64
	Back       string
	Focus      bool // htmx moves focus to the button after the swap
	// Plain drops htmx from the form: on a feed's page subscribing switches
	// the whole page between excerpts and the reader, so it must reload.
	Plain bool
}

// Secondary is true in the catalog, where every row has a Subscribe
// button: one accent action per view, so there they are plain buttons. The
// catalog is where back points, also in the htmx answer to a click there.
func (c subControl) Secondary() bool { return c.Back == "/feeds" }

// subscriptionsOf maps the signed-in user's subscriptions by feed id, nil
// when signed out.
func (s *server) subscriptionsOf(r *http.Request) (map[int64]subView, error) {
	u := voodooauth.UserFrom(r.Context())
	if u == nil {
		return nil, nil
	}
	rows, err := s.r.ListSubscriptions(r.Context(), db.ListSubscriptionsParams{UserID: u.ID, Since: since()})
	if err != nil {
		return nil, err
	}
	m := make(map[int64]subView, len(rows))
	for _, sr := range rows {
		m[sr.ID] = subView{ID: sr.ID, Unread: sr.Unread}
	}
	return m, nil
}

// subFor is the subscribe control for one feed, nil when signed out (subs
// is nil).
func subFor(subs map[int64]subView, feedID int64, name, back string) *subControl {
	if subs == nil {
		return nil
	}
	sv, ok := subs[feedID]
	return &subControl{FeedID: feedID, Name: name, Subscribed: ok, Unread: sv.Unread, Back: back}
}
