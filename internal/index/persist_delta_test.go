// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/bright-interaction/mesh/internal/graph"
)

func deltaFixture(changed bool) *graph.Graph {
	g := graph.New()
	a := &graph.Node{ID: "note:a", Kind: "note", Label: "A", NoteID: "a", NotePath: "a.md"}
	b := &graph.Node{ID: "note:b", Kind: "note", Label: "B"}
	tail := "note:c"
	e := graph.Edge{Source: a.ID, Target: b.ID, Relation: "references", Confidence: "EXTRACTED", ConfidenceScore: 1, Weight: 1}
	if changed {
		a.Kind, a.Label, a.NoteID, a.NotePath = "heading", "renamed", "owner", "sub/a.md"
		a.Anchor, a.SourceLoc, a.Community = "anchor", "L9", 9
		a.Attrs = map[string]any{"superseded_by": "note:d", "nested": map[string]any{"enabled": true}}
		// An unrelated note can acquire a new community from the global rebuild.
		b.Community = 42
		tail = "note:d"
		e.Confidence, e.ConfidenceScore, e.Weight, e.SourceLoc = "INFERRED", .5, 2, "L5"
	}
	g.AddNode(a)
	g.AddNode(b)
	g.AddNode(&graph.Node{ID: tail, Kind: "note", Label: tail, Attrs: map[string]any{}})
	g.AddEdge(e)
	duplicate := e
	duplicate.Weight = 999 // AddEdge's first-key-wins contract must survive persistence.
	g.AddEdge(duplicate)
	g.AddEdge(graph.Edge{Source: b.ID, Target: tail, Relation: "contains", Confidence: "EXTRACTED", Weight: 1})
	// Full persistence omits edges whose source node is absent.
	g.AddEdge(graph.Edge{Source: "absent", Target: a.ID, Relation: "references", Confidence: "EXTRACTED", Weight: 1})
	return g
}

