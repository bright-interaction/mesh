package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testServer(t *testing.T) *httptest.Server {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, body string) {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("hub.md", "---\nid: hub\ntype: note\nwhen: 2026-01-01\ntags: [core]\n---\n# Hub\n[[alpha]] [[beta]]\n")
	write("alpha.md", "---\nid: alpha\ntype: note\nwhen: 2026-01-01\n---\n# Alpha\n[[beta]]\n")
	write("beta.md", "---\nid: beta\ntype: note\nwhen: 2026-01-01\n---\n# Beta\nleaf\n")

	s, err := NewServer(dir)
	if err != nil {
		t.Fatalf("NewServer (is git/index ok?): %v", err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(func() { ts.Close(); s.Close() })
	return ts
}

func get(t *testing.T, ts *httptest.Server, path string) (int, string, http.Header) {
	t.Helper()
	resp, err := http.Get(ts.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

func TestServerRoutes(t *testing.T) {
	ts := testServer(t)

	// SPA shell.
	code, body, _ := get(t, ts, "/")
	if code != 200 || !strings.Contains(body, "<canvas") {
		t.Fatalf("/ should serve the SPA shell: %d", code)
	}

	// graph.json is valid + carries the notes.
	code, body, hdr := get(t, ts, "/graph.json")
	if code != 200 || !strings.HasPrefix(hdr.Get("Content-Type"), "application/json") {
		t.Fatalf("/graph.json: %d %s", code, hdr.Get("Content-Type"))
	}
	var exp Export
	if err := json.Unmarshal([]byte(body), &exp); err != nil {
		t.Fatalf("graph.json not valid Export: %v", err)
	}
	if exp.Meta.NodeCount != 3 || exp.Meta.IndexID == "" {
		t.Fatalf("graph.json should have 3 notes + an index, got %+v", exp.Meta)
	}

	// Assets serve with the right content types.
	for _, tc := range []struct{ path, ctype string }{
		{"/assets/app.js", "application/javascript"},
		{"/assets/style.css", "text/css"},
		{"/assets/fonts/geist.woff2", "font/woff2"},
	} {
		code, _, hdr := get(t, ts, tc.path)
		if code != 200 || !strings.HasPrefix(hdr.Get("Content-Type"), tc.ctype) {
			t.Fatalf("%s: %d %s (want %s)", tc.path, code, hdr.Get("Content-Type"), tc.ctype)
		}
	}

	// Traversal + unknown assets are refused.
	if code, _, _ := get(t, ts, "/assets/../server.go"); code == 200 {
		t.Fatal("path traversal must not serve files outside assets")
	}
	if code, _, _ := get(t, ts, "/assets/nope.js"); code != 404 {
		t.Fatalf("unknown asset should 404, got %d", code)
	}
}

func TestDashboardAPI(t *testing.T) {
	ts := testServer(t)
	// A search bumps the queries counter the dashboard reports.
	if code, _, _ := get(t, ts, "/api/search?q=alpha"); code != 200 {
		t.Fatalf("search status %d", code)
	}
	code, body, _ := get(t, ts, "/api/dashboard")
	if code != 200 {
		t.Fatalf("dashboard status %d", code)
	}
	var d struct {
		Usage struct {
			Queries int `json:"queries"`
			Notes   int `json:"notes"`
		} `json:"usage"`
		EstTokensSaved int            `json:"est_tokens_saved"`
		Coverage       map[string]int `json:"coverage"`
	}
	if err := json.Unmarshal([]byte(body), &d); err != nil {
		t.Fatal(err)
	}
	if d.Usage.Queries < 1 {
		t.Errorf("queries counter = %d, want >= 1", d.Usage.Queries)
	}
	if d.Usage.Notes != 3 {
		t.Errorf("notes = %d, want 3", d.Usage.Notes)
	}
	if d.EstTokensSaved < 1 {
		t.Errorf("est_tokens_saved = %d, want > 0 after a query", d.EstTokensSaved)
	}
	if d.Coverage["note"] != 3 {
		t.Errorf("coverage[note] = %d, want 3", d.Coverage["note"])
	}
}
