package jobs

import (
	"net/url"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// prepareHTML readies an item's HTML for the reading page, which embeds it
// inside its own markup under the entry's h2 title. It runs before
// sanitizing, so the sanitizer has the last word on what is kept. It
// re-serializes s through the HTML5 parser and, on the way:
//
//   - balances tags: bluemonday keeps a stray </div> or </li>, which would
//     close the page's elements around the entry;
//   - drops every id and name: the UGC policy keeps id (and name on <map>),
//     with which a feed could claim the page's ids (title-<n>, item-<n>,
//     unread-total, ...) and hijack aria references, the #item-<n> return
//     anchor or an htmx target; bluemonday cannot remove an attribute from
//     a policy;
//   - resolves relative href, src and cite against base (the item's link,
//     else the feed's site), since on scry they would point at scry; an
//     in-page "#frag" becomes a link to that section of the original post
//     (its target id is gone here anyway). With no base they are dropped.
//     gofeed has already applied any Atom xml:base;
//   - marks images loading="lazy", which also keeps the bodies of collapsed
//     read entries (closed <details>, not rendered) from loading theirs;
//   - removes the first heading whose text is the item's title (the page
//     shows the title already) and demotes the rest two levels (h1 -> h3,
//     h2 -> h4, ..., capped at h6), so a post sits under its h2 title in
//     the page outline, in its own order.
//
// On a parse error s is returned as is (still sanitized afterwards).
func prepareHTML(s string, base *url.URL, title string) string {
	if strings.TrimSpace(s) == "" {
		return s
	}
	ctx := &html.Node{Type: html.ElementNode, Data: "div", DataAtom: atom.Div}
	nodes, err := html.ParseFragment(strings.NewReader(s), ctx)
	if err != nil {
		return s
	}
	p := preparer{base: base, title: normText(title)}
	var b strings.Builder
	for _, n := range nodes {
		p.walk(n)
	}
	for _, n := range nodes {
		if p.removed == n {
			continue
		}
		if err := html.Render(&b, n); err != nil {
			return s
		}
	}
	return b.String()
}

type preparer struct {
	base    *url.URL
	title   string
	removed *html.Node // the top-level node removed as the title, if any
}

func (p *preparer) walk(n *html.Node) {
	for c := n.FirstChild; c != nil; {
		next := c.NextSibling
		p.walk(c)
		c = next
	}
	if n.Type != html.ElementNode {
		return
	}
	kept := n.Attr[:0]
	for _, a := range n.Attr {
		if a.Namespace != "" {
			kept = append(kept, a)
			continue
		}
		switch a.Key {
		case "id", "name", "loading":
			continue
		case "href", "src", "cite":
			v, ok := p.resolve(a.Val)
			if !ok {
				continue
			}
			a.Val = v
		}
		kept = append(kept, a)
	}
	n.Attr = kept
	if n.DataAtom == atom.Img {
		n.Attr = append(n.Attr, html.Attribute{Key: "loading", Val: "lazy"})
	}
	if lvl := headingLevel(n); lvl > 0 {
		if p.title != "" && p.removed == nil && normText(text(n)) == p.title {
			p.removed = n
			if n.Parent != nil {
				n.Parent.RemoveChild(n)
			}
			return
		}
		lvl = min(lvl+2, 6)
		n.Data = "h" + string(rune('0'+lvl))
		n.DataAtom = atom.Lookup([]byte(n.Data))
	}
}

// resolve makes v absolute against the base; ok is false when v is
// relative and there is no base, or it does not parse.
func (p *preparer) resolve(v string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(v))
	if err != nil {
		return "", false
	}
	if u.IsAbs() {
		return v, true // the sanitizer judges the scheme
	}
	if p.base == nil {
		return "", false
	}
	return p.base.ResolveReference(u).String(), true
}

func headingLevel(n *html.Node) int {
	switch n.DataAtom {
	case atom.H1:
		return 1
	case atom.H2:
		return 2
	case atom.H3:
		return 3
	case atom.H4:
		return 4
	case atom.H5:
		return 5
	case atom.H6:
		return 6
	}
	return 0
}

func text(n *html.Node) string {
	var b strings.Builder
	var f func(*html.Node)
	f = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			f(c)
		}
	}
	f(n)
	return b.String()
}

// normText folds case and whitespace for comparing a heading to a title.
func normText(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// contentBase is the URL relative links in an item resolve against: its
// link, else the feed's site link; nil when neither is absolute http(s).
func contentBase(itemLink, siteLink string) *url.URL {
	for _, s := range []string{itemLink, siteLink} {
		u, err := url.Parse(strings.TrimSpace(s))
		if err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" {
			return u
		}
	}
	return nil
}
