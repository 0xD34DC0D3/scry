package server

import (
	"database/sql"
	"errors"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/0xD34DC0D3/voodoo/voodooauth"

	"github.com/0xD34DC0D3/scry/internal/db"
	"github.com/0xD34DC0D3/scry/internal/web"
)

// The public views, open to everyone: Everything (the latest items across
// feeds; the home page signed out), the catalog, and one feed's items.
// Signed out, and on Everything for anyone, posts are excerpts (plain
// text) linking out. item_contents.html is for signed-in users: the reader
// (reading.go) on / and a subscribed feed's page, and a feed's page they
// do not subscribe to (full posts, no read state).

const (
	// window is how far back the lists reach, by posted time (see itemView).
	window = 30 * 24 * time.Hour
	// pageSize items per page; an "older" link fetches the next page.
	pageSize = 50
	// publicCache lets browsers (and any shared cache) reuse an anonymous
	// page for a minute: feeds sync hourly at most, so a minute is never
	// meaningfully stale, and it absorbs reload storms.
	publicCache = "public, max-age=60"
	// privateCache: a request carrying a credential gets a page naming the
	// user, which nothing may store.
	privateCache = "private, no-store"
)

func (s *server) publicRoutes(mux *http.ServeMux) {
	// {$} so only "/" matches; a bare "/" would answer every unknown path.
	mux.HandleFunc("GET /{$}", s.home)
	mux.HandleFunc("GET "+everythingPath, s.everything)
	mux.HandleFunc("GET "+feedsPath, s.catalog)
	mux.HandleFunc("GET /feeds/{id}", s.feedItems)
}

// itemView is one post, as every list renders it (the "post" template):
// a compact row, or the expanded card with the full body (Expanded). URL
// and CommentsURL go into href through html/template's normal escaping, so
// a javascript: link from a feed renders as #ZgotmplZ.
type itemView struct {
	ID, FeedID          int64
	URL, Title, Excerpt string
	Domain              string // URL's host unless it is the feed's own, for telling sources apart
	Author, CommentsURL string
	FeedName            string // empty on a feed's own page
	// Posted is the item's published date, clamped to no later than when
	// scry first saw it, or first-seen when the feed gave no date. Lists
	// sort, window and page by it (posted_at in queries.sql).
	Posted time.Time
	// PostedAgo is Posted as relative text ("3h ago"), computed at render
	// time. Pages are cached a minute at most, so minute precision holds;
	// computing it server-side needs no script, which the CSP forbids inline.
	PostedAgo string
	// HTML comes only from item_contents.html, which bluemonday's UGC
	// policy sanitized at ingest; nothing else may become template.HTML.
	// Empty on excerpt lists and for link-only or pruned items.
	HTML template.HTML
	// Tracked: the post is in one of the user's subscriptions, so it has
	// read state (Unread, Marked) and mark forms.
	Tracked bool
	Unread  bool
	Marked  bool   // has an item_reads row that "mark unread" can remove
	Back    string // where a plain (no-JS) mark post returns to
	// ScopeFeed is the page's feed filter, so a mark action's counts match it.
	ScopeFeed int64
	Focus     bool // htmx moves focus here after the swap
}

// Expanded is the card with the full body: a post with HTML, unless it is
// one the user has read (a read post collapses to a row with "Show post").
func (i itemView) Expanded() bool { return i.HTML != "" && (!i.Tracked || i.Unread) }

// Read is a tracked post the user has read.
func (i itemView) Read() bool { return i.Tracked && !i.Unread }

// Actions says whether a compact row has a control: "Show post" for a
// read body, or a mark form.
func (i itemView) Actions() bool { return i.Tracked && (i.HTML != "" || i.Unread || i.Marked) }

// agoPrecision is the finest unit ago names. Items want minutes; the
// catalog's last-post column wants days, where "3h ago" is noise.
type agoPrecision int

const (
	toMinute agoPrecision = iota
	toDay
)

