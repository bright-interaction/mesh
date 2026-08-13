// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"os"
	"path/filepath"
	"testing"
)

// A note id is vault-GLOBAL, but a migration looks at one file at a time. Synthesizing the
// id from that file's basename alone therefore says nothing about the rest of the vault,
// and the commonest legacy vault in existence is one with two README.md (or two index.md,
// or a note copied as a template) in different folders.
//
// The indexer already refused to hold both: it gives the id to the first file in walk
// order and quarantines the other, which is what makes `mesh doctor` print STALE. The
// remedy doctor names is `mesh migrate --apply`, and migrate used to stamp `id: readme`
// into BOTH files, turning a recoverable accident into a collision cemented on disk that
// no reindex could clear.

// migrateVault runs a full pass over root the way `mesh migrate --apply` does: one
// Migration, every file, in walk order.
func migrateVault(t *testing.T, root string, dryRun bool) []*MigrateResult {
	t.Helper()
	files, err := Walk(root)
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewMigration(root)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]*MigrateResult, 0, len(files))
	for _, f := range files {
		res, err := m.File(f, dryRun)
		if err != nil {
			t.Fatalf("migrate %s: %v", f, err)
		}
		out = append(out, res)
	}
	return out
}

// twoREADMEVault is the vault the report names: two files whose only id is their
// basename, in two folders, with nothing in frontmatter to tell them apart.
func twoREADMEVault(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, sub := range []string{"a", "b"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
		body := "# Readme " + sub + "\n" + sub + " content\n"
		if err := os.WriteFile(filepath.Join(dir, sub, "README.md"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestMigrateGivesTwoREADMEsTwoDistinctIDs is the defect proper. Both files must come out
// of `mesh migrate --apply` with an id, and the two ids must differ.
func TestMigrateGivesTwoREADMEsTwoDistinctIDs(t *testing.T) {
	dir := twoREADMEVault(t)
	migrateVault(t, dir, false)

	aID := NoteIDForFile(filepath.Join(dir, "a", "README.md"))
	bID := NoteIDForFile(filepath.Join(dir, "b", "README.md"))
	if aID == "" || bID == "" {
		t.Fatalf("migrate left a file with no id: a=%q b=%q", aID, bID)
	}
	if aID == bID {
		t.Fatalf("migrate wrote id %q into BOTH a/README.md and b/README.md: the remedy `mesh doctor` "+
			"points at has cemented the duplicate id on disk, and only one of the two notes is retrievable "+
			"from now on", aID)
	}
	// The incumbent keeps the id. The indexer gives a contested id to the first file in
	// walk order, so a migration that moved THAT one would re-point every citation and
	// graph edge already resolved against it, and hand the id to a note nothing links to.
	if aID != "readme" {
		t.Errorf("the first file in walk order must keep the plain slug, got %q", aID)
	}
	if bID != "readme-2" {
		t.Errorf("the shadowed file must take the next suffix, got %q", bID)
	}
	claimed, err := ClaimedIDs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 2 {
		t.Errorf("the migrated vault must hold 2 distinct ids, got %d: %v", len(claimed), claimed)
	}
}

// TestMigrateIsIdempotentAcrossTheWholeVault: a second pass over an already-migrated vault
// must change nothing. A per-file claim that forgot who already holds an id would see
// readme taken by a/README.md and start renaming on every run, so the vault never settles.
func TestMigrateIsIdempotentAcrossTheWholeVault(t *testing.T) {
	dir := twoREADMEVault(t)
	migrateVault(t, dir, false)

	before := map[string]string{}
	for _, sub := range []string{"a", "b"} {
		p := filepath.Join(dir, sub, "README.md")
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		before[sub] = string(data)
	}

	for _, res := range migrateVault(t, dir, false) {
		if res.Changed {
			t.Errorf("a second migration pass rewrote %s: %v", res.Path, res.Actions)
		}
	}
	for _, sub := range []string{"a", "b"} {
		data, err := os.ReadFile(filepath.Join(dir, sub, "README.md"))
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != before[sub] {
			t.Errorf("%s/README.md changed on a second pass:\n--- before ---\n%s\n--- after ---\n%s", sub, before[sub], string(data))
		}
	}
}

// TestMigrateDryRunPreviewsTheIDsItWouldWrite: a dry run that names different ids than the
// --apply it previews is worse than no preview, and this is the output the operator reads
// before deciding to write.
func TestMigrateDryRunPreviewsTheIDsItWouldWrite(t *testing.T) {
	dir := twoREADMEVault(t)
	var previewed []string
	for _, res := range migrateVault(t, dir, true) {
		for _, a := range res.Actions {
			if len(a) > len("added id ") && a[:len("added id ")] == "added id " {
				previewed = append(previewed, a[len("added id "):])
			}
		}
	}
	if len(previewed) != 2 {
		t.Fatalf("the dry run must preview an id for each file, got %v", previewed)
	}
	if previewed[0] == previewed[1] {
		t.Fatalf("the dry run previewed id %q for both files", previewed[0])
	}
	// Nothing on disk moved, and the apply run agrees with what was previewed.
	if got := NoteIDForFile(filepath.Join(dir, "b", "README.md")); got != "readme" {
		t.Errorf("the dry run wrote to disk: b/README.md now reports id %q", got)
	}
	migrateVault(t, dir, false)
	for i, sub := range []string{"a", "b"} {
		if got := NoteIDForFile(filepath.Join(dir, sub, "README.md")); got != previewed[i] {
			t.Errorf("%s/README.md: dry run said %q, apply wrote %q", sub, previewed[i], got)
		}
	}
}

// TestMigrateYieldsToAnIDAlreadyDeclaredInFrontmatter: the collision does not need a
// second unkeyed file. A note that already declares `id: readme` owns it, wherever it
// sits, and a migration must step around it rather than mint a second claimant.
func TestMigrateYieldsToAnIDAlreadyDeclaredInFrontmatter(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "keyed"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Deliberately named so its BASENAME does not collide: only its frontmatter id does,
	// which is precisely what a per-file basename slug cannot see.
	keyed := filepath.Join(dir, "keyed", "overview.md")
	if err := os.WriteFile(keyed, []byte("---\nid: readme\ntype: note\nwhen: \"2026-01-01\"\n---\n# Overview\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Readme\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	migrateVault(t, dir, false)

	if got := NoteIDForFile(keyed); got != "readme" {
		t.Errorf("the note that already declared id readme must keep it, got %q", got)
	}
	if got := NoteIDForFile(filepath.Join(dir, "README.md")); got == "readme" {
		t.Fatalf("migrate minted id readme for README.md while keyed/overview.md already declares it; " +
			"the indexer will quarantine one of the two")
	}
}

// declaredID returns the id a file DECLARES in its frontmatter, which is not the same
// question NoteIDForFile answers: that one falls back to the basename, so it reports a
// perfectly good id for a note whose frontmatter literally says `id:` with nothing after
// it. To see what a migration actually wrote, the block has to be read.
func declaredID(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fmText, _, had := SplitFrontmatter(string(data))
	if !had {
		t.Fatalf("%s has no frontmatter after a migration", path)
	}
	fm, _, err := ParseFrontmatter([]byte(fmText))
	if err != nil {
		t.Fatalf("%s: frontmatter does not parse after a migration: %v", path, err)
	}
	return fm.ID
}

// TestMigrateNamesEveryFileWhenTheSlugIsEmpty: a filename that slugs to nothing ("___.md",
// "...md") left migrate with no base at all, and it wrote a bare `id:` into the note. That
// is a note whose frontmatter asserts it has no identity, written by the tool whose job is
// to give it one, and two of them in a vault assert it identically.
func TestMigrateNamesEveryFileWhenTheSlugIsEmpty(t *testing.T) {
	dir := t.TempDir()
	names := []string{"___.md", "...md"}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("# X\nbody\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	migrateVault(t, dir, false)

	seen := map[string]string{}
	for _, name := range names {
		id := declaredID(t, filepath.Join(dir, name))
		if id == "" {
			t.Errorf("%s was migrated with an empty id: the note declares that it has no identity", name)
			continue
		}
		if prev, dup := seen[id]; dup {
			t.Errorf("%s and %s were both given id %q", prev, name, id)
		}
		seen[id] = name
	}
}
