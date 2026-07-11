// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Store is the SQLite index. Concurrency model (spec section 3.2): the database
// is opened in WAL mode with two pools. All writes funnel through a single
// writer goroutine over a channel, so there is never a second writer and no
// "database is locked" contention. Reads use a separate pool, which WAL serves
// concurrently with the writer. This is the foundation the fsnotify watcher
// (later) needs: it can stream upserts while mesh_search reads, with no deadlock.
type Store struct {
	dir     string
	dbPath  string
	writeDB *sql.DB
	readDB  *sql.DB
	jobs    chan job
	done    chan struct{}
	wg      sync.WaitGroup // tracks the writer goroutine so Close can join it

	mu      sync.Mutex  // guards dropped
	dropped []FileError // notes dropped as unparseable by the last full reindex
}

// SchemaVersion bumps whenever schema.sql changes shape. The index is a derived,
// deletable artifact (the markdown vault is the source of truth), so a version
// mismatch drops and rebuilds rather than running a migration. This is why Mesh
// uses no goose/golang-migrate: there is no irreplaceable data to migrate.
// Note: the source-code tables (code_files/code_symbols/code_edges/code_search)
// were added additively via CREATE TABLE IF NOT EXISTS, so they appear on existing
// databases without a destructive rebuild and the version stays 2. Bump this only
// for a shape change to an existing table, which requires the drop+rebuild below.
// v3: notes gained review_by + source columns (provenance / lifecycle, Phase A).
// v4: notes gained a scope column (access-control partition; absent = dev).
const SchemaVersion = 4

type job struct {
	fn    func(*sql.Tx) error
	reply chan error
}

const (
	// walSizeLimit caps mesh.db-wal on disk. SQLite's WAL autocheckpoint is PASSIVE:
	// it resets the write pointer but NEVER shrinks the file, and nothing here ever
	// issued a TRUNCATE, so the WAL only grew to its high-water mark and stayed there
	// (observed 223MB, which starved writers into SQLITE_BUSY). journal_size_limit
	// makes every checkpoint truncate the WAL back to at most this size. 16MB holds
	// the largest single reindex transaction's frames without thrashing.
	walSizeLimit = 16 * 1024 * 1024
	// walCheckpointInterval is how often the single writer goroutine runs a PASSIVE
	// checkpoint, so a WAL left behind by an idle stretch still gets checkpointed and
	// capped by journal_size_limit (autocheckpoint only fires on writes). PASSIVE never
	// blocks the writer on a reader; TRUNCATE-to-zero is the out-of-process mesh-doctor's job.
	walCheckpointInterval = 2 * time.Minute
)

func dsn(path string) string {
	return fmt.Sprintf(
		"file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(30000)&_pragma=foreign_keys(on)&_pragma=journal_size_limit(%d)",
		path, walSizeLimit,
	)
}

// Open creates (or opens) <vaultRoot>/.mesh/mesh.db, applies the schema, and
// starts the writer goroutine.
func Open(vaultRoot string) (*Store, error) {
	return OpenAt(vaultRoot, filepath.Join(vaultRoot, ".mesh"))
}

// OpenAt is like Open but stores the index in an explicit directory instead of
// <vaultRoot>/.mesh. The hub uses this to index its served vault into a dir OUTSIDE
// the git repo, so the index is never synced to clients.
func OpenAt(vaultRoot, meshDir string) (*Store, error) {
	if err := os.MkdirAll(meshDir, 0o755); err != nil {
		return nil, err
	}
	dbPath := filepath.Join(meshDir, "mesh.db")

	writeDB, err := sql.Open("sqlite", dsn(dbPath))
	if err != nil {
		return nil, err
	}
	writeDB.SetMaxOpenConns(1) // the single write connection

	readDB, err := sql.Open("sqlite", dsn(dbPath))
	if err != nil {
		writeDB.Close()
		return nil, err
	}

	if err := ensureSchema(writeDB); err != nil {
		writeDB.Close()
		readDB.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}

	s := &Store{
		dir:     meshDir,
		dbPath:  dbPath,
		writeDB: writeDB,
		readDB:  readDB,
		jobs:    make(chan job),
		done:    make(chan struct{}),
	}
	s.wg.Add(1)
	go s.writer()
	return s, nil
}