// ago renders t relative to now, the one helper for every relative time
// on the public pages. Past a week, a relative count stops being useful, so
// it gives the date (with the year when it is not now's). Times in the
// future (clock skew between feeds and us) read as the newest label, and
// "yesterday" means 24-48 hours ago: the server does not know the reader's
// time zone, so UTC calendar days would be wrong for many of them. Empty
// for a zero time (a feed with no items).
func ago(t, now time.Time, p agoPrecision) string {
	if t.IsZero() {
		return ""
	}
	d := now.Sub(t)
	switch {
	case p == toDay && d < 24*time.Hour:
		return "today"
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d/time.Hour))
	case d < 48*time.Hour:
		return "yesterday"
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%d days ago", int(d/(24*time.Hour)))
	}
	t, now = t.UTC(), now.UTC()
	if t.Year() != now.Year() {
		return t.Format("Jan 2, 2006")
	}
	return t.Format("Jan 2")
}

// domain is the host a link leads to, without "www.". Computed at render
// time, so nothing stored needs a migration when the rule changes.
func domain(s string) string {
	u, err := url.Parse(s)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(u.Hostname(), "www.")
}

const (
	// titleMax is how much excerpt stands in for a missing title, and
	// urlMax how much of a URL: enough to recognize the story, short enough
	// that the heading stays one or two lines.
	titleMax = 80
	urlMax   = 60
)

// Heading is the text of the item's title line. An untitled item gets its
// excerpt's start, else its URL as host + path, so the line reads like a
// title rather than a raw link; "(untitled)" only when there is nothing.
func (i itemView) Heading() string {
	if t := strings.TrimSpace(i.Title); t != "" {
		return t
	}
	if e := strings.Join(strings.Fields(i.Excerpt), " "); e != "" {
		return ellipsize(e, titleMax)
	}
	if u := readableURL(i.URL); u != "" {
		return truncate(u, urlMax)
	}
	return "(untitled)"
}

// ShowExcerpt is false when the post has a full body (shown or behind
// "Show post" instead), or when the excerpt already serves, whole, as the
// heading, so the same words are not printed twice. A heading cut short from
// a longer excerpt leaves the full text below rather than losing the rest.
func (i itemView) ShowExcerpt() bool {
	if i.Excerpt == "" || i.HTML != "" {
		return false
	}
	if strings.TrimSpace(i.Title) != "" {
		return true
	}
	return len([]rune(strings.Join(strings.Fields(i.Excerpt), " "))) > titleMax
}

// ellipsize cuts s (single-spaced) to at most n runes on a word boundary and
// adds "…"; short text is returned unchanged. A first word longer than n is
// cut mid-word rather than leaving nothing.
func ellipsize(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	cut := n
	if r[n] != ' ' {
		for cut > 0 && r[cut-1] != ' ' {
			cut--
		}
		if cut == 0 {
			cut = n
		}
	}
	return strings.TrimRight(string(r[:cut]), " ,;:.-") + "…"
}

// truncate cuts s to at most n runes, ending in "…" when it cut.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// readableURL is an http(s) link as host and path, without scheme, "www.",
// query, fragment, or trailing slash; "" for anything else.
func readableURL(s string) string {
	u, err := url.Parse(s)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	return strings.TrimPrefix(u.Host, "www.") + strings.TrimRight(u.Path, "/")
}

// linkDomain is domain(link), or "" when it equals the host of any of own
// (the feed's URLs): on a site's own feed the domain says nothing, but on an
// aggregator it is the most useful field, and there it differs.
func linkDomain(link string, own ...string) string {
	d := domain(link)
	for _, o := range own {
		if d != "" && strings.EqualFold(d, domain(o)) {
			return ""
		}
	}
	return d
}

