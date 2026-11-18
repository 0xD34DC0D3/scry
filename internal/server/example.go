package server

import (
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/0xD34DC0D3/voodoo/voodooauth"

	"github.com/0xD34DC0D3/scry/internal/jobs"
	"github.com/0xD34DC0D3/scry/internal/web"
)

// EXAMPLE pages: the template's demonstration of the render helper, Reader,
// Writer, an htmx form that also works without JS, and auth (anyone may read,
// only a signed-in user may add, one admin-only page, and an admin-only button
// that sends an Inngest event whose job adds a note). Step 2 deletes this
// file, views/notes.html, views/admin.html and the notes table.

const maxNoteRunes = 500

func subViews() (fs.FS, error) { return fs.Sub(viewsFS, "views") }

func (s *server) exampleRoutes(mux *http.ServeMux) {
	// {$} so only "/" matches; a bare "/" would answer every unknown path.
	mux.HandleFunc("GET /{$}", s.notesPage)
	mux.Handle("POST /notes", s.auth.RequireUser(http.HandlerFunc(s.addNote)))
	// scry's admins are voodoo's (is_admin); there is no local role table.
	mux.Handle("GET /admin", s.auth.RequireAdmin(http.HandlerFunc(s.adminPage)))
	mux.Handle("POST /example/ping", s.auth.RequireAdmin(http.HandlerFunc(s.sendPing)))
}

// sendPing sends the example event. The note appears only once the job has
// run, so htmx gets a fragment that re-fetches the list shortly after.
func (s *server) sendPing(w http.ResponseWriter, r *http.Request) {
	if err := jobs.SendPing(r.Context(), s.jobs); err != nil {
		slog.Error("send ping", "err", err)
		http.Error(w, "could not send the event", http.StatusBadGateway)
		return
	}
	if r.Header.Get("HX-Request") != "true" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.web.Render(w, r, web.View{Page: "ping-sent", Fragment: "ping-sent"})
}

func (s *server) adminPage(w http.ResponseWriter, r *http.Request) {
	s.web.Render(w, r, web.View{Page: "admin", Title: "Admin", Data: voodooauth.UserFrom(r.Context())})
}

type noteView struct {
	Body    string
	Created string
}

func (s *server) view(r *http.Request) (web.View, error) {
	notes, err := s.r.ListNotes(r.Context(), 50)
	if err != nil {
		return web.View{}, err
	}
	nv := make([]noteView, len(notes))
	for i, n := range notes {
		nv[i] = noteView{n.Body, time.Unix(n.CreatedAt, 0).UTC().Format("2006-01-02 15:04")}
	}
	return web.View{Page: "notes", Fragment: "notes-panel", Title: "Notes", Data: nv}, nil
}

func (s *server) notesPage(w http.ResponseWriter, r *http.Request) {
	v, err := s.view(r)
	if err != nil {
		slog.Error("list notes", "err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	s.web.Render(w, r, v)
}

func (s *server) addNote(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	body := strings.TrimSpace(r.PostFormValue("body"))
	if body == "" || utf8.RuneCountInString(body) > maxNoteRunes {
		http.Error(w, "note must be 1-500 characters", http.StatusBadRequest)
		return
	}
	if _, err := s.w.CreateNote(r.Context(), body); err != nil {
		slog.Error("create note", "err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	// Without JS the browser needs post/redirect/get so reload does not repost.
	if r.Header.Get("HX-Request") != "true" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	v, err := s.view(r)
	if err != nil {
		slog.Error("list notes", "err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	s.web.Render(w, r, v)
}
