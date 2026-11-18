package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/0xD34DC0D3/voodoo/voodooauth"
	"github.com/inngest/inngestgo"

	"github.com/0xD34DC0D3/scry/internal/jobs"
)

// fakeJobs stands in for the Inngest client: Serve reports what reached the
// handler, Send records events (or fails with sendErr). Unused methods panic
// via the nil embed.
type fakeJobs struct {
	inngestgo.Client
	sent    []any
	sendErr error
}

func (f *fakeJobs) Send(_ context.Context, evt any) (string, error) {
	if f.sendErr != nil {
		return "", f.sendErr
	}
	f.sent = append(f.sent, evt)
	return "id", nil
}

// Serve reads the whole body, as the SDK does, and says how much arrived and
// whether voodoo auth ran (it put a user on the context).
func (f *fakeJobs) Serve() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, err := io.Copy(io.Discard, r.Body)
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			http.Error(w, "too large", http.StatusRequestEntityTooLarge)
			return
		}
		fmt.Fprintf(w, "read=%d user=%v", n, voodooauth.UserFrom(r.Context()) != nil)
	})
}

func TestJobsRouteThroughMiddleware(t *testing.T) {
	h := newTestHandlerWith(t, &fakeJobs{})
	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	post := func(size int) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, jobs.Path, bytes.NewReader(make([]byte, size)))
		// Browser CSRF signals and a session cookie must not matter here.
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		req.Header.Set("Origin", "https://evil.example")
		return do(h, withCookie(req, "admin"))
	}

	// Well over the 64 KB cap every other route has.
	big := 1 << 20
	rec := post(big)
	if want := fmt.Sprintf("read=%d user=false", big); rec.Code != http.StatusOK || rec.Body.String() != want {
		t.Fatalf("got %d %q, want 200 %q", rec.Code, rec.Body.String(), want)
	}
	if rec := post(jobs.MaxBody + 1); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("over jobs.MaxBody: %d, want 413", rec.Code)
	}
	if !strings.Contains(logBuf.String(), `route=/api/inngest`) {
		t.Errorf("route missing from log: %s", logBuf.String())
	}
}

// The real SDK handler answers through the stack: in dev mode GET is its
// unauthenticated inspection, which lists the registered functions
// (sync-feed and schedule-feeds).
func TestJobsRouteRealSDK(t *testing.T) {
	h := newTestHandler(t)
	rec := do(h, httptest.NewRequest(http.MethodGet, jobs.Path, nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"function_count":3`) {
		t.Fatalf("GET %s = %d %s", jobs.Path, rec.Code, rec.Body.String())
	}
}