type feedView struct {
	ID          int64
	Name        string // title, or the URL when the feed has none yet
	Heading     string // Name for the feed page: the host instead of a bare URL
	URL         string
	SiteURL     string
	Paused      bool
	RecentItems int64
	LastPost    time.Time // zero when the feed has no items
	// Catalog only: SiteHost is SiteURL's host for display; LastPostLabel is
	// LastPost as a short, human date.
	SiteHost, LastPostLabel string
	Sub                     *subControl // the signed-in user's subscribe button; nil signed out
	// Admin is the feed's fetch health, set only for an admin's catalog:
	// failure details never reach anyone else's page, even by mistake in a
	// template.
	Admin *feedAdmin
	// Focus names the admin button ("resume", "pause", "sync") that takes
	// focus after an htmx action on this feed swapped the panel.
	Focus string
}

type feedAdmin struct {
	FailCount int64
	LastError string
}

// catalogData is /feeds. Admin adds the add form, each feed's status and
// its actions; URL, Error and Notice come from those actions.
type catalogData struct {
	Feeds  []feedView
	Admin  bool
	URL    string // the add form's value, kept when it was rejected
	Error  string
	Notice string
	// After an htmx action the clicked button is gone with the panel swap,
	// so one of these puts autofocus where focus belongs: FocusAdd on the
	// URL field after an add, FocusFeed's FocusAction button after a
	// pause, resume or sync, FocusHeading on the panel's heading after a
	// remove or a failed action (the row is gone).
	FocusAdd     bool
	FocusFeed    int64
	FocusAction  string
	FocusHeading bool
}

// postList is one page of posts and its Older link; every list page
// embeds it, and the "posts-page" fragment renders it.
type postList struct {
	Items []itemView
	Older string // next page's URL, empty on the last page
	// Shown and InWindow place the page in the window ("Showing 100 of
	// 312") next to the Older link. Shown counts every item down to this
	// page's last, so it is right in an htmx fragment that only appends a
	// page. Set only on the feed and Everything pages, with an Older link.
	Shown, InWindow int64
}

type itemsData struct {
	postList
	Feed *feedView // nil on Everything
	// Everything only: Switch is the home stream's view switch, for a
	// signed-in user with a subscription; Nudge says to subscribe, for one
	// without.
	Switch *viewSwitch
	Nudge  bool
}

func feedName(title, url string) string {
	if title != "" {
		return title
	}
	return url
}

// feedHeading is what the feed page shows as its title: the feed's title,
// else its host (a bare URL reads as noise in an h1). feedName stays the
// catalog and Everything label.
func feedHeading(title, url string) string {
	if title != "" {
		return title
	}
	if d := domain(url); d != "" {
		return d
	}
	return url
}

// cursor reads ?before=<posted_at>-<id>. The first page has none and
// starts above every row.
func cursor(r *http.Request) (at, id int64, ok bool) {
	v := r.URL.Query().Get("before")
	if v == "" {
		return math.MaxInt64, math.MaxInt64, true
	}
	a, b, found := strings.Cut(v, "-")
	if !found {
		return 0, 0, false
	}
	at, err1 := strconv.ParseInt(a, 10, 64)
	id, err2 := strconv.ParseInt(b, 10, 64)
	return at, id, err1 == nil && err2 == nil
}

// older returns the next page's URL when the query, asked for one row more
// than a page of size, filled it. path may carry a query already.
func older(path string, items []itemView, size int) ([]itemView, string) {
	if len(items) <= size {
		return items, ""
	}
	last := items[size-1]
	return items[:size], fmt.Sprintf("%s%sbefore=%d-%d", path, querySep(path), last.Posted.Unix(), last.ID)
}

func querySep(path string) string {
	if strings.Contains(path, "?") {
		return "&"
	}
	return "?"
}

// focusFirst gives an Older page's first post autofocus: htmx focuses it
// after the swap, so keyboard focus lands on the first new post instead of
// falling back to <body> when the clicked link is replaced.
func focusFirst(r *http.Request, items []itemView) {
	if isHTMX(r) && r.URL.Query().Get("before") != "" && len(items) > 0 {
		items[0].Focus = true
	}
}

// lastPost converts a last_post_at column; 0 means no items.
func lastPost(unix int64) time.Time {
	if unix == 0 {
		return time.Time{}
	}
	return time.Unix(unix, 0).UTC()
}

