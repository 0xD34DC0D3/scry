package server

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/0xD34DC0D3/scry/internal/db"
)

// syncItems writes items the way a sync does (internal/jobs store): one
// transaction upserting each item and its content with first_seen_at =
// the sync's now. An item already stored keeps its id and first_seen_at.
func syncItems(t *testing.T, d *db.DB, feedID int64, now int64, keys ...string) map[string]int64 {
	t.Helper()
	ctx := context.Background()
	tx, err := d.Writer.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	q := db.New(d.Writer).WithTx(tx)
	ids := map[string]int64{}
	for _, k := range keys {
		id, err := q.UpsertItem(ctx, db.UpsertItemParams{
			FeedID: feedID, DedupKey: k, Url: "https://post.example/" + k, Title: "Post " + k,
			FirstSeenAt: now,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := q.UpsertItemContent(ctx, db.UpsertItemContentParams{
			ItemID: id, Html: "<p>Body of " + k + "</p>", Excerpt: "Body of " + k, FetchedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
		ids[k] = id
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return ids
}

var (
	totalRe = regexp.MustCompile(`<span id="unread-total" class="n">(\d+)</span>`)
	uptoRe  = regexp.MustCompile(`name="upto" value="(\d+)"`)
)

// readingState fetches the reading page at path
// and returns the page, its total unread count and its upto watermark.
func readingState(t *testing.T, h http.Handler, path string) (body string, total int, upto string) {
	t.Helper()
	rec := do(h, get(path, "user", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", path, rec.Code, rec.Body)
	}
	body = rec.Body.String()
	m := totalRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no unread total in %s", body)
	}
	total, _ = strconv.Atoi(m[1])
	if u := uptoRe.FindStringSubmatch(body); u != nil {
		upto = u[1]
	}
	return body, total, upto
}

func feedCount(body string, feedID int64) string {
	m := regexp.MustCompile(fmt.Sprintf(`<span id="unread-feed-%d" class="count">(\d*)</span>`, feedID)).FindStringSubmatch(body)
	if m == nil {
		return "missing"
	}
	return m[1]
}

func wantCounts(t *testing.T, h http.Handler, total int, perFeed map[int64]string) {
	t.Helper()
	body, got, _ := readingState(t, h, homePath)
	if got != total {
		t.Errorf("unread total = %d, want %d", got, total)
	}
	for id, want := range perFeed {
		if c := feedCount(body, id); c != want {
			t.Errorf("feed %d unread = %q, want %q", id, c, want)
		}
	}
}

func post(t *testing.T, h http.Handler, path string, vals url.Values, hdr map[string]string) *http.Response {
	t.Helper()
	rec := do(h, postForm(path, vals, "user", hdr))
	return rec.Result()
}

// The milestone's done check: a user subscribes to two feeds, reads items,
// marks all read, and the unread count is right after a new sync.
func TestReadingDone(t *testing.T) {
	h, d := newTestServer(t, nil)
	a := seedFeed(t, d, "https://a.example/feed")
	b := seedFeed(t, d, "https://b.example/feed")
	before := time.Now().Add(-2 * time.Hour).Unix()
	syncItems(t, d, a.ID, before, "a0") // seen before the subscription

	for _, f := range []db.Feed{a, b} {
		res := post(t, h, fmt.Sprintf("/feeds/%d/subscribe", f.ID), url.Values{"back": {"/feeds"}}, nil)
		if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/feeds" {
			t.Fatalf("subscribe: %d %q", res.StatusCode, res.Header.Get("Location"))
		}
	}
	// Items from before the subscription are not unread.
	wantCounts(t, h, 0, map[int64]string{a.ID: "", b.ID: ""})
	// Every sync below is stamped in the past (eligibility is by item id,
	// not time), so "mark all read" up to the click's own time would
	// wrongly cover sync 2.

	// Sync 1: new items in both feeds; a0 comes again and stays old.
	t1 := time.Now().Unix() - 1800
	ia := syncItems(t, d, a.ID, t1, "a0", "a1", "a2")
	ib := syncItems(t, d, b.ID, t1, "b1")
	wantCounts(t, h, 3, map[int64]string{a.ID: "2", b.ID: "1"})

	// Read a1 with htmx: the entry comes back read, counts out of band.
	rec := do(h, postForm(fmt.Sprintf("/items/%d/read", ia["a1"]), url.Values{"back": {"/"}}, "user", hx))
	frag := rec.Body.String()
	if rec.Code != http.StatusOK || strings.Contains(frag, "<html") ||
		!strings.Contains(frag, `class="entry read"`) || !strings.Contains(frag, "Mark unread") ||
		!strings.Contains(frag, `<span id="unread-total" class="n" hx-swap-oob="true">2</span>`) ||
		!strings.Contains(frag, fmt.Sprintf(`<span id="unread-feed-%d" class="count" hx-swap-oob="true">1</span>`, a.ID)) {
		t.Fatalf("htmx mark read: %d %s", rec.Code, frag)
	}
	// Read b1 with a plain form: 303 back to the list.
	res := post(t, h, fmt.Sprintf("/items/%d/read", ib["b1"]), url.Values{"back": {"/#item-5"}}, nil)
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/#item-5" {
		t.Fatalf("plain mark read: %d %q", res.StatusCode, res.Header.Get("Location"))
	}
	wantCounts(t, h, 1, map[int64]string{a.ID: "1", b.ID: ""})

	// The page is rendered; then sync 2 lands before "mark all read" is
	// clicked. What arrived after the render must stay unread.
	_, _, upto := readingState(t, h, homePath)
	t2 := t1 + 600
	syncItems(t, d, a.ID, t2, "a0", "a1", "a2", "a3")
	syncItems(t, d, b.ID, t2, "b1", "b2")
	res = post(t, h, "/mark-all-read", url.Values{"upto": {upto}}, nil)
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != homePath {
		t.Fatalf("mark all read: %d %q", res.StatusCode, res.Header.Get("Location"))
	}
	wantCounts(t, h, 2, map[int64]string{a.ID: "1", b.ID: "1"})

	// Sync 3 refetches everything and brings one new item.
	t3 := t2 + 600
	syncItems(t, d, a.ID, t3, "a0", "a1", "a2", "a3", "a4")
	syncItems(t, d, b.ID, t3, "b1", "b2")
	wantCounts(t, h, 3, map[int64]string{a.ID: "2", b.ID: "1"})

	// Mark all read for one feed only, with htmx: the panel comes back.
	_, _, upto = readingState(t, h, homePath)
	rec = do(h, postForm("/mark-all-read", url.Values{"upto": {upto}, "feed": {strconv.FormatInt(b.ID, 10)}}, "user", hx))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `<div id="reading-panel">`) || strings.Contains(rec.Body.String(), "<html") {
		t.Fatalf("htmx mark all read: %d %s", rec.Code, rec.Body)
	}
	wantCounts(t, h, 2, map[int64]string{a.ID: "2", b.ID: ""})

	// And everything.
	_, _, upto = readingState(t, h, homePath)
	post(t, h, "/mark-all-read", url.Values{"upto": {upto}}, nil)
	wantCounts(t, h, 0, map[int64]string{a.ID: "", b.ID: ""})
	if body, _, _ := readingState(t, h, homePath); !strings.Contains(body, "All caught up") {
		t.Errorf("no caught-up state: %s", body)
	}
}

// A stale watermark never moves read_before_at backwards.
func TestMarkAllReadNeverBackwards(t *testing.T) {
	h, d := newTestServer(t, nil)
	a := seedFeed(t, d, "https://a.example/feed")
	post(t, h, fmt.Sprintf("/feeds/%d/subscribe", a.ID), nil, nil)
	t1 := time.Now().Unix() + 10
	syncItems(t, d, a.ID, t1, "a1")
	_, _, early := readingState(t, h, homePath)
	syncItems(t, d, a.ID, t1+10, "a2")
	_, _, late := readingState(t, h, homePath)
	post(t, h, "/mark-all-read", url.Values{"upto": {late}}, nil)
	post(t, h, "/mark-all-read", url.Values{"upto": {early}}, nil)
	wantCounts(t, h, 0, nil)
}

func TestReadingUnreadView(t *testing.T) {
	h, d := newTestServer(t, nil)
	a := seedFeed(t, d, "https://a.example/feed")
	post(t, h, fmt.Sprintf("/feeds/%d/subscribe", a.ID), nil, nil)
	ids := syncItems(t, d, a.ID, time.Now().Unix()+10, "a1", "a2")
	post(t, h, fmt.Sprintf("/items/%d/read", ids["a1"]), nil, nil)

	unread := do(h, get(homePath, "user", nil)).Body.String()
	if strings.Contains(unread, "Post a1") || !strings.Contains(unread, "Post a2") || !strings.Contains(unread, "<p>Body of a2</p>") {
		t.Errorf("unread view: %s", unread)
	}
	all := do(h, get(followingPath, "user", nil)).Body.String()
	if !strings.Contains(all, "Post a1") || !strings.Contains(all, "Post a2") || !strings.Contains(all, "Show post") {
		t.Errorf("all view: %s", all)
	}

	// Mark unread brings it back.
	res := post(t, h, fmt.Sprintf("/items/%d/unread", ids["a1"]), url.Values{"back": {"/following"}}, nil)
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/following" {
		t.Fatalf("mark unread: %d %q", res.StatusCode, res.Header.Get("Location"))
	}
	wantCounts(t, h, 2, nil)
}

// A subscribed feed's page is the reader scoped to that feed; anyone else
// gets its excerpts.
func TestReadingFeedPage(t *testing.T) {
	h, d := newTestServer(t, nil)
	a := seedFeed(t, d, "https://a.example/feed")
	b := seedFeed(t, d, "https://b.example/feed")
	other := seedFeed(t, d, "https://c.example/feed")
	for _, f := range []db.Feed{a, b} {
		post(t, h, fmt.Sprintf("/feeds/%d/subscribe", f.ID), nil, nil)
	}
	now := time.Now().Unix() + 10
	syncItems(t, d, a.ID, now, "a1")
	syncItems(t, d, b.ID, now, "b1")
	syncItems(t, d, other.ID, now, "c1")

	feedA := fmt.Sprintf("/feeds/%d", a.ID)
	body := do(h, get(feedA, "user", nil)).Body.String()
	for _, w := range []string{
		"Post a1", "<p>Body of a1</p>", `<div id="reading-panel">`,
		fmt.Sprintf(`name="feed" value="%d"`, a.ID), // mark-all and item forms carry the scope
		`<a href="/feeds">← Feeds</a>`, `readonly value="https://a.example/feed"`,
		fmt.Sprintf(`action="/feeds/%d/unsubscribe">`, a.ID), // a plain post: the page changes mode
		`<a href="` + feedA + `" aria-current="page">Unread <span id="unread-total" class="n">1</span></a>`,
		`<a href="` + feedA + `?view=all">All</a>`,
		`<a href="/feeds" aria-current="true">feeds</a>`,
	} {
		if !strings.Contains(body, w) {
			t.Errorf("subscribed feed page lacks %q", w)
		}
	}
	for _, n := range []string{"Post b1", "read →", "feed page", "All feeds", "/reading", "subscribed ·", "Following"} {
		if strings.Contains(body, n) {
			t.Errorf("subscribed feed page has %q", n)
		}
	}
	// Not subscribed: excerpts and a Subscribe button, no reader.
	rec := do(h, get(fmt.Sprintf("/feeds/%d?view=all", other.ID), "user", nil))
	body = rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "Post c1") || strings.Contains(body, "<p>Body of c1</p>") ||
		strings.Contains(body, "reading-panel") || !strings.Contains(body, fmt.Sprintf(`action="/feeds/%d/subscribe">`, other.ID)) {
		t.Errorf("unsubscribed feed page: %d %s", rec.Code, body)
	}
	for _, p := range []string{"/feeds/x", "/feeds/-1", "/feeds/99999"} {
		if rec := do(h, get(p, "user", nil)); rec.Code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", p, rec.Code)
		}
	}
	for _, p := range []string{"/?before=nope", "/following?before=nope", feedA + "?before=nope", feedA + "?view=all&before=1-"} {
		if rec := do(h, get(p, "user", nil)); rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", p, rec.Code)
		}
	}
}

