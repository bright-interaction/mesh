// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/bright-interaction/mesh/internal/vault"
)

// Two files with the same effective id is an ordinary accident: two README.md or index.md
// in different folders with no frontmatter id, or a note copied as a template that kept
// its id: line. Only one of them can be indexed, so the question is not whether one is
// dropped but whether Mesh drops the SAME one every time and says so.
//
// The full path used to answer differently from the incremental one AND differently from
// itself: INSERT OR REPLACE gave notes.path to the last file walked while BuildGraph gave
// nodes.note_path to the first, so a search card showed one file's path with the other
// file's snippet, nothing was recorded as dropped, and the loser (having no notes row)
// came back as Added from every DriftReport, which made `mesh doctor` print STALE forever
// over an index a reindex could not change.

func dupIDVault(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, sub := range []string{"a", "b"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// dupNoteRow returns the vault-relative path the notes table holds for an id, and the path
// the graph node for the same id points at. They are two different columns written by two
// different writers, and a note that indexes correctly has them equal.
func dupNoteRow(t *testing.T, s *Store, id string) (notePath, nodePath string) {
	t.Helper()
	if err := s.readDB.QueryRow(`SELECT path FROM notes WHERE id=?`, id).Scan(&notePath); err != nil {
		t.Fatalf("no notes row for id %q: %v", id, err)
	}
	if err := s.readDB.QueryRow(`SELECT note_path FROM nodes WHERE id=?`, "note:"+id).Scan(&nodePath); err != nil {
		t.Fatalf("no graph node for id %q: %v", id, err)
	}
	return notePath, nodePath
}

// mustDropped, which asserts the record could be read at all, lives in
// schema_version_readonly_test.go with the rest of that guard.

func droppedPaths(fes []FileError) []string {
	out := make([]string, 0, len(fes))
	for _, fe := range fes {
		out = append(out, fe.Path)
	}
	return out
}

// TestFullReindexQuarantinesADuplicateInsteadOfLosingIt: the cold case, both files
// present before anything is indexed. The full pass must keep exactly one, point the
// notes row and the graph node at the SAME file, and report the other as dropped.
func TestFullReindexQuarantinesADuplicateInsteadOfLosingIt(t *testing.T) {
	dir := dupIDVault(t)
	write(t, dir, "a/README.md", "# Readme A\nalpha content\n")
	write(t, dir, "b/README.md", "# Readme B\nbravo content\n")

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, _, err := ReindexFull(s, dir); err != nil {
		t.Fatal(err)
	}

	notePath, nodePath := dupNoteRow(t, s, "readme")
	if notePath != nodePath {
		t.Errorf("notes.path and nodes.note_path name DIFFERENT files for id readme (%q vs %q): "+
			"a search card would carry one file's path next to the other file's snippet", notePath, nodePath)
	}
	if notePath != filepath.Join("a", "README.md") {
		t.Errorf("the first file in walk order must win the id, got %q", notePath)
	}
	dropped := mustDropped(t, s)
	if len(dropped) != 1 || dropped[0].Path != filepath.Join("b", "README.md") {
		t.Fatalf("the losing file must be reported as dropped, got %v", droppedPaths(dropped))
	}
	if !errors.Is(dropped[0].Err, ErrDuplicateNoteID) {
		t.Errorf("the drop must classify as a duplicate id (the remedy differs from unparseable YAML), got %v", dropped[0].Err)
	}
}

// TestADuplicateDoesNotManufactureEndlessDrift: the file left out of the index has no
// notes row BY DESIGN, and a drift check that does not know that reports it as Added on
// every single run. That is the "never converges" face of this defect: `mesh doctor`
// prints STALE and exits 1 forever while `mesh index` produces a byte-identical index
// every time, and the remedy doctor points at (mesh migrate --apply) stamps the SAME id
// into both files and cements the collision on disk.
func TestADuplicateDoesNotManufactureEndlessDrift(t *testing.T) {
	dir := dupIDVault(t)
	write(t, dir, "a/README.md", "# Readme A\nalpha content\n")
	write(t, dir, "b/README.md", "# Readme B\nbravo content\n")

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// Twice: a second pass over an unchanged vault is the shape the operator runs after
	// being told to reindex, and it must not change the answer either.
	for pass := 1; pass <= 2; pass++ {
		if _, _, err := ReindexFull(s, dir); err != nil {
			t.Fatal(err)
		}
		d, derr := s.DriftReport(dir)
		if derr != nil {
			t.Fatal(derr)
		}
		if d.Any() {
			t.Fatalf("pass %d: a freshly indexed vault still reports drift (+%v ~%v -%v), so mesh doctor says STALE "+
				"forever and no reindex can clear it", pass, d.Added, d.Changed, d.Removed)
		}
	}
}

// TestFullAndIncrementalAgreeOnTheDuplicateWinner is the defect proper: which of the two
// notes you can actually retrieve must not depend on whether `mesh index` or the watcher
// ran last. The incumbent (the file already indexed under the id) keeps it on BOTH paths.
func TestFullAndIncrementalAgreeOnTheDuplicateWinner(t *testing.T) {
	// Both vaults start identically: only a/README.md exists and is indexed, so it is the
	// incumbent owner of id "readme".
	setup := func(t *testing.T) (string, *Store) {
		t.Helper()
		dir := dupIDVault(t)
		write(t, dir, "a/README.md", "# Readme A\nalpha content\n")
		s, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		return dir, s
	}

	// The full path: index, then b/README.md appears, then reindex from scratch.
	fullDir, fullStore := setup(t)
	if _, _, err := ReindexFull(fullStore, fullDir); err != nil {
		t.Fatal(err)
	}
	write(t, fullDir, "b/README.md", "# Readme B\nbravo content\n")
	if _, _, err := ReindexFull(fullStore, fullDir); err != nil {
		t.Fatal(err)
	}
	fullPath, fullNodePath := dupNoteRow(t, fullStore, "readme")
	fullDropped := droppedPaths(mustDropped(t, fullStore))

	// The incremental path: the LiveIndexer's cache is seeded FIRST (its first Reconcile
	// is a full one), so the reconcile that sees b/README.md is genuinely the incremental
	// one, which is the path a running `mesh mcp` or the hub actually takes.
	incDir, incStore := setup(t)
	live := NewLiveIndexer(incStore, incDir)
	if _, err := live.Reconcile(true); err != nil {
		t.Fatal(err)
	}
	write(t, incDir, "b/README.md", "# Readme B\nbravo content\n")
	rec, err := live.Reconcile(true)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Reindexed && rec.Added > 0 {
		t.Errorf("the quarantined file must not be indexed as Added, got Added=%d", rec.Added)
	}
	incPath, incNodePath := dupNoteRow(t, incStore, "readme")
	incDropped := droppedPaths(mustDropped(t, incStore))

	if fullPath != incPath {
		t.Errorf("the full path and the incremental path gave id readme to DIFFERENT files "+
			"(%q vs %q): which note is retrievable flips with whichever ran last", fullPath, incPath)
	}
	if want := filepath.Join("a", "README.md"); fullPath != want {
		t.Errorf("the incumbent must keep the id it is already indexed under: got %q, want %q", fullPath, want)
	}
	if fullNodePath != fullPath || incNodePath != incPath {
		t.Errorf("the graph node must point at the same file as the notes row: full %q/%q, incremental %q/%q",
			fullPath, fullNodePath, incPath, incNodePath)
	}
	if len(fullDropped) != 1 || len(incDropped) != 1 || fullDropped[0] != incDropped[0] {
		t.Errorf("both paths must report the same dropped file, got full=%v incremental=%v", fullDropped, incDropped)
	}
	if want := filepath.Join("b", "README.md"); len(fullDropped) == 1 && fullDropped[0] != want {
		t.Errorf("the challenger is what gets dropped, got %q want %q", fullDropped[0], want)
	}
}

// TestNoteIDsAgreeBetweenVaultAndIndex: vault.NoteIDForFile is what CreateNote claims an
// id against, index.effectiveID is what the indexer keys notes.id and the graph node on.
// If they ever disagree, CreateNote goes back to writing notes the indexer quarantines.
// The subjects are ENUMERATED from a vault walk rather than listed here by hand, so a new
// id shape has to be handled by both or this fails.
func TestNoteIDsAgreeBetweenVaultAndIndex(t *testing.T) {
	dir := dupIDVault(t)
	write(t, dir, "plain.md", "---\nid: explicit-id\ntype: note\nwhen: 2026-01-01\n---\n# Plain\nbody\n")
	write(t, dir, "a/README.md", "# No frontmatter at all\n")
	write(t, dir, "b/MiXeDcAsE.md", "---\ntype: note\nwhen: 2026-01-01\n---\n# No id key\nbody\n")
	write(t, dir, "a/quoted.md", "---\nid: \"quoted-id\"\ntype: note\nwhen: 2026-01-01\n---\n# Quoted\nbody\n")
	// Frontmatter that fails a full unmarshal (an unquoted colon) but still declares an id.
	write(t, dir, "b/messy.md", "---\nid: messy\ntype: note\nupdated: 2026-06-18 (post-mortem: root cause)\n---\n# Messy\n")

	files, err := vault.Walk(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("the walk found no files, so this guard would pass vacuously")
	}
	for _, f := range files {
		got := vault.NoteIDForFile(f)
		pn, perr := ParseFile(f)
		if perr != nil {
			// Unparseable: the indexer has no id for it at all, and the vault side is
			// allowed to fall back to the basename (conservative: it can only make an id
			// look taken, never free).
			continue
		}
		if want := effectiveID(pn); got != want {
			t.Errorf("%s: vault.NoteIDForFile=%q but index.effectiveID=%q; CreateNote would claim one id "+
				"and the indexer would file the note under another", f, got, want)
		}
	}
}
