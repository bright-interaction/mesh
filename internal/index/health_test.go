package index

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeNote writes a note file under root and returns its ParsedNote (Path matches
// what ComputeHealth reads from disk).
func writeNote(t *testing.T, root, rel, content string) *ParsedNote {
	t.Helper()
	full := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	pn, err := Parse(rel, []byte(content))
	if err != nil {
		t.Fatal(err)
	}
	return pn
}

func TestHealthDeadRefAndOverdue(t *testing.T) {
	dir := t.TempDir()
	// A tiny source root so the code index has a known file.
	srcRoot := filepath.Join(dir, "src")
	if err := os.MkdirAll(filepath.Join(srcRoot, "internal", "foo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcRoot, "internal", "foo", "bar.go"), []byte("package foo\nfunc Bar(){}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	good := writeNote(t, dir, "notes/good.md", "---\nid: good\ntype: note\nwhen: 2026-01-01\n---\n# Good\nSee internal/foo/bar.go for the impl.\n")
	dead := writeNote(t, dir, "notes/dead.md", "---\nid: dead\ntype: note\nwhen: 2026-01-01\n---\n# Dead\nThe logic moved to internal/foo/ghost.go which is gone.\n")
	overdue := writeNote(t, dir, "notes/od.md", "---\nid: od\ntype: note\nwhen: 2026-01-01\nreview_by: 2000-01-01\n---\n# Old\nstale content.\n")
	notes := []*ParsedNote{good, dead, overdue}
	g, _ := BuildGraph(notes)

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.IndexVault(notes, g); err != nil {
		t.Fatal(err)
	}
	if _, err := ReindexCode(s, []string{srcRoot}, map[string]bool{"go": true}); err != nil {
		t.Fatal(err)
	}

	findings, err := s.ComputeHealth(dir, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{} // note_id -> issue
	for _, f := range findings {
		got[f.NoteID] = f.Issue
	}
	if got["dead"] != "dead_ref" {
		t.Errorf("dead note should flag dead_ref, got %v", got["dead"])
	}
	if got["od"] != "overdue" {
		t.Errorf("overdue note should flag overdue, got %v", got["od"])
	}
	if _, flagged := got["good"]; flagged {
		t.Errorf("good note (references an existing file) should not be flagged")
	}
}

func TestHealthContradiction(t *testing.T) {
	dir := t.TempDir()
	// Two gotchas sharing a tag where A.do overlaps B.dont.
	a := writeNote(t, dir, "gotchas/a.md", "---\nid: a\ntype: gotcha\nwhen: 2026-01-01\ntags: [deploy]\ndo: always force push to deploy quickly\ndont: wait for review\nwhy: speed\n---\n# A\n")
	b := writeNote(t, dir, "gotchas/b.md", "---\nid: b\ntype: gotcha\nwhen: 2026-01-01\ntags: [deploy]\ndo: open a pull request\ndont: force push to deploy quickly ever\nwhy: safety\n---\n# B\n")
	// An unrelated gotcha (different tag, no overlap) must not be flagged.
	c := writeNote(t, dir, "gotchas/c.md", "---\nid: c\ntype: gotcha\nwhen: 2026-01-01\ntags: [logging]\ndo: use structured logs\ndont: print to stdout\nwhy: clarity\n---\n# C\n")
	notes := []*ParsedNote{a, b, c}
	g, _ := BuildGraph(notes)

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.IndexVault(notes, g); err != nil {
		t.Fatal(err)
	}

	findings, err := s.ComputeContradictions(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) == 0 {
		t.Fatal("expected a contradiction between a and b")
	}
	for _, f := range findings {
		if f.NoteID == "c" {
			t.Errorf("note c should not be flagged as a contradiction")
		}
		if f.Issue != "contradiction" {
			t.Errorf("issue = %q, want contradiction", f.Issue)
		}
	}
	// Counts surface it.
	counts, _ := s.HealthCounts()
	if counts["contradiction"] == 0 {
		t.Error("HealthCounts missing contradiction")
	}
}