// The home stream: one switch narrows it, the unread count in its label;
// the chips link to each feed's page.
func TestHomeStream(t *testing.T) {
	h, d := newTestServer(t, nil)
	a := seedFeed(t, d, "https://a.example/feed")
	b := seedFeed(t, d, "https://b.example/feed")
	if _, err := d.Writer.Exec(`UPDATE feeds SET title = 'Alpha' WHERE id = ?`, a.ID); err != nil {
		t.Fatal(err)
	}
	post(t, h, fmt.Sprintf("/feeds/%d/subscribe", a.ID), nil, nil)
	now := time.Now().Unix() + 10
	ia := syncItems(t, d, a.ID, now, "a1", "a2")
	syncItems(t, d, b.ID, now, "b1")
	post(t, h, fmt.Sprintf("/items/%d/read", ia["a1"]), nil, nil)

	seg := func(href, label string, current bool) string {
		c := ""
		if current {
			c = ` aria-current="page"`
		}
		return `<a href="` + href + `"` + c + `>` + label
	}
	unread := `Unread <span id="unread-total" class="n">1</span></a>`
	chip := fmt.Sprintf(`<a href="/feeds/%d"><span class="name">Alpha</span>`, a.ID)
	tests := []struct {
		path      string
		want, not []string
	}{
		{"/", []string{seg("/", unread, true), seg("/following", "Following", false), seg("/everything", "Everything", false),
			chip, "Post a2", "<p>Body of a2</p>", `<h1 class="vh">Unread</h1>`, `<a class="app" href="/" aria-current="page">scry</a>`},
			[]string{"Post a1", "Post b1", "All feeds", "Sign in"}},
		{"/following", []string{seg("/", unread, false), seg("/following", "Following", true), "Post a1", "Post a2",
			fmt.Sprintf(`<a href="/feeds/%d?view=all"><span class="name">Alpha</span>`, a.ID), `<a class="app" href="/" aria-current="true">scry</a>`},
			[]string{"Post b1"}},
		{"/everything", []string{seg("/", unread, false), seg("/everything", "Everything", true), "Post a1", "Post b1", "Body of b1", `<ol class="items">`},
			[]string{"<p>Body of", "Mark all read", "subscribed to any", "Sign in"}},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			rec := do(h, get(tc.path, "user", nil))
			body := rec.Body.String()
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d", rec.Code)
			}
			for _, w := range tc.want {
				if !strings.Contains(body, w) {
					t.Errorf("missing %q", w)
				}
			}
			for _, n := range tc.not {
				if strings.Contains(body, n) {
					t.Errorf("unexpected %q", n)
				}
			}
			for _, n := range []string{`href="/reading`, ">reading</a>", ">latest</a>"} {
				if strings.Contains(body, n) {
					t.Errorf("unexpected %q", n)
				}
			}
		})
	}
	// Signed out, / is Everything: no switch, the sign-in note.
	body := do(h, get("/", "", nil)).Body.String()
	if strings.Contains(body, `class="seg"`) || !strings.Contains(body, "to subscribe and read full posts") || !strings.Contains(body, "Post b1") || !strings.Contains(body, "<h1>Latest</h1>") {
		t.Errorf("signed-out home: %s", body)
	}
}

