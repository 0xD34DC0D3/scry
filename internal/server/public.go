package server

import (
	"database/sql"
	"errors"
	"fmt"
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

// The public views, open to everyone: latest items across feeds, the
// catalog, and one feed's items. Excerpts only (plain text), linking out;
// item_contents.html is for signed-in readers (step 6).

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
	mux.HandleFunc("GET /{$}", s.latest)
	mux.HandleFunc("GET /feeds", s.catalog)
	mux.HandleFunc("GET /feeds/{id}", s.feedItems)
}

// itemView is one list entry. URL and CommentsURL go into href through
// html/template's normal escaping, so a javascript: link from a feed renders
// as #ZgotmplZ.
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
}

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

// ShowExcerpt is false only when the excerpt already serves, whole, as the
// heading, so the same words are not printed twice. A heading cut short from
// a longer excerpt leaves the full text below rather than losing the rest.
func (i itemView) ShowExcerpt() bool {
	if i.Excerpt == "" {
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
}

type itemsData struct {
	Feed  *feedView // nil on the latest page
	Items []itemView
	Older string // next page's URL, empty on the last page
}

func feedName(title, url string) string {
	if title != "" {
		return title
	}
	return url
}

// feedHeading is what the feed page shows as its title: the feed's title,
// else its host (a bare URL reads as noise in an h1). feedName stays the
// catalog and Latest label.
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
// than a page, filled it.
func older(path string, items []itemView) ([]itemView, string) {
	if len(items) <= pageSize {
		return items, ""
	}
	last := pageSize - 1
	return items[:pageSize], fmt.Sprintf("%s?before=%d-%d", path, items[last].Posted.Unix(), items[last].ID)
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

func (s *server) latest(w http.ResponseWriter, r *http.Request) {
	at, id, ok := cursor(r)
	if !ok {
		s.badRequest(w, r, "That page link is malformed.")
		return
	}
	rows, err := s.r.ListLatestItems(r.Context(), db.ListLatestItemsParams{
		Since: since(), BeforeAt: at, BeforeID: id, Lim: pageSize + 1,
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
	d.Items, d.Older = older("/", items)
	setCache(w, r)
	s.web.Render(w, r, web.View{Page: "latest", Fragment: "items-page", Title: "Latest", Data: d, Nav: "latest"})
}

func (s *server) catalog(w http.ResponseWriter, r *http.Request) {
	rows, err := s.r.ListCatalog(r.Context(), since())
	if err != nil {
		s.internalError(w, r, "list catalog", err)
		return
	}
	now := time.Now().UTC()
	feeds := make([]feedView, len(rows))
	for i, f := range rows {
		feeds[i] = feedView{
			ID: f.ID, Name: feedName(f.Title, f.Url), URL: f.Url, SiteURL: f.SiteUrl,
			Paused: f.DisabledAt.Valid, RecentItems: f.RecentItems,
			SiteHost: domain(f.SiteUrl), LastPost: lastPost(f.LastPostAt),
		}
		feeds[i].LastPostLabel = ago(feeds[i].LastPost, now, toDay)
	}
	setCache(w, r)
	s.web.Render(w, r, web.View{Page: "catalog", Title: "Feeds", Data: feeds, Nav: "feeds"})
}

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
	f, err := s.r.GetCatalogFeed(r.Context(), fid)
	if errors.Is(err, sql.ErrNoRows) {
		s.notFound(w, r)
		return
	}
	if err != nil {
		s.internalError(w, r, "get feed", err)
		return
	}
	rows, err := s.r.ListFeedItems(r.Context(), db.ListFeedItemsParams{
		FeedID: fid, Since: since(), BeforeAt: at, BeforeID: id, Lim: pageSize + 1,
	})
	if err != nil {
		s.internalError(w, r, "list feed items", err)
		return
	}
	now := time.Now()
	items := make([]itemView, len(rows))
	for i, it := range rows {
		posted := time.Unix(it.PostedAt, 0).UTC()
		items[i] = itemView{
			ID: it.ID, URL: it.Url, Title: it.Title, Excerpt: it.Excerpt, FeedID: it.FeedID,
			Domain: linkDomain(it.Url, f.SiteUrl, f.Url), Author: it.Author, CommentsURL: it.CommentsUrl,
			Posted: posted, PostedAgo: ago(posted, now, toMinute),
		}
	}
	fv := &feedView{ID: f.ID, Name: feedName(f.Title, f.Url), URL: f.Url, SiteURL: f.SiteUrl, Paused: f.DisabledAt.Valid}
	fv.Heading = feedHeading(f.Title, f.Url)
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
	d.Items, d.Older = older(fmt.Sprintf("/feeds/%d", fid), items)
	setCache(w, r)
	s.web.Render(w, r, web.View{Page: "feed", Fragment: "items-page", Title: fv.Heading, Data: d, Nav: "feeds", NavSub: true})
}
