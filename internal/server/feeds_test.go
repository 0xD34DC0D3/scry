package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/0xD34DC0D3/scry/internal/db"
)

var hx = map[string]string{"HX-Request": "true"}

func seedFeed(t *testing.T, d *db.DB, u string) db.Feed {
	t.Helper()
	f, err := db.New(d.Writer).CreateFeed(context.Background(), u)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func getFeed(t *testing.T, d *db.DB, id int64) (db.Feed, bool) {
	t.Helper()
	feeds, err := db.New(d.Reader).ListFeeds(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range feeds {
		if f.ID == id {
			return f, true
		}
	}
	return db.Feed{}, false
}

func countFeeds(t *testing.T, d *db.DB) int {
	t.Helper()
	feeds, err := db.New(d.Reader).ListFeeds(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return len(feeds)
}

func actionPath(id int64, action string) string {
	return fmt.Sprintf("%s/%d/%s", feedsPath, id, action)
}

func isFragment(body string) bool {
	return !strings.Contains(body, "<html") && strings.Contains(body, `id="feeds-panel"`)
}

func TestFeedRoutesAuth(t *testing.T) {
	routes := []struct {
		name   string
		req    func(id int64, cookie string) *http.Request
		okCode int
	}{
		{"list", func(_ int64, c string) *http.Request {
			req := httptest.NewRequest(http.MethodGet, feedsPath, nil)
			if c != "" {
				withCookie(req, c)
			}
			return req
		}, http.StatusOK},
		{"add", func(_ int64, c string) *http.Request {
			return postForm(feedsPath, url.Values{"url": {"https://new.example/feed"}}, c, nil)
		}, http.StatusSeeOther},
		{"pause", func(id int64, c string) *http.Request { return postForm(actionPath(id, "pause"), nil, c, nil) }, http.StatusSeeOther},
		{"resume", func(id int64, c string) *http.Request { return postForm(actionPath(id, "resume"), nil, c, nil) }, http.StatusSeeOther},
		{"remove", func(id int64, c string) *http.Request { return postForm(actionPath(id, "remove"), nil, c, nil) }, http.StatusSeeOther},
	}
	callers := []struct {
		cookie string
		admin  bool
		want   int
	}{
		{"", false, http.StatusUnauthorized}, // not a browser: 401, not a redirect
		{"user", false, http.StatusForbidden},
		{"admin", true, 0},
	}
	for _, rt := range routes {
		for _, c := range callers {
			t.Run(rt.name+"/"+c.cookie, func(t *testing.T) {
				h, d := newTestServer(t, nil)
				f := seedFeed(t, d, "https://seed.example/feed")
				want := c.want
				if c.admin {
					want = rt.okCode
				}
				if rec := do(h, rt.req(f.ID, c.cookie)); rec.Code != want {
					t.Fatalf("status = %d, want %d", rec.Code, want)
				}
				if !c.admin {
					got, ok := getFeed(t, d, f.ID)
					if !ok || got.DisabledAt.Valid || countFeeds(t, d) != 1 {
						t.Errorf("non-admin changed the catalog: %+v exists=%v", got, ok)
					}
				}
			})
		}
	}
}

func TestAddFeedInvalid(t *testing.T) {
	for _, tc := range []struct{ name, url, msg string }{
		{"empty", "", "absolute http or https"},
		{"blank", "   ", "absolute http or https"},
		{"relative", "/feed.xml", "absolute http or https"},
		{"no scheme", "example.com/feed", "absolute http or https"},
		{"no host", "https:///feed", "absolute http or https"},
		{"opaque", "http:example.com", "absolute http or https"},
		{"ftp", "ftp://example.com/feed", "absolute http or https"},
		{"javascript", "javascript:alert(1)", "absolute http or https"},
		{"userinfo", "https://bob:secret@example.com/feed", "user name or password"},
		{"user only", "https://bob@example.com/feed", "user name or password"},
		{"too long", "https://example.com/" + strings.Repeat("a", maxFeedURL), "too long"},
		// Under the cap as typed, over it once each space becomes %20. The "x"
		// keeps TrimSpace from eating the spaces.
		{"too long encoded", "https://e.com/" + strings.Repeat(" ", 2000) + "x", "too long"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, d := newTestServer(t, nil)
			rec := do(h, addFeedReq(tc.url, nil))
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "<html") || !strings.Contains(rec.Body.String(), tc.msg) {
				t.Errorf("plain: %d, want 400 full page with %q", rec.Code, tc.msg)
			}
			rec = do(h, addFeedReq(tc.url, hx))
			if rec.Code != http.StatusOK || !isFragment(rec.Body.String()) || !strings.Contains(rec.Body.String(), tc.msg) {
				t.Errorf("htmx: %d, want 200 fragment with %q: %s", rec.Code, tc.msg, rec.Body.String())
			}
			if n := countFeeds(t, d); n != 0 {
				t.Errorf("%d feeds stored", n)
			}
		})
	}
}

func TestAddFeedNormalizes(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"  https://example.com/feed.xml  ", "https://example.com/feed.xml"},
		{"HTTPS://Example.COM/Path/Feed?Q=1", "https://example.com/Path/Feed?Q=1"},
		{"http://example.com:8080/rss", "http://example.com:8080/rss"},
	} {
		t.Run(tc.in, func(t *testing.T) {
			h, d := newTestServer(t, nil)
			if rec := do(h, addFeedReq(tc.in, nil)); rec.Code != http.StatusSeeOther {
				t.Fatalf("status = %d", rec.Code)
			}
			feeds, _ := db.New(d.Reader).ListFeeds(context.Background())
			if len(feeds) != 1 || feeds[0].Url != tc.want {
				t.Fatalf("stored %+v, want %q", feeds, tc.want)
			}
			// New feeds are due at once and active.
			if feeds[0].NextFetchAt != 0 || feeds[0].DisabledAt.Valid {
				t.Errorf("new feed not due/active: %+v", feeds[0])
			}
		})
	}
}