// dropOnVersionChange lists the tables wiped and rebuilt on a schema-version change.
// It must be every table in schema.sql EXCEPT those in schemaKeep. A test asserts it
// stays in sync with schema.sql so a newly-added table cannot silently leak stale
// rows (or an orphaned renamed table) on a version bump.
var dropOnVersionChange = []string{"notes", "nodes", "edges", "search_index", "corpus_stats", "meta", "code_files", "code_symbols", "code_edges", "code_search", "note_health", "note_code_links"}

// schemaKeep are tables deliberately preserved across a schema-version rebuild:
//   - metrics: accumulated usage counters, NOT re-derivable from the vault.
//   - vectors: BYOAI embeddings + the content-hash embed cache. These ARE derivable
//     but only by RE-EMBEDDING every chunk (a paid API call), and reindex does not
//     re-embed; they stay keyed by the same note ids and the note_hash staleness
//     check excludes any whose content changed. So a notes-shape bump must not wipe
//     them. (If the vectors table's OWN shape ever changes, drop it for that release.)
//   - note_reuse: the flywheel measurement (authoring time + observed reuse events),
//     accumulated at runtime and NOT re-derivable from the vault.
//   - pending_notes: auto-extracted write-back candidates awaiting review, not yet in
//     the vault, so they would be lost on a rebuild if dropped.
var schemaKeep = map[string]bool{"metrics": true, "vectors": true, "note_reuse": true, "pending_notes": true}

// keepShapeVersion tracks the COLUMN SHAPE of the schemaKeep tables. ensureSchema runs
// schema.sql with CREATE TABLE IF NOT EXISTS, which is a no-op on an existing table, so
// adding a column to a kept table in schema.sql would NOT apply on a live hub DB and the
// change would silently do nothing. TestKeptTableShapeGuard fingerprints each kept
// table's DDL against a baked-in value; if you change a kept table's columns you must
// bump this version (which makes that release drop+rebuild the kept tables, accepting
// the one-time data loss) or otherwise migrate the live rows, and update the guard.
const keepShapeVersion = 1

