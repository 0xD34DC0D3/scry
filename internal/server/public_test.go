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
				`href="https://a.example/">Visit site</a>`, `<a href="/feeds">← All feeds</a>`,
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
		for _, p := range []string{"/", "/feeds", feedA} {
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
	for _, p := range []string{"/", fmt.Sprintf("/feeds/%d", f.a.ID)} {
		t.Run(p, func(t *testing.T) {
			full := do(f.h, get(p, "", nil)).Body.String()
			frag := do(f.h, get(p, "", hx)).Body.String()
			if !strings.Contains(full, "<html") || !strings.Contains(full, `<ol class="items">`) {
				t.Errorf("full page: %s", full)
			}
			if strings.Contains(frag, "<html") || strings.Contains(frag, "<ol") || !strings.Contains(frag, `<li class="item">`) || !strings.Contains(frag, "Alpha two") {
				t.Errorf("fragment: %s", frag)
			}
		})
	}
}

var olderRe = regexp.MustCompile(`href="(/[^"]*before=[^"]+)"`)

// Paging walks every in-window item exactly once, through equal
// first_seen_at values across page boundaries, and stops at the window.
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

	for _, start := range []string{"/", fmt.Sprintf("/feeds/%d", fd.ID)} {
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
