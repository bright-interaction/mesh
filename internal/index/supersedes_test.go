// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/vault"
)

// note builds a minimal ParsedNote for the supersedes pass.
func supNote(key, id string, supersedes ...string) *ParsedNote {
	return &ParsedNote{
		Path: "gotchas/" + key + ".md",
		Key:  key,
		FM: &vault.Frontmatter{
			ID:         id,
			Type:       "gotcha",
			Title:      key,
			When:       "2026-01-01",
			Supersedes: vault.StringList(supersedes),
		},
		Body: "body of " + key,
	}
}

// TestSupersedesMarksTheRetiredNote guards the note-level UPDATE path. `supersedes:` was
// in the frontmatter schema and in the retrieval hash - which documents it as
// "retrieval-critical" - while nothing consumed it, so a corrected diagnosis became a
// NEW note and the note it corrected kept its rank and kept being retrieved first. The
// live vault shows the workaround: authors encode the relation into filenames
// ("correction-...", "root-cause-found-...", "supersedes-the-..."), which reads to a
// human and is invisible to ranking.
func TestSupersedesMarksTheRetiredNote(t *testing.T) {
	old := supNote("old-diagnosis", "old-diagnosis")
	new := supNote("new-diagnosis", "new-diagnosis", "old-diagnosis")

	g, issues := BuildGraph([]*ParsedNote{old, new})
	for _, is := range issues {
		if strings.Contains(is.Kind, "link") {
			t.Fatalf("unexpected link issue: %+v", is)
		}
	}

	// The attr lands on the TARGET, because the target is the note retrieval must demote.
	n, ok := g.Node("note:old-diagnosis")
	if !ok {
		t.Fatal("superseded note is missing from the graph")
	}
	if got := n.Attrs["superseded_by"]; got != "new-diagnosis" {
		t.Fatalf("superseded_by = %v, want new-diagnosis", got)
	}
	// The superseding note must NOT be marked itself.
	if n2, _ := g.Node("note:new-diagnosis"); n2.Attrs["superseded_by"] != nil {
		t.Fatalf("the superseding note was marked superseded: %v", n2.Attrs["superseded_by"])
	}
}

// A forward reference must resolve exactly like a backward one: the target may be built
// after the superseding note, which is the whole reason this runs as a second pass.
func TestSupersedesResolvesForwardReferences(t *testing.T) {
	// Superseding note FIRST in the slice, target second.
	new := supNote("new-diagnosis", "new-diagnosis", "old-diagnosis")
	old := supNote("old-diagnosis", "old-diagnosis")

	g, _ := BuildGraph([]*ParsedNote{new, old})
	n, ok := g.Node("note:old-diagnosis")
	if !ok {
		t.Fatal("superseded note missing")
	}
	if got := n.Attrs["superseded_by"]; got != "new-diagnosis" {
		t.Fatalf("forward reference did not resolve: superseded_by = %v", got)
	}
}

// Supersedes and wikilinks share one target namespace. In particular, the stable id
// remains addressable after a filename changes, and canonical Unicode/case aliases
// resolve identically instead of turning a valid UPDATE into a broken link.
func TestSupersedesResolvesStableIDAfterRenameAndCanonicalization(t *testing.T) {
	tests := []struct {
		name string
		old  *ParsedNote
		raw  string
	}{
		{
			name: "stable id differs from renamed filename",
			old:  supNote("renamed-file", "stable-target"),
			raw:  "STABLE-TARGET",
		},
		{
			name: "unicode id uses NFC case-folded lookup",
			old:  supNote("renamed-unicode-file", "Café-target"),
			raw:  "CAFE\u0301-TARGET",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			newer := supNote("newer", "newer", tc.raw)
			g, issues := BuildGraph([]*ParsedNote{tc.old, newer})
			for _, issue := range issues {
				if strings.Contains(issue.Msg, "supersedes:") {
					t.Fatalf("valid stable-id target was rejected: %+v", issue)
				}
			}
			target, ok := g.Node("note:" + tc.old.FM.ID)
			if !ok {
				t.Fatal("target node missing")
			}
			if got := target.Attrs["superseded_by"]; got != "newer" {
				t.Fatalf("superseded_by = %v, want newer", got)
			}
		})
	}
}

func TestSupersedesRefusesStableIDBasenameCollision(t *testing.T) {
	stable := supNote("renamed-file", "legacy-name")
	basename := supNote("legacy-name", "different-note")
	newer := supNote("newer", "newer", "legacy-name")

	g, issues := BuildGraph([]*ParsedNote{stable, basename, newer})
	for _, id := range []string{"legacy-name", "different-note"} {
		n, _ := g.Node("note:" + id)
		if n != nil && n.Attrs["superseded_by"] != nil {
			t.Fatalf("ambiguous shorthand retired note %s", id)
		}
	}
	var found bool
	for _, issue := range issues {
		if issue.Kind == "ambiguous-link" && strings.Contains(issue.Msg, "supersedes:") &&
			strings.Contains(issue.Msg, "by filename") && strings.Contains(issue.Msg, "by stable id") {
			found = true
		}
	}
	if !found {
		t.Fatalf("stable-id/basename collision was not reported: %+v", issues)
	}
}

