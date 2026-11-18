package server

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/0xD34DC0D3/scry/internal/db"
	"github.com/0xD34DC0D3/scry/internal/jobs"
	"github.com/0xD34DC0D3/scry/internal/web"
)

// maxFeedURL caps a feed URL in bytes.
const maxFeedURL = 2048

const feedsPath = "/admin/feeds"

// The feed catalog: admins list, add, pause, resume and remove feeds. scry's
// admins are voodoo's (is_admin); there is no local role table.
func (s *server) feedRoutes(mux *http.ServeMux) {
	admin := func(h http.HandlerFunc) http.Handler { return s.auth.RequireAdmin(h) }
	mux.Handle("GET "+feedsPath, admin(s.feedsPage))
	mux.Handle("POST "+feedsPath, admin(s.addFeed))
	// POST, not DELETE, so the remove button works as a plain form.
	mux.Handle("POST "+feedsPath+"/{id}/pause", admin(s.feedAction((*db.Queries).PauseFeed)))
	mux.Handle("POST "+feedsPath+"/{id}/resume", admin(s.feedAction((*db.Queries).ResumeFeed)))
	mux.Handle("POST "+feedsPath+"/{id}/remove", admin(s.feedAction((*db.Queries).DeleteFeed)))
	mux.Handle("POST "+feedsPath+"/{id}/sync", admin(s.syncFeed))
}

type feedsData struct {
	Feeds  []db.Feed
	URL    string // the add form's value, kept when it was rejected
	Error  string
	Notice string
}

func (s *server) renderFeeds(w http.ResponseWriter, r *http.Request, d feedsData, status int) {
	feeds, err := s.r.ListFeeds(r.Context())
	if err != nil {
		s.internalError(w, r, "list feeds", err)
		return
	}
	d.Feeds = feeds
	v := web.View{Page: "feeds", Fragment: "feeds-panel", Title: "Manage feeds", Data: d, Nav: "admin", Wide: true}
	// htmx swaps only 2xx responses by default, so a rejected add answers it
	// with 200 and the error inside the fragment; a plain post gets the 4xx.
	if r.Header.Get("HX-Request") != "true" {
		v.Status = status
	}
	s.web.Render(w, r, v)
}

func (s *server) feedsPage(w http.ResponseWriter, r *http.Request) {
	s.renderFeeds(w, r, feedsData{}, http.StatusOK)
}

// done answers a successful action: the fresh panel for htmx, otherwise
// post/redirect/get so a reload does not repost.
func (s *server) done(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("HX-Request") != "true" {
		http.Redirect(w, r, feedsPath, http.StatusSeeOther)
		return
	}
	s.renderFeeds(w, r, feedsData{}, http.StatusOK)
}

func (s *server) addFeed(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			s.web.Error(w, r, http.StatusRequestEntityTooLarge, "That request was too large.")
			return
		}
		s.badRequest(w, r, "That form could not be read.")
		return
	}
	raw := strings.TrimSpace(r.PostFormValue("url"))
	u, msg := normalizeFeedURL(raw)
	if msg != "" {
		s.renderFeeds(w, r, feedsData{URL: raw, Error: msg}, http.StatusBadRequest)
		return
	}
	if _, err := s.w.CreateFeed(r.Context(), u); err != nil {
		// ON CONFLICT DO NOTHING returns no row for a URL already stored.
		if errors.Is(err, sql.ErrNoRows) {
			s.renderFeeds(w, r, feedsData{URL: raw, Error: "That feed is already in the catalog."}, http.StatusConflict)
			return
		}
		s.internalError(w, r, "create feed", err)
		return
	}
	s.done(w, r)
}

// feedAction runs a by-id write; zero rows affected means no such feed
// (removed meanwhile, say), answered with the fresh panel and an error.
func (s *server) feedAction(q func(*db.Queries, context.Context, int64) (int64, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			s.notFound(w, r)
			return
		}
		n, err := q(s.w, r.Context(), id)
		if err != nil {
			s.internalError(w, r, "feed action", err)
			return
		}
		if n == 0 {
			s.renderFeeds(w, r, feedsData{Error: "That feed no longer exists."}, http.StatusNotFound)
			return
		}
		s.done(w, r)
	}
}

// syncFeed queues a sync through Inngest; the fetch itself runs in the job,
// so the panel shows its result only after a reload. A paused feed is sent
// too and the job skips it (it checks at its read and again in its write):
// the button is hidden for paused feeds, and a pause can land between this
// check and the job anyway.
func (s *server) syncFeed(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFound(w, r)
		return
	}
	if _, err := s.r.GetFeed(r.Context(), id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			s.renderFeeds(w, r, feedsData{Error: "That feed no longer exists."}, http.StatusNotFound)
			return
		}
		s.internalError(w, r, "get feed", err)
		return
	}
	if _, err := s.jobs.Send(r.Context(), jobs.SyncFeed(id)); err != nil {
		slog.Error("send sync event", "feed_id", id, "err", err)
		s.renderFeeds(w, r, feedsData{Error: "Could not queue the sync; try again."}, http.StatusServiceUnavailable)
		return
	}
	if r.Header.Get("HX-Request") != "true" {
		http.Redirect(w, r, feedsPath, http.StatusSeeOther)
		return
	}
	s.renderFeeds(w, r, feedsData{Notice: "Sync queued."}, http.StatusOK)
}

// normalizeFeedURL returns the form stored in feeds.url, or a message for
// the admin. Private addresses are not checked here: the fetcher refuses
// them on every request, redirects included.
func normalizeFeedURL(raw string) (string, string) {
	const want = "Enter an absolute http or https URL, such as https://example.com/feed.xml."
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return "", want
	}
	if u.User != nil {
		return "", "Remove the user name or password from the URL."
	}
	// url.Parse already lowercases the scheme.
	u.Host = strings.ToLower(u.Host)
	// Checked after encoding, which can grow the URL (a space becomes %20).
	normalized := u.String()
	if len(normalized) > maxFeedURL {
		return "", "That URL is too long (at most 2048 bytes once encoded)."
	}
	return normalized, ""
}
