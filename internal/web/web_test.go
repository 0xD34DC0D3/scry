package web

import (
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
	New("app", pages, voodooauth.New("http://voodoo.invalid")).Render(rec, httptest.NewRequest(http.MethodGet, "/", nil), View{Page: "p", Data: struct{ Missing *struct{ Field int } }{}})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "partial") {
		t.Errorf("partial output leaked: %q", rec.Body.String())
	}
	if rec.Header().Get("Vary") != "HX-Request" {
		t.Errorf("Vary = %q", rec.Header().Get("Vary"))
	}
}
