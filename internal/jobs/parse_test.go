package jobs

import (
	"strings"
	"testing"
)

// rssItem wraps item children in a one-item RSS 2.0 feed.
func rssItem(children string) []byte {
	return []byte(`<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0"><channel><title>t</title><link>https://example.com/</link>
<item>` + children + `</item></channel></rss>`)
}

func parseOne(t *testing.T, body []byte) parsedItem {
	t.Helper()
	p, err := parseFeed(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.items) != 1 {
		t.Fatalf("%d items", len(p.items))
	}
	return p.items[0]
}

// A summary that is only links ("Comments", "Read more") says nothing; one
// with text keeps its links.
func TestLinkOnlyExcerpts(t *testing.T) {
	tests := []struct {
		name, children string
		wantExcerpt    string // "" means html is dropped too
	}{
		{
			name: "hacker news",
			children: `<title>Show HN: A thing</title><link>https://thing.example/</link>
<pubDate>Tue, 17 Nov 2026 12:00:00 +0000</pubDate>
<comments>https://news.ycombinator.com/item?id=45000001</comments>
<description><![CDATA[<a href="https://news.ycombinator.com/item?id=45000001">Comments</a>]]></description>`,
		},
		{
			name: "lobsters link post",
			children: `<title>Why SQLite</title><link>https://sqlite.example/why</link>
<guid isPermaLink="false">https://lobste.rs/s/abc123</guid>
<author>alice@users.lobste.rs (alice)</author>
<comments>https://lobste.rs/s/abc123/why_sqlite</comments>
<description>&lt;p&gt;&lt;a href="https://lobste.rs/s/abc123/why_sqlite"&gt;Comments&lt;/a&gt;&lt;/p&gt;</description>`,
		},
		{
			name: "lobsters text post",
			children: `<title>What are you working on?</title><link>https://lobste.rs/s/def456/what_are_you_working_on</link>
<guid isPermaLink="false">https://lobste.rs/s/def456</guid>
<comments>https://lobste.rs/s/def456/what_are_you_working_on</comments>
<description>&lt;p&gt;Share what you are building this week.&lt;/p&gt;
&lt;p&gt;&lt;a href="https://lobste.rs/s/def456/what_are_you_working_on"&gt;Comments&lt;/a&gt;&lt;/p&gt;</description>`,
			wantExcerpt: "Share what you are building this week. Comments",
		},
		{
			name: "read more only",
			children: `<title>Post</title><link>https://blog.example/post</link>
<description><![CDATA[ <a href="https://blog.example/post">Read more</a>&nbsp; ]]></description>`,
		},
		{
			name: "image only is kept",
			children: `<title>Comic</title><link>https://comic.example/1</link>
<description><![CDATA[<a href="https://comic.example/1"><img src="https://comic.example/1.png" alt=""></a>]]></description>`,
		},
		{
			name: "link-only content falls back to description",
			children: `<title>Essay</title><link>https://blog.example/essay</link>
<description>&lt;p&gt;An essay about feeds.&lt;/p&gt;</description>
<content:encoded xmlns:content="http://purl.org/rss/1.0/modules/content/"><![CDATA[<p><a href="https://blog.example/essay">Continue reading</a></p>]]></content:encoded>`,
			wantExcerpt: "An essay about feeds.",
		},
		{
			name: "text around a link is kept",
			children: `<title>Note</title><link>https://blog.example/n</link>
<description><![CDATA[See <a href="https://blog.example/x">this</a> first.]]></description>`,
			wantExcerpt: "See this first.",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			it := parseOne(t, rssItem(tc.children))
			if it.excerpt != tc.wantExcerpt {
				t.Errorf("excerpt = %q, want %q", it.excerpt, tc.wantExcerpt)
			}
			keepHTML := tc.wantExcerpt != "" || strings.HasPrefix(tc.name, "image")
			if (it.html != "") != keepHTML {
				t.Errorf("html = %q, want kept: %v", it.html, keepHTML)
			}
		})
	}
}

func TestCommentsURL(t *testing.T) {
	tests := []struct {
		name, comments, want string
	}{
		{"hacker news", "https://news.ycombinator.com/item?id=45000001", "https://news.ycombinator.com/item?id=45000001"},
		{"http, padded", "  http://forum.example/t/1  ", "http://forum.example/t/1"},
		{"upper-case scheme", "HTTPS://forum.example/t/1", "HTTPS://forum.example/t/1"},
		{"relative", "/item?id=1", ""},
		{"scheme-relative", "//forum.example/t/1", ""},
		{"javascript", "javascript:alert(1)", ""},
		{"javascript with slashes", "javascript://forum.example/%0aalert(1)", ""},
		{"ftp", "ftp://files.example/t/1", ""},
		{"no host", "https:///t/1", ""},
		{"too long", "https://forum.example/" + strings.Repeat("a", maxComments), ""},
		{"absent", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			children := `<title>x</title><link>https://example.com/x</link>`
			if tc.comments != "" {
				children += `<comments><![CDATA[` + tc.comments + `]]></comments>`
			}
			if got := parseOne(t, rssItem(children)).commentsURL; got != tc.want {
				t.Errorf("commentsURL = %q, want %q", got, tc.want)
			}
		})
	}
}

// Atom has no <comments>; the RSS translator must not leak into it.
func TestCommentsURLAtom(t *testing.T) {
	p, err := parseFeed([]byte(atomBody))
	if err != nil {
		t.Fatal(err)
	}
	if got := p.items[0].commentsURL; got != "" {
		t.Errorf("commentsURL = %q", got)
	}
}

// Content links leak no referrer; script and handlers are gone.
func TestContentSanitized(t *testing.T) {
	it := parseOne(t, rssItem(`<description><![CDATA[<p onclick="x()">See <a href="https://blog.example/x">this</a>.</p><script>alert(1)</script><iframe src="https://evil.example/"></iframe>]]></description>`))
	if !strings.Contains(it.html, `rel="nofollow noreferrer"`) {
		t.Errorf("link lacks rel noreferrer: %s", it.html)
	}
	for _, bad := range []string{"onclick", "script", "iframe"} {
		if strings.Contains(it.html, bad) {
			t.Errorf("html keeps %s: %s", bad, it.html)
		}
	}
}

// Stray end tags cannot reach the page, where they would close the
// reading entry around the content.
func TestContentBalanced(t *testing.T) {
	it := parseOne(t, rssItem(`<description><![CDATA[<p>x</div></li></article></ol><blockquote>open <ul><li>y</ol>]]></description>`))
	want := `<p>x</p><blockquote>open <ul><li>y</li></ul></blockquote>`
	if it.html != want {
		t.Errorf("html = %q, want %q", it.html, want)
	}
}

// Feed markup cannot define ids or names: they would collide with the
// reading page's own (aria targets, #item-<n> anchors, htmx targets).
func TestContentNoIDs(t *testing.T) {
	it := parseOne(t, rssItem(`<description><![CDATA[<h2 id="title-1">Hi</h2><p id="unread-total">x <a name="item-2" href="https://a.example/">y</a></p><map name="m"><area alt="a" href="https://a.example/"></map><dfn id="reading-panel">z</dfn>]]></description>`))
	for _, bad := range []string{"id=", "name="} {
		if strings.Contains(it.html, bad) {
			t.Errorf("html keeps %s: %s", bad, it.html)
		}
	}
	if !strings.Contains(it.html, "<h4>Hi</h4>") {
		t.Errorf("content lost: %s", it.html)
	}
}
