package server

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The catalog's Last post column covers feeds whose items all left the
// 30-day window, and feeds with none.
func TestCatalogLastPost(t *testing.T) {
	h, d := newTestServer(t, nil)
	quiet := seedFeed(t, d, "https://quiet.example/feed")
	empty := seedFeed(t, d, "https://empty.example/feed")
	seen := time.Now().Add(-100 * 24 * time.Hour).UTC()
	seedItemAt(t, d, quiet.ID, "q1", "https://quiet.example/1", "t", "", seen.Unix())
	body := do(h, get("/feeds", "", nil)).Body.String()
	want := `<time datetime="` + seen.Format("2006-01-02T15:04:05Z") + `" title="` + seen.Format("2006-01-02 15:04") + ` UTC">` + ago(seen, time.Now().UTC(), toDay) + `</time>`
	if !strings.Contains(body, want) {
		t.Errorf("missing %q in %s", want, body)
	}
	if n := strings.Count(body, `<span class="muted">never</span>`); n != 1 {
		t.Errorf("feeds shown as never = %d, want 1 (%s)", n, fmt.Sprint(empty.ID))
	}
}

// A feed with nothing in the window says when it last posted, using the
// item date (published_at, else first_seen_at, never after first_seen_at).
func TestFeedLastPost(t *testing.T) {
	h, d := newTestServer(t, nil)
	never := seedFeed(t, d, "https://never.example/feed")
	dated := seedFeed(t, d, "https://dated.example/feed")
	undated := seedFeed(t, d, "https://undated.example/feed")
	future := seedFeed(t, d, "https://future.example/feed")

	day := int64(24 * 60 * 60)
	now := time.Now().Unix()
	older := seedItemAt(t, d, dated.ID, "d1", "https://dated.example/1", "t", "", now-60*day)
	newer := seedItemAt(t, d, dated.ID, "d2", "https://dated.example/2", "t", "", now-50*day)
	seedItemAt(t, d, undated.ID, "u1", "https://undated.example/1", "t", "", now-45*day)
	// A feed claiming a future date is clamped to when we saw it.
	fut := seedItemAt(t, d, future.ID, "f1", "https://future.example/1", "t", "", now-35*day)
	for id, pub := range map[int64]int64{older: now - 90*day, newer: now - 52*day, fut: now + 10*day} {
		if _, err := d.Writer.Exec(`UPDATE items SET published_at = ? WHERE id = ?`, pub, id); err != nil {
			t.Fatal(err)
		}
	}
	date := func(unix int64) string { return time.Unix(unix, 0).UTC().Format("Jan 2, 2006") }
	tests := []struct {
		name string
		id   int64
		want string
	}{
		{"no items", never.ID, "This feed hasn't posted yet."},
		// older published long ago, newer published before it was seen: the
		// newest item date is newer's published_at.
		{"published_at wins", dated.ID, `last post was <time datetime="` + time.Unix(now-52*day, 0).UTC().Format("2006-01-02T15:04:05Z") + `">` + date(now-52*day) + `</time>.`},
		{"no published_at", undated.ID, date(now - 45*day)},
		{"future published_at is clamped", future.ID, date(now - 35*day)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := do(h, get(fmt.Sprintf("/feeds/%d", tc.id), "", nil)).Body.String()
			if !strings.Contains(body, tc.want) {
				t.Errorf("missing %q in %s", tc.want, body)
			}
			if !strings.Contains(body, `<a href="/everything">See the latest from every feed</a>`) {
				t.Error("lost the link to Latest")
			}
		})
	}
}

// Error responses come through the layout as HTML and are never cacheable,
// signed in or not.
func TestErrorPages(t *testing.T) {
	f := newPublicFixture(t)
	tests := []struct {
		name, method, path string
		status             int
	}{
		{"unknown path", http.MethodGet, "/nope", http.StatusNotFound},
		{"unknown nested path", http.MethodGet, "/feeds/1/extra", http.StatusNotFound},
		{"unknown feed", http.MethodGet, "/feeds/999999", http.StatusNotFound},
		{"non-numeric feed", http.MethodGet, "/feeds/abc", http.StatusNotFound},
		// The catch-all has no method, so a wrong method is a 404 too.
		{"wrong method", http.MethodGet, feedsPath + "/1/pause", http.StatusNotFound},
		{"bad cursor", http.MethodGet, "/?before=x", http.StatusBadRequest},
		{"bad feed cursor", http.MethodGet, fmt.Sprintf("/feeds/%d?before=1-", f.a.ID), http.StatusBadRequest},
	}
	for _, tc := range tests {
		for _, cookie := range []string{"", "user"} {
			t.Run(tc.name+" "+cookie, func(t *testing.T) {
				req := get(tc.path, cookie, nil)
				req.Method = tc.method
				rec := do(f.h, req)
				checkErrorPage(t, rec.Code, rec.Header(), rec.Body.String(), tc.status)
			})
		}
	}
}

