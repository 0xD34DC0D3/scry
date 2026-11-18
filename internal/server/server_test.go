package server

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xD34DC0D3/voodoo/voodooauth"
	"github.com/inngest/inngestgo"
	"github.com/pressly/goose/v3"

	"github.com/0xD34DC0D3/scry/internal/db"
	"github.com/0xD34DC0D3/scry/internal/jobs"
	"github.com/0xD34DC0D3/scry/migrations"
)

// fakeVoodoo answers /me: cookie "user" is a plain user, "admin" an admin,
// anything else 401.
func fakeVoodoo(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ck, err := r.Cookie(voodooauth.CookieName)
		switch {
		case r.URL.Path != "/me":
			http.NotFound(w, r)
		case err == nil && ck.Value == "user":
			_, _ = w.Write([]byte(`{"id":"u1","email":"user@example.com","is_admin":false,"timezone":"UTC"}`))
		case err == nil && ck.Value == "admin":
			_, _ = w.Write([]byte(`{"id":"u2","email":"admin@example.com","is_admin":true,"timezone":"UTC"}`))
		default:
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

const (
	testBaseURL = "https://scry.example.com"
	testSiteURL = "https://site.example.com"
)

// newTestHandler uses the real jobs client against the (absent) dev server:
// INNGEST_DEV keeps the SDK from demanding a signing key.
func newTestHandler(t *testing.T) http.Handler {
	t.Helper()
	h, _ := newTestServer(t, nil)
	return h
}

// newTestHandlerWith uses jc as the jobs client, or the real one when nil.
func newTestHandlerWith(t *testing.T, jc inngestgo.Client) http.Handler {
	t.Helper()
	h, _ := newTestServer(t, jc)
	return h
}

// newTestServer also returns the migrated DB behind the handler.
func newTestServer(t *testing.T, jc inngestgo.Client) (http.Handler, *db.DB) {
	t.Helper()
	auth := voodooauth.New(fakeVoodoo(t))
	auth.AppURL = testBaseURL
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	p, err := goose.NewProvider(goose.DialectSQLite3, d.Writer, migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Up(context.Background()); err != nil {
		t.Fatal(err)
	}
	if jc == nil {
		t.Setenv("INNGEST_DEV", "1")
		if jc, err = jobs.New(jobs.Deps{DB: d}); err != nil {
			t.Fatal(err)
		}
	}
	return New(d, auth, jc, testSiteURL), d
}

func withCookie(req *http.Request, v string) *http.Request {
	req.AddCookie(&http.Cookie{Name: voodooauth.CookieName, Value: v})
	return req
}

func do(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// postForm posts form values as the given voodoo cookie ("" for none).
func postForm(path string, vals url.Values, cookie string, hdr map[string]string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(vals.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if cookie != "" {
		withCookie(req, cookie)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	return req
}

func addFeedReq(u string, hdr map[string]string) *http.Request {
	return postForm(feedsPath, url.Values{"url": {u}}, "admin", hdr)
}

func TestHealthz(t *testing.T) {
	rec := do(newTestHandler(t), httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /healthz = %d, want 200", rec.Code)
	}
}

func TestSecurityHeaders(t *testing.T) {
	h := newTestHandler(t)
	// Also on a 404 and on a rejected cross-origin post: headers wrap everything.
	cross := addFeedReq("https://x.example/feed", map[string]string{"Sec-Fetch-Site": "cross-site"})
	for name, req := range map[string]*http.Request{
		"page":    httptest.NewRequest(http.MethodGet, "/", nil),
		"static":  httptest.NewRequest(http.MethodGet, "/static/style.css", nil),
		"missing": httptest.NewRequest(http.MethodGet, "/nope", nil),
		"csrf":    cross,
	} {
		t.Run(name, func(t *testing.T) {
			hd := do(h, req).Header()
			csp := hd.Get("Content-Security-Policy")
			if !strings.Contains(csp, "script-src 'self'") || strings.Contains(csp, "unsafe-inline") {
				t.Errorf("CSP = %q", csp)
			}
			if hd.Get("X-Content-Type-Options") != "nosniff" || hd.Get("Referrer-Policy") != "same-origin" {
				t.Errorf("missing nosniff/referrer headers: %v", hd)
			}
		})
	}
}

func TestStatic(t *testing.T) {
	h := newTestHandler(t)
	rec := do(h, httptest.NewRequest(http.MethodGet, "/static/htmx.min.js?v=abc", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Cache-Control"), "immutable") {
		t.Errorf("versioned: %d %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
	rec = do(h, httptest.NewRequest(http.MethodGet, "/static/fonts/OFL.txt", nil))
	if rec.Code != http.StatusOK || strings.Contains(rec.Header().Get("Cache-Control"), "immutable") {
		t.Errorf("unversioned: %d %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
}

func TestCrossOriginPostRejected(t *testing.T) {
	h := newTestHandler(t)
	for name, hdr := range map[string]map[string]string{
		"sec-fetch-site": {"Sec-Fetch-Site": "cross-site"},
		"origin":         {"Origin": "https://evil.example"},
	} {
		t.Run(name, func(t *testing.T) {
			if rec := do(h, addFeedReq("https://x.example/feed", hdr)); rec.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403", rec.Code)
			}
		})
	}
	// Same-origin browser post is allowed.
	ok := addFeedReq("https://fine.example/feed", map[string]string{"Sec-Fetch-Site": "same-origin"})
	if rec := do(h, ok); rec.Code != http.StatusSeeOther {
		t.Errorf("same-origin status = %d, want 303", rec.Code)
	}
}

func TestOversizedBodyRejected(t *testing.T) {
	h := newTestHandler(t)
	rec := do(h, addFeedReq(strings.Repeat("a", maxBody+1), nil))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rec.Code)
	}
}

func TestPanicRecovered(t *testing.T) {
	h := chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }),
		requestLogger, securityHeaders, recoverer(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "page", http.StatusInternalServerError)
		}))
	rec := do(h, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "page") {
		t.Errorf("status = %d, want 500 from the page func", rec.Code)
	}
	if rec.Header().Get("Content-Security-Policy") == "" {
		t.Error("500 lacks security headers")
	}
}

func TestAuth(t *testing.T) {
	h := newTestHandler(t)
	page := func(path, cookie string) *http.Request {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Accept", "text/html")
		if cookie != "" {
			withCookie(req, cookie)
		}
		return req
	}

	t.Run("signed-in header", func(t *testing.T) {
		body := do(h, page("/", "user")).Body.String()
		if !strings.Contains(body, "user@example.com") || !strings.Contains(body, "/logout") {
			t.Errorf("want email and logout form, got: %s", body)
		}
	})
	t.Run("anonymous header", func(t *testing.T) {
		body := do(h, page("/", "")).Body.String()
		if !strings.Contains(body, "/login?next=") || strings.Contains(body, "/logout") {
			t.Errorf("want login link only, got: %s", body)
		}
	})
	t.Run("admin link on home", func(t *testing.T) {
		if !strings.Contains(do(h, page("/", "admin")).Body.String(), `href="/admin/feeds"`) {
			t.Error("admin home lacks the catalog link")
		}
		for _, c := range []string{"user", ""} {
			if strings.Contains(do(h, page("/", c)).Body.String(), `href="/admin/feeds"`) {
				t.Errorf("cookie %q sees the catalog link", c)
			}
		}
	})
	t.Run("anonymous browser redirect", func(t *testing.T) {
		rec := do(h, page(feedsPath, ""))
		loc := rec.Header().Get("Location")
		// next= comes from SCRY_BASE_URL, not the request's Host.
		if rec.Code != http.StatusFound || !strings.HasSuffix(loc, "/login?next="+url.QueryEscape(testBaseURL+feedsPath)) {
			t.Errorf("anonymous: %d Location=%q", rec.Code, loc)
		}
	})
}

// The auth middleware passes the mux a copy of the request, so the route must
// still reach the log through recordRoute.
func TestLoggedRouteSurvivesAuth(t *testing.T) {
	h := newTestHandler(t)
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	do(h, withCookie(httptest.NewRequest(http.MethodGet, feedsPath, nil), "admin"))
	do(h, httptest.NewRequest(http.MethodGet, "/nope", nil))
	out := buf.String()
	if !strings.Contains(out, `route="GET /admin/feeds"`) {
		t.Errorf("route missing from log: %s", out)
	}
	// The catch-all 404 pattern "/" names unknown paths; the raw path is
	// never logged.
	if !strings.Contains(out, "route=/ ") || strings.Contains(out, "/nope") {
		t.Errorf("unmatched route logged wrong: %s", out)
	}
}

func TestPanicLoggedWithRoute(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	mux := http.NewServeMux()
	mux.HandleFunc("GET /boom", func(http.ResponseWriter, *http.Request) { panic("boom") })
	do(chain(recordRoute(mux), requestLogger, recoverer(func(http.ResponseWriter, *http.Request) {})), httptest.NewRequest(http.MethodGet, "/boom", nil))
	if out := buf.String(); !strings.Contains(out, `route="GET /boom"`) {
		t.Errorf("panic logged without its route: %s", out)
	}
}
