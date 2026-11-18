package jobs

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/0xD34DC0D3/scry/internal/db"
	"github.com/0xD34DC0D3/scry/migrations"
)

const rssBody = `<?xml version="1.0"?>
<rss version="2.0"><channel>
  <title>Example RSS</title><link>https://example.com/</link>
  <item><title>First</title><link>https://example.com/1</link><guid>g1</guid>
    <author>ann@example.com (Ann)</author>
    <comments>https://example.com/1#comments</comments>
    <pubDate>Mon, 02 Jan 2006 15:04:05 GMT</pubDate>
    <description>&lt;p&gt;Hello &amp;amp; &lt;b&gt;world&lt;/b&gt;&lt;/p&gt;</description></item>
  <item><title>Second</title><link>https://example.com/2</link>
    <description>Two</description></item>
</channel></rss>`

const atomBody = `<?xml version="1.0" encoding="utf-8"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <title>Example Atom</title><link href="https://atom.example/"/>
  <id>urn:feed</id><updated>2006-01-02T15:04:05Z</updated>
  <entry><title>A1</title><id>urn:a1</id><link href="https://atom.example/a1"/>
    <updated>2006-01-02T15:04:05Z</updated><author><name>Bea</name></author>
    <summary>Short</summary>
    <content type="html">&lt;p&gt;Full &lt;em&gt;text&lt;/em&gt;&lt;/p&gt;</content></entry>
</feed>`

func openDB(t *testing.T) *db.DB {
	t.Helper()
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
	return d
}

type env struct {
	d    Deps
	feed int64
}

// newEnv serves h on loopback and adds a feed pointing at it. The fetcher
// may reach exactly that server: any other loopback or private address
// still meets the real check.
func newEnv(t *testing.T, h http.Handler) env {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	d := openDB(t)
	f, err := db.New(d.Writer).CreateFeed(context.Background(), srv.URL+"/feed")
	if err != nil {
		t.Fatal(err)
	}
	ap := netip.MustParseAddrPort(srv.Listener.Addr().String())
	fe := newFetcher()
	fe.allow = func(got netip.AddrPort) bool { return got == ap }
	return env{d: Deps{DB: d, fetcher: fe}, feed: f.ID}
}

func (e env) sync(t *testing.T, now time.Time) syncResult {
	t.Helper()
	res, err := e.d.syncFeed(context.Background(), e.feed, now)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func (e env) getFeed(t *testing.T) db.Feed {
	t.Helper()
	f, err := db.New(e.d.DB.Reader).GetFeed(context.Background(), e.feed)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (e env) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := e.d.DB.Writer.Exec(q, args...); err != nil {
		t.Fatal(err)
	}
}

type itemRow struct {
	key, url, title, author, html, excerpt string
	comments                               string
	published                              *int64
	firstSeen                              int64
}

func (e env) items(t *testing.T) map[string]itemRow {
	t.Helper()
	rows, err := e.d.DB.Reader.Query(`SELECT i.dedup_key, i.url, i.title, i.author, i.comments_url, i.published_at, i.first_seen_at, c.html, c.excerpt
		FROM items i JOIN item_contents c ON c.item_id = i.id WHERE i.feed_id = ?`, e.feed)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]itemRow{}
	for rows.Next() {
		var r itemRow
		if err := rows.Scan(&r.key, &r.url, &r.title, &r.author, &r.comments, &r.published, &r.firstSeen, &r.html, &r.excerpt); err != nil {
			t.Fatal(err)
		}
		out[r.key] = r
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func serve(ctype, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", ctype)
		fmt.Fprint(w, body)
	}
}

var now = time.Unix(1_800_000_000, 0)

func TestSyncRSS(t *testing.T) {
	var ua atomic.Value
	e := newEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua.Store(r.Header.Get("User-Agent"))
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Last-Modified", "Mon, 02 Jan 2006 15:04:05 GMT")
		fmt.Fprint(w, rssBody)
	}))
	e.exec(t, `UPDATE feeds SET fail_count = 3, last_error = 'old' WHERE id = ?`, e.feed)

	if res := e.sync(t, now); res.Status != "synced" || res.Items != 2 {
		t.Fatalf("result %+v", res)
	}
	if got, _ := ua.Load().(string); !strings.HasPrefix(got, "scry/") {
		t.Errorf("User-Agent = %q", got)
	}
	f := e.getFeed(t)
	if f.Title != "Example RSS" || f.SiteUrl != "https://example.com/" || f.Etag != `"v1"` ||
		f.LastModified != "Mon, 02 Jan 2006 15:04:05 GMT" || f.FailCount != 0 || f.LastError != "" ||
		f.NextFetchAt != now.Add(syncInterval).Unix() {
		t.Errorf("feed %+v", f)
	}
	items := e.items(t)
	first := items["g1"]
	if first.url != "https://example.com/1" || first.title != "First" || first.firstSeen != now.Unix() ||
		first.published == nil || *first.published != time.Date(2006, 1, 2, 15, 4, 5, 0, time.UTC).Unix() {
		t.Errorf("first %+v", first)
	}
	if first.html != "<p>Hello &amp; <b>world</b></p>" || first.excerpt != "Hello & world" {
		t.Errorf("first html %q excerpt %q", first.html, first.excerpt)
	}
	if first.comments != "https://example.com/1#comments" {
		t.Errorf("first comments_url %q", first.comments)
	}
	// No guid: the link is the key. No date: published_at stays NULL.
	if second, ok := items["https://example.com/2"]; !ok || second.published != nil || second.excerpt != "Two" {
		t.Errorf("second %+v (all: %v)", second, items)
	}
}

