// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/graph"
)

func TestGraphDeltaCommunityOnlyAvoidsOtherColumns(t *testing.T) {
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
		_, err := tx.Exec(`UPDATE nodes SET community=NULL WHERE id='note:c';
			CREATE TABLE community_mutations(kind TEXT, id TEXT);
			CREATE TRIGGER count_node_metadata AFTER UPDATE OF kind,note_id,attrs ON nodes
			BEGIN INSERT INTO community_mutations VALUES('metadata',NEW.id); END;
			CREATE TRIGGER count_node_community AFTER UPDATE OF community ON nodes
			BEGIN INSERT INTO community_mutations VALUES('community',NEW.id); END;`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	a, _ := g.Node("note:a")
	a.Label = "changed label" // must still use the complete writer
	b, _ := g.Node("note:b")
	b.Community = 42 // only this column changed
	// c repairs NULL community to zero without assigning any other column.
	apply := func() {
		t.Helper()
		if err := s.Write(func(tx *sql.Tx) error {
			return writeGraphDeltaContext(context.Background(), tx, g)
		}); err != nil {
			t.Fatal(err)
		}
	}
	apply()
	apply() // no-op delta must not write again
	var metadata, community, unexpected int
	if err := s.readDB.QueryRow(`SELECT
		count(*) FILTER (WHERE kind='metadata'),
		count(*) FILTER (WHERE kind='community'),
		count(*) FILTER (WHERE kind='metadata' AND id!='note:a')
		FROM community_mutations`).Scan(&metadata, &community, &unexpected); err != nil {
		t.Fatal(err)
	}
	if metadata != 1 || community != 3 || unexpected != 0 {
		t.Fatalf("column writes: metadata=%d community=%d unexpected=%d", metadata, community, unexpected)
	}
	got := snapshotTables(t, s)
	if err := s.Write(func(tx *sql.Tx) error { return writeGraphTables(tx, g) }); err != nil {
		t.Fatal(err)
	}
	if got != snapshotTables(t, s) {
		t.Fatal("community delta differs from the full writer")
	}
}

func TestGraphDeltaCommunityOnlyInvalidatesReader(t *testing.T) {
	s, m, _ := revisionFixture(t)
	g := deltaFixture(false)
	apply := func() {
		t.Helper()
		if err := s.Write(func(tx *sql.Tx) error {
			return writeGraphDeltaContext(context.Background(), tx, g)
		}); err != nil {
			t.Fatal(err)
		}
	}
	apply()
	before := readRevision(t, m)
	n, _ := g.Node("note:b")
	n.Community = 42
	apply()
	after := readRevision(t, m)
	if !before.Tracked || !after.Tracked || before.Epoch != after.Epoch || after.Revision != before.Revision+1 {
		t.Fatalf("community update must invalidate exactly once: before=%+v after=%+v", before, after)
	}
	apply()
	if got := readRevision(t, m); got != after {
		t.Fatalf("unchanged graph invalidated reader: got=%+v want=%+v", got, after)
	}
}

func TestGraphDeltaCommunityOnlyRollbackPreservesRevision(t *testing.T) {
	for _, cancelAfterUpdate := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel=%t", cancelAfterUpdate), func(t *testing.T) {
			s, m, _ := revisionFixture(t)
			g := deltaFixture(false)
			if err := s.Write(func(tx *sql.Tx) error { return writeGraphTables(tx, g) }); err != nil {
				t.Fatal(err)
			}
			before, revision := snapshotTables(t, s), readRevision(t, m)
			n, _ := g.Node("note:b")
			n.Community = 42
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			failure := errors.New("failure after community delta")
			if cancelAfterUpdate {
				failure = context.Canceled
			}
			err := s.Write(func(tx *sql.Tx) error {
				if err := writeGraphDeltaContext(ctx, tx, g); err != nil {
					return err
				}
				if cancelAfterUpdate {
					cancel()
					return ctx.Err()
				}
				return failure
			})
			if !errors.Is(err, failure) {
				t.Fatalf("expected injected failure, got %v", err)
			}
			if snapshotTables(t, s) != before || readRevision(t, m) != revision {
				t.Fatal("failed community update escaped transaction rollback")
			}
			if err := s.Write(func(tx *sql.Tx) error {
				return writeGraphDeltaContext(context.Background(), tx, g)
			}); err != nil {
				t.Fatal(err)
			}
			if got := readRevision(t, m); got.Revision != revision.Revision+1 {
				t.Fatalf("retry did not persist the rolled-back delta: got=%+v before=%+v", got, revision)
			}
		})
	}
}

func TestGraphDeltaCommunityOnlyRejectsSuppressedUpdate(t *testing.T) {
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
		_, err := tx.Exec(`CREATE TRIGGER suppress_community BEFORE UPDATE OF community ON nodes
			BEGIN SELECT RAISE(IGNORE); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	before := snapshotTables(t, s)
	n, _ := g.Node("note:b")
	n.Community = 42
	err = s.Write(func(tx *sql.Tx) error { return writeGraphDeltaContext(context.Background(), tx, g) })
	if err == nil || !strings.Contains(err.Error(), "affected 0 rows") {
		t.Fatalf("suppressed update must not succeed: %v", err)
	}
	if snapshotTables(t, s) != before {
		t.Fatal("suppressed update changed graph snapshot")
	}
}

// A bounded relabel fixture, not a vault capacity test. Community renumbering
// can touch many nodes even when their labels and larger attributes are unchanged.
// Includes the existing revision triggers and synchronous transaction commit.
func BenchmarkGraphCommunityRelabel(b *testing.B) {
	s, err := Open(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { s.Close() })
	g := graph.NewSized(2048)
	for i := 0; i < 2048; i++ {
		id := fmt.Sprintf("note:%04d", i)
		g.AddNode(&graph.Node{ID: id, Kind: "note", Label: id, NoteID: id, Community: i % 64,
			Attrs: map[string]any{"do": strings.Repeat("synthetic metadata ", 32)}})
	}
	nodes := g.Nodes()
	if err := s.Write(func(tx *sql.Tx) error { return writeGraphTables(tx, g) }); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, n := range nodes {
			n.Community++
		}
		if err := s.Write(func(tx *sql.Tx) error {
			return writeGraphDeltaContext(context.Background(), tx, g)
		}); err != nil {
			b.Fatal(err)
		}
	}
}
