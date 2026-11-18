package jobs

import (
	"strings"
	"testing"
)

// Excerpt of a stored Go Blog entry (Atom, no xml:base): relative image and
// links, a site-name h1, and the post's title as an h1.
const goBlogEntry = `<entry><title>Seventeen Years of Go</title>
<link rel="alternate" href="https://go.dev/blog/17years"></link>
<id>tag:blog.golang.org,2013:blog.golang.org/17years</id>
<updated>2026-11-10T00:00:00+00:00</updated>
<content type="html">&lt;h1&gt;&lt;a href=&#34;/blog/&#34;&gt;The Go Blog&lt;/a&gt;&lt;/h1&gt;
&lt;h1&gt;Seventeen  Years of Go&lt;/h1&gt;
&lt;img src=&#34;17years/go17.png&#34; alt=&#34;The Go gopher&#34; width=&#34;480&#34; height=&#34;270&#34;/&gt;
&lt;h2 id=&#34;thanks&#34;&gt;Thanks&lt;/h2&gt;
&lt;p&gt;See &lt;a href=&#34;/doc/go1compat&#34;&gt;compatibility&lt;/a&gt; and &lt;a href=&#34;#thanks&#34;&gt;below&lt;/a&gt;, or &lt;a href=&#34;//pkg.go.dev/sync&#34;&gt;sync&lt;/a&gt;.&lt;/p&gt;
&lt;blockquote cite=&#34;/talks/x&#34;&gt;&lt;p&gt;q&lt;/p&gt;&lt;/blockquote&gt;</content></entry>`

func atomFeed(entries string) []byte {
	return []byte(`<?xml version="1.0" encoding="utf-8"?>
<feed xmlns="http://www.w3.org/2005/Atom"><title>The Go Blog</title><id>tag:x</id>
<updated>2026-11-10T00:00:00+00:00</updated>` + entries + `</feed>`)
}

func TestContentPrepared(t *testing.T) {
	it := parseOne(t, atomFeed(goBlogEntry))
	for _, want := range []string{
		`<h3><a href="https://go.dev/blog/" rel="nofollow noreferrer">The Go Blog</a></h3>`,
		`<img src="https://go.dev/blog/17years/go17.png" alt="The Go gopher" width="480" height="270" loading="lazy"/>`,
		`<h4>Thanks</h4>`,
		`href="https://go.dev/doc/go1compat"`,
		`href="https://go.dev/blog/17years#thanks"`,
		`href="https://pkg.go.dev/sync"`,
		`<blockquote cite="https://go.dev/talks/x">`,
	} {
		if !strings.Contains(it.html, want) {
			t.Errorf("html lacks %s\n%s", want, it.html)
		}
	}
	// The title heading is gone; it is the page's h2.
	if strings.Contains(it.html, "Seventeen") {
		t.Errorf("title heading kept: %s", it.html)
	}
}

func TestPrepareHTMLNoBase(t *testing.T) {
	got := prepareHTML(`<p><a href="/x">a</a> <img src="y.png" loading="eager"> <a href="https://ok.example/">b</a></p>`, nil, "")
	want := `<p><a>a</a> <img loading="lazy"/> <a href="https://ok.example/">b</a></p>`
	if got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestPrepareHTMLHeadings(t *testing.T) {
	tests := []struct{ in, title, want string }{
		{`<h1>Title</h1><h2>A</h2><h3>B</h3><h5>C</h5>`, "title", `<h4>A</h4><h5>B</h5><h6>C</h6>`},
		{`<div><h2>Other</h2><h2>Title</h2></div><h2>Title</h2>`, "Title", `<div><h4>Other</h4></div><h4>Title</h4>`},
		{`<h1>Not it</h1>`, "", `<h3>Not it</h3>`},
	}
	for _, tc := range tests {
		if got := prepareHTML(tc.in, nil, tc.title); got != tc.want {
			t.Errorf("prepareHTML(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
}