// The removed /reading routes are gone, not redirected.
func TestReadingRoutesGone(t *testing.T) {
	h, _ := newTestServer(t, nil)
	for _, p := range []string{"/reading", "/reading?view=all"} {
		if rec := do(h, get(p, "user", nil)); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", p, rec.Code)
		}
	}
	for _, p := range []string{"/reading/mark-all-read", "/reading/items/1/read"} {
		if rec := do(h, postForm(p, url.Values{"upto": {"1"}}, "user", nil)); rec.Code != http.StatusNotFound {
			t.Errorf("POST %s = %d, want 404", p, rec.Code)
		}
	}
}

func TestReadingAuth(t *testing.T) {
	h, d := newTestServer(t, nil)
	a := seedFeed(t, d, "https://a.example/feed")
	// A browser page load goes to sign in; anything else is 401.
	if rec := do(h, get(followingPath, "", map[string]string{"Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document"})); rec.Code != http.StatusFound {
		t.Errorf("anonymous page load = %d, want 302", rec.Code)
	}
	for _, p := range []string{
		fmt.Sprintf("/feeds/%d/subscribe", a.ID), fmt.Sprintf("/feeds/%d/unsubscribe", a.ID),
		"/mark-all-read", "/items/1/read", "/items/1/unread",
	} {
		if rec := do(h, postForm(p, nil, "", nil)); rec.Code != http.StatusUnauthorized {
			t.Errorf("anonymous POST %s = %d, want 401", p, rec.Code)
		}
	}
}

