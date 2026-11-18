package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/0xD34DC0D3/scry/internal/db"
)

// seedItem stores an item seen age ago, with its excerpt.
func seedItem(t *testing.T, d *db.DB, feedID int64, key, url, title, excerpt string, age time.Duration) int64 {
	t.Helper()
	return seedItemAt(t, d, feedID, key, url, title, excerpt, time.Now().Add(-age).Unix())
}

// seedItemAt stores an item first seen at the given unix time.
func seedItemAt(t *testing.T, d *db.DB, feedID int64, key, url, title, excerpt string, seen int64) int64 {
	t.Helper()
	q := db.New(d.Writer)
	id, err := q.UpsertItem(context.Background(), db.UpsertItemParams{
		FeedID: feedID, DedupKey: key, Url: url, Title: title,
		FirstSeenAt: seen,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.UpsertItemContent(context.Background(), db.UpsertItemContentParams{
		ItemID: id, Html: "<p>FULLHTML " + title + "</p>", Excerpt: excerpt, FetchedAt: time.Now().Unix(),
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

func get(path, cookie string, hdr map[string]string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Accept", "text/html")
	if cookie != "" {
		withCookie(req, cookie)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	return req
}

// publicFixture: feed a (titled, two items plus one 31 days old), feed b
// (untitled, paused, one item with a javascript: URL).
type publicFixture struct {
	h    http.Handler
	d    *db.DB
	a, b db.Feed
}

func newPublicFixture(t *testing.T) publicFixture {
	t.Helper()
	h, d := newTestServer(t, nil)
	a := seedFeed(t, d, "https://a.example/feed")
	b := seedFeed(t, d, "https://b.example/feed")
	if _, err := d.Writer.Exec(`UPDATE feeds SET title = 'Alpha News', site_url = 'https://a.example/' WHERE id = ?`, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.New(d.Writer).PauseFeed(context.Background(), b.ID); err != nil {
		t.Fatal(err)
	}
	seedItem(t, d, a.ID, "a1", "https://a.example/1", "Alpha one", "first <b>excerpt</b>", 2*time.Hour)
	seedItem(t, d, a.ID, "a2", "https://a.example/2", "Alpha two", "second excerpt", time.Hour)
	seedItem(t, d, a.ID, "old", "https://a.example/old", "Alpha ancient", "old excerpt", 31*24*time.Hour)
	seedItem(t, d, b.ID, "b1", "javascript:alert(1)", "Bravo evil", "evil excerpt", 3*time.Hour)
	return publicFixture{h: h, d: d, a: a, b: b}
}

func TestPublicPages(t *testing.T) {
	f := newPublicFixture(t)
	feedA := fmt.Sprintf("/feeds/%d", f.a.ID)
	feedB := fmt.Sprintf("/feeds/%d", f.b.ID)
	tests := []struct {
		name, path string
		want, not  []string
	}{
		{
			name: "latest",
			path: "/",
			want: []string{
				`href="https://a.example/2"`, "Alpha two", "second excerpt",
				"first &lt;b&gt;excerpt&lt;/b&gt;", // excerpts are escaped text
				`href="` + feedA + `">Alpha News</a>`,
				`href="` + feedB + `">https://b.example/feed</a>`, // untitled feed falls back to its URL
				`href="#ZgotmplZ"`, "Bravo evil",
			},
			not: []string{"Alpha ancient", "javascript:", "FULLHTML", "<b>excerpt"},
		},
		{
			name: "catalog",
			path: "/feeds",
			want: []string{
				`href="` + feedA + `">Alpha News</a>`, `href="` + feedB + `">https://b.example/feed</a>`,
				// paused shows inline by the name; no Status column or "active" badge
				`</a> <span class="badge off">paused</span>`, `<th>Last post</th>`,
				`<span class="site-host">a.example</span>`, `data-label="Last post"`, `<time datetime=`, ` UTC">today</time>`,
			},
			not: []string{"FULLHTML", "active", "<th>Status</th>", "<code>", "https://a.example/<"},
		},
		{
			name: "feed",
			path: feedA,
			want: []string{
				"<h1>Alpha News</h1>", "Alpha one", "Alpha two",
				`href="https://a.example/">Visit site</a>`, `<a href="/feeds">← Feeds</a>`,
				`readonly value="https://a.example/feed"`, "Feed URL — paste into your reader",
			},
			not: []string{"Alpha ancient", "Bravo evil", "FULLHTML", `class="path"`, `>rss</a>`, `>website</a>`},
		},
		{
			name: "paused feed",
			path: feedB,
			want: []string{"paused", `href="#ZgotmplZ"`, "evil excerpt", "<h1>b.example</h1>", "<title>b.example"}, // untitled: host, not URL
			not:  []string{"javascript:", "Alpha"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Signed out: no redirect to login.
			rec := do(f.h, get(tc.path, "", nil))
			body := rec.Body.String()
			if rec.Code != http.StatusOK || !strings.Contains(body, "<html") {
				t.Fatalf("status %d, body: %s", rec.Code, body)
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
		})
	}
}

func TestPublicOrderNewestFirst(t *testing.T) {
	f := newPublicFixture(t)
	body := do(f.h, get("/", "", nil)).Body.String()
	two, one, evil := strings.Index(body, "Alpha two"), strings.Index(body, "Alpha one"), strings.Index(body, "Bravo evil")
	if two < 0 || two > one || one > evil {
		t.Errorf("order: two=%d one=%d evil=%d", two, one, evil)
	}
}

func TestPublicNotFound(t *testing.T) {
	f := newPublicFixture(t)
	for _, p := range []string{"/feeds/999999", "/feeds/abc", "/nope"} {
		if rec := do(f.h, get(p, "", nil)); rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", p, rec.Code)
		}
	}
	for _, p := range []string{"/?before=x", fmt.Sprintf("/feeds/%d?before=1-", f.a.ID)} {
		if rec := do(f.h, get(p, "", nil)); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", p, rec.Code)
		}
	}
}

func TestPublicCacheHeaders(t *testing.T) {
	f := newPublicFixture(t)
	feedA := fmt.Sprintf("/feeds/%d", f.a.ID)
	tests := []struct {
		name   string
		cookie string
		hdr    map[string]string
		want   string
	}{
		{"anonymous", "", nil, publicCache},
		{"anonymous htmx", "", hx, publicCache},
		{"signed in", "user", nil, privateCache},
		{"admin", "admin", nil, privateCache},
		// voodoo rejects it, so the page is anonymous, but it still carried
		// a credential.
		{"bad cookie", "garbage", nil, privateCache},
		{"api key", "", map[string]string{"Authorization": "Bearer ak_nope"}, privateCache},
		{"other auth", "", map[string]string{"Authorization": "Basic Zm9vOmJhcg=="}, privateCache},
	}
	for _, tc := range tests {
		for _, p := range []string{"/", "/everything", "/feeds", feedA} {
			t.Run(tc.name+" "+p, func(t *testing.T) {
				rec := do(f.h, get(p, tc.cookie, tc.hdr))
				if rec.Code != http.StatusOK {
					t.Fatalf("status %d", rec.Code)
				}
				if got := rec.Header().Get("Cache-Control"); got != tc.want {
					t.Errorf("Cache-Control = %q, want %q", got, tc.want)
				}
				vary := strings.Join(rec.Header().Values("Vary"), ",")
				for _, v := range []string{"HX-Request", "Cookie", "Authorization"} {
					if !strings.Contains(vary, v) {
						t.Errorf("Vary = %q, lacks %s", vary, v)
					}
				}
			})
		}
	}
	// An error is never stored.
	if cc := do(f.h, get("/feeds/999999", "", nil)).Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("404 Cache-Control = %q", cc)
	}
}

func TestPublicSignedInHeader(t *testing.T) {
	f := newPublicFixture(t)
	body := do(f.h, get("/", "user", nil)).Body.String()
	if !strings.Contains(body, "user@example.com") || !strings.Contains(body, "Alpha two") {
		t.Errorf("signed-in latest lacks email or items: %s", body)
	}
}

func TestPublicFragment(t *testing.T) {
	f := newPublicFixture(t)
	for _, p := range []string{"/", "/everything", fmt.Sprintf("/feeds/%d", f.a.ID)} {
		t.Run(p, func(t *testing.T) {
			full := do(f.h, get(p, "", nil)).Body.String()
			frag := do(f.h, get(p, "", hx)).Body.String()
			if !strings.Contains(full, "<html") || !strings.Contains(full, `<ol class="posts">`) {
				t.Errorf("full page: %s", full)
			}
			if strings.Contains(frag, "<html") || strings.Contains(frag, "<ol") || !strings.Contains(frag, `<li class="post" id="item-`) || !strings.Contains(frag, "Alpha two") {
				t.Errorf("fragment: %s", frag)
			}
		})
	}
}

var (
	olderRe = regexp.MustCompile(`href="(/[^"]*before=[^"]+)"`)
	countRe = regexp.MustCompile(`Showing (\d+) of (\d+)`)
)

// Paging walks every in-window item exactly once, through equal
// posted times across page boundaries, and stops at the window.
func TestPublicPaging(t *testing.T) {
	h, d := newTestServer(t, nil)
	fd := seedFeed(t, d, "https://many.example/feed")
	n := pageSize*2 + 5
	// Groups of 3 share a timestamp, and with pageSize 50 the groups for
	// rows 48-50 and 99-101 straddle both page boundaries, so paging is
	// correct only with the id tie-break.
	base := time.Now().Add(-time.Hour).Unix()
	for i := range n {
		seedItemAt(t, d, fd.ID, fmt.Sprint(i), fmt.Sprintf("https://many.example/%d", i), fmt.Sprintf("item-%d.", i), "", base-int64(i/3)*60)
	}
	seedItem(t, d, fd.ID, "old", "https://many.example/old", "item-old.", "", 31*24*time.Hour)
	// Move those times to published_at and mark every item first seen now,
	// as when a feed is added with its backlog: the cursor must follow
	// posted time, which then differs from first_seen_at.
	if _, err := d.Writer.Exec(`UPDATE items SET published_at = first_seen_at, first_seen_at = ?`, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}

	for _, start := range []string{"/", "/everything", fmt.Sprintf("/feeds/%d", fd.ID)} {
		t.Run(start, func(t *testing.T) {
			seen := map[string]bool{}
			path, pages := start, 0
			for path != "" {
				pages++
				if pages > 10 {
					t.Fatal("paging does not end")
				}
				hdr := map[string]string(nil)
				if pages > 1 {
					hdr = hx // later pages as the Older button fetches them
				}
				rec := do(h, get(path, "", hdr))
				if rec.Code != http.StatusOK {
					t.Fatalf("%s: %d", path, rec.Code)
				}
				body := rec.Body.String()
				for _, m := range regexp.MustCompile(`item-[0-9a-z]+\.`).FindAllString(body, -1) {
					if seen[m] {
						t.Errorf("%s seen twice", m)
					}
					seen[m] = true
				}
				path = ""
				if m := olderRe.FindStringSubmatch(body); m != nil {
					path = strings.ReplaceAll(m[1], "&amp;", "&")
				}
				// The end marker shows only where there is no Older link.
				if hasEnd := strings.Contains(body, "everything from the last 30 days"); hasEnd != (path == "") {
					t.Errorf("page %d: end marker %v with older %q", pages, hasEnd, path)
				}
				// The Older link counts everything shown so far, fragments
				// included, out of the whole window.
				pos := countRe.FindStringSubmatch(body)
				switch {
				case path == "" && pos != nil:
					t.Errorf("page %d: count %q without Older", pages, pos[0])
				case path != "" && (pos == nil || pos[1] != fmt.Sprint(len(seen)) || pos[2] != fmt.Sprint(n)):
					t.Errorf("page %d: count %v, want Showing %d of %d", pages, pos, len(seen), n)
				}
			}
			if pages != 3 || len(seen) != n || seen["item-old."] {
				t.Errorf("pages=%d seen=%d (want 3, %d), old seen=%v", pages, len(seen), n, seen["item-old."])
			}
		})
	}
}

func TestLinkDomain(t *testing.T) {
	tests := []struct {
		name string
		link string
		own  []string
		want string
	}{
		{"same host", "https://a.example/1", []string{"https://a.example/feed"}, ""},
		{"www ignored", "https://www.a.example/1", []string{"https://a.example/feed"}, ""},
		{"own www ignored", "https://a.example/1", []string{"https://www.a.example/"}, ""},
		{"case ignored", "https://A.example/1", []string{"https://a.example/feed"}, ""},
		{"aggregator", "https://other.example/1", []string{"https://news.example/rss"}, "other.example"},
		{"subdomain differs", "https://blog.a.example/1", []string{"https://a.example/"}, "blog.a.example"},
		{"matches second", "https://a.example/1", []string{"", "https://a.example/feed"}, ""},
		{"no own", "https://a.example/1", nil, "a.example"},
		{"unparsable link", "javascript:alert(1)", []string{"https://a.example/"}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := linkDomain(tc.link, tc.own...); got != tc.want {
				t.Errorf("linkDomain(%q, %v) = %q, want %q", tc.link, tc.own, got, tc.want)
			}
		})
	}
}

func TestItemHeading(t *testing.T) {
	long := strings.Repeat("word ", 30)
	tests := []struct {
		name        string
		it          itemView
		want        string
		showExcerpt bool
	}{
		{"titled", itemView{Title: "Real", URL: "https://a.example/x", Excerpt: "body"}, "Real", true},
		{"titled no excerpt", itemView{Title: "Real"}, "Real", false},
		{"short excerpt", itemView{URL: "https://a.example/x", Excerpt: "Just a  short\nnote"}, "Just a short note", false},
		{"long excerpt on word boundary", itemView{Excerpt: long}, strings.TrimRight(strings.Repeat("word ", 16), " ") + "…", true},
		{"exactly 80", itemView{Excerpt: strings.Repeat("a", 80)}, strings.Repeat("a", 80), false},
		{"one huge word", itemView{Excerpt: strings.Repeat("a", 100)}, strings.Repeat("a", 80) + "…", true},
		{"url fallback", itemView{URL: "https://www.a.example/2026/post/?utm=1#x"}, "a.example/2026/post", false},
		{"url long", itemView{URL: "https://a.example/" + strings.Repeat("p", 100)}, ("a.example/" + strings.Repeat("p", 100))[:59] + "…", false},
		{"bare host", itemView{URL: "https://a.example/"}, "a.example", false},
		{"javascript url", itemView{URL: "javascript:alert(1)"}, "(untitled)", false},
		{"nothing", itemView{}, "(untitled)", false},
		{"blank title", itemView{Title: "  ", Excerpt: "from excerpt"}, "from excerpt", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.it.Heading(); got != tc.want {
				t.Errorf("Heading() = %q, want %q", got, tc.want)
			}
			if got := tc.it.ShowExcerpt(); got != tc.showExcerpt {
				t.Errorf("ShowExcerpt() = %v, want %v", got, tc.showExcerpt)
			}
		})
	}
}

// An untitled item renders its excerpt as the linked heading, once.
func TestPublicUntitledItem(t *testing.T) {
	f := newPublicFixture(t)
	seedItem(t, f.d, f.a.ID, "u", "https://a.example/u", "", "Untitled body text", time.Minute)
	body := do(f.h, get("/", "", nil)).Body.String()
	if !strings.Contains(body, `<a href="https://a.example/u">Untitled body text</a>`) {
		t.Errorf("excerpt not used as heading:\n%s", body)
	}
	if n := strings.Count(body, "Untitled body text"); n != 1 {
		t.Errorf("excerpt appears %d times, want 1", n)
	}
}

func TestDomain(t *testing.T) {
	tests := []struct{ in, want string }{
		{"https://www.example.com/a", "example.com"},
		{"https://news.ycombinator.com/item?id=1", "news.ycombinator.com"},
		{"http://Example.org:8080/x", "Example.org"},
		{"https://wwwx.example/", "wwwx.example"},
		{"/relative", ""},
		{"javascript:alert(1)", ""},
		{"", ""},
		{"http://[::1", ""}, // unparsable
	}
	for _, tc := range tests {
		if got := domain(tc.in); got != tc.want {
			t.Errorf("domain(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The meta line carries the link's domain, the author, and a discussion
// link; a javascript: discussion URL from the DB still never renders
// executable.
func TestPublicItemMeta(t *testing.T) {
	f := newPublicFixture(t)
	seedItem(t, f.d, f.a.ID, "w", "https://www.w.example/post", "Www post", "", 30*time.Minute)
	for _, q := range []string{
		`UPDATE items SET comments_url = 'https://talk.example/t?id=2&amp=1', author = 'Ann' WHERE dedup_key = 'a2'`,
		`UPDATE items SET comments_url = 'javascript:alert(2)' WHERE dedup_key = 'b1'`,
	} {
		if _, err := f.d.Writer.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{"/", fmt.Sprintf("/feeds/%d", f.a.ID), fmt.Sprintf("/feeds/%d", f.b.ID)} {
		t.Run(path, func(t *testing.T) {
			body := do(f.h, get(path, "", nil)).Body.String()
			want := []string{` · by Ann`, `<a href="https://talk.example/t?id=2&amp;amp=1">discussion</a>`, ` · w.example`}
			if path == fmt.Sprintf("/feeds/%d", f.b.ID) {
				want = []string{`<a href="#ZgotmplZ">discussion</a>`}
			}
			for _, w := range want {
				if !strings.Contains(body, w) {
					t.Errorf("missing %q", w)
				}
			}
			for _, n := range []string{"javascript:", "www.w.example<", " · www."} {
				if strings.Contains(body, n) {
					t.Errorf("unexpected %q", n)
				}
			}
		})
	}
}

// Lists order, window and count by posted time: published_at, clamped to
// first_seen_at, else first_seen_at. A just-added feed's backlog keeps its
// own dates instead of landing on top, a future date cannot pin an item
// above newer ones, and an old post seen today stays out of the window.
func TestPublicPostedAt(t *testing.T) {
	h, d := newTestServer(t, nil)
	fd := seedFeed(t, d, "https://dates.example/feed")
	now := time.Now()
	type seed struct {
		key       string
		seen      time.Duration // ago
		published *time.Duration
	}
	ago := func(x time.Duration) *time.Duration { return &x }
	seeds := []seed{
		{"undated", time.Hour, nil},                           // posted 1h ago
		{"backlog", time.Minute, ago(5 * 24 * time.Hour)},     // posted 5 days ago
		{"future", 3 * time.Hour, ago(-24 * time.Hour)},       // clamped to 3h ago
		{"resurfaced", time.Minute, ago(40 * 24 * time.Hour)}, // outside the window
	}
	for _, s := range seeds {
		seedItemAt(t, d, fd.ID, s.key, "https://dates.example/"+s.key, "post-"+s.key+".", "", now.Add(-s.seen).Unix())
		if s.published != nil {
			if _, err := d.Writer.Exec(`UPDATE items SET published_at = ? WHERE dedup_key = ?`, now.Add(-*s.published).Unix(), s.key); err != nil {
				t.Fatal(err)
			}
		}
	}
	futureAt := now.Add(-3 * time.Hour).UTC().Format("2006-01-02T15:04:05Z")

	for _, path := range []string{"/", fmt.Sprintf("/feeds/%d", fd.ID)} {
		t.Run(path, func(t *testing.T) {
			body := do(h, get(path, "", nil)).Body.String()
			got := regexp.MustCompile(`post-[a-z]+\.`).FindAllString(body, -1)
			want := []string{"post-undated.", "post-future.", "post-backlog."}
			if strings.Join(got, " ") != strings.Join(want, " ") {
				t.Errorf("order %v, want %v", got, want)
			}
			if !strings.Contains(body, `datetime="`+futureAt+`"`) {
				t.Errorf("future date not clamped to first seen %s", futureAt)
			}
		})
	}
	t.Run("catalog", func(t *testing.T) {
		body := do(h, get("/feeds", "", nil)).Body.String()
		if !strings.Contains(body, `<td data-label="Posts (30 days)">3</td>`) {
			t.Errorf("catalog count is not 3: %s", body)
		}
	})
}

// One table for both precisions: items (toMinute) and the catalog's last
// post (toDay).
func TestAgo(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	back := func(d time.Duration) time.Time { return now.Add(-d) }
	tests := []struct {
		name string
		t    time.Time
		p    agoPrecision
		want string
	}{
		{"never", time.Time{}, toMinute, ""},
		{"never, days", time.Time{}, toDay, ""},
		{"future", back(-time.Hour), toMinute, "just now"},
		{"future, days", back(-time.Hour), toDay, "today"},
		{"now", now, toMinute, "just now"},
		{"59s", back(59 * time.Second), toMinute, "just now"},
		{"1m", back(time.Minute), toMinute, "1m ago"},
		{"59m59s", back(time.Hour - time.Second), toMinute, "59m ago"},
		{"1h", back(time.Hour), toMinute, "1h ago"},
		{"23h59m", back(24*time.Hour - time.Minute), toMinute, "23h ago"},
		{"hours, days", back(3 * time.Hour), toDay, "today"},
		{"24h", back(24 * time.Hour), toMinute, "yesterday"},
		{"30h, days", back(30 * time.Hour), toDay, "yesterday"},
		{"47h", back(47 * time.Hour), toMinute, "yesterday"},
		{"2 days", back(48 * time.Hour), toMinute, "2 days ago"},
		{"5 days, days", back(5 * 24 * time.Hour), toDay, "5 days ago"},
		{"under 7 days", back(7*24*time.Hour - time.Minute), toMinute, "6 days ago"},
		{"7 days", back(7 * 24 * time.Hour), toMinute, "Nov 10"},
		{"14 days, days", back(14 * 24 * time.Hour), toDay, "Nov 3"},
		{"this year, days", time.Date(2026, 1, 9, 0, 0, 0, 0, time.UTC), toDay, "Jan 9"},
		{"last year", back(330 * 24 * time.Hour), toMinute, "Dec 22, 2025"},
		{"earlier year, days", time.Date(2025, 12, 30, 0, 0, 0, 0, time.UTC), toDay, "Dec 30, 2025"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ago(tc.t, now, tc.p); got != tc.want {
				t.Errorf("ago(%v, %v) = %q, want %q", tc.t, tc.p, got, tc.want)
			}
		})
	}
	// A zoned now still dates in UTC, like the title attribute.
	zoned := time.Date(2026, 1, 20, 0, 0, 0, 0, time.FixedZone("x", -5*3600))
	if got := ago(time.Date(2026, 1, 1, 0, 30, 0, 0, time.UTC), zoned, toMinute); got != "Jan 1" {
		t.Errorf("zoned now: %q", got)
	}
}

// The meta line shows relative time, keeping the machine-readable datetime
// and putting the absolute UTC time in title.
func TestPublicRelativeTime(t *testing.T) {
	h, d := newTestServer(t, nil)
	fd := seedFeed(t, d, "https://rel.example/feed")
	seen := time.Now().Add(-3*time.Hour - 5*time.Minute).Truncate(time.Second).UTC()
	seedItemAt(t, d, fd.ID, "r", "https://rel.example/r", "Rel", "", seen.Unix())
	want := `<time datetime="` + seen.Format(time.RFC3339) + `" title="` + seen.Format("2006-01-02 15:04") + ` UTC">3h ago</time>`
	for _, path := range []string{"/", fmt.Sprintf("/feeds/%d", fd.ID)} {
		if body := do(h, get(path, "", nil)).Body.String(); !strings.Contains(body, want) {
			t.Errorf("%s lacks %s", path, want)
		}
	}
}
