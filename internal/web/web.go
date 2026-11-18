// Package web holds the shared HTML chrome (layout template, stylesheet,
// fonts, vendored htmx) and the one helper that renders a page.
//
// Vendored: static/htmx.min.js is htmx 2.0.11 from
// https://cdn.jsdelivr.net/npm/htmx.org@2.0.11/dist/htmx.min.js
// sha256 d6fdc75f204e6bdefa99b69bf1e6d4ac69b8a364f77929f45c13476b4000f717
// To upgrade: download the new file, update this comment, run the tests.
package web

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"

	"github.com/0xD34DC0D3/voodoo/voodooauth"
)

// SiteURL is the org landing page the header tile and footer link to.
const SiteURL = "https://0xd34dc0d3.dev"

//go:embed layout.html
var layoutFS embed.FS

//go:embed static
var staticFS embed.FS

// static is rooted so "style.css" is a top-level entry, ready to mount under
// "/static/".
var static = func() fs.FS {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err)
	}
	return sub
}()

// version fingerprints the static files. Pages link them as ?v=<version>, so
// a request carrying it can be cached forever: a changed file changes the URL.
var version = func() string {
	h := sha256.New()
	err := fs.WalkDir(static, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(static, p)
		h.Write([]byte(p))
		h.Write(b)
		return err
	})
	if err != nil {
		panic(err)
	}
	return hex.EncodeToString(h.Sum(nil))[:10]
}()

// StaticHandler serves the embedded static files; mount it at "/static/".
// Versioned URLs (?v=) are immutable. Unversioned ones (fonts, referenced
// from the CSS) get a short cache so a changed file still reaches readers.
func StaticHandler() http.Handler {
	files := http.StripPrefix("/static/", http.FileServerFS(static))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("v") != "" {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "public, max-age=3600")
		}
		files.ServeHTTP(w, r)
	})
}

// Renderer renders page templates inside the shared layout.
type Renderer struct {
	t    *template.Template
	app  string
	auth *voodooauth.Client
}

// New parses the layout plus every *.html in pages into one template set, so
// feature packages embed only their own templates. Templates are program
// text, so a parse error panics at startup. auth supplies the login and
// logout links in the header.
func New(app string, pages fs.FS, auth *voodooauth.Client) *Renderer {
	t := template.Must(template.ParseFS(layoutFS, "layout.html"))
	return &Renderer{t: template.Must(t.ParseFS(pages, "*.html")), app: app, auth: auth}
}

// View describes one response. Page is the full-page template, Fragment the
// block that alone replaces part of the page for an htmx request. Status is
// the HTTP status, 200 when zero.
type View struct {
	Page     string
	Fragment string
	Title    string
	Data     any
	Status   int
}

// page is what templates see: layout fields plus the handler's Data. User
// is nil when signed out; every page can show who is signed in.
type page struct {
	Title, App, SiteURL, Ver string
	User                     *voodooauth.User
	LoginURL, LogoutURL      string
	Data                     any
}

// Render writes the full page, or just the fragment when the request has
// HX-Request: true. Vary is always set so a cache never serves one to the
// other. The template runs into a buffer first so a failure is a clean 500
// instead of a half-written page.
func (r *Renderer) Render(w http.ResponseWriter, req *http.Request, v View) {
	w.Header().Add("Vary", "HX-Request")
	name := v.Page
	if v.Fragment != "" && req.Header.Get("HX-Request") == "true" {
		name = v.Fragment
	}
	var buf bytes.Buffer
	p := page{
		Title: v.Title, App: r.app, SiteURL: SiteURL, Ver: version, Data: v.Data,
		User:      voodooauth.UserFrom(req.Context()),
		LoginURL:  r.auth.LoginURL(req),
		LogoutURL: r.auth.LogoutURL(),
	}
	if err := r.t.ExecuteTemplate(&buf, name, p); err != nil {
		slog.Error("render", "template", name, "err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if v.Status != 0 {
		w.WriteHeader(v.Status)
	}
	_, _ = buf.WriteTo(w)
}