func TestGraphDeltaWritesOnlyChangedRows(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Write(func(tx *sql.Tx) error { return writeGraphTables(tx, deltaFixture(false)) }); err != nil {
		t.Fatal(err)
	}
	// Count actual SQL row mutations, not elapsed time or prepared statements.
	if err := s.Write(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`CREATE TABLE graph_mutations(kind TEXT)`); err != nil {
			return err
		}
		for _, table := range []string{"nodes", "edges"} {
			for _, op := range []string{"INSERT", "UPDATE", "DELETE"} {
				if _, err := tx.Exec(fmt.Sprintf("CREATE TRIGGER count_%s_%s AFTER %s ON %s BEGIN INSERT INTO graph_mutations VALUES('%s'); END", table, op, op, table, table)); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	apply := func(g *graph.Graph) {
		t.Helper()
		if _, err := s.IndexVaultIncremental(nil, nil, g); err != nil {
			t.Fatal(err)
		}
	}
	count := func(kind string, want int) {
		t.Helper()
		var got int
		if err := s.readDB.QueryRow("SELECT count(*) FROM graph_mutations WHERE kind=?", kind).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("%s mutations = %d, want %d", kind, got, want)
		}
	}
	apply(deltaFixture(false))
	count("nodes", 0)
	count("edges", 0)
	apply(deltaFixture(true))
	count("nodes", 4) // two updates, one delete, one insert
	count("edges", 3) // metadata update, old key delete, new key insert
	apply(deltaFixture(true))
	count("nodes", 4)
	count("edges", 3)
	got := snapshotTables(t, s)
	if err := s.Write(func(tx *sql.Tx) error { return writeGraphTables(tx, deltaFixture(true)) }); err != nil {
		t.Fatal(err)
	}
	if want := snapshotTables(t, s); got != want {
		t.Fatalf("delta differs from full rewrite\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestGraphDeltaRepairsNullableRowsAndRemovesAll(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	g := deltaFixture(false)
	if err := s.Write(func(tx *sql.Tx) error {
		if err := writeGraphTables(tx, g); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE nodes SET note_id=NULL,note_path=NULL,anchor=NULL,source_loc=NULL,community=NULL,attrs=NULL`); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE edges SET source_loc=NULL`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.IndexVaultIncremental(nil, nil, g); err != nil {
		t.Fatal(err)
	}
	got := snapshotTables(t, s)
	if err := s.Write(func(tx *sql.Tx) error { return writeGraphTables(tx, g) }); err != nil {
		t.Fatal(err)
	}
	if got != snapshotTables(t, s) {
		t.Fatal("nullable rows did not converge to the full writer")
	}
	if _, err := s.IndexVaultIncremental(nil, nil, graph.New()); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.readDB.QueryRow(`SELECT (SELECT count(*) FROM nodes)+(SELECT count(*) FROM edges)`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("empty graph retained rows: count=%d err=%v", count, err)
	}
}

// Reusing scan storage must not alias retained deletion keys or carry nullable
// metadata between rows. Compact edge values must still distinguish ALL three
// key fields even when many edges share a source, target or relation.
func TestGraphDeltaCompactRowsMatchFullWriter(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	fixture := func(revision int) *graph.Graph {
		g := graph.New()
		for i := 0; i < 24; i++ {
			if revision > 0 && i%4 == 0 {
				continue // several keys must survive scratch reuse until deletion
			}
			id := fmt.Sprintf("note:%d", i)
			n := &graph.Node{ID: id, Kind: "note", Label: id, Community: i}
			if i%2 == 0 {
				n.NoteID, n.NotePath, n.Anchor, n.SourceLoc = id, id+".md", "anchor", "L9"
				n.Attrs = map[string]any{"revision": revision, "body": "metadata\x00with unicode ä"}
			}
			if revision > 0 && i%3 == 0 {
				n.Label = "changed " + id
				n.Community += revision
			}
			g.AddNode(n)
			for j, relation := range []string{"references", "contains", "references\x00tail"} {
				for offset := 1; offset <= 2; offset++ {
					e := graph.Edge{Source: id, Target: fmt.Sprintf("note:%d", (i+offset)%24), Relation: relation,
						Confidence: "EXTRACTED", ConfidenceScore: float64(j) / 2, Weight: float64(offset)}
					if j%2 == 0 {
						e.SourceLoc = "source\x00" + id
					}
					if revision > 0 && j == 1 {
						e.Confidence, e.Weight = "INFERRED", float64(revision)
					}
					g.AddEdge(e)
				}
			}
		}
		return g
	}
	if err := s.Write(func(tx *sql.Tx) error {
		if err := writeGraphTables(tx, fixture(0)); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE nodes SET note_id=NULL,note_path=NULL,anchor=NULL,source_loc=NULL,community=NULL,attrs=NULL WHERE community%2=0`); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE edges SET source_loc=NULL WHERE relation='contains'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, revision := range []int{1, 2, 0} { // delete, update, then restore missing rows
		g := fixture(revision)
		if _, err := s.IndexVaultIncremental(nil, nil, g); err != nil {
			t.Fatal(err)
		}
		got := snapshotTables(t, s)
		if err := s.Write(func(tx *sql.Tx) error { return writeGraphTables(tx, g) }); err != nil {
			t.Fatal(err)
		}
		if want := snapshotTables(t, s); got != want {
			t.Fatalf("revision %d: compact delta differs from full writer\ngot:\n%s\nwant:\n%s", revision, got, want)
		}
	}
}

func TestGraphDeltaFailureRollsBackNotesFTSAndGraph(t *testing.T) {
	dir := writeVault(t)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := Reindex(s, dir); err != nil {
		t.Fatal(err)
	}
	seedVectors(t, s)
	before := snapshotTables(t, s)
	if err := s.Write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`CREATE TRIGGER fail_graph_edge BEFORE INSERT ON edges BEGIN SELECT RAISE(ABORT,'test graph failure'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	pn, err := ParseFile(filepath.Join(dir, "a.md"))
	if err != nil {
		t.Fatal(err)
	}
	pn.Path, pn.Body, pn.FM.Title = "a.md", "changed searchable content", "Changed A"
	if _, err := s.IndexVaultIncremental([]*ParsedNote{pn}, []string{"c"}, deltaFixture(true)); err == nil {
		t.Fatal("expected graph write failure")
	}
	if before != snapshotTables(t, s) {
		t.Fatal("failed graph delta leaked notes, FTS, vectors or graph changes")
	}
	if err := s.Write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`DROP TRIGGER fail_graph_edge`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.IndexVaultIncremental([]*ParsedNote{pn}, []string{"c"}, deltaFixture(true)); err != nil {
		t.Fatalf("retry against committed baseline: %v", err)
	}
	got := snapshotTables(t, s)
	if err := s.Write(func(tx *sql.Tx) error { return writeGraphTables(tx, deltaFixture(true)) }); err != nil {
		t.Fatal(err)
	}
	if got != snapshotTables(t, s) {
		t.Fatal("retry did not converge to full graph persistence")
	}
}

func TestGraphDeltaReaderSeesCommittedSnapshot(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.IndexVaultIncremental(nil, nil, deltaFixture(false)); err != nil {
		t.Fatal(err)
	}
	before := snapshotTables(t, s)
	ready, release := make(chan struct{}), make(chan struct{})
	defer func() {
		if release != nil {
			close(release)
		}
	}()
	done := make(chan error, 1)
	// Capture the channel value; the caller clears its variable after releasing it.
	go func(unblock <-chan struct{}) {
		done <- s.Write(func(tx *sql.Tx) error {
			if err := writeGraphDeltaContext(context.Background(), tx, deltaFixture(true)); err != nil {
				return err
			}
			close(ready)
			<-unblock
			return nil
		})
	}(release)
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("writer failed before pause: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("writer did not reach commit boundary")
	}
	if got := snapshotTables(t, s); got != before {
		t.Fatal("reader saw uncommitted graph delta")
	}
	close(release)
	release = nil
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := snapshotTables(t, s); got == before {
		t.Fatal("reader did not see committed graph delta")
	}
}

func TestGraphDeltaCancellationRollsBack(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.IndexVaultIncremental(nil, nil, deltaFixture(false)); err != nil {
		t.Fatal(err)
	}
	before := snapshotTables(t, s)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err = s.Write(func(tx *sql.Tx) error {
		if err := writeGraphDeltaContext(ctx, tx, deltaFixture(true)); err != nil {
			return err
		}
		cancel() // cancellation after mutations must still roll back the transaction
		return ctx.Err()
	})
	if !errors.Is(err, context.Canceled) || before != snapshotTables(t, s) {
		t.Fatalf("canceled transaction did not preserve snapshot: %v", err)
	}
	if err := s.Write(func(tx *sql.Tx) error { return writeGraphDeltaContext(ctx, tx, deltaFixture(true)) }); !errors.Is(err, context.Canceled) {
		t.Fatalf("already canceled delta: %v", err)
	}
}

// Isolate persistence from graph building and parsing. Both cases use the same
// graph and single changed label, including synchronous commit costs.
func BenchmarkGraphPersistence(b *testing.B) {
	for _, delta := range []bool{false, true} {
		b.Run(fmt.Sprintf("delta=%t", delta), func(b *testing.B) {
			s, err := Open(b.TempDir())
			if err != nil {
				b.Fatal(err)
			}
			defer s.Close()
			g := graph.NewSized(24000)
			for i := 0; i < 24000; i++ {
				id := fmt.Sprintf("note:%d", i)
				g.AddNode(&graph.Node{ID: id, Kind: "note", Label: id, NoteID: id, NotePath: id + ".md"})
				for j := 1; j <= 2; j++ {
					g.AddEdge(graph.Edge{Source: id, Target: fmt.Sprintf("note:%d", (i+j)%24000), Relation: "references", Confidence: "EXTRACTED", ConfidenceScore: 1, Weight: 1})
				}
			}
			if err := s.Write(func(tx *sql.Tx) error { return writeGraphTables(tx, g) }); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				n, _ := g.Node("note:0")
				n.Label = fmt.Sprintf("revision %d", i)
				if err := s.Write(func(tx *sql.Tx) error {
					if delta {
						return writeGraphDeltaContext(context.Background(), tx, g)
					}
					return writeGraphTables(tx, g)
				}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
