// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeVault lays down a small linked, tagged vault and returns its root.
func writeVault(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"a.md": "---\nid: a\ntype: decision\nwhen: 2026-01-01\ndo: x\ndont: y\nwhy: use modernc\n---\n# A\nlinks [[b]] and [[d]] #core\n",
		"b.md": "---\nid: b\ntype: gotcha\nwhen: 2026-01-01\ndo: x\ndont: y\nwhy: watch out\n---\n# B\nbody about [[c]] #core\n",
		"c.md": "---\nid: c\ntype: note\nwhen: 2026-01-01\n---\n# C\nstandalone note #misc\n",
	}
	for rel, body := range files {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// snapshotTables dumps the deterministic content of every index table (ordered by
// a stable key) so two index states can be compared regardless of insertion order
// / rowid. This is the oracle: an incremental reconcile must produce exactly what a
// full reindex of the same final disk produces.
func snapshotTables(t *testing.T, s *Store) string {
	t.Helper()
	var b strings.Builder
	dump := func(label, query string) {
		b.WriteString("== " + label + " ==\n")
		rows, err := s.readDB.Query(query)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		defer rows.Close()
		cols, _ := rows.Columns()
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		for rows.Next() {
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatalf("%s scan: %v", label, err)
			}
			for i, v := range vals {
				if i > 0 {
					b.WriteByte('|')
				}
				switch x := v.(type) {
				case []byte:
					fmt.Fprintf(&b, "%x", x)
				default:
					fmt.Fprintf(&b, "%v", x)
				}
			}
			b.WriteByte('\n')
		}
	}
	dump("notes", `SELECT id,path,type,title,retrieval_hash,frontmatter,updated FROM notes ORDER BY id`)
	dump("nodes", `SELECT id,kind,label,note_id,note_path,anchor,source_loc,community,attrs FROM nodes ORDER BY id`)
	dump("edges", `SELECT source,target,relation,confidence,confidence_score,weight,source_loc FROM edges ORDER BY source,target,relation`)
	dump("fts", `SELECT node_id,title,body FROM search_index ORDER BY node_id`)
	dump("vectors", `SELECT node_id,chunk_ix,model,dim,content_hash,note_hash FROM vectors ORDER BY node_id,chunk_ix`)
	return b.String()
}

// TestIncrementalMatchesFullReindex is the oracle: for every hash-affecting
// mutation, an incremental reconcile leaves the index byte-identical to a full
// reindex of the same final disk state.
func TestIncrementalMatchesFullReindex(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(t *testing.T, dir string)
	}{
		{"edit body", func(t *testing.T, dir string) {
			write(t, dir, "a.md", "---\nid: a\ntype: decision\nwhen: 2026-01-01\ndo: x\ndont: y\nwhy: use modernc\n---\n# A\nnow links only [[c]] #core\n")
		}},
		{"add note resolves a broken link", func(t *testing.T, dir string) {
			// a.md links [[d]] which was broken; adding d.md must create the inbound edge.
			write(t, dir, "d.md", "---\nid: d\ntype: note\nwhen: 2026-01-01\n---\n# D\nthe missing target #core\n")
		}},
		{"remove note", func(t *testing.T, dir string) {
			rm(t, dir, "b.md")
		}},
		{"id change", func(t *testing.T, dir string) {
			write(t, dir, "c.md", "---\nid: c2\ntype: note\nwhen: 2026-01-01\n---\n# C\nstandalone note #misc\n")
		}},
		{"rename keeps id", func(t *testing.T, dir string) {
			body, _ := os.ReadFile(filepath.Join(dir, "b.md"))
			rm(t, dir, "b.md")
			if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "sub", "b.md"), body, 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"remove note prunes its vector", func(t *testing.T, dir string) {
			rm(t, dir, "c.md")
		}},
		{"file corrupted to invalid yaml in place", func(t *testing.T, dir string) {
			// b.md becomes unparseable: a full reindex drops it, so incremental must too
			// (it must not keep serving the stale cached note).
			write(t, dir, "b.md", "---\nid: [unterminated\ntype: gotcha\n---\n# B\nbroken\n")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeVault(t)
			s, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			live := NewLiveIndexer(s, dir)
			if _, err := live.Reconcile(true); err != nil { // seed (full)
				t.Fatalf("seed: %v", err)
			}
			// Seed a vector per note so the orphan-prune / stale paths are exercised.
			seedVectors(t, s)

			tc.mutate(t, dir)

			if _, err := live.Reconcile(true); err != nil { // incremental
				t.Fatalf("incremental: %v", err)
			}
			got := snapshotTables(t, s)

			// Authoritative baseline: a full reindex of the SAME final disk on the SAME
			// store. IndexVault keeps existing vectors (prunes only orphans), so the
			// vector rows are comparable.
			if _, err := Reindex(s, dir); err != nil {
				t.Fatalf("full reindex: %v", err)
			}
			want := snapshotTables(t, s)

			if got != want {
				t.Errorf("incremental != full reindex for %q\n--- incremental ---\n%s\n--- full ---\n%s", tc.name, got, want)
			}
		})
	}
}