func since() int64 { return time.Now().Add(-window).Unix() }

// setCache marks a public page. Anything carrying a credential is never
// stored, even when voodoo rejected it or was down and the page came out
// anonymous. Vary on Cookie and Authorization so a stored anonymous page is
// never handed to a request that carries one (a browser that just signed
// in, say); Render adds Vary: HX-Request for fragment vs full page.
func setCache(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Add("Vary", "Cookie")
	h.Add("Vary", "Authorization")
	if hasCredential(r) || voodooauth.UserFrom(r.Context()) != nil {
		h.Set("Cache-Control", privateCache)
		return
	}
	h.Set("Cache-Control", publicCache)
}

func hasCredential(r *http.Request) bool {
	if ck, err := r.Cookie(voodooauth.CookieName); err == nil && ck.Value != "" {
		return true
	}
	return r.Header.Get("Authorization") != ""
}

// everything lists the newest items from every feed as excerpts: /everything,
// and / when there is no one to show Unread to. Its Older link always
// names /everything, which serves the same list either way.
func (s *server) everything(w http.ResponseWriter, r *http.Request) {
	s.renderEverything(w, r, false)
}

// renderEverything is Everything; noSubs says the caller already found the
// signed-in user has no subscriptions (home), so they are not listed again.
func (s *server) renderEverything(w http.ResponseWriter, r *http.Request, noSubs bool) {
	at, id, ok := cursor(r)
	if !ok {
		s.badRequest(w, r, "That page link is malformed.")
		return
	}
	cut := since()
	rows, err := s.r.ListLatestItems(r.Context(), db.ListLatestItemsParams{
		Since: cut, BeforeAt: at, BeforeID: id, Lim: pageSize + 1,
	})
	if err != nil {
		s.internalError(w, r, "list latest items", err)
		return
	}
	now := time.Now()
	items := make([]itemView, len(rows))
	for i, it := range rows {
		posted := time.Unix(it.PostedAt, 0).UTC()
		items[i] = itemView{
			ID: it.ID, URL: it.Url, Title: it.Title, Excerpt: it.Excerpt, FeedID: it.FeedID,
			Domain: linkDomain(it.Url, it.FeedUrl), Author: it.Author, CommentsURL: it.CommentsUrl,
			FeedName: feedName(it.FeedTitle, it.FeedUrl), Posted: posted, PostedAgo: ago(posted, now, toMinute),
		}
	}
	d := itemsData{}
	d.Items, d.Older = older(everythingPath, items, pageSize)
	focusFirst(r, d.Items)
	if d.Older != "" {
		last := d.Items[len(d.Items)-1]
		c, err := s.r.CountLatestItems(r.Context(), db.CountLatestItemsParams{
			Since: cut, BeforeAt: last.Posted.Unix(), BeforeID: last.ID,
		})
		if err != nil {
			s.internalError(w, r, "count latest items", err)
			return
		}
		d.Shown, d.InWindow = c.Total-c.Older, c.Total
	}
	title := "Latest"
	if voodooauth.UserFrom(r.Context()) != nil {
		var subs map[int64]subView
		if !noSubs {
			if subs, err = s.subscriptionsOf(r); err != nil {
				s.internalError(w, r, "list subscriptions", err)
				return
			}
		}
		if len(subs) == 0 {
			d.Nudge = true
		} else {
			var unread int64
			for _, sv := range subs {
				unread += sv.Unread
			}
			d.Switch = homeSwitch(everythingPath, unread)
			title = "Everything"
		}
	}
	setCache(w, r)
	s.web.Render(w, r, web.View{Page: "everything", Fragment: "posts-page", Title: title, Data: d, Nav: "home", NavSub: r.URL.Path != homePath})
}

func (s *server) catalog(w http.ResponseWriter, r *http.Request) {
	s.renderCatalog(w, r, catalogData{}, http.StatusOK)
}