func TestReadingErrors(t *testing.T) {
	h, d := newTestServer(t, nil)
	a := seedFeed(t, d, "https://a.example/feed")
	b := seedFeed(t, d, "https://b.example/feed")
	post(t, h, fmt.Sprintf("/feeds/%d/subscribe", a.ID), nil, nil)
	ib := syncItems(t, d, b.ID, time.Now().Unix(), "b1")
	tests := []struct {
		name string
		path string
		vals url.Values
		want int
	}{
		{"subscribe unknown feed", "/feeds/99999/subscribe", nil, http.StatusNotFound},
		{"subscribe bad id", "/feeds/x/subscribe", nil, http.StatusNotFound},
		{"read unknown item", "/items/99999/read", nil, http.StatusNotFound},
		{"read item of unsubscribed feed", fmt.Sprintf("/items/%d/read", ib["b1"]), nil, http.StatusNotFound},
		{"mark all without upto", "/mark-all-read", nil, http.StatusBadRequest},
		{"mark all bad feed", "/mark-all-read", url.Values{"upto": {"1"}, "feed": {"x"}}, http.StatusBadRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(h, postForm(tc.path, tc.vals, "user", nil))
			if rec.Code != tc.want {
				t.Errorf("= %d, want %d", rec.Code, tc.want)
			}
			if !strings.Contains(rec.Body.String(), "<html") {
				t.Errorf("error page is not HTML: %s", rec.Body)
			}
		})
	}
	// No read row was written for the unsubscribed feed's item.
	var n int
	if err := d.Reader.QueryRow(`SELECT count(*) FROM item_reads`).Scan(&n); err != nil || n != 0 {
		t.Errorf("item_reads = %d (%v), want 0", n, err)
	}
}