func TestSupersedesRefusesUnicodeCaseAliasedQualifiedPaths(t *testing.T) {
	a := supNote("café", "target-a")
	a.Path = "Team/Café.md"
	b := supNote("café", "target-b")
	b.Path = "team/Cafe\u0301.md"
	newer := supNote("newer", "newer", "TEAM/CAFE\u0301")

	g, issues := BuildGraph([]*ParsedNote{a, b, newer})
	for _, id := range []string{"target-a", "target-b"} {
		n, _ := g.Node("note:" + id)
		if n != nil && n.Attrs["superseded_by"] != nil {
			t.Fatalf("normalized path alias retired note %s", id)
		}
	}
	var found bool
	for _, issue := range issues {
		if issue.Kind == "ambiguous-link" && strings.Contains(issue.Msg, "supersedes:") &&
			strings.Contains(issue.Msg, "normalized paths") {
			found = true
		}
	}
	if !found {
		t.Fatalf("normalized path ambiguity was not reported: %+v", issues)
	}
}

// A supersedes pointing at nothing must be REPORTED, not silently dropped: a typo that
// retires no note looks identical to a correction that worked.
func TestSupersedesReportsUnresolvableTarget(t *testing.T) {
	n := supNote("new-diagnosis", "new-diagnosis", "a-note-that-does-not-exist")
	_, issues := BuildGraph([]*ParsedNote{n})

	var found bool
	for _, is := range issues {
		if is.Kind == "broken-link" && strings.Contains(is.Msg, "supersedes:") {
			found = true
		}
	}
	if !found {
		t.Fatalf("an unresolvable supersedes target was not reported; issues=%+v", issues)
	}
}

// Self-supersede would demote the very note the author is promoting, and is always a
// typo (usually a copied id).
func TestSupersedesRefusesSelfReference(t *testing.T) {
	n := supNote("self", "self", "self")
	g, issues := BuildGraph([]*ParsedNote{n})

	if node, _ := g.Node("note:self"); node.Attrs["superseded_by"] != nil {
		t.Fatal("a note superseded itself and was demoted by its own correction")
	}
	var found bool
	for _, is := range issues {
		if strings.Contains(is.Msg, "is this note itself") {
			found = true
		}
	}
	if !found {
		t.Fatalf("self-supersede was not reported; issues=%+v", issues)
	}
}

// An ambiguous basename must refuse to bind rather than guess: retiring the WRONG note
// is strictly worse than retiring none.
func TestSupersedesRefusesAmbiguousTarget(t *testing.T) {
	a := &ParsedNote{Path: "gotchas/dupe.md", Key: "dupe", Body: "x",
		FM: &vault.Frontmatter{ID: "dupe-a", Type: "gotcha", Title: "dupe", When: "2026-01-01"}}
	b := &ParsedNote{Path: "decisions/dupe.md", Key: "dupe", Body: "x",
		FM: &vault.Frontmatter{ID: "dupe-b", Type: "decision", Title: "dupe", When: "2026-01-01"}}
	n := supNote("newer", "newer", "dupe")

	g, issues := BuildGraph([]*ParsedNote{a, b, n})
	for _, id := range []string{"note:dupe-a", "note:dupe-b"} {
		if node, ok := g.Node(id); ok && node.Attrs["superseded_by"] != nil {
			t.Fatalf("%s was retired on an ambiguous name", id)
		}
	}
	var found bool
	for _, is := range issues {
		if is.Kind == "ambiguous-link" && strings.Contains(is.Msg, "supersedes:") {
			found = true
		}
	}
	if !found {
		t.Fatalf("ambiguous supersedes target was not reported; issues=%+v", issues)
	}
}

