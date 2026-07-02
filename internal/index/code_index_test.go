// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bright-interaction/mesh/internal/index/code"
)

func tmpStore(t testing.TB) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestCodeIndexRoundTrip(t *testing.T) {
	s := tmpStore(t)
	files := []*code.CodeFile{
		{Path: "demo/a.go", Lang: "go", Package: "demo", Symbols: []code.Symbol{
			{Name: "Hello", Kind: "func", Start: 10, End: 12, Signature: "func Hello(name string) string", Calls: []string{"greet", "ToUpper"}},
			{Name: "greet", Kind: "func", Start: 14, End: 14, Signature: "func greet(n string) string"},
		}},
		{Path: "demo/b.go", Lang: "go", Package: "demo", Symbols: []code.Symbol{
			{Name: "Greeter.Greet", Kind: "method", Start: 5, End: 5, Signature: "func (g *Greeter) Greet() string", Calls: []string{"Hello"}},
		}},
	}
	st, err := s.IndexCodeFull(files)
	if err != nil {
		t.Fatalf("IndexCodeFull: %v", err)
	}
	if st.Files != 2 || st.Symbols != 3 {
		t.Fatalf("stats = %+v, want 2 files / 3 symbols", st)
	}
	if st.Edges != 2 {
		t.Errorf("edges = %d, want 2 (Hello->greet, Greet->Hello); ToUpper has no symbol", st.Edges)
	}

	hits, err := s.SearchCode("Hello", 5, nil)
	if err != nil {
		t.Fatalf("SearchCode: %v", err)
	}
	if len(hits) == 0 || hits[0].Name != "Hello" {
		t.Fatalf("search Hello = %+v, want Hello first", hits)
	}
	if hits[0].Path != "demo/a.go" || hits[0].Line != 10 {
		t.Errorf("Hello loc = %s:%d, want demo/a.go:10", hits[0].Path, hits[0].Line)
	}

	id := "code:demo/a.go#Hello"
	callers, callees, err := s.CodeNeighbors(id)
	if err != nil {
		t.Fatalf("CodeNeighbors: %v", err)
	}
	if len(callees) != 1 || callees[0].Name != "greet" {
		t.Errorf("Hello callees = %+v, want [greet]", callees)
	}
	if len(callers) != 1 || callers[0].Name != "Greeter.Greet" {
		t.Errorf("Hello callers = %+v, want [Greeter.Greet]", callers)
	}

	if filtered, _ := s.SearchCode("Hello", 5, []string{"py"}); len(filtered) != 0 {
		t.Errorf("py-filtered search returned %d, want 0", len(filtered))
	}

	// A second full index must be idempotent (wipe + rebuild), not accumulate.
	st2, err := s.IndexCodeFull(files)
	if err != nil {
		t.Fatalf("re-index: %v", err)
	}
	if st2.Symbols != 3 || st2.Edges != 2 {
		t.Errorf("re-index stats = %+v, want stable 3 symbols / 2 edges", st2)
	}
}

func TestSplitIdent(t *testing.T) {
	cases := map[string][]string{
		"DeployHandler": {"deployhandler", "deploy", "handler"},
		"Server.Search": {"server.search", "server", "search"},
		"HTTPServer":    {"httpserver", "http", "server"},
		"fetch_user":    {"fetch_user", "fetch", "user"},
	}
	for in, want := range cases {
		got := splitIdent(in)
		for _, w := range want {
			if !strings.Contains(" "+got+" ", " "+w+" ") {
				t.Errorf("splitIdent(%q) = %q, missing %q", in, got, w)
			}
		}
	}
}

// TestReindexCodeIncremental drives the mtime-drift path end to end: a second
// run with no changes parses nothing, and a change/add/delete run rewrites only
// the drifted files while the call graph stays globally consistent.
func TestReindexCodeIncremental(t *testing.T) {
	s := tmpStore(t)
	root := t.TempDir()
	write := func(rel, body string, at time.Time) {
		t.Helper()
		p := filepath.Join(root, rel)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, at, at); err != nil {
			t.Fatal(err)
		}
	}
	symCount := func(name string) int {
		t.Helper()
		var n int
		if err := s.readDB.QueryRow(`SELECT count(*) FROM code_symbols WHERE name = ?`, name).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	t0 := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	write("a.go", "package demo\n\nfunc Alpha() { beta() }\n", t0)
	write("b.go", "package demo\n\nfunc beta() {}\n", t0)
	langs := map[string]bool{"go": true}

	st, err := ReindexCode(s, []string{root}, langs) // empty index falls back to full
	if err != nil {
		t.Fatalf("initial ReindexCode: %v", err)
	}
	if st.Files != 2 || st.Unchanged != 0 {
		t.Fatalf("initial run: files=%d unchanged=%d, want 2/0", st.Files, st.Unchanged)
	}

	st, err = ReindexCode(s, []string{root}, langs) // no drift: nothing parsed
	if err != nil {
		t.Fatalf("no-op ReindexCode: %v", err)
	}
	if st.Files != 0 || st.Unchanged != 2 || st.Removed != 0 {
		t.Fatalf("no-op run: files=%d unchanged=%d removed=%d, want 0/2/0", st.Files, st.Unchanged, st.Removed)
	}

	t1 := t0.Add(time.Hour)
	write("a.go", "package demo\n\nfunc Alpha() { Gamma() }\n\nfunc AlphaTwo() {}\n", t1)
	write("c.go", "package demo\n\nfunc Gamma() {}\n", t1)
	if err := os.Remove(filepath.Join(root, "b.go")); err != nil {
		t.Fatal(err)
	}

	st, err = ReindexCode(s, []string{root}, langs)
	if err != nil {
		t.Fatalf("incremental ReindexCode: %v", err)
	}
	if st.Files != 2 || st.Removed != 1 || st.Unchanged != 0 {
		t.Fatalf("incremental run: files=%d removed=%d unchanged=%d, want 2/1/0", st.Files, st.Removed, st.Unchanged)
	}
	if n := symCount("beta"); n != 0 {
		t.Errorf("beta should be gone with b.go, still has %d rows", n)
	}
	if n := symCount("AlphaTwo"); n != 1 {
		t.Errorf("AlphaTwo rows = %d, want 1", n)
	}
	if n := symCount("Gamma"); n != 1 {
		t.Errorf("Gamma rows = %d, want 1", n)
	}
	base := filepath.Base(root)
	_, callees, err := s.CodeNeighbors("code:" + base + "/a.go#Alpha")
	if err != nil {
		t.Fatalf("CodeNeighbors: %v", err)
	}
	found := false
	for _, c := range callees {
		if c.Name == "Gamma" {
			found = true
		}
	}
	if !found {
		t.Errorf("edge Alpha -> Gamma missing after incremental rebuild (callees: %+v)", callees)
	}
}

// BenchmarkReindexCode indexes the whole mesh module (real Go corpus) into a temp
// store, so the headline number reflects parse + write on actual code. Uses the
// full path: the incremental default would no-op every iteration after the first.
func BenchmarkReindexCode(b *testing.B) {
	wd, _ := os.Getwd()                   // .../mesh/internal/index
	root := filepath.Join(wd, "..", "..") // the mesh module root
	s := tmpStore(b)
	langs := map[string]bool{"go": true}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		st, err := ReindexCodeFull(s, []string{root}, langs)
		if err != nil {
			b.Fatalf("ReindexCode: %v", err)
		}
		if i == 0 {
			b.ReportMetric(float64(st.Files), "files")
			b.ReportMetric(float64(st.Symbols), "symbols")
			b.ReportMetric(float64(st.Edges), "edges")
		}
	}
}
