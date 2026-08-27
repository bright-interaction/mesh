// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"

	"github.com/bright-interaction/mesh/internal/graph"
)

// LoadGraph reconstructs the in-memory graph from the persisted nodes + edges
// tables. The CLI uses this for retrieval without re-parsing the vault; the
// long-running daemon (MCP) keeps the graph in memory instead.
func (s *Store) LoadGraph() (*graph.Graph, error) {
	return loadGraph(s.readDB)
}

type graphQueryer interface {
	Query(string, ...any) (*sql.Rows, error)
}

func loadGraph(q graphQueryer) (*graph.Graph, error) {
	g := graph.New()

	nrows, err := q.Query(`SELECT id, kind, label, COALESCE(note_id,''), COALESCE(note_path,''), COALESCE(anchor,''), COALESCE(source_loc,''), COALESCE(community,0), COALESCE(attrs,'') FROM nodes`)
	if err != nil {
		return nil, err
	}
	// A leaked *sql.Rows pins a WAL read snapshot for the life of the process, which
	// stops every checkpoint from reclaiming past it. In a long-running daemon that
	// grows the WAL without bound and starves other processes' writes into SQLITE_BUSY.
	defer nrows.Close()
	for nrows.Next() {
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

	erows, err := q.Query(`SELECT source, target, relation, confidence, confidence_score, weight, COALESCE(source_loc,'') FROM edges`)
	if err != nil {
		return nil, err
	}
	defer erows.Close()
	for erows.Next() {
		var e graph.Edge
		if err := erows.Scan(&e.Source, &e.Target, &e.Relation, &e.Confidence, &e.ConfidenceScore, &e.Weight, &e.SourceLoc); err != nil {
			return nil, err
		}
		g.AddEdge(e)
	}
	// Match BuildGraph: recompute degrees in a final pass so both paths agree exactly.
	g.RecomputeDegrees()
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

// NoteVersionMatches reports whether the current committed notes row still names the
// expected path and retrieval version. Keep path and hash in one query: two independent
// getters could straddle an owner commit and manufacture a version that never existed.
func (s *Store) NoteVersionMatches(noteID, expectedPath, expectedHash string) (bool, error) {
	var path, hash string
	err := s.readDB.QueryRow(`SELECT path, retrieval_hash FROM notes WHERE id = ?`, noteID).Scan(&path, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return filepath.Clean(path) == filepath.Clean(expectedPath) && hash == expectedHash, nil
}

func (s *Store) loadGraphAtNoteVersion(noteID, expectedPath, expectedHash string, afterVersionCheck func()) (*graph.Graph, bool, error) {
	tx, err := s.readDB.Begin()
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	var path, hash string
	err = tx.QueryRow(`SELECT path, retrieval_hash FROM notes WHERE id = ?`, noteID).Scan(&path, &hash)
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
	g, err := loadGraph(tx)
	if err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return g, true, nil
}