// TestSupersedesWinnerIsDeterministicAcrossSnapshotOrder guards the many-to-one case:
// five notes all supersede the same target. ReconcileIncremental feeds BuildGraph the
// result of NoteCache.Snapshot(), which ranges a Go map with no defined order, so
// before the fix the winner stamped on the target's superseded_by attr flipped from
// rebuild to rebuild on the SAME vault content. This simulates that map-range input
// hundreds of times and requires exactly one winner ever appears.
func TestSupersedesWinnerIsDeterministicAcrossSnapshotOrder(t *testing.T) {
	target := supNote("target", "target")
	var claimants []*ParsedNote
	for i := 1; i <= 5; i++ {
		id := fmt.Sprintf("fix-%d", i)
		claimants = append(claimants, supNote(id, id, "target"))
	}

	winners := map[string]int{}
	for i := 0; i < 300; i++ {
		// Rebuild the input slice by ranging a fresh map each time, exactly like
		// NoteCache.Snapshot() does. Go deliberately randomizes map iteration order
		// per range, so 300 iterations is enough to surface any order-dependence.
		byID := map[string]*ParsedNote{effectiveID(target): target}
		for _, c := range claimants {
			byID[effectiveID(c)] = c
		}
		notes := make([]*ParsedNote, 0, len(byID))
		for _, pn := range byID {
			notes = append(notes, pn)
		}

		g, _ := BuildGraph(notes)
		n, ok := g.Node("note:target")
		if !ok {
			t.Fatal("target note missing from graph")
		}
		winner, _ := n.Attrs["superseded_by"].(string)
		winners[winner]++
	}
	if len(winners) != 1 {
		t.Fatalf("superseded_by winner is not deterministic across snapshot order: %v", winners)
	}
}

// v6 already hashed `supersedes`, but did not derive an edge or target attribute from
// it. An unchanged vault therefore reports no drift after upgrading unless the index
// generation itself changes. The v7 semantic bump must refuse old read-only state,
// rebuild it on a writer, and retain paid embedding rows through that rebuild.
func TestSchemaV7RebuildsSupersedesStateAndPreservesVectors(t *testing.T) {
	root := t.TempDir()
	writeNote := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeNote("old.md", "---\nid: old\ntype: gotcha\nwhen: 2026-01-01\n---\n# Old\nlegacy diagnosis\n")
	writeNote("new.md", "---\nid: new\ntype: gotcha\nwhen: 2026-01-02\nsupersedes: [old]\n---\n# New\ncorrected diagnosis\n")

	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReindexFull(s, root); err != nil {
		s.Close()
		t.Fatal(err)
	}
	var oldHash string
	if err := s.readDB.QueryRow(`SELECT retrieval_hash FROM notes WHERE id='old'`).Scan(&oldHash); err != nil {
		s.Close()
		t.Fatal(err)
	}
	if err := s.Write(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO vectors(node_id,chunk_ix,model,dim,embedding,content_hash,note_hash)
			VALUES('note:old',0,'upgrade-model',1,?, 'cached-content', ?)`, []byte{0, 0, 0, 0}, oldHash); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO meta(key,value) VALUES
			('vector_model','upgrade-model'),('vector_dim','1')
			ON CONFLICT(key) DO UPDATE SET value=excluded.value`); err != nil {
			return err
		}
		// Reproduce the derived graph state emitted by the last v6 binary while leaving
		// the Markdown and retrieval hash untouched.
		if _, err := tx.Exec(`DELETE FROM edges WHERE relation='supersedes'`); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE nodes SET attrs='{"type":"gotcha","scope":"dev","when":"2026-01-01"}' WHERE id='note:old'`); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE meta SET value=? WHERE key='schema_version'`, fmt.Sprint(SchemaVersion-1))
		return err
	}); err != nil {
		s.Close()
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	if ro, err := OpenReadOnly(root); err == nil || !errors.Is(err, ErrSchemaMismatch) {
		if ro != nil {
			_ = ro.Close()
		}
		t.Fatalf("read-only open accepted v6 derived graph state: %v", err)
	}

	rebuilt, recovered, err := OpenRebuild(root)
	if err != nil {
		t.Fatal(err)
	}
	defer rebuilt.Close()
	if recovered {
		t.Fatal("ordinary v6 semantic upgrade was reported as corruption recovery")
	}
	var vectors int
	if err := rebuilt.readDB.QueryRow(`SELECT count(*) FROM vectors WHERE node_id='note:old' AND model='upgrade-model'`).Scan(&vectors); err != nil {
		t.Fatal(err)
	}
	if vectors != 1 {
		t.Fatalf("v7 semantic rebuild retained %d paid vectors, want 1", vectors)
	}
	var model string
	if err := rebuilt.readDB.QueryRow(`SELECT value FROM meta WHERE key='vector_model'`).Scan(&model); err != nil || model != "upgrade-model" {
		t.Fatalf("vector model metadata after v7 rebuild = %q, %v", model, err)
	}

	g, _, err := ReindexFull(rebuilt, root)
	if err != nil {
		t.Fatal(err)
	}
	target, ok := g.Node("note:old")
	if !ok || target.Attrs["superseded_by"] != "new" {
		t.Fatalf("v7 rebuild did not derive superseded_by: node=%+v", target)
	}
	if err := rebuilt.readDB.QueryRow(`SELECT count(*) FROM vectors WHERE node_id='note:old'`).Scan(&vectors); err != nil || vectors != 1 {
		t.Fatalf("reindex after v7 rebuild retained %d vectors, err=%v", vectors, err)
	}
}
