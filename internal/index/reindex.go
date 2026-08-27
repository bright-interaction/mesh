// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
	return ReindexFullContext(context.Background(), s, root)
}

// ReindexFullContext is ReindexFull with cooperative cancellation across every
// expensive phase. Cancellation before the single persist transaction commits leaves
// the previously committed index authoritative.
func ReindexFullContext(ctx context.Context, s *Store, root string) (*graph.Graph, []*ParsedNote, error) {
	return reindexFullContext(ctx, s, root, nil)
}

// reindexFullContext's onPersisted hook marks the one-way publication boundary. It is
// nil in production and lets tests cancel at that exact boundary without racing a
// polling goroutine against the best-effort post-commit work.
func reindexFullContext(ctx context.Context, s *Store, root string, onPersisted func()) (*graph.Graph, []*ParsedNote, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	files, err := vault.WalkContext(ctx, root)
	if err != nil {
		return nil, nil, err
	}
	notes, ferrs, err := ParseFilesContext(ctx, files, 0)
	if err != nil {
		return nil, nil, err
	}
	for _, pn := range notes {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if rel, err := filepath.Rel(root, pn.Path); err == nil {
			pn.Path = rel
		}
	}
	// Quarantine duplicate ids BEFORE the graph and the notes table are written, and
	// record the drop. Without this the full path let both files through and the two
	// stores disagreed about which one the id meant: INSERT OR REPLACE gave notes.path to
	// the last file walked, AddNode gave nodes.note_path to the first, so a search card
	// paired one file's path with the other file's snippet. The loser also had no notes
	// row, which every later DriftReport read as Added, so `mesh doctor` printed STALE
	// forever over an index that was already byte-identical to a fresh one.
	incumbent, ierr := s.IDOwnersContext(ctx)
	if ierr != nil {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		// A cold or unreadable index is not a reason to fail the pass: with no incumbent
		// the tie falls back to walk order, which is the same rule a first index uses.
		incumbent = nil
	}
	notes, dups := ClaimUniqueIDs(notes, incumbent)
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	dropped, err := relativeDroppedContext(ctx, root, append(ferrs, dups...))
	if err != nil {
		return nil, nil, err
	}
	g, _, err := BuildGraphContext(ctx, notes)
	if err != nil {
		return nil, nil, err
	}
	if _, err := g.DetectCommunitiesContext(ctx, 0); err != nil {
		return nil, nil, err
	}
	if _, err := s.indexVaultContext(ctx, notes, g, dropped, true); err != nil {
		return nil, nil, err
	}
	if onPersisted != nil {
		onPersisted()
	}
	// The durable dropped set committed atomically with notes/graph above. Publish the
	// same set in memory only after that commit, so cancellation can never pair an old
	// note snapshot with a freshly-cleared dropped_notes table (or vice versa).
	s.publishDropped(root, dropped)
	// Refresh the note<->code bridge. note_code_links was written ONLY by the code-index
	// commands, never by a note reindex, so a note written after startup never linked to
	// any symbol and a deleted note left its rows behind. Best-effort and cheap when
	// there is no code index: LinkNotesToCode returns immediately if code_symbols is
	// empty, so a vault with the code index disabled pays nothing.
	if ctx.Err() == nil {
		_, _ = s.LinkNotesToCodeContext(ctx, root)
	}
	// Persist is the point of no return: once the new snapshot commits, the caller must
	// receive its graph even if its context is canceled during the best-effort bridge
	// refresh. Returning a cancellation here would leave a long-running caller serving
	// the old in-memory graph over the newly committed database indefinitely.
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
// RecordDropped is recordDropped for a caller outside this package that runs its own
// pass (`mesh index` parses, dedupes and calls IndexVault itself so it can print stats
// and support --dry-run). Without it that command wrote a correct index and told nobody
// what it had to leave out: dropped_notes stayed empty, so `mesh health` and mesh_health
// reported a clean vault right after a CLI index had quarantined a note.
func (s *Store) RecordDropped(root string, ferrs []FileError) { s.recordDropped(root, ferrs) }

func (s *Store) recordDropped(root string, ferrs []FileError) {
	_ = s.recordDroppedContext(context.Background(), root, ferrs)
}

func (s *Store) recordDroppedContext(ctx context.Context, root string, ferrs []FileError) error {
	if ctx == nil {
		ctx = context.Background()
	}
	rel, err := relativeDroppedContext(ctx, root, ferrs)
	if err != nil {
		return err
	}
	if s.publishDropped(root, rel) {
		if err := s.persistDroppedContext(ctx, rel); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func relativeDroppedContext(ctx context.Context, root string, ferrs []FileError) ([]FileError, error) {
	rel := make([]FileError, 0, len(ferrs))
	for _, fe := range ferrs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		p := fe.Path
		// Relativize only a path that really is under root. Callers hand this a MIX:
		// parse failures carry the walked path (usually absolute) while a duplicate-id
		// quarantine already carries the vault-relative one. Against a relative root
		// ("sub"), Rel happily turns the already-relative "x.md" into "../x.md", which
		// names no file at all and would send the operator hunting outside the vault.
		if r, err := filepath.Rel(root, p); err == nil && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator)) {
			p = r
		}
		rel = append(rel, FileError{Path: p, Err: fe.Err})
	}
	return rel, ctx.Err()
}

// publishDropped updates the writable Store's in-memory view and emits new findings.
// It returns whether a caller that has not already persisted rel must replace the
// dropped_notes table.
func (s *Store) publishDropped(root string, rel []FileError) bool {
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
	return firstPass || fresh > 0 || len(rel) != len(prev)
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
	_ = s.persistDroppedContext(context.Background(), rel)
}