// ensureSchema applies the schema, dropping and rebuilding if the stored version
// differs. No data is lost that matters: everything is re-derivable from the
// markdown vault, so this replaces a migration tool.
func ensureSchema(db *sql.DB) error {
	var current int
	// meta may not exist yet; ignore the scan error in that case.
	_ = db.QueryRow(`SELECT CAST(value AS INTEGER) FROM meta WHERE key='schema_version'`).Scan(&current)
	if current != 0 && current != SchemaVersion {
		for _, t := range dropOnVersionChange {
			if _, err := db.Exec("DROP TABLE IF EXISTS " + t); err != nil {
				return err
			}
		}
	}
	if _, err := db.Exec(SchemaSQL); err != nil {
		return err
	}
	_, err := db.Exec(
		`INSERT INTO meta(key,value) VALUES('schema_version',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		fmt.Sprint(SchemaVersion),
	)
	return err
}

func (s *Store) Path() string { return s.dbPath }

// MeshDir returns the vault's .mesh directory (where mesh.db and the solo
// config.toml live).
func (s *Store) MeshDir() string { return s.dir }

// NoteDate carries the lifecycle dates retrieval needs for freshness decay.
type NoteDate struct {
	Updated  string // frontmatter updated/when (YYYY-MM-DD)
	ReviewBy string // frontmatter review_by (YYYY-MM-DD), if any
}

// NoteDates returns id -> lifecycle dates for every note, for freshness decay.
func (s *Store) NoteDates() (map[string]NoteDate, error) {
	rows, err := s.readDB.Query(`SELECT id, COALESCE(updated,''), COALESCE(review_by,'') FROM notes`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]NoteDate{}
	for rows.Next() {
		var id, upd, rev string
		if err := rows.Scan(&id, &upd, &rev); err != nil {
			return nil, err
		}
		out[id] = NoteDate{Updated: upd, ReviewBy: rev}
	}
	return out, rows.Err()
}

func (s *Store) writer() {
	defer s.wg.Done()
	// Periodically checkpoint the WAL from inside the single writer so mesh.db-wal
	// cannot grow without bound even across idle stretches (autocheckpoint only fires
	// on writes). PASSIVE, not TRUNCATE: PASSIVE returns immediately and never blocks
	// the writer on a live reader, while journal_size_limit (DSN) still truncates the
	// file to <=16MB after the checkpoint. Running it on this goroutine means it never
	// races a write: the select serves one job OR one checkpoint per iteration.
	ticker := time.NewTicker(walCheckpointInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case j := <-s.jobs:
			j.reply <- s.runTx(j.fn)
		case <-ticker.C:
			// Best-effort and non-blocking; journal_size_limit caps the file. Full
			// zeroing (TRUNCATE) + reaping stale readers is the hourly mesh-doctor's job.
			_, _ = s.writeDB.Exec("PRAGMA wal_checkpoint(PASSIVE)")
		}
	}
}

func (s *Store) runTx(fn func(*sql.Tx) error) (err error) {
	tx, err := s.writeDB.Begin()
	if err != nil {
		return err
	}
	defer func() {
		// A panic in fn (a nil deref on malformed parsed data, a panic inside Exec)
		// must never kill the single writer goroutine: a dead writer would leave every
		// future Write blocked forever on the jobs channel. Recover it into an error so
		// the writer keeps serving and the caller is told what happened. This mirrors
		// the hub Store, which already guards this.
		if r := recover(); r != nil {
			_ = tx.Rollback()
			err = fmt.Errorf("index write panicked: %v", r)
			return
		}
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// Write runs fn inside a transaction on the single writer goroutine.
func (s *Store) Write(fn func(*sql.Tx) error) error {
	reply := make(chan error, 1)
	select {
	case s.jobs <- job{fn: fn, reply: reply}:
		return <-reply
	case <-s.done:
		return fmt.Errorf("store is closed")
	}
}

// Count returns the row count of a table (read pool). The table name is a fixed
// internal identifier, never user input.
func (s *Store) Count(table string) (int, error) {
	var n int
	err := s.readDB.QueryRow("SELECT count(*) FROM " + table).Scan(&n)
	return n, err
}

// Close stops the writer goroutine and closes both pools. It waits for the writer to
// drain any in-flight transaction before closing the pools, so a write racing
// shutdown completes cleanly instead of hitting a closed DB.
func (s *Store) Close() error {
	close(s.done)
	s.wg.Wait()
	// The writer has drained, so a clean shutdown is the safe moment to TRUNCATE the WAL
	// back to zero: journal_size_limit only caps mesh.db-wal to 16MB, so without this a
	// cleanly-restarted process inherits (and re-grows from) a 16MB high-water mark. This
	// is the in-process, restart-time complement to the hourly mesh-doctor's out-of-process
	// TRUNCATE (which reaps stale readers of processes that never Close). Bounded to a few
	// seconds so a reader in another `mesh mcp --watch` process can never stall shutdown.
	s.checkpointTruncateBestEffort()
	errW := s.writeDB.Close()
	errR := s.readDB.Close()
	if errW != nil {
		return errW
	}
	return errR
}

// checkpointTruncateBestEffort runs a single TRUNCATE checkpoint on shutdown to reclaim
// the WAL file. Best-effort and tightly bounded: a short busy_timeout plus a context
// deadline mean that if another process holds a read lock it gives up quickly rather than
// blocking Close for the DSN's 30s busy_timeout. Full durability across never-closing
// multi-writer processes remains the deferred read-only-MCP refactor (see the mesh WAL
// decision note); this only covers the clean-restart path.
func (s *Store) checkpointTruncateBestEffort() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := s.writeDB.Conn(ctx)
	if err != nil {
		return
	}
	defer conn.Close()
	_, _ = conn.ExecContext(ctx, "PRAGMA busy_timeout=2000")
	_, _ = conn.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
}
