package jobs

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"html"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/microcosm-cc/bluemonday"
	"github.com/mmcdole/gofeed"
	"github.com/mmcdole/gofeed/rss"
)

const (
	maxExcerpt  = 300  // runes
	maxComments = 2048 // bytes, as for feed URLs
)

// Policies are safe for concurrent use once built.
var (
	ugc = bluemonday.UGCPolicy()
	// The space keeps "<p>a</p><p>b</p>" from reading as "ab"; script and
	// style contents are dropped, not kept as text.
	strict = bluemonday.StrictPolicy().AddSpaceWhenStrippingTag(true)
	// noLinks is strict minus link text, to tell a real summary from one
	// that is only "Comments" or "Read more" links.
	noLinks = bluemonday.StrictPolicy().AddSpaceWhenStrippingTag(true).SkipElementsContent("a")
)

type parsedFeed struct {
	title, siteURL string
	items          []parsedItem
}

type parsedItem struct {
	dedupKey, url, title, author string
	commentsURL                  string
	publishedAt                  *time.Time
	html, excerpt                string
}

func parseFeed(body []byte) (parsedFeed, error) {
	fp := gofeed.NewParser()
	fp.RSSTranslator = rssTranslator{}
	f, err := fp.Parse(bytes.NewReader(body))
	if err != nil {
		return parsedFeed{}, err
	}
	out := parsedFeed{title: strings.TrimSpace(f.Title), siteURL: strings.TrimSpace(f.Link)}
	for _, it := range f.Items {
		// Full content when the feed has it, else the description; a
		// link-only one ("Continue reading") falls through to the next, and
		// if both are, the item has none.
		var clean string
		for _, raw := range []string{it.Content, it.Description} {
			if c := ugc.Sanitize(raw); !linksOnly(c) {
				clean = c
				break
			}
		}
		p := parsedItem{
			dedupKey:    dedupKey(it),
			url:         strings.TrimSpace(it.Link),
			title:       strings.TrimSpace(it.Title),
			commentsURL: commentsURL(it.Custom[commentsKey]),
			html:        clean,
			excerpt:     excerpt(clean),
		}
		if len(it.Authors) > 0 {
			p.author = strings.TrimSpace(it.Authors[0].Name)
		}
		p.publishedAt = cmp.Or(it.PublishedParsed, it.UpdatedParsed)
		out.items = append(out.items, p)
	}
	return out, nil
}

// commentsKey holds RSS <comments> in gofeed.Item.Custom. A colon cannot
// start an XML element name, so no unknown element gofeed keeps there can
// collide with it.
const commentsKey = ":comments"

// rssTranslator is gofeed's RSS translation plus <comments>, which gofeed
// v1.5.0 parses but leaves off the universal Item.
type rssTranslator struct{ gofeed.DefaultRSSTranslator }

func (t rssTranslator) Translate(feed any) (*gofeed.Feed, error) {
	f, err := t.DefaultRSSTranslator.Translate(feed)
	if err != nil {
		return nil, err
	}
	src := feed.(*rss.Feed) // the default translator has checked
	// The default translator maps items one to one, in order.
	for i, it := range f.Items {
		if i >= len(src.Items) {
			break
		}
		if c := src.Items[i].Comments; c != "" {
			if it.Custom == nil {
				it.Custom = map[string]string{}
			}
			it.Custom[commentsKey] = c
		}
	}
	return f, nil
}

// commentsURL keeps only an absolute http(s) URL with a host, so the
// discussion link can only lead to another site, never run script or
// resolve against scry.
func commentsURL(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > maxComments {
		return ""
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	return s
}

// linksOnly reports sanitized HTML with no text outside links and no image,
// such as Hacker News' lone "Comments" link. Shown as an excerpt it says
// nothing, and the item's own links already cover it.
func linksOnly(clean string) bool {
	if strings.Contains(clean, "<img") {
		return false
	}
	return strings.TrimSpace(html.UnescapeString(noLinks.Sanitize(clean))) == ""
}

// dedupKey is the guid, else the link, else a hash of title and date. The
// raw date strings are hashed so a feed that changes nothing keeps its keys.
func dedupKey(it *gofeed.Item) string {
	if g := strings.TrimSpace(it.GUID); g != "" {
		return g
	}
	if l := strings.TrimSpace(it.Link); l != "" {
		return l
	}
	sum := sha256.Sum256([]byte(it.Title + "\x00" + it.Published + "\x00" + it.Updated))
	return hex.EncodeToString(sum[:])
}

// excerpt turns sanitized HTML into at most maxExcerpt runes of plain text.
// It is stored as text and escaped when rendered, so entities are decoded.
func excerpt(s string) string {
	s = strings.Join(strings.Fields(html.UnescapeString(strict.Sanitize(s))), " ")
	return truncate(s, maxExcerpt)
}

// truncate cuts s to at most n runes, ending with "…" when it cut.
func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	i := 0
	for j := range s {
		if i == n-1 { // room for the ellipsis
			return strings.TrimRight(s[:j], " ") + "…"
		}
		i++
	}
	return s
}
