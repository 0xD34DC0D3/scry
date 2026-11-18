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
)

// maxFeedURL caps a feed URL in bytes.
const maxFeedURL = 2048

// feedsPath is the one catalog page. Everyone reads it (public.go);
// admins also add, pause, resume, sync and remove feeds from it.
const feedsPath = "/feeds"

// The catalog's admin actions. scry's admins are voodoo's (is_admin); there
// is no local role table. Users' subscribe/unsubscribe share the
// /feeds/{id}/ prefix (reading.go); each action is its own literal pattern,
// so the mux never has to choose between them.
func (s *server) feedRoutes(mux *http.ServeMux) {
	admin := func(h http.HandlerFunc) http.Handler { return s.auth.RequireAdmin(h) }
	mux.Handle("POST "+feedsPath, admin(s.addFeed))
	// POST, not DELETE, so the remove button works as a plain form.
	// Each names where focus goes after its htmx swap: the row's button
	// that replaced the clicked one (Pause swaps in Resume and back), or
	// the panel heading once the row is gone.
	mux.Handle("POST "+feedsPath+"/{id}/pause", admin(s.feedAction((*db.Queries).PauseFeed, "resume")))
	mux.Handle("POST "+feedsPath+"/{id}/resume", admin(s.feedAction((*db.Queries).ResumeFeed, "pause")))
	mux.Handle("POST "+feedsPath+"/{id}/remove", admin(s.feedAction((*db.Queries).DeleteFeed, "")))
	mux.Handle("POST "+feedsPath+"/{id}/sync", admin(s.syncFeed))
}

// done answers a successful action: the fresh panel for htmx, otherwise
// post/redirect/get so a reload does not repost.
func (s *server) done(w http.ResponseWriter, r *http.Request, d catalogData) {
	if !isHTMX(r) {
		http.Redirect(w, r, feedsPath, http.StatusSeeOther)
		return
	}
	s.renderCatalog(w, r, d, http.StatusOK)
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
		s.renderCatalog(w, r, catalogData{URL: raw, Error: msg, FocusAdd: true}, http.StatusBadRequest)
		return
	}
	if _, err := s.w.CreateFeed(r.Context(), u); err != nil {
		// ON CONFLICT DO NOTHING returns no row for a URL already stored.
		if errors.Is(err, sql.ErrNoRows) {
			s.renderCatalog(w, r, catalogData{URL: raw, Error: "That feed is already in the catalog.", FocusAdd: true}, http.StatusConflict)
			return
		}
		s.internalError(w, r, "create feed", err)
		return
	}
	s.done(w, r, catalogData{FocusAdd: true})
}

// feedAction runs a by-id write; zero rows affected means no such feed
// (removed meanwhile, say), answered with the fresh panel and an error.
// focus is the row's button to focus afterwards, "" for the heading.
func (s *server) feedAction(q func(*db.Queries, context.Context, int64) (int64, error), focus string) http.HandlerFunc {
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
			s.renderCatalog(w, r, catalogData{Error: "That feed no longer exists.", FocusHeading: true}, http.StatusNotFound)
			return
		}
		s.done(w, r, catalogData{FocusFeed: id, FocusAction: focus, FocusHeading: focus == ""})
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
			s.renderCatalog(w, r, catalogData{Error: "That feed no longer exists.", FocusHeading: true}, http.StatusNotFound)
			return
		}
		s.internalError(w, r, "get feed", err)
		return
	}
	if _, err := s.jobs.Send(r.Context(), jobs.SyncFeed(id)); err != nil {
		slog.Error("send sync event", "feed_id", id, "err", err)
		s.renderCatalog(w, r, catalogData{Error: "Couldn't start the sync. Try again.", FocusFeed: id, FocusAction: "sync"}, http.StatusServiceUnavailable)
		return
	}
	s.done(w, r, catalogData{Notice: "Sync queued.", FocusFeed: id, FocusAction: "sync"})
}

// normalizeFeedURL returns the form stored in feeds.url, or a message for
// the admin. Private addresses are not checked here: the fetcher refuses
// them on every request, redirects included.
func normalizeFeedURL(raw string) (string, string) {
	const want = "Enter a full http or https URL, like https://example.com/feed.xml."
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
