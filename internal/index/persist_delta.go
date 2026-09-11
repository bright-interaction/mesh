// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/bright-interaction/mesh/internal/graph"
	"github.com/bright-interaction/mesh/internal/latency"
)

// These comparable records preserve SQL NULL distinctly from an empty string or
// zero: a legacy/externally edited row must converge to the full writer's values.
type graphNodeRow struct {
	id, kind, label                            string
	noteID, notePath, anchor, sourceLoc, attrs sql.NullString
	community                                  sql.NullInt64
}

type graphEdgeKey struct{ source, target, relation string }

type graphEdgeRow struct {
	graphEdgeKey
	confidence              string
	confidenceScore, weight float64
	sourceLoc               sql.NullString
}

func graphText(s string) sql.NullString { return sql.NullString{String: s, Valid: true} }

// writeGraphDeltaContext compares the WHOLE rebuilt graph, not just changed
// notes: community labels and superseded_by attributes can change globally.
// The baseline is read inside the same writer transaction as notes/FTS and the
// delta. No process-local baseline can get ahead of a failed commit or restart.
// This remains O(nodes+edges) work/memory but only changed rows incur SQL writes.
func writeGraphDeltaContext(ctx context.Context, tx *sql.Tx, g *graph.Graph) error {
	trace := latency.Start("persist_graph_delta", "encode")
	defer trace.End()
	nodes := make(map[string]graphNodeRow, g.NodeCount())
	edges := make(map[graphEdgeKey]graphEdgeRow, g.EdgeCount())
	for _, n := range g.Nodes() {
		if err := ctx.Err(); err != nil {
			return err
		}
		attrs, err := json.Marshal(n.Attrs)
		if err != nil {
			return err
		}
		nodes[n.ID] = graphNodeRow{
			id: n.ID, kind: n.Kind, label: n.Label,
			noteID: graphText(n.NoteID), notePath: graphText(n.NotePath),
			anchor: graphText(n.Anchor), sourceLoc: graphText(n.SourceLoc),
			community: sql.NullInt64{Int64: int64(n.Community), Valid: true},
			attrs:     graphText(string(attrs)),
		}
		// Match the full writer: only outbound edges of present source nodes
		// are persisted. Graph.AddEdge already keeps the first duplicate key.
		for _, e := range g.Neighbors(n.ID) {
			if err := ctx.Err(); err != nil {
				return err
			}
			key := graphEdgeKey{e.Source, e.Target, e.Relation}
			edges[key] = graphEdgeRow{key, e.Confidence, e.ConfidenceScore, e.Weight, graphText(e.SourceLoc)}
		}
	}
	trace.Phase("nodes")
	if err := diffGraphNodes(ctx, tx, nodes); err != nil {
		return err
	}
	trace.Phase("edges")
	return diffGraphEdges(ctx, tx, edges)
}

func diffGraphNodes(ctx context.Context, tx *sql.Tx, wanted map[string]graphNodeRow) error {
	trace := latency.Start("persist_graph_nodes", "scan")
	defer trace.End()
	rows, err := tx.QueryContext(ctx, `SELECT id,kind,label,note_id,note_path,anchor,source_loc,community,attrs FROM nodes`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var removed []string
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		var old graphNodeRow
		if err := rows.Scan(&old.id, &old.kind, &old.label, &old.noteID, &old.notePath, &old.anchor, &old.sourceLoc, &old.community, &old.attrs); err != nil {
			return err
		}
		if next, ok := wanted[old.id]; !ok {
			removed = append(removed, old.id)
		} else if next == old {
			delete(wanted, old.id)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	// Finish the cursor before mutating the table being scanned.
	if err := rows.Close(); err != nil {
		return err
	}
	trace.Phase("delete")
	del, err := tx.PrepareContext(ctx, `DELETE FROM nodes WHERE id=?`)
	if err != nil {
		return err
	}
	defer del.Close()
	for _, id := range removed {
		if _, err := del.ExecContext(ctx, id); err != nil {
			return err
		}
	}
	trace.Phase("upsert")
	put, err := tx.PrepareContext(ctx, `INSERT INTO nodes(id,kind,label,note_id,note_path,anchor,source_loc,community,attrs)
		VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET
		kind=excluded.kind,label=excluded.label,note_id=excluded.note_id,note_path=excluded.note_path,
		anchor=excluded.anchor,source_loc=excluded.source_loc,community=excluded.community,attrs=excluded.attrs`)
	if err != nil {
		return err
	}
	defer put.Close()
	for _, n := range wanted {
		if _, err := put.ExecContext(ctx, n.id, n.kind, n.label, n.noteID, n.notePath, n.anchor, n.sourceLoc, n.community, n.attrs); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func diffGraphEdges(ctx context.Context, tx *sql.Tx, wanted map[graphEdgeKey]graphEdgeRow) error {
	trace := latency.Start("persist_graph_edges", "scan")
	defer trace.End()
	rows, err := tx.QueryContext(ctx, `SELECT source,target,relation,confidence,confidence_score,weight,source_loc FROM edges`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var removed []graphEdgeKey
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		var old graphEdgeRow
		if err := rows.Scan(&old.source, &old.target, &old.relation, &old.confidence, &old.confidenceScore, &old.weight, &old.sourceLoc); err != nil {
			return err
		}
		if next, ok := wanted[old.graphEdgeKey]; !ok {
			removed = append(removed, old.graphEdgeKey)
		} else if next == old {
			delete(wanted, old.graphEdgeKey)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	trace.Phase("delete")
	del, err := tx.PrepareContext(ctx, `DELETE FROM edges WHERE source=? AND target=? AND relation=?`)
	if err != nil {
		return err
	}
	defer del.Close()
	for _, e := range removed {
		if _, err := del.ExecContext(ctx, e.source, e.target, e.relation); err != nil {
			return err
		}
	}
	trace.Phase("upsert")
	put, err := tx.PrepareContext(ctx, `INSERT INTO edges(source,target,relation,confidence,confidence_score,weight,source_loc)
		VALUES(?,?,?,?,?,?,?) ON CONFLICT(source,target,relation) DO UPDATE SET
		confidence=excluded.confidence,confidence_score=excluded.confidence_score,weight=excluded.weight,source_loc=excluded.source_loc`)
	if err != nil {
		return err
	}
	defer put.Close()
	for _, e := range wanted {
		if _, err := put.ExecContext(ctx, e.source, e.target, e.relation, e.confidence, e.confidenceScore, e.weight, e.sourceLoc); err != nil {
			return err
		}
	}
	return ctx.Err()
}
