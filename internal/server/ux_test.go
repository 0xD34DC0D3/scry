package server

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

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
		{"/", "", `<a href="/" aria-current="page">latest</a>`, []string{`href="/feeds" aria-current`}},
		{"/feeds", "", `<a href="/feeds" aria-current="page">feeds</a>`, []string{`href="/" aria-current`}},
		// One feed sits below /feeds: the section, not the page.
		{fmt.Sprintf("/feeds/%d", f.a.ID), "", `<a href="/feeds" aria-current="true">feeds</a>`, []string{`aria-current="page"`}},
		{feedsPath, "admin", `<a href="/admin/feeds" aria-current="page">admin</a>`, []string{`href="/feeds" aria-current`}},
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

// The admin link on /feeds is cache-safe only because an admin request
// carries a credential, which makes the page private.
func TestCatalogAdminLink(t *testing.T) {
	f := newPublicFixture(t)
	for _, tc := range []struct {
		cookie string
		want   bool
	}{{"", false}, {"user", false}, {"garbage", false}, {"admin", true}} {
		t.Run(tc.cookie, func(t *testing.T) {
			rec := do(f.h, get("/feeds", tc.cookie, nil))
			if got := strings.Contains(rec.Body.String(), `href="/admin/feeds">Manage feeds →</a>`); got != tc.want {
				t.Errorf("manage link shown = %v, want %v", got, tc.want)
			}
			if tc.want && rec.Header().Get("Cache-Control") != privateCache {
				t.Errorf("admin catalog Cache-Control = %q", rec.Header().Get("Cache-Control"))
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
		{"latest anonymous", "/", "", []string{`class="empty"`, `href="/feeds">Browse the feeds</a>`}, []string{"end of 30 days"}},
		{"latest admin", "/", "admin", []string{`href="/admin/feeds">Manage feeds</a>`}, []string{"Browse the feeds"}},
		{"feed", fmt.Sprintf("/feeds/%d", empty.ID), "", []string{`class="empty"`, `<a href="/">See the latest from every feed</a>`}, []string{"end of 30 days"}},
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
	if body := do(h2, get("/feeds", "admin", nil)).Body.String(); !strings.Contains(body, `<a href="/admin/feeds">Add one</a>`) {
		t.Errorf("admin empty catalog lacks the add link: %s", body)
	}
}

func TestEndOfList(t *testing.T) {
	f := newPublicFixture(t)
	const end = `<li class="end">— end of 30 days —</li>`
	for _, p := range []string{"/", fmt.Sprintf("/feeds/%d", f.a.ID)} {
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
			if body := rec.Body.String(); rec.Code != http.StatusOK || strings.Contains(body, `class="item"`) || !strings.Contains(body, end) {
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
	frag := do(f.h, get("/", "", hx)).Body.String()
	if strings.Contains(full, "autofocus") {
		t.Error("full page has autofocus")
	}
	if strings.Count(frag, "autofocus") != 1 || !strings.Contains(frag, `<li class="item" tabindex="-1" autofocus>`) {
		t.Errorf("fragment autofocus: %s", frag)
	}
	for _, p := range []string{"/", "/feeds", fmt.Sprintf("/feeds/%d", f.a.ID), "/nope"} {
		if body := do(f.h, get(p, "", nil)).Body.String(); strings.Contains(body, "<h3") {
			t.Errorf("%s skips from h1 to h3", p)
		}
	}
}

func TestAdminPageUX(t *testing.T) {
	h, d := newTestServer(t, nil)
	fd := seedFeed(t, d, "https://broken.example/"+strings.Repeat("x", 100))
	if _, err := d.Writer.Exec(`UPDATE feeds SET fail_count = 2, last_error = 'dial tcp: refused' WHERE id = ?`, fd.ID); err != nil {
		t.Fatal(err)
	}
	body := do(h, get(feedsPath, "admin", nil)).Body.String()
	for _, w := range []string{
		`<main id="main" class="wide">`, "<h1>Manage feeds</h1>", "<title>Manage feeds · scry</title>",
		`<details class="last-error"><summary>show</summary><code>dial tcp: refused</code></details>`,
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
	// Public pages keep the narrow measure.
	if strings.Contains(do(h, get("/feeds", "admin", nil)).Body.String(), `class="wide"`) {
		t.Error("public catalog is wide")
	}
}

func TestOversizedBodyIsHTML(t *testing.T) {
	h := newTestHandler(t)
	rec := do(h, postForm(feedsPath, url.Values{"url": {strings.Repeat("a", maxBody+1)}}, "admin", nil))
	checkErrorPage(t, rec.Code, rec.Header(), rec.Body.String(), http.StatusRequestEntityTooLarge)
}
