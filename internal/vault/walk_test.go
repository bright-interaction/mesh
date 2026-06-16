package vault

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWalkSkipsConflictSiblings(t *testing.T) {
	dir := t.TempDir()
	wr := func(name string) {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("---\nid: x\n---\n# x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	wr("note.md")
	wr("note.sync-conflict-20260616-bob-1a2b3c4d.md")
	wr("decisions/d.md")
	wr("decisions/d.sync-conflict-20260616-alice-deadbeef.md")

	files, err := Walk(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if IsConflictSibling(filepath.Base(f)) {
			t.Errorf("Walk returned a conflict sibling: %s", f)
		}
	}
	if len(files) != 2 {
		t.Errorf("expected 2 real notes, got %d: %v", len(files), files)
	}
}

func TestIsConflictSibling(t *testing.T) {
	yes := []string{"x.sync-conflict-20260616-bob-1a2b3c4d.md", "a/b.sync-conflict-20260101-u-00000000.md"}
	no := []string{"x.md", "notes/y.md", "sync-conflict.md", "x.conflict.md"}
	for _, n := range yes {
		if !IsConflictSibling(n) {
			t.Errorf("%q should be a conflict sibling", n)
		}
	}
	for _, n := range no {
		if IsConflictSibling(n) {
			t.Errorf("%q should NOT be a conflict sibling", n)
		}
	}
}
