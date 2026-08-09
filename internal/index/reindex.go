// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"database/sql"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

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
	// Refresh the note<->code bridge. note_code_links was written ONLY by the code-index
	// commands, never by a note reindex, so a note written after startup never linked to
	// any symbol and a deleted note left its rows behind. Best-effort and cheap when
	// there is no code index: LinkNotesToCode returns immediately if code_symbols is
	// empty, so a vault with the code index disabled pays nothing.
	_, _ = s.LinkNotesToCode(root)
	return g, notes, nil
}

// recordDropped remembers and logs the notes an index pass could not index (invalid
// YAML frontmatter is the usual cause; a duplicate effective id is the other) so a
// silently dropped note is visible in the logs, not only when someone runs
// `mesh structure`. A broken frontmatter block otherwise removes a note from search and
// the graph with zero signal, which hid three real notes for weeks. Paths are made
// vault-relative.
//
// The recorded set is REPLACED, not merged: every pass (full or incremental) walks the
// whole vault, and an unindexed file is by definition absent from the notes table, so it
// can never be skipped by the incremental mtime fast path (which only applies to files
// already in the index). Only entries that are NEW since the last record are logged,
// because the watcher calls this on every reconcile tick and re-warning about the same
// broken file forever would drown the log the warning exists to be seen in.
func (s *Store) recordDropped(root string, ferrs []FileError) {
	rel := make([]FileError, 0, len(ferrs))
	for _, fe := range ferrs {
		p := fe.Path
		if r, err := filepath.Rel(root, p); err == nil {
			p = r
		}
		rel = append(rel, FileError{Path: p, Err: fe.Err})
	}

	s.mu.Lock()
	prev := make(map[string]string, len(s.dropped))
	for _, fe := range s.dropped {
		prev[fe.Path] = errText(fe.Err)
	}
	s.dropped = rel
	firstPass := !s.droppedSynced
	s.droppedSynced = true
	s.mu.Unlock()

	fresh := 0
	for _, fe := range rel {
		if was, seen := prev[fe.Path]; seen && was == errText(fe.Err) {
			continue
		}
		fresh++
		slog.Warn("mesh: dropping note; it is invisible to search and the graph until it is fixed",
			"path", fe.Path, "err", fe.Err)
	}
	if fresh > 0 {
		slog.Warn("mesh reindex dropped notes", "new", fresh, "total", len(rel), "root", root)
	}
	// A shorter set with no new entries (someone fixed a note) is a change too, and it
	// must reach the table or the fixed note stays flagged forever.
	if firstPass || fresh > 0 || len(rel) != len(prev) {
		s.persistDropped(rel)
	}
}

// persistDropped mirrors the dropped set into the index so a reader in ANOTHER process
// can surface it: every `mesh mcp` window is read-only now, so its own memory of what an
// index pass dropped is permanently empty. Called only when the set actually changed (and
// once per store on the first pass, since the table may hold a previous run's rows), so a
// watcher sitting on a clean vault does not add a WAL frame every periodic tick.
//
// Best effort: a note that cannot be indexed is already the degraded case, and failing a
// reindex over the bookkeeping about it would be a strictly worse outcome.
func (s *Store) persistDropped(rel []FileError) {
	if s.readOnly {
		return // the owning writer records these; a read-only store only reads them back
	}
	at := time.Now().Unix()
	err := s.Write(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM dropped_notes`); err != nil {
			return err
		}
		ins, err := tx.Prepare(`INSERT OR REPLACE INTO dropped_notes(path,err,duplicate,detected_at) VALUES(?,?,?,?)`)
		if err != nil {
			return err
		}
		defer ins.Close()
		for _, fe := range rel {
			dup := 0
			if errors.Is(fe.Err, ErrDuplicateNoteID) {
				dup = 1
			}
			if _, err := ins.Exec(fe.Path, errText(fe.Err), dup, at); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		slog.Warn("mesh: could not record dropped notes in the index", "err", err, "dropped", len(rel))
	}
}

// persistedDropErr rehydrates a dropped-note error read back out of the index. The
// message is the original one; Unwrap restores the ErrDuplicateNoteID sentinel so
// errors.Is keeps classifying a quarantined duplicate AFTER the finding crossed a process
// boundary. mesh_health branches on exactly that to tell "fix the frontmatter" from "two
// notes claim one id", and the two have different remedies.
type persistedDropErr struct {
	msg string
	dup bool
}

func (e *persistedDropErr) Error() string { return e.msg }

func (e *persistedDropErr) Unwrap() error {
	if e.dup {
		return ErrDuplicateNoteID
	}
	return nil
}

// droppedFromIndex reads the dropped set the owning writer recorded. A store opened
// against an index an older mesh wrote has no such table; that is "nothing known", not an
// error worth failing a health call over.
func (s *Store) droppedFromIndex() []FileError {
	rows, err := s.readDB.Query(`SELECT path, err, duplicate FROM dropped_notes ORDER BY path`)
	if err != nil {
		if !strings.Contains(err.Error(), "no such table") {
			slog.Warn("mesh: could not read dropped notes from the index", "err", err)
		}
		return nil
	}
	defer rows.Close()
	var out []FileError
	for rows.Next() {
		var path, msg string
		var dup int
		if err := rows.Scan(&path, &msg, &dup); err != nil {
			return out
		}
		fe := FileError{Path: path}
		if msg != "" || dup != 0 {
			fe.Err = &persistedDropErr{msg: msg, dup: dup != 0}
		}
		out = append(out, fe)
	}
	return out
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// DroppedNotes returns the notes the last index pass dropped as unparseable or
// quarantined (empty when the whole vault parsed cleanly). Feeds health/status surfaces
// so an operator can find a note that vanished from the index.
//
// A read-only store never runs a pass, so it has nothing of its own to report and reads
// what the owning writer recorded instead. Without that, mesh_health in every `mesh mcp`
// window would report a clean vault no matter how many notes were missing from it.
func (s *Store) DroppedNotes() []FileError {
	if s.readOnly {
		return s.droppedFromIndex()
	}
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
