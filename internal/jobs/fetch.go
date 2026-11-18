package jobs

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"
)

const (
	fetchTimeout = 15 * time.Second
	maxFeedBody  = 10 << 20
	maxRedirects = 5
	userAgent    = "scry/1 (RSS reader; +https://scry.0xd34dc0d3.dev)"
)

// fetcher GETs feeds. Feed URLs come from admins and from redirects chosen by
// feed servers, so every connection is checked against the address actually
// dialled: that covers redirects and DNS answers that change between lookups.
type fetcher struct {
	timeout time.Duration
	maxBody int64
	// allow admits one exact ip:port that the public-address rule would
	// refuse. Tests set it to their loopback httptest server; production
	// never sets it, and there is no flag or env var that can.
	allow func(netip.AddrPort) bool
}

func newFetcher() *fetcher {
	return &fetcher{timeout: fetchTimeout, maxBody: maxFeedBody}
}

// fetched is one response. notModified means a 304: keep what is stored.
type fetched struct {
	notModified  bool
	body         []byte
	etag         string
	lastModified string
}

var errTooLarge = errors.New("feed larger than 10 MB")

// client is built per fetch so a test's allow func is read at call time; the
// cost is a fresh connection per feed, which a sync of one feed pays anyway.
func (f *fetcher) client() *http.Client {
	d := &net.Dialer{
		Timeout: f.timeout,
		Control: func(_, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil {
				return err
			}
			if publicAddr(ap.Addr()) || (f.allow != nil && f.allow(ap)) {
				return nil
			}
			return fmt.Errorf("refusing non-public address %s", ap.Addr())
		},
	}
	return &http.Client{
		Timeout: f.timeout,
		Transport: &http.Transport{
			Proxy:               nil, // never HTTP(S)_PROXY: the proxy would dial for us, unchecked
			DialContext:         d.DialContext,
			TLSHandshakeTimeout: f.timeout,
			ForceAttemptHTTP2:   true,
			DisableKeepAlives:   true,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return errors.New("too many redirects")
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return fmt.Errorf("redirect to %s scheme refused", req.URL.Scheme)
			}
			return nil
		},
	}
}

// fetch does a conditional GET. Any non-2xx other than 304 is an error.
func (f *fetcher) fetch(ctx context.Context, rawURL, etag, lastModified string) (fetched, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fetched{}, err
	}
	if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
		return fetched{}, fmt.Errorf("%s scheme refused", req.URL.Scheme)
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/rss+xml, application/atom+xml, application/xml;q=0.9, text/xml;q=0.9, */*;q=0.5")
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	if lastModified != "" {
		req.Header.Set("If-Modified-Since", lastModified)
	}

	resp, err := f.client().Do(req)
	if err != nil {
		return fetched{}, err
	}
	defer resp.Body.Close()

	out := fetched{etag: resp.Header.Get("ETag"), lastModified: resp.Header.Get("Last-Modified")}
	if resp.StatusCode == http.StatusNotModified {
		// Servers may omit validators on a 304; the ones we sent still hold.
		out.notModified = true
		out.etag = cmp.Or(out.etag, etag)
		out.lastModified = cmp.Or(out.lastModified, lastModified)
		return out, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fetched{}, fmt.Errorf("HTTP %s", resp.Status)
	}
	if resp.ContentLength > f.maxBody {
		return fetched{}, errTooLarge
	}
	// One byte over the cap tells a full-size body from an oversize one; a
	// truncated feed would parse as a feed missing its newest items.
	body, err := io.ReadAll(io.LimitReader(resp.Body, f.maxBody+1))
	if err != nil {
		return fetched{}, err
	}
	if int64(len(body)) > f.maxBody {
		return fetched{}, errTooLarge
	}
	out.body = body
	return out, nil
}

// Ranges that IsGlobalUnicast and IsPrivate leave open but that are not the
// public internet, or that embed an IPv4 address that could be private.
var blockedPrefixes = func() []netip.Prefix {
	var ps []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8",       // "this network"
		"100.64.0.0/10",   // carrier-grade NAT
		"192.0.0.0/24",    // IETF protocol assignments
		"192.0.2.0/24",    // documentation
		"198.18.0.0/15",   // benchmarking
		"198.51.100.0/24", // documentation
		"203.0.113.0/24",  // documentation
		"240.0.0.0/4",     // reserved, and broadcast
		"::/96",           // IPv4-compatible: embeds IPv4
		"::ffff:0:0:0/96", // SIIT IPv4-translated: embeds IPv4
		"fec0::/10",       // site-local (deprecated, still private)
		"64:ff9b::/96",    // NAT64: embeds IPv4
		"64:ff9b:1::/48",  // local-use NAT64
		"2001::/32",       // Teredo: embeds IPv4
		"2001:db8::/32",   // documentation
		"2002::/16",       // 6to4: embeds IPv4
	} {
		ps = append(ps, netip.MustParsePrefix(s))
	}
	return ps
}()

// publicAddr reports whether a is a public unicast address. IsGlobalUnicast
// already rules out unspecified, loopback, multicast and link-local.
func publicAddr(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsGlobalUnicast() || a.IsPrivate() {
		return false
	}
	for _, p := range blockedPrefixes {
		if p.Contains(a) {
			return false
		}
	}
	return true
}
