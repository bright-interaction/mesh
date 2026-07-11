// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/bright-interaction/mesh/internal/graph"
	"github.com/bright-interaction/mesh/internal/vault"
)

// ReindexFull walks the vault, parses every note, builds the graph + communities,
// persists everything, and returns BOTH the in-memory graph and the parsed notes
// (so a long-running caller can seed its NoteCache without a second parse). It does
// NOT re-read the graph from the DB: the returned graph is the one just built, so a
// caller that already holds the graph in memory skips the LoadGraph round-trip.
func ReindexFull(s *Store, root string) (*graph.Graph, []*ParsedNote, error) {
	files, err := vault.Walk(root)
	if err != nil {
		return nil, nil, err
	}
	notes, ferrs := ParseFiles(files, 0)
	s.recordDropped(root, ferrs)
	for _, pn := range notes {
		if rel, err := filepath.Rel(root, pn.Path); err == nil {
			pn.Path = rel
		}
	}
	g, _ := BuildGraph(notes)
	g.DetectCommunities(0)
	if _, err := s.IndexVault(notes, g); err != nil {
		return nil, nil, err
	}
	return g, notes, nil
}

// recordDropped remembers and logs the notes ParseFiles could not parse (invalid
// YAML frontmatter is the usual cause) so a silently dropped note is visible in the
// logs on EVERY reindex, not only when someone runs `mesh structure`. A broken
// frontmatter block otherwise removes a note from search and the graph with zero
// signal, which hid three real notes for weeks. Paths are made vault-relative.
func (s *Store) recordDropped(root string, ferrs []FileError) {
	rel := make([]FileError, 0, len(ferrs))
	for _, fe := range ferrs {
		p := fe.Path
		if r, err := filepath.Rel(root, p); err == nil {
			p = r
		}
		rel = append(rel, FileError{Path: p, Err: fe.Err})
		slog.Warn("mesh: dropping unparseable note; it is invisible to search and the graph until the frontmatter is fixed",
			"path", p, "err", fe.Err)
	}
	s.mu.Lock()
	s.dropped = rel
	s.mu.Unlock()
	if len(ferrs) > 0 {
		slog.Warn("mesh reindex dropped unparseable notes", "count", len(ferrs), "root", root)
	}
}

// DroppedNotes returns the notes the last full reindex dropped as unparseable
// (empty when the whole vault parsed cleanly). Feeds health/status surfaces so an
// operator can find a note that vanished from the index.
func (s *Store) DroppedNotes() []FileError {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]FileError, len(s.dropped))
	copy(out, s.dropped)
	return out
}

// Reindex walks the vault, parses it, builds the graph + communities, persists
// everything, and returns the freshly DB-loaded in-memory graph. Used by the CLI
// index path; long-running watchers use ReindexFull + ReconcileIncremental instead.
func Reindex(s *Store, root string) (*graph.Graph, error) {
	if _, _, err := ReindexFull(s, root); err != nil {
		return nil, err
	}
	return s.LoadGraph()
}

// NoteRef is a lightweight note descriptor for delta/listing queries.
type NoteRef struct {
	ID    string `json:"id"`
	Path  string `json:"path"`
	Type  string `json:"type"`
	Mtime int64  `json:"mtime"`
}

// ChangedSince returns notes whose file mtime is newer than the given unix
// timestamp, newest first. Lets an agent resuming a session pull only deltas.
func (s *Store) ChangedSince(since int64) ([]NoteRef, error) {
	rows, err := s.readDB.Query(`SELECT id, path, type, mtime FROM notes WHERE mtime > ? ORDER BY mtime DESC`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NoteRef
	for rows.Next() {
		var n NoteRef
		if err := rows.Scan(&n.ID, &n.Path, &n.Type, &n.Mtime); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// NotePath resolves a note id to its vault-relative path (read pool).
func (s *Store) NotePath(id string) (string, error) {
	var p string
	err := s.readDB.QueryRow(`SELECT path FROM notes WHERE id = ?`, id).Scan(&p)
	return p, err
}

// NoteScope returns a note's access scope(s) by id. Used to scope-check a direct
// fetch (which resolves id -> path -> file, bypassing the retriever's card filter).
// A missing scope falls back to the fail-safe default (dev-only).
func (s *Store) NoteScope(id string) ([]string, error) {
	var sc string
	err := s.readDB.QueryRow(`SELECT scope FROM notes WHERE id = ?`, id).Scan(&sc)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, p := range strings.Split(sc, ",") {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		out = []string{"dev"}
	}
	return out, nil
}
