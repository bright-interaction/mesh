package ingest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func countMD(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".md") {
			n++
		}
	}
	return n
}

func TestGitHubIngestProvenanceAndUpsert(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// One page, fewer than 100 items, so the connector stops after page 1.
		if strings.Contains(r.URL.RawQuery, "page=1") {
			w.Write([]byte(`[{"number":7,"title":"Fix the deploy","body":"it broke","html_url":"https://github.com/o/r/issues/7","created_at":"2026-05-01T10:00:00Z","user":{"login":"alex"}}]`))
			return
		}
		w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	apiBaseOverride = srv.URL
	t.Cleanup(func() { apiBaseOverride = "" })

	vault := t.TempDir()
	gh := &GitHub{Owner: "o", Repo: "r", Client: srv.Client()}
	res, err := Run(context.Background(), vault, gh, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Written != 1 {
		t.Fatalf("written = %d, want 1", res.Written)
	}
	dir := filepath.Join(vault, "imported", "github")
	if countMD(t, dir) != 1 {
		t.Fatalf("expected 1 imported note, got %d", countMD(t, dir))
	}
	// Find the file + check provenance.
	entries, _ := os.ReadDir(dir)
	b, _ := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	body := string(b)
	for _, want := range []string{"source: import:github", "source_url: https://github.com/o/r/issues/7", "author: alex", "imported_at:", "Fix the deploy"} {
		if !strings.Contains(body, want) {
			t.Errorf("imported note missing %q:\n%s", want, body)
		}
	}
	// Re-pull must upsert, not duplicate.
	if _, err := Run(context.Background(), vault, gh, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if countMD(t, dir) != 1 {
		t.Fatalf("re-pull duplicated notes: got %d, want 1", countMD(t, dir))
	}
}

func TestSlackIngest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true,"messages":[{"type":"message","ts":"1714557600.000200","user":"U1","text":"deploy is green again"}]}`))
	}))
	defer srv.Close()
	apiBaseOverride = srv.URL
	t.Cleanup(func() { apiBaseOverride = "" })

	vault := t.TempDir()
	res, err := Run(context.Background(), vault, &Slack{Channel: "C1", Client: srv.Client()}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Written != 1 {
		t.Fatalf("written = %d, want 1", res.Written)
	}
	dir := filepath.Join(vault, "imported", "slack")
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("expected 1 slack note, got %d", len(entries))
	}
	b, _ := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if !strings.Contains(string(b), "source: import:slack") || !strings.Contains(string(b), "deploy is green again") {
		t.Errorf("slack note missing provenance/body:\n%s", b)
	}
}