// back must stay on this site.
func TestLocalBack(t *testing.T) {
	tests := map[string]string{
		"/feeds/3?view=all&before=1-2#item-3": "/feeds/3?view=all&before=1-2#item-3",
		"/#item-3":                            "/#item-3",
		"/feeds":                              "/feeds",
		"":                                    "/def",
		"//evil.example/x":                    "/def",
		"/\\evil.example":                     "/def",
		"https://evil.example/":               "/def",
		"following":                           "/def",
		"/a\r\nLocation: x":                   "/def",
	}
	for in, want := range tests {
		r := postForm("/x", url.Values{"back": {in}}, "", nil)
		if got := localBack(r, "/def"); got != want {
			t.Errorf("localBack(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSubscribeControls(t *testing.T) {
	h, d := newTestServer(t, nil)
	a := seedFeed(t, d, "https://a.example/feed")
	feedPage := fmt.Sprintf("/feeds/%d", a.ID)
	sub := fmt.Sprintf(`action="/feeds/%d/subscribe"`, a.ID)
	unsub := fmt.Sprintf(`action="/feeds/%d/unsubscribe"`, a.ID)

	// Signed out: no buttons, a sign-in prompt.
	for _, p := range []string{"/feeds", feedPage} {
		body := do(h, get(p, "", nil)).Body.String()
		if strings.Contains(body, sub) || !strings.Contains(body, "to subscribe and read full posts") {
			t.Errorf("signed out %s: %s", p, body)
		}
	}
	// Signed in: a Subscribe button on both; htmx in the catalog, a plain
	// post on the feed's page (subscribing turns it into the reader).
	for p, htmx := range map[string]bool{"/feeds": true, feedPage: false} {
		body := do(h, get(p, "user", nil)).Body.String()
		if !strings.Contains(body, sub) || strings.Contains(body, fmt.Sprintf(`hx-post="/feeds/%d/subscribe"`, a.ID)) != htmx {
			t.Errorf("signed in %s: %s", p, body)
		}
	}
	// htmx subscribe answers with the control alone, now Unsubscribe.
	rec := do(h, postForm(fmt.Sprintf("/feeds/%d/subscribe", a.ID), url.Values{"back": {feedPage}}, "user", hx))
	frag := rec.Body.String()
	if rec.Code != http.StatusOK || strings.Contains(frag, "<html") || !strings.Contains(frag, unsub) || rec.Header().Get("Cache-Control") != privateCache {
		t.Fatalf("htmx subscribe: %d %s", rec.Code, frag)
	}
	if body := do(h, get("/feeds", "user", nil)).Body.String(); !strings.Contains(body, unsub) {
		t.Errorf("catalog after subscribe: %s", body)
	}
	// Subscribing twice is fine; unsubscribing removes the row.
	post(t, h, fmt.Sprintf("/feeds/%d/subscribe", a.ID), nil, nil)
	res := post(t, h, fmt.Sprintf("/feeds/%d/unsubscribe", a.ID), url.Values{"back": {"//evil.example"}}, nil)
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != feedPage {
		t.Errorf("unsubscribe: %d %q", res.StatusCode, res.Header.Get("Location"))
	}
	var n int
	if err := d.Reader.QueryRow(`SELECT count(*) FROM subscriptions`).Scan(&n); err != nil || n != 0 {
		t.Errorf("subscriptions = %d (%v), want 0", n, err)
	}
}

func TestReadingHeaders(t *testing.T) {
	h, d := newTestServer(t, nil)
	a := seedFeed(t, d, "https://a.example/feed")
	b := seedFeed(t, d, "https://b.example/feed")
	post(t, h, fmt.Sprintf("/feeds/%d/subscribe", a.ID), nil, nil)
	feedA, feedB := fmt.Sprintf("/feeds/%d", a.ID), fmt.Sprintf("/feeds/%d", b.ID)
	// The personal views: never stored, and article images allowed.
	for _, p := range []string{homePath, followingPath, feedA, feedA + "?view=all"} {
		for _, hdr := range []map[string]string{nil, hx} {
			rec := do(h, get(p, "user", hdr))
			if cc := rec.Header().Get("Cache-Control"); cc != privateCache {
				t.Errorf("%s Cache-Control = %q", p, cc)
			}
			vary := strings.Join(rec.Header().Values("Vary"), ",")
			for _, v := range []string{"HX-Request", "Cookie", "Authorization"} {
				if !strings.Contains(vary, v) {
					t.Errorf("%s Vary = %q, lacks %s", p, vary, v)
				}
			}
			csp := rec.Header().Get("Content-Security-Policy")
			if !strings.Contains(csp, "img-src 'self' https:") || !strings.Contains(csp, "script-src 'self'") || strings.Contains(csp, "unsafe-inline") {
				t.Errorf("%s reading CSP = %q", p, csp)
			}
		}
	}
	// Excerpt pages keep images to 'self', signed in or not.
	for _, c := range []struct{ path, cookie string }{
		{everythingPath, "user"}, {"/feeds", "user"}, {feedB, "user"}, {homePath, ""}, {feedA, ""},
	} {
		if csp := do(h, get(c.path, c.cookie, nil)).Header().Get("Content-Security-Policy"); strings.Contains(csp, "img-src") {
			t.Errorf("%s (%q) CSP widened: %q", c.path, c.cookie, csp)
		}
	}
}

// Content is rendered as stored (it was sanitized at ingest); titles and
// URLs are escaped; an item whose content was pruned still lists.
func TestReadingContent(t *testing.T) {
	h, d := newTestServer(t, nil)
	a := seedFeed(t, d, "https://a.example/feed")
	post(t, h, fmt.Sprintf("/feeds/%d/subscribe", a.ID), nil, nil)
	now := time.Now().Unix() + 10
	q := db.New(d.Writer)
	ctx := context.Background()
	evil, err := q.UpsertItem(ctx, db.UpsertItemParams{FeedID: a.ID, DedupKey: "evil", Url: "javascript:alert(1)", Title: "<script>x</script>", FirstSeenAt: now})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.UpsertItemContent(ctx, db.UpsertItemContentParams{ItemID: evil, Html: `<p>Kept <em>markup</em></p>`, FetchedAt: now}); err != nil {
		t.Fatal(err)
	}
	ids := syncItems(t, d, a.ID, now, "pruned")
	if _, err := d.Writer.Exec(`DELETE FROM item_contents WHERE item_id = ?`, ids["pruned"]); err != nil {
		t.Fatal(err)
	}

	body := do(h, get(homePath, "user", nil)).Body.String()
	for _, want := range []string{
		`<div class="prose"><p>Kept <em>markup</em></p></div>`,
		`&lt;script&gt;x&lt;/script&gt;`,
		`href="#ZgotmplZ"`,
		"Post pruned",
		`class="entry unread link-only"`,
		`aria-describedby="title-`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("reading page lacks %q", want)
		}
	}
	if strings.Contains(body, "<script>x") {
		t.Error("title not escaped")
	}
	// Full HTML stays off the excerpt pages, for anyone.
	for _, c := range []struct{ path, cookie string }{
		{"/", ""}, {everythingPath, ""}, {everythingPath, "user"}, {fmt.Sprintf("/feeds/%d", a.ID), ""},
	} {
		if pub := do(h, get(c.path, c.cookie, nil)).Body.String(); strings.Contains(pub, "<em>markup</em>") {
			t.Errorf("%s (%q) shows full HTML", c.path, c.cookie)
		}
	}
}

func TestReadingPaging(t *testing.T) {
	h, d := newTestServer(t, nil)
	a := seedFeed(t, d, "https://a.example/feed")
	post(t, h, fmt.Sprintf("/feeds/%d/subscribe", a.ID), nil, nil)
	now := time.Now().Unix() + 10
	keys := make([]string, readingPageSize+5)
	for i := range keys {
		keys[i] = fmt.Sprintf("k%02d", i)
	}
	syncItems(t, d, a.ID, now, keys...) // one first_seen_at for all: ties broken by id

	feedA := fmt.Sprintf("/feeds/%d", a.ID)
	// Every view pages by its own cursor, keeping its scope in the link.
	for _, path := range []string{homePath, followingPath, feedA, feedA + "?view=all"} {
		t.Run(path, func(t *testing.T) {
			seen := map[string]bool{}
			body := do(h, get(path, "user", nil)).Body.String()
			m := olderRe.FindStringSubmatch(body)
			if m == nil {
				t.Fatalf("no Older link: %s", body)
			}
			older := strings.ReplaceAll(m[1], "&amp;", "&")
			sep := "?"
			if strings.Contains(path, "?") {
				sep = "&"
			}
			if !strings.HasPrefix(older, path+sep+"before=") {
				t.Errorf("Older link drops the scope: %s", older)
			}
			for _, k := range keys {
				if strings.Contains(body, "Post "+k+"<") {
					seen[k] = true
				}
			}
			frag := do(h, get(older, "user", hx)).Body.String()
			if strings.Contains(frag, "<html") || !strings.Contains(frag, "autofocus") || !strings.Contains(frag, "end of 30 days") {
				t.Errorf("Older fragment: %s", frag)
			}
			if strings.Count(frag, "autofocus") != 1 {
				t.Errorf("autofocus count = %d", strings.Count(frag, "autofocus"))
			}
			for _, k := range keys {
				if strings.Contains(frag, "Post "+k+"<") {
					if seen[k] {
						t.Errorf("%s on both pages", k)
					}
					seen[k] = true
				}
			}
			if len(seen) != len(keys) {
				t.Errorf("saw %d of %d items", len(seen), len(keys))
			}
			if full := do(h, get(path, "user", nil)).Body.String(); strings.Contains(full, "autofocus") {
				t.Error("full page autofocuses")
			}
		})
	}
}

func TestReadingEmptyStates(t *testing.T) {
	h, d := newTestServer(t, nil)
	a := seedFeed(t, d, "https://a.example/feed")
	b := seedFeed(t, d, "https://b.example/feed")
	syncItems(t, d, b.ID, time.Now().Unix(), "b1")
	// No subscriptions: / is Everything with a nudge, and no switch.
	for _, p := range []string{homePath, everythingPath} {
		rec := do(h, get(p, "user", nil))
		body := rec.Body.String()
		if !strings.Contains(body, "subscribed to any feeds yet") || !strings.Contains(body, `<a href="/feeds">Browse the feeds</a>`) ||
			!strings.Contains(body, "Post b1") || strings.Contains(body, `class="seg"`) || strings.Contains(body, "Sign in") ||
			rec.Header().Get("Cache-Control") != privateCache {
			t.Errorf("%s with no subscriptions: %s", p, body)
		}
	}
	if body := do(h, get(followingPath, "user", nil)).Body.String(); !strings.Contains(body, "subscribed to any feeds yet") || strings.Contains(body, `class="seg"`) {
		t.Errorf("following with no subscriptions: %s", body)
	}
	post(t, h, fmt.Sprintf("/feeds/%d/subscribe", a.ID), nil, nil)
	if body := do(h, get(homePath, "user", nil)).Body.String(); !strings.Contains(body, "All caught up") || !strings.Contains(body, `href="/following">See every post from your feeds`) {
		t.Errorf("caught up: %s", body)
	}
	feedA := fmt.Sprintf("/feeds/%d", a.ID)
	if body := do(h, get(feedA, "user", nil)).Body.String(); !strings.Contains(body, "All caught up") || !strings.Contains(body, `href="`+feedA+`?view=all">See every post from this feed`) {
		t.Errorf("feed caught up: %s", body)
	}
	if body := do(h, get(followingPath, "user", nil)).Body.String(); !strings.Contains(body, "Nothing from your feeds in the last 30 days") {
		t.Errorf("all empty: %s", body)
	}
}

// A sync that started before the subscription (so its first_seen_at is
// earlier) but committed after it still brings unread items; items stored
// before the subscription, refetched by that sync, stay old.
func TestSubscribeDuringSync(t *testing.T) {
	h, d := newTestServer(t, nil)
	a := seedFeed(t, d, "https://a.example/feed")
	syncStart := time.Now().Add(-10 * time.Second).Unix()
	syncItems(t, d, a.ID, syncStart-3600, "old")
	post(t, h, fmt.Sprintf("/feeds/%d/subscribe", a.ID), nil, nil)
	// The in-flight sync commits now, stamped with its start time.
	syncItems(t, d, a.ID, syncStart, "old", "new")
	wantCounts(t, h, 1, map[int64]string{a.ID: "1"})
	if body := do(h, get(homePath, "user", nil)).Body.String(); !strings.Contains(body, "Post new") || strings.Contains(body, "Post old") {
		t.Errorf("unread list: %s", body)
	}
}

// Without JS, marking an entry read on an Older page returns to that page.
func TestReadingBackKeepsCursor(t *testing.T) {
	h, d := newTestServer(t, nil)
	a := seedFeed(t, d, "https://a.example/feed")
	post(t, h, fmt.Sprintf("/feeds/%d/subscribe", a.ID), nil, nil)
	keys := make([]string, readingPageSize+3)
	for i := range keys {
		keys[i] = fmt.Sprintf("k%02d", i)
	}
	syncItems(t, d, a.ID, time.Now().Unix(), keys...)
	first := do(h, get(homePath, "user", nil)).Body.String()
	if strings.Contains(first, `name="back" value="/?before=`) {
		t.Errorf("first page back carries a cursor")
	}
	m := olderRe.FindStringSubmatch(first)
	if m == nil {
		t.Fatal("no Older link")
	}
	older := strings.ReplaceAll(m[1], "&amp;", "&")
	second := do(h, get(older, "user", nil)).Body.String()
	b := regexp.MustCompile(`name="back" value="([^"]+)"`).FindStringSubmatch(second)
	if b == nil {
		t.Fatalf("no back field: %s", second)
	}
	back := strings.ReplaceAll(b[1], "&amp;", "&")
	if !strings.HasPrefix(back, older+"#item-") {
		t.Fatalf("back on page 2 = %q, want %s#item-...", back, older)
	}
	// And the redirect goes there.
	id := regexp.MustCompile(`action="/items/(\d+)/read"`).FindStringSubmatch(second)[1]
	res := post(t, h, "/items/"+id+"/read", url.Values{"back": {back}}, nil)
	if res.Header.Get("Location") != back {
		t.Errorf("Location = %q, want %q", res.Header.Get("Location"), back)
	}
}

// Every htmx action whose swap removes the clicked button puts autofocus
// on what replaces it.
func TestReadingActionFocus(t *testing.T) {
	h, d := newTestServer(t, nil)
	a := seedFeed(t, d, "https://a.example/feed")
	syncItems(t, d, a.ID, time.Now().Unix()-60, "a1")
	rec := do(h, postForm(fmt.Sprintf("/feeds/%d/subscribe", a.ID), nil, "user", hx))
	if !regexp.MustCompile(`<button [^>]*autofocus>Unsubscribe</button>`).MatchString(rec.Body.String()) {
		t.Errorf("subscribe fragment focus: %s", rec.Body)
	}
	rec = do(h, postForm(fmt.Sprintf("/feeds/%d/unsubscribe", a.ID), nil, "user", hx))
	if !regexp.MustCompile(`<button [^>]*autofocus>Subscribe</button>`).MatchString(rec.Body.String()) {
		t.Errorf("unsubscribe fragment focus: %s", rec.Body)
	}
	post(t, h, fmt.Sprintf("/feeds/%d/subscribe", a.ID), nil, nil)
	ids := syncItems(t, d, a.ID, time.Now().Unix(), "a1", "a2")
	for _, action := range []string{"read", "unread"} {
		rec = do(h, postForm(fmt.Sprintf("/items/%d/%s", ids["a2"], action), nil, "user", hx))
		if !strings.Contains(rec.Body.String(), fmt.Sprintf(`id="item-%d" tabindex="-1" autofocus>`, ids["a2"])) {
			t.Errorf("mark %s fragment focus: %s", action, rec.Body)
		}
	}
	_, _, upto := readingState(t, h, homePath)
	rec = do(h, postForm("/mark-all-read", url.Values{"upto": {upto}}, "user", hx))
	if !strings.Contains(rec.Body.String(), `<a href="/" aria-current="page" autofocus>Unread <span id="unread-total"`) || strings.Count(rec.Body.String(), "autofocus") != 1 {
		t.Errorf("mark all read focus: %s", rec.Body)
	}
	// Full pages never autofocus.
	if body := do(h, get(followingPath, "user", nil)).Body.String(); strings.Contains(body, "autofocus") {
		t.Error("full page autofocuses")
	}
}

// A filtered page counts that feed only, and hides "mark all read" when
// it has nothing unread even if other feeds do.
func TestReadingFilteredTally(t *testing.T) {
	h, d := newTestServer(t, nil)
	a := seedFeed(t, d, "https://a.example/feed")
	b := seedFeed(t, d, "https://b.example/feed")
	for _, f := range []db.Feed{a, b} {
		post(t, h, fmt.Sprintf("/feeds/%d/subscribe", f.ID), nil, nil)
	}
	now := time.Now().Unix()
	ia := syncItems(t, d, a.ID, now, "a1", "a2")
	syncItems(t, d, b.ID, now, "b1", "b2", "b3")

	pathA := fmt.Sprintf("/feeds/%d", a.ID)
	body, total, _ := readingState(t, h, pathA)
	if total != 2 || !strings.Contains(body, "Mark all read") {
		t.Errorf("feed a: total %d", total)
	}
	// htmx mark read from the filtered page: the out-of-band total is a's.
	rec := do(h, postForm(fmt.Sprintf("/items/%d/read", ia["a1"]), url.Values{"feed": {strconv.FormatInt(a.ID, 10)}}, "user", hx))
	if !strings.Contains(rec.Body.String(), `<span id="unread-total" class="n" hx-swap-oob="true">1</span>`) {
		t.Errorf("scoped total: %s", rec.Body)
	}
	post(t, h, fmt.Sprintf("/items/%d/read", ia["a2"]), nil, nil)
	body, total, _ = readingState(t, h, pathA)
	if total != 0 || strings.Contains(body, `action="/mark-all-read"`) {
		t.Errorf("feed a caught up: total %d, mark-all shown: %v", total, strings.Contains(body, "mark-all-read"))
	}
	if _, total, _ = readingState(t, h, homePath); total != 3 {
		t.Errorf("all feeds total = %d, want 3", total)
	}
}

// Entries with a body get a second Mark read at the top; link-only ones
// are a compact row with one.
func TestEntryMarkControls(t *testing.T) {
	h, d := newTestServer(t, nil)
	a := seedFeed(t, d, "https://a.example/feed")
	post(t, h, fmt.Sprintf("/feeds/%d/subscribe", a.ID), nil, nil)
	ids := syncItems(t, d, a.ID, time.Now().Unix(), "full", "bare")
	if _, err := d.Writer.Exec(`UPDATE item_contents SET html = '' WHERE item_id = ?`, ids["bare"]); err != nil {
		t.Fatal(err)
	}
	body := do(h, get(homePath, "user", nil)).Body.String()
	for k, want := range map[string]int{"full": 2, "bare": 1} {
		if n := strings.Count(body, fmt.Sprintf(`action="/items/%d/read"`, ids[k])); n != want {
			t.Errorf("%s: %d mark-read forms, want %d", k, n, want)
		}
	}
}

// The reading list sorts and dates items by posted time, like the public
// lists: a post published days ago but first seen now sits below a newer
// one and reads "3 days ago"; it still counts as unread, since unread is
// about arrival.
func TestReadingPostedOrder(t *testing.T) {
	h, d := newTestServer(t, nil)
	a := seedFeed(t, d, "https://a.example/feed")
	post(t, h, fmt.Sprintf("/feeds/%d/subscribe", a.ID), nil, nil)
	q := db.New(d.Writer)
	now := time.Now()
	for _, it := range []struct {
		key string
		pub time.Time
	}{
		{"backfill", now.Add(-3*24*time.Hour - time.Hour)},
		{"fresh", now.Add(-time.Hour)},
	} {
		if _, err := q.UpsertItem(t.Context(), db.UpsertItemParams{
			FeedID: a.ID, DedupKey: it.key, Url: "https://a.example/" + it.key, Title: "Post " + it.key,
			PublishedAt: sql.NullInt64{Int64: it.pub.Unix(), Valid: true}, FirstSeenAt: now.Unix(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	body, total, _ := readingState(t, h, homePath)
	if total != 2 {
		t.Errorf("unread = %d, want 2", total)
	}
	fresh, backfill := strings.Index(body, "Post fresh"), strings.Index(body, "Post backfill")
	if fresh < 0 || backfill < 0 || fresh > backfill {
		t.Errorf("order: fresh at %d, backfill at %d", fresh, backfill)
	}
	if !strings.Contains(body, ">3 days ago</time>") || !strings.Contains(body, ">1h ago</time>") {
		t.Errorf("relative times missing: %s", body)
	}
	// A post published before the window is not listed or counted.
	if _, err := q.UpsertItem(t.Context(), db.UpsertItemParams{
		FeedID: a.ID, DedupKey: "ancient", Url: "https://a.example/ancient", Title: "Post ancient",
		PublishedAt: sql.NullInt64{Int64: now.Add(-40 * 24 * time.Hour).Unix(), Valid: true}, FirstSeenAt: now.Unix(),
	}); err != nil {
		t.Fatal(err)
	}
	body, total, _ = readingState(t, h, followingPath)
	if total != 2 || strings.Contains(body, "Post ancient") {
		t.Errorf("out-of-window post: total %d", total)
	}
}
