// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"

	"github.com/bright-interaction/mesh/internal/graph"
	"github.com/bright-interaction/mesh/internal/latency"
)

// LoadGraph reconstructs the in-memory graph from the persisted nodes + edges
// tables. The CLI uses this for retrieval without re-parsing the vault; the
// long-running daemon (MCP) keeps the graph in memory instead.
func (s *Store) LoadGraph() (*graph.Graph, error) {
	return s.LoadGraphContext(context.Background())
}

// LoadGraphContext reconstructs the graph while honoring cancellation during both SQL
// scans and the final degree pass.
func (s *Store) LoadGraphContext(ctx context.Context) (*graph.Graph, error) {
	return s.loadGraphSnapshotContext(ctx, nil)
}

// loadGraphSnapshotContext keeps nodes and edges on one SQLite read snapshot. The hook
// is a deterministic test seam after the node scan has established that snapshot.
func (s *Store) loadGraphSnapshotContext(ctx context.Context, afterNodes func()) (*graph.Graph, error) {
	trace := latency.Start("load_graph_snapshot", "begin_read")
	defer trace.End()
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	tx, err := s.readDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	trace.Phase("graph")
	g, err := loadGraphContextAfterNodes(ctx, tx, afterNodes)
	if err != nil {
		return nil, err
	}
	trace.Phase("commit_read")
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return g, nil
}

type graphQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func loadGraph(q graphQueryer) (*graph.Graph, error) {
	return loadGraphContext(context.Background(), q)
}

func loadGraphContext(ctx context.Context, q graphQueryer) (*graph.Graph, error) {
	return loadGraphContextAfterNodes(ctx, q, nil)
}

func loadGraphContextAfterNodes(ctx context.Context, q graphQueryer, afterNodes func()) (*graph.Graph, error) {
	trace := latency.Start("load_graph", "nodes")
	defer trace.End()
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	g := graph.New()

	nrows, err := q.QueryContext(ctx, `SELECT id, kind, label, COALESCE(note_id,''), COALESCE(note_path,''), COALESCE(anchor,''), COALESCE(source_loc,''), COALESCE(community,0), COALESCE(attrs,'') FROM nodes`)
	if err != nil {
		return nil, err
	}
	// A leaked *sql.Rows pins a WAL read snapshot for the life of the process, which
	// stops every checkpoint from reclaiming past it. In a long-running daemon that
	// grows the WAL without bound and starves other processes' writes into SQLITE_BUSY.
	defer nrows.Close()
	for nrows.Next() {
		if err := ctx.Err(); err != nil {
			nrows.Close()
			return nil, err
		}
		n := &graph.Node{}
		var attrs string
		if err := nrows.Scan(&n.ID, &n.Kind, &n.Label, &n.NoteID, &n.NotePath, &n.Anchor, &n.SourceLoc, &n.Community, &attrs); err != nil {
			nrows.Close()
			return nil, err
		}
		if attrs != "" && attrs != "null" {
			_ = json.Unmarshal([]byte(attrs), &n.Attrs)
		}
		g.AddNode(n)
	}
	if err := nrows.Err(); err != nil {
		nrows.Close()
		return nil, err
	}
	nrows.Close()
	if afterNodes != nil {
		afterNodes()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	trace.Phase("edges")
	erows, err := q.QueryContext(ctx, `SELECT source, target, relation, confidence, confidence_score, weight, COALESCE(source_loc,'') FROM edges`)
	if err != nil {
		return nil, err
	}
	defer erows.Close()
	for erows.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var e graph.Edge
		if err := erows.Scan(&e.Source, &e.Target, &e.Relation, &e.Confidence, &e.ConfidenceScore, &e.Weight, &e.SourceLoc); err != nil {
			return nil, err
		}
		g.AddEdge(e)
	}
	// Match BuildGraph: recompute degrees in a final pass so both paths agree exactly.
	trace.Phase("degrees")
	if err := g.RecomputeDegreesContext(ctx); err != nil {
		return nil, err
	}
	return g, erows.Err()
}

// LoadGraphAtNoteVersion loads one SQLite snapshot only when it contains the expected
// note version. The version check is the transaction's first read, so the graph queries
// see that same committed snapshot even if the owner removes or replaces the row while
// the load is in progress. This is the publication primitive used by read-only MCP
// write-back receipts: success means the graph actually swapped into memory contains
// the bytes whose hash was acknowledged.
func (s *Store) LoadGraphAtNoteVersion(noteID, expectedPath, expectedHash string) (*graph.Graph, bool, error) {
	return s.loadGraphAtNoteVersion(noteID, expectedPath, expectedHash, nil)
}

// LoadGraphAtNoteVersionContext keeps the version gate and graph on one snapshot,
// with the caller's deadline covering connection acquisition and every SQL phase.
func (s *Store) LoadGraphAtNoteVersionContext(ctx context.Context, noteID, expectedPath, expectedHash string) (*graph.Graph, bool, error) {
	return s.loadGraphAtNoteVersionContext(ctx, noteID, expectedPath, expectedHash, nil)
}

// NoteVersionMatches reports whether the current committed notes row still names the
// expected path and retrieval version. Keep path and hash in one query: two independent
// getters could straddle an owner commit and manufacture a version that never existed.
func (s *Store) NoteVersionMatches(noteID, expectedPath, expectedHash string) (bool, error) {
	return s.NoteVersionMatchesContext(context.Background(), noteID, expectedPath, expectedHash)
}

// NoteVersionMatchesContext is the cancelable final publication check.
func (s *Store) NoteVersionMatchesContext(ctx context.Context, noteID, expectedPath, expectedHash string) (bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var path, hash string
	err := s.readDB.QueryRowContext(ctx, `SELECT path, retrieval_hash FROM notes WHERE id = ?`, noteID).Scan(&path, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return filepath.Clean(path) == filepath.Clean(expectedPath) && hash == expectedHash, nil
}

func (s *Store) loadGraphAtNoteVersion(noteID, expectedPath, expectedHash string, afterVersionCheck func()) (*graph.Graph, bool, error) {
	return s.loadGraphAtNoteVersionContext(context.Background(), noteID, expectedPath, expectedHash, afterVersionCheck)
}

func (s *Store) loadGraphAtNoteVersionContext(ctx context.Context, noteID, expectedPath, expectedHash string, afterVersionCheck func()) (*graph.Graph, bool, error) {
	trace := latency.Start("load_versioned_graph", "begin_read")
	defer trace.End()
	if ctx == nil {
		ctx = context.Background()
	}
	tx, err := s.readDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	trace.Phase("version_check")
	var path, hash string
	err = tx.QueryRowContext(ctx, `SELECT path, retrieval_hash FROM notes WHERE id = ?`, noteID).Scan(&path, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if filepath.Clean(path) != filepath.Clean(expectedPath) || hash != expectedHash {
		return nil, false, nil
	}
	if afterVersionCheck != nil {
		afterVersionCheck()
	}
	trace.Phase("graph")
	g, err := loadGraphContext(ctx, tx)
	if err != nil {
		return nil, false, err
	}
	trace.Phase("commit_read")
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return g, true, nil
}