func TestSyncAtom(t *testing.T) {
	e := newEnv(t, serve("application/atom+xml", atomBody))
	if res := e.sync(t, now); res.Status != "synced" || res.Items != 1 {
		t.Fatalf("result %+v", res)
	}
	if f := e.getFeed(t); f.Title != "Example Atom" || f.SiteUrl != "https://atom.example/" {
		t.Errorf("feed %+v", f)
	}
	a := e.items(t)["urn:a1"]
	// Content wins over summary.
	if a.author != "Bea" || a.html != "<p>Full <em>text</em></p>" || a.excerpt != "Full text" || a.published == nil {
		t.Errorf("item %+v", a)
	}
}

func TestSyncNotModified(t *testing.T) {
	e := newEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"v1"` && r.Header.Get("If-Modified-Since") == "Mon, 02 Jan 2006 15:04:05 GMT" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Last-Modified", "Mon, 02 Jan 2006 15:04:05 GMT")
		fmt.Fprint(w, rssBody)
	}))
	e.sync(t, now)
	e.exec(t, `UPDATE feeds SET fail_count = 2, last_error = 'x' WHERE id = ?`, e.feed)
	e.exec(t, `UPDATE item_contents SET html = 'kept'`)
	e.exec(t, `UPDATE feeds SET title = 'Kept title' WHERE id = ?`, e.feed)

	later := now.Add(time.Hour)
	if res := e.sync(t, later); res.Status != "not_modified" {
		t.Fatalf("result %+v", res)
	}
	f := e.getFeed(t)
	if f.FailCount != 0 || f.LastError != "" || f.Etag != `"v1"` || f.LastModified == "" ||
		f.Title != "Kept title" || f.NextFetchAt != later.Add(syncInterval).Unix() {
		t.Errorf("feed %+v", f)
	}
	items := e.items(t)
	if len(items) != 2 || items["g1"].html != "kept" {
		t.Errorf("304 touched items: %+v", items)
	}
}

func TestSyncDedup(t *testing.T) {
	body := rssBody
	var mu atomic.Value
	mu.Store(body)
	e := newEnv(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, mu.Load().(string))
	}))
	e.sync(t, now)
	// A marker, so a second sync that reset first_seen_at would show.
	e.exec(t, `UPDATE items SET first_seen_at = 1`)
	body = strings.Replace(body, "<description>Two</description>", "<description>Two, edited</description>", 1)
	mu.Store(strings.Replace(body, "/1#comments<", "/1#talk<", 1))

	res := e.sync(t, now.Add(time.Hour))
	if res.Status != "synced" || res.Items != 2 {
		t.Fatalf("result %+v", res)
	}
	items := e.items(t)
	if len(items) != 2 {
		t.Fatalf("%d items", len(items))
	}
	for k, it := range items {
		if it.firstSeen != 1 {
			t.Errorf("%s first_seen_at = %d, want 1", k, it.firstSeen)
		}
	}
	if got := items["https://example.com/2"].html; got != "Two, edited" {
		t.Errorf("content not rewritten: %q", got)
	}
	if got := items["g1"].comments; got != "https://example.com/1#talk" {
		t.Errorf("comments_url not rewritten: %q", got)
	}
}

func TestSyncSanitizes(t *testing.T) {
	const body = `<rss version="2.0"><channel><title>t</title><item><guid>x</guid>
<description><![CDATA[<p onclick="steal()">Hi <a href="https://ok.example/" onmouseover="x()">link</a></p><script>alert(1)</script><img src="https://img.example/a.png" onerror="boom()"><style>p{}</style>]]></description>
</item></channel></rss>`
	e := newEnv(t, serve("application/rss+xml", body))
	e.sync(t, now)
	it := e.items(t)["x"]
	for _, bad := range []string{"<script", "alert", "onclick", "onmouseover", "onerror", "<style"} {
		if strings.Contains(it.html, bad) {
			t.Errorf("html keeps %q: %s", bad, it.html)
		}
	}
	if !strings.Contains(it.html, `href="https://ok.example/"`) || !strings.Contains(it.html, "<img") {
		t.Errorf("html lost safe markup: %s", it.html)
	}
	if it.excerpt != "Hi link" {
		t.Errorf("excerpt = %q", it.excerpt)
	}
}

func TestSyncFailures(t *testing.T) {
	big := strings.Repeat("x", 2048)
	for _, tc := range []struct {
		name    string
		h       http.HandlerFunc
		timeout time.Duration
		want    string
	}{
		{"bad xml", serve("application/rss+xml", "<rss><channel><item>"), 0, ""},
		{"http 500", func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "no", 500) }, 0, "HTTP 500"},
		{"oversize with length", serve("text/xml", big), 0, "larger than"},
		{"oversize chunked", func(w http.ResponseWriter, _ *http.Request) {
			// Flushing first forces chunked encoding: no Content-Length to check.
			w.(http.Flusher).Flush()
			fmt.Fprint(w, big)
		}, 0, "larger than"},
		{"timeout", func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		}, 100 * time.Millisecond, "Timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, tc.h)
			e.d.fetcher.maxBody = 1024
			if tc.timeout > 0 {
				e.d.fetcher.timeout = tc.timeout
			}
			res := e.sync(t, now)
			if res.Status != "failed" || !strings.Contains(res.Error, tc.want) {
				t.Fatalf("result %+v, want failed with %q", res, tc.want)
			}
			f := e.getFeed(t)
			if f.FailCount != 1 || f.LastError != res.Error || f.NextFetchAt != now.Add(30*time.Minute).Unix() {
				t.Errorf("feed %+v", f)
			}
			if n := len(e.items(t)); n != 0 {
				t.Errorf("%d items stored", n)
			}
		})
	}
}

func TestSyncSkips(t *testing.T) {
	var hits atomic.Int32
	e := newEnv(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, rssBody)
	}))
	e.exec(t, `UPDATE feeds SET disabled_at = 5 WHERE id = ?`, e.feed)
	if res := e.sync(t, now); res.Status != "skipped" {
		t.Errorf("paused: %+v", res)
	}
	if f := e.getFeed(t); f.FailCount != 0 || f.NextFetchAt != 0 || f.DisabledAt.Int64 != 5 {
		t.Errorf("paused feed changed: %+v", f)
	}
	missing := e
	missing.feed = e.feed + 100
	if res := missing.sync(t, now); res.Status != "skipped" {
		t.Errorf("missing: %+v", res)
	}
	if hits.Load() != 0 {
		t.Errorf("%d fetches", hits.Load())
	}
}

// The redirect target is loopback on another port: only the allow seam's
// exact address is admitted, so this is the public-address check refusing.
func TestSyncRedirectToPrivateRefused(t *testing.T) {
	var reached atomic.Bool
	private := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached.Store(true)
		fmt.Fprint(w, rssBody)
	}))
	t.Cleanup(private.Close)
	for name, target := range map[string]string{
		"loopback": private.URL + "/feed",
		"metadata": "http://169.254.169.254/latest/meta-data/",
		"ftp":      "ftp://example.com/feed",
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, http.RedirectHandler(target, http.StatusFound))
			res := e.sync(t, now)
			if res.Status != "failed" || !strings.Contains(res.Error, "refus") {
				t.Fatalf("result %+v", res)
			}
		})
	}
	if reached.Load() {
		t.Error("private server was reached")
	}
}

// Without the seam, the default fetcher refuses the loopback test server.
func TestDefaultFetcherRefusesLoopback(t *testing.T) {
	e := newEnv(t, serve("text/xml", rssBody))
	e.d.fetcher = newFetcher()
	if res := e.sync(t, now); res.Status != "failed" || !strings.Contains(res.Error, "refusing non-public address 127.0.0.1") {
		t.Fatalf("result %+v", res)
	}
}

func TestPublicAddr(t *testing.T) {
	for _, tc := range []struct {
		ip   string
		want bool
	}{
		{"93.184.216.34", true},
		{"2606:2800:220:1:248:1893:25c8:1946", true},
		{"127.0.0.1", false},
		{"::1", false},
		{"0.0.0.0", false},
		{"::", false},
		{"10.1.2.3", false},
		{"172.16.0.1", false},
		{"192.168.1.1", false},
		{"169.254.169.254", false},
		{"fe80::1", false},
		{"fec0::1", false},
		{"::7f00:1", false},
		{"::a9fe:a9fe", false},
		{"::ffff:0:7f00:1", false},
		{"fd00::1", false},
		{"100.64.0.1", false},
		{"224.0.0.1", false},
		{"ff02::1", false},
		{"255.255.255.255", false},
		{"::ffff:127.0.0.1", false},
		{"::ffff:10.0.0.1", false},
		{"64:ff9b::a00:1", false},
		{"2002:a00:1::", false},
		{"198.18.0.1", false},
	} {
		if got := publicAddr(netip.MustParseAddr(tc.ip)); got != tc.want {
			t.Errorf("publicAddr(%s) = %v, want %v", tc.ip, got, tc.want)
		}
	}
}

func TestDedupKeyAndExcerpt(t *testing.T) {
	const feed = `<rss version="2.0"><channel><title>t</title>
<item><title>Same</title><pubDate>Mon, 02 Jan 2006 15:04:05 GMT</pubDate></item>
<item><title>Same</title><pubDate>Tue, 03 Jan 2006 15:04:05 GMT</pubDate></item>
<item><title>Same</title><pubDate>Mon, 02 Jan 2006 15:04:05 GMT</pubDate></item>
</channel></rss>`
	p, err := parseFeed([]byte(feed))
	if err != nil {
		t.Fatal(err)
	}
	k := []string{p.items[0].dedupKey, p.items[1].dedupKey, p.items[2].dedupKey}
	if len(k[0]) != 64 || k[0] == k[1] || k[0] != k[2] {
		t.Errorf("hash keys %v", k)
	}

	long := strings.Repeat("é ", 400)
	got := excerpt("<p>" + long + "</p>")
	if n := len([]rune(got)); n > maxExcerpt || !strings.HasSuffix(got, "…") {
		t.Errorf("excerpt %d runes: %q", n, got)
	}
	if got := excerpt("<p>a</p>\n\n<p>b&nbsp; c</p>"); got != "a b c" {
		t.Errorf("excerpt = %q", got)
	}
}

// A pause that lands while the fetch is in flight wins: nothing is written
// and the counters stay as they were, whether the fetch worked or failed.
func TestSyncPausedDuringFetch(t *testing.T) {
	for name, status := range map[string]int{"success": http.StatusOK, "failure": http.StatusInternalServerError} {
		t.Run(name, func(t *testing.T) {
			var e env
			e = newEnv(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				e.exec(t, `UPDATE feeds SET disabled_at = 5 WHERE id = ?`, e.feed)
				w.WriteHeader(status)
				fmt.Fprint(w, rssBody)
			}))
			e.exec(t, `UPDATE feeds SET fail_count = 2, last_error = 'x', next_fetch_at = 7 WHERE id = ?`, e.feed)
			if res := e.sync(t, now); res.Status != "skipped" {
				t.Fatalf("result %+v", res)
			}
			if f := e.getFeed(t); f.FailCount != 2 || f.LastError != "x" || f.NextFetchAt != 7 || f.Title != "" {
				t.Errorf("feed changed: %+v", f)
			}
			if n := len(e.items(t)); n != 0 {
				t.Errorf("%d items written", n)
			}
		})
	}
}
