// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"context"
	"database/sql"
	"testing"

	"github.com/bright-interaction/mesh/internal/graph"
)

func TestLoadGraphPreservesExactSQLEdgeIdentity(t *testing.T) {
	for _, versioned := range []bool{false, true} {
		name := "ordinary"
		if versioned {
			name = "note-version-gated"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			owner, err := Open(root)
			if err != nil {
				t.Fatal(err)
			}
			defer owner.Close()
			// These are separate SQL primary-key tuples but collide when flattened
			// into source + NUL + target + NUL + relation.
			edges := []graph.Edge{
				{Source: "note:a\x00note:b", Target: "note:c", Relation: graph.RelReferences, Confidence: graph.ConfExtracted, ConfidenceScore: 0.5, Weight: 0.25, SourceLoc: "L1"},
				{Source: "note:a", Target: "note:b\x00note:c", Relation: graph.RelReferences, Confidence: graph.ConfInferred, ConfidenceScore: 0.75, Weight: 0.5, SourceLoc: "L2"},
			}
			if err := owner.Write(func(tx *sql.Tx) error {
				if _, err := tx.Exec(`INSERT INTO notes(id,path,type,title,retrieval_hash,frontmatter,mtime) VALUES('published','published.md','note','Published','expected','{}',1);
					INSERT INTO nodes(id,kind,label,note_id,note_path) VALUES('note:published','note','Published','published','published.md')`); err != nil {
					return err
				}
				for _, edge := range edges {
					for _, id := range []string{edge.Source, edge.Target} {
						if _, err := tx.Exec(`INSERT INTO nodes(id,kind,label) VALUES(?,'note','Edge endpoint')`, id); err != nil {
							return err
						}
					}
					if _, err := tx.Exec(`INSERT INTO edges(source,target,relation,confidence,confidence_score,weight,source_loc) VALUES(?,?,?,?,?,?,?)`, edge.Source, edge.Target, edge.Relation, edge.Confidence, edge.ConfidenceScore, edge.Weight, edge.SourceLoc); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			reader, err := OpenReadOnly(root)
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			ctx := context.Background()
			turnover := func() {
				if err := owner.Write(func(tx *sql.Tx) error {
					_, err := tx.Exec(`UPDATE edges SET weight=weight+1; UPDATE notes SET retrieval_hash='changed' WHERE id='published'`)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			var got *graph.Graph
			if versioned {
				if g, matched, err := reader.LoadGraphAtNoteVersionContext(ctx, "published", "published.md", "wrong"); err != nil || matched || g != nil {
					t.Fatalf("mismatched note version accepted: matched=%v graph=%v err=%v", matched, g, err)
				}
				var matched bool
				got, matched, err = reader.loadGraphAtNoteVersionContext(ctx, "published", "published.md", "expected", turnover)
				if err != nil || !matched {
					t.Fatalf("expected snapshot not loaded: matched=%v err=%v", matched, err)
				}
			} else {
				got, err = reader.loadGraphSnapshotContext(ctx, turnover)
				if err != nil {
					t.Fatal(err)
				}
			}
			assertExactEdges := func(g *graph.Graph, weightOffset float64) {
				t.Helper()
				if g.EdgeCount() != len(edges) || g.NodeCount() != 5 {
					t.Fatalf("SQL tuples collapsed: nodes=%d edges=%d", g.NodeCount(), g.EdgeCount())
				}
				for _, edge := range edges {
					edge.Weight += weightOffset
					out, in := g.Neighbors(edge.Source), g.RefsTo(edge.Target)
					if len(out) != 1 || out[0] != edge || len(in) != 1 || in[0] != edge {
						t.Fatalf("edge tuple/payload changed: want=%+v outbound=%+v inbound=%+v", edge, out, in)
					}
					for _, id := range []string{edge.Source, edge.Target} {
						node, ok := g.Node(id)
						if !ok || node.Degree != 1 || node.KnowledgeDegree != 1 {
							t.Fatalf("endpoint degree mismatch for %q: %+v", id, node)
						}
					}
				}
				if node, ok := g.Node("note:published"); !ok || node.Degree != 0 || node.KnowledgeDegree != 0 {
					t.Fatalf("publication node changed: %+v", node)
				}
			}
			// The racing owner commit must not leak into the proof's read snapshot.
			assertExactEdges(got, 0)
			if g, matched, err := reader.LoadGraphAtNoteVersionContext(ctx, "published", "published.md", "expected"); err != nil || matched || g != nil {
				t.Fatalf("superseded note version accepted: matched=%v graph=%v err=%v", matched, g, err)
			}
			current, matched, err := reader.LoadGraphAtNoteVersionContext(ctx, "published", "published.md", "changed")
			if err != nil || !matched {
				t.Fatalf("committed turnover unavailable: matched=%v err=%v", matched, err)
			}
			assertExactEdges(current, 1)
		})
	}
}