// renderCatalog renders /feeds, or its "catalog-panel" fragment for htmx.
// htmx swaps only 2xx responses by default, so a rejected admin action
// answers it with 200 and the error inside the fragment; a plain post gets
// status on the full page. setCache keeps the admin view private: an admin
// request carries a credential.
func (s *server) renderCatalog(w http.ResponseWriter, r *http.Request, d catalogData, status int) {
	rows, err := s.r.ListCatalog(r.Context(), since())
	if err != nil {
		s.internalError(w, r, "list catalog", err)
		return
	}
	now := time.Now().UTC()
	subs, err := s.subscriptionsOf(r)
	if err != nil {
		s.internalError(w, r, "list subscriptions", err)
		return
	}
	u := voodooauth.UserFrom(r.Context())
	d.Admin = u != nil && u.IsAdmin
	feeds := make([]feedView, len(rows))
	for i, f := range rows {
		feeds[i] = feedView{
			ID: f.ID, Name: feedName(f.Title, f.Url), URL: f.Url, SiteURL: f.SiteUrl,
			Paused: f.DisabledAt.Valid, RecentItems: f.RecentItems,
			SiteHost: domain(f.SiteUrl), LastPost: lastPost(f.LastPostAt),
		}
		feeds[i].LastPostLabel = ago(feeds[i].LastPost, now, toDay)
		feeds[i].Sub = subFor(subs, f.ID, feeds[i].Name, feedsPath)
		if d.Admin {
			feeds[i].Admin = &feedAdmin{FailCount: f.FailCount, LastError: f.LastError}
			if f.ID == d.FocusFeed {
				feeds[i].Focus = d.FocusAction
			}
		}
	}
	// The user's own feeds first, each group in the query's (title) order.
	sort.SliceStable(feeds, func(i, j int) bool { return subscribed(feeds[i]) && !subscribed(feeds[j]) })
	d.Feeds = feeds
	setCache(w, r)
	// Admins get the wide measure for the status and action columns.
	v := web.View{Page: "catalog", Fragment: "catalog-panel", Title: "Feeds", Data: d, Nav: "feeds", Wide: d.Admin}
	if !isHTMX(r) {
		v.Status = status
	}
	s.web.Render(w, r, v)
}

func subscribed(f feedView) bool { return f.Sub != nil && f.Sub.Subscribed }

