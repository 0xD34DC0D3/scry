package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/0xD34DC0D3/voodoo/voodooauth"
)

func TestRenderErrorIsCleanly500(t *testing.T) {
	// Fails midway through execution, after some output was produced.
	pages := fstest.MapFS{"p.html": {Data: []byte(`{{define "p"}}partial {{.Data.Missing.Field}}{{end}}`)}}
	rec := httptest.NewRecorder()
	// A public page sets its cache policy before rendering; the 500 must
	// not inherit it.
	rec.Header().Set("Cache-Control", "public, max-age=60")
	New("app", "https://site.example", pages, voodooauth.New("http://voodoo.invalid")).Render(rec, httptest.NewRequest(http.MethodGet, "/", nil), View{Page: "p", Data: struct{ Missing *struct{ Field int } }{}})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "partial") {
		t.Errorf("partial output leaked: %q", rec.Body.String())
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	if rec.Header().Get("Vary") != "HX-Request" {
		t.Errorf("Vary = %q", rec.Header().Get("Vary"))
	}
}

func TestAccountChip(t *testing.T) {
	for _, tc := range []struct {
		name, email string
		want        []string
	}{
		{"ascii", "linell@example.com", []string{`title="linell@example.com"`, `aria-hidden="true">l</span>`, `Signed in as linell@example.com`}},
		{"multibyte", "\u00e9mile@example.com", []string{`aria-hidden="true">é</span>`}},
		{"escaped", `a"<b>@example.com`, []string{`title="a&#34;&lt;b&gt;@example.com"`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"id": "u1", "email": tc.email, "timezone": "UTC"})
			}))
			defer vd.Close()
			auth := voodooauth.New(vd.URL)
			pages := fstest.MapFS{"p.html": {Data: []byte(`{{define "p"}}{{template "top" .}}{{template "bottom" .}}{{end}}`)}}
			r := New("app", "https://site.example", pages, auth)
			h := auth.Middleware(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				r.Render(w, req, View{Page: "p"})
			}))
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.AddCookie(&http.Cookie{Name: voodooauth.CookieName, Value: "x"})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			body := rec.Body.String()
			for _, w := range tc.want {
				if !strings.Contains(body, w) {
					t.Errorf("missing %q in %s", w, body)
				}
			}
			if strings.Contains(body, "<span>"+tc.email) {
				t.Errorf("bare email still printed in header")
			}
			if !strings.Contains(body, "sign out") {
				t.Errorf("sign out button missing")
			}
		})
	}
}