// seedVectors stores one stub vector per current note with a matching note_hash, so
// the oracle exercises the orphan-prune (delete) and stale-keep (edit) vector paths.
func seedVectors(t *testing.T, s *Store) {
	t.Helper()
	nfs, err := s.NoteFiles()
	if err != nil {
		t.Fatal(err)
	}
	var rows []VectorRow
	for _, nf := range nfs {
		h, _ := s.NoteRetrievalHash(nf.NodeID)
		rows = append(rows, VectorRow{NodeID: nf.NodeID, ChunkIx: 0, Vec: []float32{1, 0}, ContentHash: "c", NoteHash: h})
	}
	if err := s.ReplaceVectors("stub", rows); err != nil {
		t.Fatal(err)
	}
}

// TestIncrementalCosmeticEditIsNoOp: an edit that does not change retrieval_hash
// (e.g. rewriting identical content) is a no-op, exactly as the full Reconcile is.
func TestIncrementalCosmeticEditIsNoOp(t *testing.T) {
	dir := writeVault(t)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	live := NewLiveIndexer(s, dir)
	if _, err := live.Reconcile(true); err != nil {
		t.Fatal(err)
	}
	// Rewrite a.md with byte-identical content (a touch-like save).
	body, _ := os.ReadFile(filepath.Join(dir, "a.md"))
	if err := os.WriteFile(filepath.Join(dir, "a.md"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	rec, err := live.Reconcile(true)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Reindexed {
		t.Errorf("a content-identical rewrite must not trigger a reindex, got Reindexed=true")
	}
}

// TestIncrementalRefusesDuplicateID: two live files resolving to the same id must
// be refused with a clear error (matching the full path's atomic refusal), leaving
// the existing index intact and NOT clobbering the live note or flip-flopping.
func TestIncrementalRefusesDuplicateID(t *testing.T) {
	dir := writeVault(t)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	live := NewLiveIndexer(s, dir)
	if _, err := live.Reconcile(true); err != nil {
		t.Fatal(err)
	}
	before, _ := s.Count("notes")

	// dup.md declares id "b", already owned by b.md.
	write(t, dir, "dup.md", "---\nid: b\ntype: note\nwhen: 2026-01-01\n---\n# Dup\nclashing id\n")

	if _, err := live.Reconcile(true); err == nil {
		t.Fatal("a duplicate note id must be refused with an error, not silently applied")
	}
	// The existing index is untouched: original note count, b.md still owns id b.
	after, _ := s.Count("notes")
	if after != before {
		t.Errorf("a refused reconcile must not change the index: notes %d -> %d", before, after)
	}
	var path string
	if err := s.readDB.QueryRow(`SELECT path FROM notes WHERE id='b'`).Scan(&path); err != nil {
		t.Fatal(err)
	}
	if path != "b.md" {
		t.Errorf("the live note must not be clobbered: id b is now at %q, want b.md", path)
	}
	// It stays a stable error (converges, does not flip-flop) until the dup is fixed.
	if _, err := live.Reconcile(true); err == nil {
		t.Fatal("duplicate id must keep erroring until fixed (no flip-flop to success)")
	}
}

func write(t *testing.T, dir, rel, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func rm(t *testing.T, dir, rel string) {
	t.Helper()
	if err := os.Remove(filepath.Join(dir, rel)); err != nil {
		t.Fatal(err)
	}
}
