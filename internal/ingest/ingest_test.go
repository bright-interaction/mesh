// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

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

func TestLinearIngest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "lin_api_test" {
			t.Errorf("Linear auth header = %q, want raw key (no Bearer)", got)
		}
		w.Write([]byte(`{"data":{"issues":{"nodes":[{"identifier":"ENG-7","title":"Ship connectors","description":"do it","url":"https://linear.app/x/issue/ENG-7","createdAt":"2026-05-01T10:00:00Z","creator":{"name":"alex"}}]}}}`))
	}))
	defer srv.Close()
	apiBaseOverride = srv.URL
	t.Cleanup(func() { apiBaseOverride = "" })

	vault := t.TempDir()
	res, err := Run(context.Background(), vault, &Linear{Token: "lin_api_test", Client: srv.Client()}, time.Time{})
	if err != nil || res.Written != 1 {
		t.Fatalf("linear run: written=%d err=%v", res.Written, err)
	}
	entries, _ := os.ReadDir(filepath.Join(vault, "imported", "linear"))
	if len(entries) != 1 {
		t.Fatalf("want 1 linear note, got %d", len(entries))
	}
	b, _ := os.ReadFile(filepath.Join(vault, "imported", "linear", entries[0].Name()))
	for _, want := range []string{"source: import:linear", "ENG-7", "author: alex", "linear.app/x/issue/ENG-7"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("linear note missing %q", want)
		}
	}
}

func TestJiraIngestADF(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rest/api/3/search/jql" {
			t.Errorf("jira path = %q, want /rest/api/3/search/jql", r.URL.Path)
		}
		w.Write([]byte(`{"issues":[{"key":"OPS-3","fields":{"summary":"Rotate keys","created":"2026-04-02T08:00:00.000+0000","creator":{"displayName":"sam"},"description":{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"rotate the shared key"}]}]}}}]}`))
	}))
	defer srv.Close()
	apiBaseOverride = srv.URL
	t.Cleanup(func() { apiBaseOverride = "" })

	vault := t.TempDir()
	res, err := Run(context.Background(), vault, &Jira{Site: "https://acme.atlassian.net", Email: "me@acme.com", Token: "t", Client: srv.Client()}, time.Time{})
	if err != nil || res.Written != 1 {
		t.Fatalf("jira run: written=%d err=%v", res.Written, err)
	}
	entries, _ := os.ReadDir(filepath.Join(vault, "imported", "jira"))
	b, _ := os.ReadFile(filepath.Join(vault, "imported", "jira", entries[0].Name()))
	body := string(b)
	for _, want := range []string{"source: import:jira", "OPS-3", "author: sam", "rotate the shared key"} {
		if !strings.Contains(body, want) {
			t.Errorf("jira note missing %q:\n%s", want, body)
		}
	}
}

func TestNotionIngestTitle(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Notion-Version") == "" {
			t.Error("Notion-Version header missing")
		}
		w.Write([]byte(`{"results":[{"id":"page-1","url":"https://notion.so/page-1","created_time":"2026-03-01T00:00:00.000Z","last_edited_time":"2026-06-01T00:00:00.000Z","properties":{"Name":{"type":"title","title":[{"plain_text":"Launch plan"}]}}}]}`))
	}))
	defer srv.Close()
	apiBaseOverride = srv.URL
	t.Cleanup(func() { apiBaseOverride = "" })

	vault := t.TempDir()
	res, err := Run(context.Background(), vault, &Notion{Token: "t", Client: srv.Client()}, time.Time{})
	if err != nil || res.Written != 1 {
		t.Fatalf("notion run: written=%d err=%v", res.Written, err)
	}
	entries, _ := os.ReadDir(filepath.Join(vault, "imported", "notion"))
	b, _ := os.ReadFile(filepath.Join(vault, "imported", "notion", entries[0].Name()))
	for _, want := range []string{"source: import:notion", "Launch plan", "notion.so/page-1"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("notion note missing %q", want)
		}
	}
}

type fakeConn struct {
	sinces    []time.Time
	truncated bool // when true, Pull reports a truncated window
}

func (f *fakeConn) Name() string { return "fake" }
func (f *fakeConn) Key() string  { return "fake:1" }
func (f *fakeConn) Pull(_ context.Context, since time.Time) ([]Doc, bool, error) {
	f.sinces = append(f.sinces, since)
	return []Doc{{ExternalID: "x", Title: "X", CreatedAt: "2026-01-01"}}, f.truncated, nil
}

func TestIncrementalHighWaterMark(t *testing.T) {
	vault := t.TempDir()
	fc := &fakeConn{}
	if _, err := RunIncremental(context.Background(), vault, fc, Opts{}); err != nil {
		t.Fatal(err)
	}
	if !fc.sinces[0].IsZero() {
		t.Fatalf("first run since = %v, want zero (full)", fc.sinces[0])
	}
	if _, err := RunIncremental(context.Background(), vault, fc, Opts{}); err != nil {
		t.Fatal(err)
	}
	if fc.sinces[1].IsZero() {
		t.Fatalf("second run since is zero; high-water mark not applied")
	}
	// --full ignores the mark.
	if _, err := RunIncremental(context.Background(), vault, fc, Opts{Full: true}); err != nil {
		t.Fatal(err)
	}
	if !fc.sinces[2].IsZero() {
		t.Fatalf("--full run since = %v, want zero", fc.sinces[2])
	}
	if _, err := os.Stat(statePath(vault)); err != nil {
		t.Fatalf("state file not written: %v", err)
	}
}

// A truncated pull (the connector hit its page cap with more upstream) must NOT
// advance the high-water mark, so the un-pulled tail is re-fetched next run instead
// of being silently skipped forever. Regression test for the ingest data-loss bug.
func TestIncrementalTruncatedDoesNotAdvanceMark(t *testing.T) {
	vault := t.TempDir()
	fc := &fakeConn{truncated: true}
	if _, err := RunIncremental(context.Background(), vault, fc, Opts{}); err != nil {
		t.Fatal(err)
	}
	if _, err := RunIncremental(context.Background(), vault, fc, Opts{}); err != nil {
		t.Fatal(err)
	}
	// Both runs must see a zero `since`: a truncated first run must not have stamped a
	// mark, so the second run still pulls the whole window.
	if !fc.sinces[0].IsZero() || !fc.sinces[1].IsZero() {
		t.Fatalf("truncated pull advanced the mark: sinces=%v, want both zero", fc.sinces)
	}
	// Once the connector reports a complete window, the mark advances again.
	fc.truncated = false
	if _, err := RunIncremental(context.Background(), vault, fc, Opts{}); err != nil {
		t.Fatal(err)
	}
	if _, err := RunIncremental(context.Background(), vault, fc, Opts{}); err != nil {
		t.Fatal(err)
	}
	if fc.sinces[3].IsZero() {
		t.Fatalf("mark not applied after a complete (non-truncated) pull")
	}
}
