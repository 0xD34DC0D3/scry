// Package server builds scry's HTTP handler: routes plus the middleware stack.
package server

import (
	"embed"
	"io/fs"
	"net/http"

	"github.com/0xD34DC0D3/voodoo/voodooauth"
	"github.com/inngest/inngestgo"

	"github.com/0xD34DC0D3/scry/internal/db"
	"github.com/0xD34DC0D3/scry/internal/jobs"
	"github.com/0xD34DC0D3/scry/internal/web"
)

const appName = "scry"

//go:embed views/*.html
var viewsFS embed.FS

type server struct {
	web  *web.Renderer
	auth *voodooauth.Client
	r    *db.Queries // read pool
	w    *db.Queries // single writer
	jobs inngestgo.Client
}

// New returns the full handler. Outermost first: log, security headers,
// recover (inside the logger so a panic is logged as a 500), body cap, CSRF,
// auth. Headers sit outside the CSRF check so rejections carry them too; auth
// sits inside it so a rejected cross-origin post costs no call to voodoo.
// Auth only identifies the caller; routes enforce with auth.Require*.
//
// jobs.Path is server-to-server (Inngest, authenticated by its signing key):
// it gets a larger body cap and skips CSRF and voodoo auth, but still runs
// through the log, headers and recover like every other route.
func New(d *db.DB, auth *voodooauth.Client, jc inngestgo.Client) http.Handler {
	views, err := fs.Sub(viewsFS, "views")
	if err != nil {
		panic(err) // embedded at build time; cannot fail at runtime
	}
	s := &server{web: web.New(appName, views, auth), auth: auth, r: db.New(d.Reader), w: db.New(d.Writer), jobs: jc}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.Handle("GET /static/", web.StaticHandler())
	// No method: the SDK answers GET (inspect), POST (call) and PUT (sync).
	mux.Handle(jobs.Path, jc.Serve())
	s.publicRoutes(mux)
	s.feedRoutes(mux)

	return chain(recordRoute(mux), requestLogger, securityHeaders, recoverer, limitBody, crossOrigin, skipJobs(auth.Middleware))
}