func (s *Store) persistDroppedContext(ctx context.Context, rel []FileError) error {
	if s.ReadOnly() {
		return nil // the owning writer records these; a read-only store only reads them back
	}
	at := time.Now().Unix()
	err := s.WriteContext(ctx, func(tx *sql.Tx) error {
		return writeDroppedRowsContext(ctx, tx, rel, at)
	})
	if err != nil {
		slog.Warn("mesh: could not record dropped notes in the index", "err", err, "dropped", len(rel))
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
	}
	return nil
}

func writeDroppedRowsContext(ctx context.Context, tx *sql.Tx, rel []FileError, at int64) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM dropped_notes`); err != nil {
		return err
	}
	ins, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO dropped_notes(path,err,duplicate,detected_at) VALUES(?,?,?,?)`)
	if err != nil {
		return err
	}
	defer ins.Close()
	for _, fe := range rel {
		if err := ctx.Err(); err != nil {
			return err
		}
		dup := 0
		if errors.Is(fe.Err, ErrDuplicateNoteID) {
			dup = 1
		}
		if _, err := ins.ExecContext(ctx, fe.Path, errText(fe.Err), dup, at); err != nil {
			return err
		}
	}
	return nil
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

// droppedFromIndex reads the dropped set the owning writer recorded. Any failure is
// REPORTED, including a missing dropped_notes table. Swallowing that (it used to return
// nil for "no such table", reasoning that an older index simply knows nothing) is exactly
// how an unmigrated index made mesh_health answer "clean vault" while every quarantined
// note stayed invisible. OpenReadOnlyAt now refuses a mismatched schema outright, so a
// missing table here means something dropped it underneath a live store: still an error,
// never an empty answer that reads as good news.
func (s *Store) droppedFromIndex() ([]FileError, error) {
	return s.droppedFromIndexContext(context.Background())
}

func (s *Store) droppedFromIndexContext(ctx context.Context) ([]FileError, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := s.readDB.QueryContext(ctx, `SELECT path, err, duplicate FROM dropped_notes ORDER BY path`)
	if err != nil {
		slog.Warn("mesh: could not read dropped notes from the index", "err", err)
		return nil, fmt.Errorf("read dropped notes from the index at %s: %w\n"+
			"  the index does not match this Mesh binary; rebuild it: mesh index <vault>", s.dbPath, err)
	}
	defer rows.Close()
	var out []FileError
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var path, msg string
		var dup int
		if err := rows.Scan(&path, &msg, &dup); err != nil {
			return out, fmt.Errorf("read dropped notes from the index at %s: %w", s.dbPath, err)
		}
		fe := FileError{Path: path}
		if msg != "" || dup != 0 {
			fe.Err = &persistedDropErr{msg: msg, dup: dup != 0}
		}
		out = append(out, fe)
	}
	return out, rows.Err()
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
//
// It returns an error rather than an empty slice when that read fails, because "no
// dropped notes" and "could not find out" are opposite answers and only one of them is
// good news. Every caller must surface the failure; a writable store never errors.
func (s *Store) DroppedNotes() ([]FileError, error) {
	return s.DroppedNotesContext(context.Background())
}

// DroppedNotesContext is DroppedNotes with caller-controlled cancellation for the
// cross-process read used by read-only stores.
func (s *Store) DroppedNotesContext(ctx context.Context) ([]FileError, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.ReadOnly() {
		return s.droppedFromIndexContext(ctx)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]FileError, len(s.dropped))
	copy(out, s.dropped)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// Reindex walks the vault, parses it, builds the graph + communities, persists
// everything, and returns the freshly DB-loaded in-memory graph. Used by the CLI
// index path; long-running watchers use ReindexFull + ReconcileIncremental instead.
func Reindex(s *Store, root string) (*graph.Graph, error) {
	return ReindexContext(context.Background(), s, root)
}

// ReindexContext is Reindex with a caller-owned lifetime.
func ReindexContext(ctx context.Context, s *Store, root string) (*graph.Graph, error) {
	return reindexContext(ctx, s, root, nil)
}

func reindexContext(ctx context.Context, s *Store, root string, onPersisted func()) (*graph.Graph, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	g, _, err := reindexFullContext(ctx, s, root, onPersisted)
	if err != nil {
		return nil, err
	}
	loaded, err := s.LoadGraphContext(ctx)
	if err != nil {
		// ReindexFullContext has committed at this point. If the caller disappeared
		// during the DB reload, publish the equivalent graph already built for that
		// commit instead of reporting cancellation and stranding the live graph.
		if ctx.Err() != nil {
			return g, nil
		}
		return nil, err
	}
	return loaded, nil
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
	return s.NotePathContext(context.Background(), id)
}

// NotePathContext is NotePath with caller-controlled cancellation.
func (s *Store) NotePathContext(ctx context.Context, id string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var p string
	err := s.readDB.QueryRowContext(ctx, `SELECT path FROM notes WHERE id = ?`, id).Scan(&p)
	return p, err
}

// NoteScope returns a note's access scope(s) by id. Used to scope-check a direct
// fetch (which resolves id -> path -> file, bypassing the retriever's card filter).
// A missing scope falls back to the fail-safe default (dev-only).
func (s *Store) NoteScope(id string) ([]string, error) {
	return s.NoteScopeContext(context.Background(), id)
}

// NoteScopeContext is NoteScope with caller-controlled cancellation.
func (s *Store) NoteScopeContext(ctx context.Context, id string) ([]string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var sc string
	err := s.readDB.QueryRowContext(ctx, `SELECT scope FROM notes WHERE id = ?`, id).Scan(&sc)
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
