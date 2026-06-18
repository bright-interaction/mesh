package web

import (
	"net/http"
	"strings"
	"testing"
)

func TestDocs(t *testing.T) {
	s, _ := cfgServer(t)
	h := s.Handler()

	// list is non-empty and has slug+title
	code, got := doJSON(t, h, "GET", "/api/docs", "")
	if code != 200 {
		t.Fatalf("docs list = %d", code)
	}
	pages, _ := got["pages"].([]any)
	if len(pages) == 0 {
		t.Fatal("no doc pages embedded")
	}
	first := pages[0].(map[string]any)
	slug, _ := first["slug"].(string)
	if slug == "" || first["title"] == "" {
		t.Fatalf("doc page missing slug/title: %+v", first)
	}

	// a page renders to HTML
	code, page := doJSON(t, h, "GET", "/api/docs/"+slug, "")
	if code != 200 {
		t.Fatalf("doc page = %d", code)
	}
	htmlStr, _ := page["html"].(string)
	if !strings.Contains(htmlStr, "<h1") {
		t.Errorf("rendered doc has no <h1>: %.80s", htmlStr)
	}

	// path traversal / unknown is rejected
	if code, _ := doJSON(t, h, "GET", "/api/docs/nope", ""); code != http.StatusNotFound {
		t.Errorf("unknown doc = %d, want 404", code)
	}
}