func checkErrorPage(t *testing.T, code int, h http.Header, body string, want int) {
	t.Helper()
	if code != want {
		t.Fatalf("status %d, want %d", code, want)
	}
	if ct := h.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q", ct)
	}
	if cc := h.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	for _, w := range []string{"<html", "<h1>" + http.StatusText(want) + "</h1>", `<nav aria-label="Main">`, `href="/">Back to scry</a>`} {
		if !strings.Contains(body, w) {
			t.Errorf("missing %q in %s", w, body)
		}
	}
}

func TestInternalErrorPage(t *testing.T) {
	h, d := newTestServer(t, nil)
	if err := d.Reader.Close(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/", "/feeds"} {
		rec := do(h, get(p, "", nil))
		checkErrorPage(t, rec.Code, rec.Header(), rec.Body.String(), http.StatusInternalServerError)
	}
}

func TestNavCurrent(t *testing.T) {
	f := newPublicFixture(t)
	tests := []struct {
		path, cookie string
		want         string
		not          []string
	}{
		{"/", "", `<a class="app" href="/" aria-current="page">scry</a>`, []string{`href="/feeds" aria-current`, ">latest</a>", ">reading</a>"}},
		{"/everything", "", `<a class="app" href="/" aria-current="true">scry</a>`, []string{`href="/feeds" aria-current`}},
		{"/feeds", "", `<a href="/feeds" aria-current="page">feeds</a>`, []string{`href="/" aria-current`}},
		// One feed sits below /feeds: the section, not the page.
		{fmt.Sprintf("/feeds/%d", f.a.ID), "", `<a href="/feeds" aria-current="true">feeds</a>`, []string{`aria-current="page"`}},
		// Admins get the same nav: the catalog is their admin page.
		{"/feeds", "admin", `<a href="/feeds" aria-current="page">feeds</a>`, []string{`href="/admin`, ">admin</a>"}},
		{"/nope", "", `<nav aria-label="Main">`, []string{"aria-current"}},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			body := do(f.h, get(tc.path, tc.cookie, nil)).Body.String()
			if !strings.Contains(body, tc.want) {
				t.Errorf("missing %q", tc.want)
			}
			for _, n := range tc.not {
				if strings.Contains(body, n) {
					t.Errorf("unexpected %q", n)
				}
			}
		})
	}
}

