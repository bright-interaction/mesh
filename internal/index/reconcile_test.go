package index

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReconcile(t *testing.T) {
	dir := t.TempDir()
	wr := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	wr("a.md", "---\nid: a\ntype: note\nwhen: 2026-01-01\n---\n# A\noriginal\n")

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// First reconcile against an empty index: a.md is new, so it rebuilds.
	rec, err := Reconcile(s, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Reindexed || rec.Added != 1 {
		t.Fatalf("first reconcile: want reindexed with 1 added, got %+v", rec)
	}
	if rec.Graph == nil {
		t.Fatal("reindex should return a graph")
	}

	// Second reconcile with no edits: convergent no-op.
	rec, err = Reconcile(s, dir)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Reindexed || rec.Any() || rec.Graph != nil {
		t.Fatalf("second reconcile should be a no-op, got %+v", rec)
	}

	// A retrieval-relevant content change triggers a rebuild.
	wr("a.md", "---\nid: a\ntype: note\nwhen: 2026-01-01\n---\n# A\nCHANGED now\n")
	rec, err = Reconcile(s, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Reindexed || rec.Changed != 1 {
		t.Fatalf("content change: want reindexed with 1 changed, got %+v", rec)
	}

	// A new note then a removal each reindex.
	wr("b.md", "---\nid: b\ntype: note\nwhen: 2026-01-01\n---\n# B\nbody\n")
	rec, _ = Reconcile(s, dir)
	if !rec.Reindexed || rec.Added != 1 {
		t.Fatalf("add: want reindexed with 1 added, got %+v", rec)
	}
	if err := os.Remove(filepath.Join(dir, "b.md")); err != nil {
		t.Fatal(err)
	}
	rec, _ = Reconcile(s, dir)
	if !rec.Reindexed || rec.Removed != 1 {
		t.Fatalf("remove: want reindexed with 1 removed, got %+v", rec)
	}
}