func (s *server) feedItems(w http.ResponseWriter, r *http.Request) {
	fid, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFound(w, r)
		return
	}
	at, id, ok := cursor(r)
	if !ok {
		s.badRequest(w, r, "That page link is malformed.")
		return
	}
	// A subscriber gets the reader scoped to this feed; another signed-in
	// user its full posts; anyone else its excerpts.
	signedIn := voodooauth.UserFrom(r.Context()) != nil
	if signedIn {
		q := readingQuery{feed: fid, all: r.URL.Query().Get("view") == "all"}
		d, found, err := s.readingData(r, q, at, id)
		if err != nil {
			s.internalError(w, r, "reading", err)
			return
		}
		if found {
			s.feedReader(w, r, d)
			return
		}
	}
	f, err := s.r.GetCatalogFeed(r.Context(), fid)
	if errors.Is(err, sql.ErrNoRows) {
		s.notFound(w, r)
		return
	}
	if err != nil {
		s.internalError(w, r, "get feed", err)
		return
	}
	path := fmt.Sprintf("/feeds/%d", fid)
	fv := &feedView{ID: f.ID, Name: feedName(f.Title, f.Url), URL: f.Url, SiteURL: f.SiteUrl, Paused: f.DisabledAt.Valid}
	fv.Heading = feedHeading(f.Title, f.Url)
	cut := since()
	now := time.Now()
	var items []itemView
	size := pageSize
	if signedIn {
		// readingData found no subscription to this feed, so the control
		// is Subscribe; posted plainly, since the page then turns into the
		// reader.
		fv.Sub = &subControl{FeedID: fid, Name: fv.Name, Back: path, Plain: true}
		// Full posts are as long as the reader's, so its page size.
		size = readingPageSize
		rows, err := s.r.ListFeedPosts(r.Context(), db.ListFeedPostsParams{
			FeedID: fid, Since: cut, BeforeAt: at, BeforeID: id, Lim: int64(size + 1),
		})
		if err != nil {
			s.internalError(w, r, "list feed posts", err)
			return
		}
		items = make([]itemView, len(rows))
		for i, it := range rows {
			items[i] = feedItem(it.ID, it.FeedID, it.Url, it.Title, it.Excerpt, it.Author, it.CommentsUrl, it.PostedAt, f, now)
			items[i].HTML = template.HTML(it.Html) // sanitized at ingest (see itemView)
		}
	} else {
		rows, err := s.r.ListFeedItems(r.Context(), db.ListFeedItemsParams{
			FeedID: fid, Since: cut, BeforeAt: at, BeforeID: id, Lim: int64(size + 1),
		})
		if err != nil {
			s.internalError(w, r, "list feed items", err)
			return
		}
		items = make([]itemView, len(rows))
		for i, it := range rows {
			items[i] = feedItem(it.ID, it.FeedID, it.Url, it.Title, it.Excerpt, it.Author, it.CommentsUrl, it.PostedAt, f, now)
		}
	}
	// Only an empty window needs it: say when a quiet feed last posted.
	if len(items) == 0 {
		last, err := s.r.GetFeedLastPostAt(r.Context(), fid)
		if err != nil {
			s.internalError(w, r, "get feed last post", err)
			return
		}
		fv.LastPost = lastPost(last)
	}
	d := itemsData{Feed: fv}
	d.Items, d.Older = older(path, items, size)
	focusFirst(r, d.Items)
	if d.Older != "" {
		last := d.Items[len(d.Items)-1]
		c, err := s.r.CountFeedItems(r.Context(), db.CountFeedItemsParams{
			FeedID: fid, Since: cut, BeforeAt: last.Posted.Unix(), BeforeID: last.ID,
		})
		if err != nil {
			s.internalError(w, r, "count feed items", err)
			return
		}
		d.Shown, d.InWindow = c.Total-c.Older, c.Total
	}
	setCache(w, r)
	if signedIn {
		setReadingCSP(w)
	}
	s.web.Render(w, r, web.View{Page: "feed", Fragment: "posts-page", Title: fv.Heading, Data: d, Nav: "feeds", NavSub: true})
}

// feedItem is one post on a feed's own page: no feed name (the page
// names it), the domain only when it is not the feed's own.
func feedItem(id, feedID int64, link, title, excerpt, author, comments string, posted int64, f db.GetCatalogFeedRow, now time.Time) itemView {
	t := time.Unix(posted, 0).UTC()
	return itemView{
		ID: id, FeedID: feedID, URL: link, Title: title, Excerpt: excerpt,
		Domain: linkDomain(link, f.SiteUrl, f.Url), Author: author, CommentsURL: comments,
		Posted: t, PostedAgo: ago(t, now, toMinute),
	}
}

// feedReader renders a subscribed feed's page: the feed's header over its
// reading list.
func (s *server) feedReader(w http.ResponseWriter, r *http.Request, d readingData) {
	f, err := s.r.GetCatalogFeed(r.Context(), d.Feed.ID)
	if errors.Is(err, sql.ErrNoRows) {
		s.notFound(w, r)
		return
	}
	if err != nil {
		s.internalError(w, r, "get feed", err)
		return
	}
	fv := &feedView{ID: f.ID, Name: feedName(f.Title, f.Url), URL: f.Url, SiteURL: f.SiteUrl, Paused: f.DisabledAt.Valid}
	fv.Heading = feedHeading(f.Title, f.Url)
	// No unread count here: the switch carries it, and stays current.
	fv.Sub = &subControl{FeedID: f.ID, Name: fv.Name, Subscribed: true, Back: fmt.Sprintf("/feeds/%d", f.ID), Plain: true}
	d.Header = fv
	s.renderReading(w, r, d, fv.Heading)
}
