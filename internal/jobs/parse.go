package jobs

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"html"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/microcosm-cc/bluemonday"
	"github.com/mmcdole/gofeed"
)

const maxExcerpt = 300 // runes

// Policies are safe for concurrent use once built.
var (
	ugc = bluemonday.UGCPolicy()
	// The space keeps "<p>a</p><p>b</p>" from reading as "ab"; script and
	// style contents are dropped, not kept as text.
	strict = bluemonday.StrictPolicy().AddSpaceWhenStrippingTag(true)
)

type parsedFeed struct {
	title, siteURL string
	items          []parsedItem
}

type parsedItem struct {
	dedupKey, url, title, author string
	publishedAt                  *time.Time
	html, excerpt                string
}

func parseFeed(body []byte) (parsedFeed, error) {
	f, err := gofeed.NewParser().Parse(bytes.NewReader(body))
	if err != nil {
		return parsedFeed{}, err
	}
	out := parsedFeed{title: strings.TrimSpace(f.Title), siteURL: strings.TrimSpace(f.Link)}
	for _, it := range f.Items {
		// Full content when the feed has it; many feeds put only a summary
		// in description.
		raw := it.Content
		if strings.TrimSpace(raw) == "" {
			raw = it.Description
		}
		clean := ugc.Sanitize(raw)
		p := parsedItem{
			dedupKey: dedupKey(it),
			url:      strings.TrimSpace(it.Link),
			title:    strings.TrimSpace(it.Title),
			html:     clean,
			excerpt:  excerpt(clean),
		}
		if len(it.Authors) > 0 {
			p.author = strings.TrimSpace(it.Authors[0].Name)
		}
		p.publishedAt = cmp.Or(it.PublishedParsed, it.UpdatedParsed)
		out.items = append(out.items, p)
	}
	return out, nil
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
