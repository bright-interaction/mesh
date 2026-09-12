// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestLoadGraphCapacityEmptyAndIndependentScanValues(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.readDB.SetMaxOpenConns(1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if g, err := s.LoadGraphContext(ctx); !errors.Is(err, context.Canceled) || g != nil {
		t.Fatalf("canceled load = %v, %v", g, err)
	}
	g, err := s.LoadGraph()
	if err != nil || len(g.Nodes()) != 0 {
		t.Fatalf("empty graph: %v", err)
	}
	if err := s.Write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO nodes(id,kind,label,attrs) VALUES
			('note:a','note','Alpha','{"do":"first"}'),
			('note:b','note','Beta','{"do":"second"}'),
			('note:c','note','Gamma',NULL);
			INSERT INTO edges(source,target,relation,confidence,weight,source_loc) VALUES
			('note:a','note:b','references','EXTRACTED',0.5,'L1'),
			('note:b','note:c','references','AMBIGUOUS',0.75,'L2')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	g, err = s.LoadGraph()
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{"note:a": "first", "note:b": "second"} {
		n, ok := g.Node(id)
		if !ok || n.Attrs["do"] != want {
			t.Fatalf("%s scan scratch leaked: %+v", id, n)
		}
	}
	n, _ := g.Node("note:c")
	if len(n.Attrs) != 0 {
		t.Fatal("NULL attrs reused previous row")
	}
	a, b := g.Neighbors("note:a"), g.Neighbors("note:b")
	if len(a) != 1 || a[0].Target != "note:b" || a[0].Weight != 0.5 || a[0].SourceLoc != "L1" || a[0].Confidence != "EXTRACTED" {
		t.Fatalf("first edge overwritten by scan scratch: %+v", a)
	}
	if len(b) != 1 || b[0].Target != "note:c" || b[0].Weight != 0.75 || b[0].SourceLoc != "L2" || b[0].Confidence != "AMBIGUOUS" {
		t.Fatalf("second edge lost: %+v", b)
	}
	// Repeat with one connection: every capacity/node/edge Rows must be closed.
	ctx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := s.LoadGraphContext(ctx); err != nil {
		t.Fatal(err)
	}
}

type graphCountBoundary struct {
	*sql.Tx
	afterCounts func()
}

func (q graphCountBoundary) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if strings.HasPrefix(query, "SELECT id, kind") {
		q.afterCounts()
	}
	return q.Tx.QueryContext(ctx, query, args...)
}

func TestLoadGraphCapacityCountSharesSnapshotWithScans(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	replaceGraphSnapshot(t, s, "old-left", "old-right")
	ctx := context.Background()
	tx, err := s.readDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	g, err := loadGraphContext(ctx, graphCountBoundary{tx, func() {
		// The capacity count has already established the read snapshot.
		replaceGraphSnapshot(t, s, "new-left", "new-right")
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := g.Node("old-left"); !ok {
		t.Fatal("count and node scan did not share a snapshot")
	}
	if _, ok := g.Node("new-left"); ok {
		t.Fatal("post-count commit leaked into graph")
	}
	if edges := g.Neighbors("old-left"); len(edges) != 1 || edges[0].Target != "old-right" {
		t.Fatalf("post-count commit leaked into edges: %+v", edges)
	}
}