func TestAddFeedDuplicate(t *testing.T) {
	h, d := newTestServer(t, nil)
	seedFeed(t, d, "https://example.com/feed")
	// Differs only in case where normalization lowercases it.
	dup := "HTTPS://EXAMPLE.com/feed"
	rec := do(h, addFeedReq(dup, nil))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "already in the catalog") || !strings.Contains(rec.Body.String(), "<html") {
		t.Errorf("plain: %d, want 409 full page", rec.Code)
	}
	rec = do(h, addFeedReq(dup, hx))
	if rec.Code != http.StatusOK || !isFragment(rec.Body.String()) || !strings.Contains(rec.Body.String(), "already in the catalog") {
		t.Errorf("htmx: %d %s", rec.Code, rec.Body.String())
	}
	if n := countFeeds(t, d); n != 1 {
		t.Errorf("%d feeds, want 1", n)
	}
}

func TestFeedsFullVsFragment(t *testing.T) {
	h, d := newTestServer(t, nil)
	f := seedFeed(t, d, "https://seed.example/feed")
	reqs := map[string]func(map[string]string) *http.Request{
		"list": func(hdr map[string]string) *http.Request {
			req := withCookie(httptest.NewRequest(http.MethodGet, feedsPath, nil), "admin")
			for k, v := range hdr {
				req.Header.Set(k, v)
			}
			return req
		},
		"add": func(hdr map[string]string) *http.Request { return addFeedReq("https://other.example/feed", hdr) },
		"pause": func(hdr map[string]string) *http.Request {
			return postForm(actionPath(f.ID, "pause"), nil, "admin", hdr)
		},
		"resume": func(hdr map[string]string) *http.Request {
			return postForm(actionPath(f.ID, "resume"), nil, "admin", hdr)
		},
	}
	for _, name := range []string{"list", "add", "pause", "resume"} {
		t.Run(name, func(t *testing.T) {
			rec := do(h, reqs[name](hx))
			if rec.Code != http.StatusOK || !isFragment(rec.Body.String()) || !strings.Contains(rec.Body.String(), "https://seed.example/feed") {
				t.Errorf("htmx: %d %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Header().Get("Vary"), "HX-Request") {
				t.Errorf("Vary = %q", rec.Header().Get("Vary"))
			}
			// The add test posted a duplicate above; use a fresh URL.
			if name == "add" {
				rec = do(h, addFeedReq("https://third.example/feed", nil))
			} else {
				rec = do(h, reqs[name](nil))
			}
			if name == "list" {
				if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<html") || !strings.Contains(rec.Body.String(), `id="feeds-panel"`) {
					t.Errorf("plain: %d", rec.Code)
				}
				return
			}
			if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != feedsPath {
				t.Errorf("plain: %d Location=%q, want 303 %s", rec.Code, rec.Header().Get("Location"), feedsPath)
			}
		})
	}
	t.Run("remove", func(t *testing.T) {
		g := seedFeed(t, d, "https://gone.example/feed")
		rec := do(h, postForm(actionPath(g.ID, "remove"), nil, "admin", hx))
		if rec.Code != http.StatusOK || !isFragment(rec.Body.String()) || strings.Contains(rec.Body.String(), "gone.example") {
			t.Errorf("htmx: %d %s", rec.Code, rec.Body.String())
		}
		g = seedFeed(t, d, "https://gone.example/feed")
		rec = do(h, postForm(actionPath(g.ID, "remove"), nil, "admin", nil))
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != feedsPath {
			t.Errorf("plain: %d", rec.Code)
		}
	})
}

