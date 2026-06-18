package index

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestMtimeStoredCorrectlyOffRoot is the regression for the fileMtime bug: the
// stored mtime must be the file's real mtime even when the process CWD is not the
// vault root (the normal MCP case). The test's CWD is the package dir, and the
// vault is a temp dir elsewhere, so this exercises exactly that off-root path.
// Before the fix (fileMtime stat'd the vault-relative path) this stored 0.
func TestMtimeStoredCorrectlyOffRoot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "n1.md")
	if err := os.WriteFile(path, []byte("---\nid: n1\ntype: note\nwhen: 2026-01-01\n---\n# N1\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	if err := os.Chtimes(path, want, want); err != nil {
		t.Fatal(err)
	}

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, _, err := ReindexFull(s, dir); err != nil {
		t.Fatal(err)
	}

	var got int64
	if err := s.readDB.QueryRow(`SELECT mtime FROM notes WHERE id='n1'`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want.Unix() {
		t.Fatalf("stored mtime = %d, want %d (the real file mtime). 0 means the off-root fileMtime bug regressed", got, want.Unix())
	}

	// mesh_changed_since must see it: a since just before the mtime returns the note,
	// a since just after returns nothing.
	since, err := s.ChangedSince(want.Unix() - 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(since) != 1 || since[0].ID != "n1" {
		t.Fatalf("ChangedSince(mtime-1) should return n1, got %+v", since)
	}
	after, err := s.ChangedSince(want.Unix() + 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 0 {
		t.Fatalf("ChangedSince(mtime+1) should be empty, got %+v", after)
	}
}