func TestEmptyStates(t *testing.T) {
	h, d := newTestServer(t, nil)
	empty := seedFeed(t, d, "https://empty.example/feed")
	tests := []struct {
		name, path, cookie string
		want, not          []string
	}{
		{"latest anonymous", "/", "", []string{`class="empty"`, `href="/feeds">Browse the feeds</a>`}, []string{"everything from the last 30 days"}},
		{"latest admin", "/", "admin", []string{`<p class="empty">Nothing in the last 30 days. <a href="/feeds">Browse the feeds</a>`}, []string{"Manage feeds"}},
		{"feed", fmt.Sprintf("/feeds/%d", empty.ID), "", []string{`class="empty"`, `<a href="/everything">See the latest from every feed</a>`}, []string{"everything from the last 30 days"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := do(h, get(tc.path, tc.cookie, nil)).Body.String()
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

	h2, _ := newTestServer(t, nil)
	if body := do(h2, get("/feeds", "", nil)).Body.String(); !strings.Contains(body, "No feeds yet.") || strings.Contains(body, "Add one") {
		t.Errorf("anonymous empty catalog: %s", body)
	}
	if body := do(h2, get("/feeds", "admin", nil)).Body.String(); !strings.Contains(body, "No feeds yet. Add one by URL above.") {
		t.Errorf("admin empty catalog lacks the add hint: %s", body)
	}
}

func TestEndOfList(t *testing.T) {
	f := newPublicFixture(t)
	const end = `<li class="end">That's everything from the last 30 days.</li>`
	for _, p := range []string{"/", "/everything", fmt.Sprintf("/feeds/%d", f.a.ID)} {
		for name, hdr := range map[string]map[string]string{"full": nil, "fragment": hx} {
			t.Run(p+" "+name, func(t *testing.T) {
				if body := do(f.h, get(p, "", hdr)).Body.String(); !strings.Contains(body, end) {
					t.Errorf("missing end marker: %s", body)
				}
			})
		}
		// Items aged out between Older clicks: the empty fragment still
		// ends the list instead of swapping in nothing.
		t.Run(p+" empty fragment", func(t *testing.T) {
			rec := do(f.h, get(p+"?before=1-1", "", hx))
			if body := rec.Body.String(); rec.Code != http.StatusOK || strings.Contains(body, `class="post`) || !strings.Contains(body, end) {
				t.Errorf("status %d, body: %s", rec.Code, body)
			}
		})
	}
}

// The fragment Older swaps in focuses its first item (htmx honours
// autofocus after a swap); a full page load must not steal focus.
func TestOlderFocusAndHeadings(t *testing.T) {
	f := newPublicFixture(t)
	full := do(f.h, get("/", "", nil)).Body.String()
	// An Older link always carries a cursor; this one starts above every item.
	frag := do(f.h, get("/?before=9223372036854775807-9223372036854775807", "", hx)).Body.String()
	if strings.Contains(full, "autofocus") {
		t.Error("full page has autofocus")
	}
	if strings.Count(frag, "autofocus") != 1 || !regexp.MustCompile(`<li class="post" id="item-\d+" tabindex="-1" autofocus>`).MatchString(frag) {
		t.Errorf("fragment autofocus: %s", frag)
	}
	for _, p := range []string{"/", "/everything", "/feeds", fmt.Sprintf("/feeds/%d", f.a.ID), "/nope"} {
		if body := do(f.h, get(p, "", nil)).Body.String(); strings.Contains(body, "<h3") {
			t.Errorf("%s skips from h1 to h3", p)
		}
	}
	// The reader pages too, each with one h1.
	do(f.h, postForm(fmt.Sprintf("/feeds/%d/subscribe", f.a.ID), nil, "user", nil))
	for _, p := range []string{"/", "/following", "/everything", fmt.Sprintf("/feeds/%d", f.a.ID)} {
		body := do(f.h, get(p, "user", nil)).Body.String()
		if strings.Contains(body, "<h3") || strings.Count(body, "<h1") != 1 {
			t.Errorf("%s signed in: headings wrong", p)
		}
	}
}

// Story and discussion links get a CSS-only off-site arrow (silent to screen
// readers) and open in the same tab; the internal feed-name link does not
// match the rule.
func TestOffsiteCue(t *testing.T) {
	f := newPublicFixture(t)
	if _, err := f.d.Writer.Exec(`UPDATE items SET comments_url = 'https://talk.example/t' WHERE dedup_key = 'a2'`); err != nil {
		t.Fatal(err)
	}
	css := do(f.h, get("/static/style.css", "", nil))
	if css.Code != http.StatusOK {
		t.Fatalf("style.css status %d", css.Code)
	}
	for _, want := range []string{`.post h2 a[href^="http"]::after`, `.item-meta a[href^="http"]::after`, `content: "↗" / "";`} {
		if !strings.Contains(css.Body.String(), want) {
			t.Errorf("style.css missing %q", want)
		}
	}
	body := do(f.h, get("/", "", nil)).Body.String()
	for _, want := range []string{`<a href="https://a.example/2">`, `<a href="https://talk.example/t">discussion</a>`, fmt.Sprintf(`<a href="/feeds/%d">Alpha News</a>`, f.a.ID)} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(body, "target=") {
		t.Error("links must open in the same tab")
	}
}

// The admin view of the catalog.
func TestAdminCatalogUX(t *testing.T) {
	h, d := newTestServer(t, nil)
	fd := seedFeed(t, d, "https://broken.example/"+strings.Repeat("x", 100))
	if _, err := d.Writer.Exec(`UPDATE feeds SET fail_count = 2, last_error = 'dial tcp: refused' WHERE id = ?`, fd.ID); err != nil {
		t.Fatal(err)
	}
	body := do(h, get(feedsPath, "admin", nil)).Body.String()
	for _, w := range []string{
		`<main id="main" class="wide">`, "<h1>Feeds</h1>", "<title>Feeds · scry</title>",
		`<details class="last-error"><summary>last error</summary><code>dial tcp: refused</code></details>`,
		`hx-disabled-elt="find button"`,
	} {
		if !strings.Contains(body, w) {
			t.Errorf("missing %q", w)
		}
	}
	// Every htmx form disables its button while in flight.
	if hx, dis := strings.Count(body, "hx-post="), strings.Count(body, `hx-disabled-elt="find button"`); hx != dis {
		t.Errorf("%d hx-post forms, %d with hx-disabled-elt", hx, dis)
	}
	// One accent action: Add. Row buttons are ghost (Subscribe included).
	if n := strings.Count(body, `<button type="submit"`); n != 1 || !strings.Contains(body, `<button type="submit">Add</button>`) {
		t.Errorf("%d accent buttons, want Add only", n)
	}
	// Everyone else keeps the narrow measure.
	for _, c := range []string{"", "user"} {
		if strings.Contains(do(h, get("/feeds", c, nil)).Body.String(), `class="wide"`) {
			t.Errorf("cookie %q: catalog is wide", c)
		}
	}
}

func TestOversizedBodyIsHTML(t *testing.T) {
	h := newTestHandler(t)
	rec := do(h, postForm(feedsPath, url.Values{"url": {strings.Repeat("a", maxBody+1)}}, "admin", nil))
	checkErrorPage(t, rec.Code, rec.Header(), rec.Body.String(), http.StatusRequestEntityTooLarge)
}

func TestFeedHeading(t *testing.T) {
	for _, tc := range []struct{ title, url, want string }{
		{"Alpha News", "https://a.example/feed", "Alpha News"},
		{"", "https://www.example.com/rss.xml", "example.com"},
		{"", "http://example.com:8080/rss", "example.com"},
		{"", "not a url", "not a url"},
	} {
		if got := feedHeading(tc.title, tc.url); got != tc.want {
			t.Errorf("feedHeading(%q, %q) = %q, want %q", tc.title, tc.url, got, tc.want)
		}
	}
}