func TestFeedActionsChangeDB(t *testing.T) {
	h, d := newTestServer(t, nil)
	f := seedFeed(t, d, "https://seed.example/feed")
	// As if the fetcher had failed and backed off.
	if _, err := d.Writer.Exec(`UPDATE feeds SET fail_count = 3, last_error = 'boom', next_fetch_at = 99 WHERE id = ?`, f.ID); err != nil {
		t.Fatal(err)
	}
	post := func(action string) {
		t.Helper()
		if rec := do(h, postForm(actionPath(f.ID, action), nil, "admin", nil)); rec.Code != http.StatusSeeOther {
			t.Fatalf("%s: %d", action, rec.Code)
		}
	}

	post("pause")
	got, _ := getFeed(t, d, f.ID)
	if !got.DisabledAt.Valid {
		t.Fatal("pause did not set disabled_at")
	}
	page := do(h, withCookie(httptest.NewRequest(http.MethodGet, feedsPath, nil), "admin")).Body.String()
	if !strings.Contains(page, "paused") || !strings.Contains(page, "boom") || !strings.Contains(page, actionPath(f.ID, "resume")) {
		t.Errorf("list does not show the paused feed with its error: %s", page)
	}
	// A marker time, so a second pause that overwrote it would show.
	if _, err := d.Writer.Exec(`UPDATE feeds SET disabled_at = 1 WHERE id = ?`, f.ID); err != nil {
		t.Fatal(err)
	}
	post("pause") // idempotent
	if again, _ := getFeed(t, d, f.ID); !again.DisabledAt.Valid || again.DisabledAt.Int64 != 1 {
		t.Errorf("second pause moved disabled_at: 1 -> %v", again.DisabledAt)
	}

	post("resume")
	got, _ = getFeed(t, d, f.ID)
	if got.DisabledAt.Valid || got.FailCount != 0 || got.LastError != "" || got.NextFetchAt != 0 {
		t.Errorf("resume left %+v", got)
	}

	post("remove")
	if _, ok := getFeed(t, d, f.ID); ok {
		t.Error("remove left the feed")
	}
}

func TestFeedActionUnknownID(t *testing.T) {
	h, d := newTestServer(t, nil)
	f := seedFeed(t, d, "https://seed.example/feed")
	for _, action := range []string{"pause", "resume", "remove"} {
		for _, id := range []string{fmt.Sprint(f.ID + 1), "abc", "-1"} {
			t.Run(action+"/"+id, func(t *testing.T) {
				path := feedsPath + "/" + id + "/" + action
				if id == "abc" {
					// Not an id at all: a bare 404 either way.
					for _, hdr := range []map[string]string{nil, hx} {
						if rec := do(h, postForm(path, nil, "admin", hdr)); rec.Code != http.StatusNotFound {
							t.Errorf("hx=%v: %d, want 404", hdr != nil, rec.Code)
						}
					}
					return
				}
				const msg = "That feed no longer exists."
				rec := do(h, postForm(path, nil, "admin", nil))
				if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "<html") || !strings.Contains(rec.Body.String(), msg) {
					t.Errorf("plain: %d, want 404 full page with %q", rec.Code, msg)
				}
				rec = do(h, postForm(path, nil, "admin", hx))
				if rec.Code != http.StatusOK || !isFragment(rec.Body.String()) || !strings.Contains(rec.Body.String(), msg) {
					t.Errorf("htmx: %d, want 200 fragment with %q: %s", rec.Code, msg, rec.Body.String())
				}
			})
		}
	}
	if countFeeds(t, d) != 1 {
		t.Error("an unknown id touched the catalog")
	}
}

func TestRemoveFeedCascades(t *testing.T) {
	h, d := newTestServer(t, nil)
	gone := seedFeed(t, d, "https://gone.example/feed")
	kept := seedFeed(t, d, "https://kept.example/feed")
	for _, fid := range []int64{gone.ID, kept.ID} {
		for _, q := range []string{
			`INSERT INTO items (feed_id, dedup_key) VALUES (?, 'k')`,
			`INSERT INTO item_contents (item_id, html, excerpt) SELECT id, '<p>x</p>', 'x' FROM items WHERE feed_id = ?`,
			`INSERT INTO subscriptions (user_id, feed_id) VALUES ('u1', ?)`,
			`INSERT INTO item_reads (user_id, item_id) SELECT 'u1', id FROM items WHERE feed_id = ?`,
		} {
			if _, err := d.Writer.Exec(q, fid); err != nil {
				t.Fatal(q, err)
			}
		}
	}
	if rec := do(h, postForm(actionPath(gone.ID, "remove"), nil, "admin", nil)); rec.Code != http.StatusSeeOther {
		t.Fatalf("remove: %d", rec.Code)
	}
	for _, table := range []string{"items", "item_contents", "subscriptions", "item_reads"} {
		var n int
		if err := d.Reader.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("%s has %d rows, want 1 (the kept feed's)", table, n)
		}
	}
}
