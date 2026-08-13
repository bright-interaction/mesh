// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bright-interaction/mesh/internal/shellpath"

	_ "modernc.org/sqlite"
)

// Store is the SQLite index. Concurrency model (spec section 3.2): the database
// is opened in WAL mode with two pools. All writes funnel through a single
// writer goroutine over a channel, so there is never a second writer and no
// "database is locked" contention. Reads use a separate pool, which WAL serves
// concurrently with the writer. This is the foundation the fsnotify watcher
// (later) needs: it can stream upserts while mesh_search reads, with no deadlock.
type Store struct {
	dir      string
	dbPath   string
	writeDB  *sql.DB
	readDB   *sql.DB
	jobs     chan job
	done     chan struct{}
	wg       sync.WaitGroup // tracks the writer goroutine so Close can join it
	readOnly bool           // true for OpenReadOnly: no writer goroutine, Write always fails

	closeOnce sync.Once // Close is idempotent: a second close(s.done) would panic
	closeErr  error     // the first Close's result, replayed to later callers

	mu      sync.Mutex  // guards dropped + droppedSynced
	dropped []FileError // notes dropped as unparseable by the last full reindex
	// droppedSynced records whether this store has mirrored its dropped set into the
	// index yet. The first pass always writes (the table may hold a previous run's
	// rows), later ones only when the set changed; see persistDropped.
	droppedSynced bool

	// Telemetry is accumulated in memory and flushed in one batched transaction by the
	// writer goroutine (see flushTelemetry). It must never be written inline: the
	// read-only MCP/web tools call IncrMetric/RecordReuse purely for counters AFTER the
	// answer is already computed, and Write is a synchronous handoff to the single
	// writer, so a search or fetch used to inherit the latency of whatever transaction
	// was running (a full reindex) or, with a second mesh process holding the SQLite
	// write lock, up to the DSN's 30s busy_timeout.
	telMu    sync.Mutex
	telCount map[string]int64 // metrics key -> pending increment
	telReuse []reuseEvent     // pending flywheel reuse events, timestamped at call time
}

// reuseEvent is one deferred RecordReuse. The timestamp is captured when the fetch
// happened, not when the batch is flushed, so the gap check stays honest.
type reuseEvent struct {
	noteID string
	gapSec int64
	at     int64
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
// v5: no column changed, but retrievalHash gained scope/updated/when/review_by/source,
// so every stored retrieval_hash is computed from an older, narrower input. The bump
// forces one rebuild that re-hashes every row; without it a note whose scope was already
// tightened before the upgrade would keep comparing equal and never reindex, leaving the
// old (wider) scope live in notes.scope. Kept embeddings survive the rebuild
// (see schemaKeep and metaKeptWithVectors); their note_hash is stale against the new
// hash, so they are excluded from retrieval until the next `mesh embed`, which re-stamps
// them from the content-hash cache without paying for a single new embedding.
// v6: dropped_notes landed in schema.sql on 2026-08-09 with NO version bump, so every
// index written by a released binary before that date is stamped 5 and has no such table.
// Nothing rebuilt it, because only the writable Open compares versions and the shipped
// per-window setup is read-only. The bump is what makes those databases identifiable, and
// OpenReadOnlyAt now refuses them by version instead of querying a table that is not there.
const SchemaVersion = 6

type job struct {
	fn    func(*sql.Tx) error
	reply chan error
}

const (
	// walSizeLimit is the size mesh.db-wal is held near on disk. SQLite's WAL
	// autocheckpoint is PASSIVE: it resets the write pointer but NEVER shrinks the file,
	// so without help the WAL only grows to its high-water mark and stays there (observed
	// 223MB, which starved writers into SQLITE_BUSY).
	//
	// journal_size_limit is set from this, but do NOT read that as a guarantee. It only
	// applies when a checkpoint RESETS the WAL, and a PASSIVE checkpoint cannot reset it
	// while any reader holds a snapshot, which is the normal state under a watch daemon.
	// This comment previously claimed "every checkpoint truncates the WAL back to at most
	// this size" and the live vault disproved it at 26.44MB. What actually bounds the file
	// is the writer loop escalating to TRUNCATE when it sees the file over this size; see
	// writer() and TestPassiveCheckpointDoesNotBoundTheWAL.
	//
	// 16MB holds the largest single reindex transaction's frames without thrashing.
	walSizeLimit = 16 * 1024 * 1024
	// walCheckpointInterval is how often the single writer goroutine checkpoints: PASSIVE
	// every tick (never blocks the writer on a reader), escalating to a best-effort
	// TRUNCATE only when the file is actually over walSizeLimit. Autocheckpoint alone
	// fires only on writes, so an idle stretch would otherwise leave the WAL as it was.
	walCheckpointInterval = 2 * time.Minute
	// telemetryFlushInterval is how often the writer goroutine drains the in-memory
	// usage counters into one batched transaction. Short enough that a dashboard is
	// never meaningfully behind, long enough that a burst of fetches costs one write
	// instead of three per request.
	telemetryFlushInterval = 2 * time.Second
)

const (
	// busyTimeoutMS is how long any mesh connection waits for another mesh process's
	// write lock before giving up. It has to be generous: a sibling watcher's reconcile
	// is a whole-vault write transaction (nodes + edges + note_code_links are rewritten
	// wholesale even for one edited note), so seconds of waiting is normal and correct.
	busyTimeoutMS = 30000
	// checkpointBusyTimeoutMS is the patience of the best-effort TRUNCATE checkpoint,
	// and of nothing else. Short on purpose so a contended vault costs one skipped tick
	// instead of stalling the writer. It is applied on the checkpoint's OWN connection;
	// see checkpointTruncateBestEffort for why it must never touch a pool.
	checkpointBusyTimeoutMS = 2000
	// walAutocheckpointPages is SQLite's own default (1000 pages, ~4MB at the default
	// 4096-byte page size), set EXPLICITLY on the DSN rather than left implicit. This is
	// the outstanding rider from the multi-writer decision (journal_size_limit already
	// shipped; wal_autocheckpoint did not). It changes nothing about the bound: at 1000
	// pages it fires well under walSizeLimit's 16MB, so it is redundant with the writer's
	// own PASSIVE-then-escalate-to-TRUNCATE loop below, not a replacement for it. Being
	// explicit means a future SQLite default change can't silently move this threshold
	// out from under the tuning the comments here already documented.
	walAutocheckpointPages = 1000
)

func dsn(path string) string { return dsnBusy(path, busyTimeoutMS) }

// dsnBusy is dsn with an explicit busy_timeout, so a connection that wants a different
// patience gets it in its DSN rather than by running a PRAGMA on a shared pool.
func dsnBusy(path string, busyMS int) string {
	return fmt.Sprintf(
		"file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(%d)&_pragma=foreign_keys(on)&_pragma=journal_size_limit(%d)&_pragma=wal_autocheckpoint(%d)",
		path, busyMS, walSizeLimit, walAutocheckpointPages,
	)
}

// dsnReadOnly is the DSN for OpenReadOnly: mode=ro refuses the OS-level open flags SQLite
// would need to write at all (SQLITE_READONLY on any write attempt, "attempt to write a
// readonly database"), and query_only(true) is defense in depth on top of that at the SQL
// layer. Deliberately does NOT set journal_mode(WAL): that pragma is a schema change, and
// asking a read-only connection to (re-)declare it risks the exact write attempt mode=ro
// exists to refuse, on a file already in WAL mode on disk from the owning writer. It also
// does not set journal_size_limit or wal_autocheckpoint: both are checkpoint tuning, and
// a connection that cannot write never checkpoints.
func dsnReadOnly(path string) string {
	return fmt.Sprintf(
		"file:%s?mode=ro&_pragma=busy_timeout(%d)&_pragma=foreign_keys(on)&_pragma=query_only(true)",
		path, busyTimeoutMS,
	)
}

// vaultMeshDirMode keeps <vault>/.mesh owner-only. It agrees with pkg/meshclient and
// internal/ingest, which create the same directory. Three packages create it and
// MkdirAll is a NO-OP on an existing directory, so the FIRST one to run silently
// decides the mode for every other one's files, and this package is almost always
// first: every mesh command opens the index. That is how a vault ends up with a
// correctly 0600 .mesh/credentials sitting in a world-traversable directory, next to
// a 0644 mesh.db holding the full text of every note.
const vaultMeshDirMode = 0o700

// Open creates (or opens) <vaultRoot>/.mesh/mesh.db, applies the schema, and
// starts the writer goroutine.
func Open(vaultRoot string) (*Store, error) {
	dir := filepath.Join(vaultRoot, ".mesh")
	if err := ensureVaultMeshDir(dir); err != nil {
		return nil, err
	}
	return OpenAt(vaultRoot, dir)
}

// ensureVaultMeshDir creates <vault>/.mesh at vaultMeshDirMode and, when it already
// exists wider than that, narrows it. The repair is the point: MkdirAll's mode applies
// only when it CREATES the directory, exactly like os.WriteFile's perm argument, so
// tightening the constant alone would leave every vault created by an older mesh at
// 0755 forever. Only the group and other bits are touched, and only downward, so an
// operator who deliberately narrowed further is not widened back.
func ensureVaultMeshDir(dir string) error {
	if err := os.MkdirAll(dir, vaultMeshDirMode); err != nil {
		return err
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		// fi.Mode(), not perm: Perm() masks off setgid/setuid/sticky, so chmod'ing the
		// masked value silently cleared them. Only group/other are meant to change.
		if err := os.Chmod(dir, fi.Mode()&^0o077); err != nil {
			// Best effort: a vault on a filesystem that refuses chmod (or one owned by
			// another uid) must still be indexable. Log it rather than fail the command,
			// but log it at WARN, because the credentials beside it are exposed.
			slog.Warn("index: cannot narrow world-readable vault .mesh",
				"dir", dir, "mode", perm.String(), "err", err)
		}
	}
	return nil
}

// indexFileMode is the mode the index files themselves carry. Narrowing the directory
// was only half the fix: SQLite creates mesh.db with 0666&^umask, which is 0644 under
// the default umask, so the file holding the full text of every note was world-readable
// on its own terms. That is invisible while it sits inside a 0700 directory and becomes
// real the moment the bytes are copied out of it, which is the normal case: a backup
// tool, a `cp -r`, a vault on a synced volume, or an older vault whose .mesh predates
// the 0700 constant.
const indexFileMode = 0o600

// narrowIndexFiles chmods mesh.db and its WAL/SHM siblings down to owner-only, both on
// create and on an existing index written by an older mesh.
//
// The siblings are not an afterthought: mesh.db-wal holds every recently committed note
// body until a checkpoint folds it back, so leaving it 0644 leaks the newest writes,
// which are the ones most worth reading. Best effort throughout, for the same reason
// ensureVaultMeshDir is: an index on a filesystem that refuses chmod must still work.
func narrowIndexFiles(dbPath string) { NarrowDBFiles(dbPath) }

// NarrowDBFiles is narrowIndexFiles for any SQLite database this project owns, exported
// so internal/hub narrows hub.db through the SAME code rather than growing a second copy
// of it. hub.db is the higher-value target of the two: it holds client and invite token
// hashes plus AES-GCM connector PATs whose key, when MESH_HUB_SECRET_KEY is unset, is
// derived from hub.db itself, so one read yields key and ciphertext together.
func NarrowDBFiles(dbPath string) {
	for _, p := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		fi, err := os.Stat(p)
		if err != nil {
			continue // -wal/-shm need not exist yet
		}
		if perm := fi.Mode().Perm(); perm&0o077 != 0 {
			if err := os.Chmod(p, fi.Mode()&^0o077); err != nil {
				slog.Warn("index: cannot narrow world-readable index file",
					"path", p, "mode", perm.String(), "err", err)
			}
		}
	}
}

// OpenAt is like Open but stores the index in an explicit directory instead of
// <vaultRoot>/.mesh. The hub uses this to index its served vault into a dir OUTSIDE
// the git repo, so the index is never synced to clients.
func OpenAt(vaultRoot, meshDir string) (*Store, error) {
	// The same 0700 treatment Open gets, because this is the OTHER entry point that
	// creates the directory. It used to MkdirAll at 0755 and never narrow: harmless
	// when reached via Open (which has already created the dir, making this call a
	// no-op) and wide open on every path that calls OpenAt directly, which is the
	// hub indexing the served vault. That index holds the full text of every note in
	// the team vault, so on a shared box it was world-traversable.
	if err := ensureVaultMeshDir(meshDir); err != nil {
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

	if err := ensureSchemaChecked(writeDB); err != nil {
		writeDB.Close()
		readDB.Close()
		if isUnreadableDB(err) {
			return nil, corruptIndexError(vaultRoot, dbPath, err)
		}
		return nil, fmt.Errorf("apply schema: %w", err)
	}

	narrowIndexFiles(dbPath)

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

// ErrReadOnly is returned by every write path on a Store opened with OpenReadOnly (Write
// itself, and anything built on it: RecordWriteback, IncrMetric, ComputeHealth, ...). It
// is a sentinel, not a message match, so a caller can branch on it (errors.Is) instead of
// getting the generic "database is locked" a real contended write would produce; the two
// must never look alike; see TestReadOnlyWriteFailsLoudly.
var ErrReadOnly = errors.New("mesh: this index store is read-only; write-back routes through the single owning writer (mesh watch / mesh sync --watch)")

// ErrNoIndexYet is returned by OpenReadOnly when the vault has no index at all. A
// sentinel because it is the one startup failure a caller can give better advice about
// than this package can: what to run instead depends on which surface you are.
var ErrNoIndexYet = errors.New("no index yet")

// OpenReadOnly opens <vaultRoot>/.mesh/mesh.db for READS ONLY: no writer goroutine, no
// write connection, Write always fails with ErrReadOnly. This is what every per-window
// `mesh mcp` server must use (see the mesh multi-writer decision note): only the single
// owning writer may hold a writable Store against a given mesh.db, so N concurrent MCP
// servers never again fight each other (or the owner) for the SQLite write lock.
//
// Unlike Open, this does NOT create the database: a read-only store has nothing to apply
// a schema with, so a vault with no index yet (or one whose owning writer has never run)
// fails closed with a clear message instead of silently producing an empty graph.
func OpenReadOnly(vaultRoot string) (*Store, error) {
	return OpenReadOnlyAt(vaultRoot, filepath.Join(vaultRoot, ".mesh"))
}

// OpenReadOnlyAt is OpenReadOnly with an explicit index directory, mirroring OpenAt.
func OpenReadOnlyAt(vaultRoot, meshDir string) (*Store, error) {
	dbPath := filepath.Join(meshDir, "mesh.db")
	// A read-only open does not CREATE the index, but SQLite still creates -wal and -shm
	// beside it at 0666&^umask, and those persist after close because a mode=ro
	// connection cannot checkpoint them away. Without this, a user whose only surfaces
	// are read-only (mesh ui without --own-index, mesh doctor, the TUI) keeps a 0755
	// .mesh and a 0644 mesh.db forever and mints two fresh 0644 files on every open.
	defer narrowIndexFiles(dbPath)
	if _, err := os.Stat(dbPath); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w at %s: start the owning writer first (`mesh watch` or `mesh sync --watch`), or run `mesh index` once", ErrNoIndexYet, dbPath)
		}
		return nil, err
	}
	readDB, err := sql.Open("sqlite", dsnReadOnly(dbPath))
	if err != nil {
		return nil, err
	}
	// Probe now, not on the first caller query: a corrupt or half-written file should
	// fail at Open like the writable path does, not surface as a confusing query error
	// deep inside the first mesh_search.
	if err := readDB.Ping(); err != nil {
		readDB.Close()
		if isUnreadableDB(err) {
			return nil, corruptIndexError(vaultRoot, dbPath, err)
		}
		return nil, fmt.Errorf("open read-only index: %w", err)
	}
	// Then check the SHAPE, not just that the file opens. Only the writable Open compares
	// schema_version (in ensureSchema) and rebuilds on a mismatch, and the shipped setup
	// had no writable process at all: `mesh mcp`, `mesh doctor`, `mesh ui` and the TUI
	// were all read-only. (`mesh mcp` now elects itself the owning writer and opens
	// writable, so it is the one surface that DOES rebuild a mismatched index; the three
	// below still land here.) So an upgraded user's index was never migrated and every
	// read-only surface queried a schema its binary does not match. That did not fail
	// loudly, it
	// failed quietly: a v5 index has no dropped_notes table, and mesh_health swallowed the
	// "no such table" and reported a CLEAN vault while quarantined notes stayed invisible.
	// An index one version older still has no note_health, and mesh_health answered a bare
	// "internal error" with the cause on stderr, which the agent client hides.
	if err := checkReadOnlySchemaVersion(readDB, vaultRoot, dbPath); err != nil {
		readDB.Close()
		return nil, err
	}
	s := &Store{
		dir:      meshDir,
		dbPath:   dbPath,
		readDB:   readDB,
		readOnly: true,
		done:     make(chan struct{}),
	}
	// Usage counters still have to reach the index. They are the flywheel measurement
	// (queries, fetches, note reuse), and after the single-writer split the only
	// processes that generate them are read-only: every `mesh mcp` window and the web
	// viewer. Dropping them here would freeze the dashboard's reuse rate at whatever the
	// last writable process left behind, while the app kept reporting it as current.
	s.wg.Add(1)
	go s.forwardTelemetry()
	return s, nil
}

// dropOnVersionChange lists the tables wiped and rebuilt on a schema-version change.
// It must be every table in schema.sql EXCEPT those in schemaKeep. A test asserts it
// stays in sync with schema.sql so a newly-added table cannot silently leak stale
// rows (or an orphaned renamed table) on a version bump.
var dropOnVersionChange = []string{"notes", "nodes", "edges", "search_index", "corpus_stats", "meta", "code_files", "code_symbols", "code_edges", "code_search", "note_health", "note_code_links", "dropped_notes"}

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
// ensureSchema stores it under meta.keep_shape_version and compares on every open, so
// the escape hatch is real: bumping it DOES drop and rebuild the kept tables. It used to
// be declared and never read, which made the documented remedy a no-op and would have
// left a newly-added column failing at INSERT time on every deployed hub.
const keepShapeVersion = 1

// metaKeptWithVectors are meta keys that DESCRIBE a schemaKeep table and therefore have
// to survive the schema-version rebuild alongside it. `vectors` is kept so a version bump
// never forces a paid re-embed, but its canonical model/dim live in `meta`, which IS
// dropped. Losing them left every kept embedding unreadable: LoadVectors, VectorStats and
// CachedVectors all filter on meta.vector_model, so the rows survived but read back as
// model="" (semantic search silently degraded to lexical+graph) and the content-hash embed
// cache missed on every chunk, forcing exactly the full paid re-embed schemaKeep exists to
// prevent. schema_version itself is deliberately NOT carried: it is rewritten below.
var metaKeptWithVectors = []string{"vector_model", "vector_dim"}

// ensureSchema applies the schema, dropping and rebuilding if the stored version
// differs. No data is lost that matters: everything is re-derivable from the
// markdown vault, so this replaces a migration tool.
// isBusy reports whether err is SQLite's "database is locked" (SQLITE_BUSY). Matched on
// the message rather than a driver error type so this stays correct if the driver is
// swapped; the string is SQLite's own and has been stable for years.
func isBusy(err error) bool {
	if err == nil {
		return false
	}
	m := err.Error()
	return strings.Contains(m, "database is locked") || strings.Contains(m, "SQLITE_BUSY")
}

// ErrIndexCorrupt marks a .mesh/mesh.db that SQLite refuses to read at all: a truncated,
// half-written or overwritten file (SQLITE_NOTADB, result code 26). It is the OTHER
// operator-facing SQLite failure next to SQLITE_BUSY, and unlike a busy lock it never
// clears by waiting. It is a sentinel so callers match with errors.Is instead of reading
// the message, because exactly one caller is allowed to act on it destructively
// (recoverCorruptIndex) and matching a string there would be far too loose.
var ErrIndexCorrupt = errors.New("index database is corrupt")

// isUnreadableDB reports whether err is SQLite refusing the file itself, in EITHER of the
// two ways it can. Matched on the message rather than a driver error type for the same
// reason isBusy is: so this survives swapping the driver. The strings are SQLite's own.
//
//   - SQLITE_NOTADB (26), "file is not a database": the 16-byte header does not match, so
//     the file is not SQLite at all. That is what junk written over the whole file gives.
//   - SQLITE_CORRUPT (11), "database disk image is malformed": the header is fine and a
//     page is not. This is the ORDINARY damage (a crash mid-write, a bad sector, a copy
//     taken while a write was in flight) and covering only code 26 left it with the exact
//     dead end this whole path exists to remove.
//
// Both mean the same thing to Mesh: the index cannot be opened and cannot be repaired in
// place, and since it is derived from the markdown the only answer is to rebuild it.
func isUnreadableDB(err error) bool {
	if err == nil {
		return false
	}
	m := err.Error()
	return strings.Contains(m, "file is not a database") || strings.Contains(m, "SQLITE_NOTADB") ||
		strings.Contains(m, "database disk image is malformed") || strings.Contains(m, "SQLITE_CORRUPT")
}

// ErrSchemaMismatch marks an index whose schema_version is not this binary's
// SchemaVersion: written by an older (or newer) Mesh and never rebuilt, because only the
// writable Open migrates and a read-only surface cannot. A sentinel so callers can give
// their own surface-specific advice (`mesh doctor` prints a status line, the viewer says
// it does not build indexes) instead of pattern-matching the message.
var ErrSchemaMismatch = errors.New("index schema mismatch")

// schemaVersionInIndex reads meta.schema_version from a read-only connection. A missing
// meta table or missing row answers 0: that is a database this Mesh never stamped, which
// is a mismatch, not an internal error.
func schemaVersionInIndex(db *sql.DB) (int, error) {
	var v int
	err := db.QueryRow(`SELECT CAST(value AS INTEGER) FROM meta WHERE key='schema_version'`).Scan(&v)
	switch {
	case err == nil:
		return v, nil
	case errors.Is(err, sql.ErrNoRows), strings.Contains(err.Error(), "no such table"):
		return 0, nil
	default:
		return 0, err
	}
}

// checkReadOnlySchemaVersion is the read-only twin of the version comparison ensureSchema
// makes on the writable path. It cannot migrate (a read-only store has no write
// connection), so it fails closed with the command that CAN: `mesh index`. Serving reads
// off a mismatched schema is what produced a false clean bill of health, and answering a
// bare "internal error" from a missing table is what hid the cause.
func checkReadOnlySchemaVersion(db *sql.DB, vaultRoot, dbPath string) error {
	found, err := schemaVersionInIndex(db)
	if err != nil {
		return fmt.Errorf("read index schema version: %w", err)
	}
	if found == SchemaVersion {
		return nil
	}
	abs, aerr := filepath.Abs(dbPath)
	if aerr != nil {
		abs = dbPath
	}
	root, rerr := filepath.Abs(vaultRoot)
	if rerr != nil {
		root = vaultRoot
	}
	stamped := fmt.Sprintf("v%d", found)
	if found == 0 {
		stamped = "unstamped"
	}
	return fmt.Errorf("%w: %s was written by a different version of Mesh (index schema %s, this binary expects v%d)\n"+
		"  read-only surfaces cannot migrate it, so this one refuses to answer from a schema it does not match\n"+
		"  your notes are safe: the index is derived from the markdown, so rebuilding it loses nothing\n"+
		"  fix: mesh index %s   (rebuilds the index for this version)\n"+
		"  stored embeddings survive the rebuild; re-run mesh embed only if you changed embedding model",
		ErrSchemaMismatch, abs, stamped, SchemaVersion, shellpath.Quote(root))
}

// indexFiles are the three files one SQLite index occupies: the database plus the WAL and
// shared-memory sidecars. A corrupt database has to take its sidecars with it, or the
// rebuilt database inherits frames written against the old, unreadable one.
func indexFiles(dbPath string) []string {
	return []string{dbPath, dbPath + "-wal", dbPath + "-shm"}
}

// corruptIndexError turns result code 26 into something an operator can act on. The bare
// driver message ("apply schema: file is not a database (26)") is emitted identically by
// search, doctor, health AND index, which is a dead end: `mesh index` is the command that
// would rebuild, so the operator is told to fix it by the one thing that cannot run. The
// message therefore names the absolute path, says the notes are safe (the index is derived
// from the markdown, which is the source of truth), and gives the repair verbatim.
func corruptIndexError(vaultRoot, dbPath string, cause error) error {
	abs, err := filepath.Abs(dbPath)
	if err != nil {
		abs = dbPath
	}
	root, err := filepath.Abs(vaultRoot)
	if err != nil {
		root = vaultRoot
	}
	return fmt.Errorf("%w: %s is not a readable SQLite database (truncated, overwritten, damaged mid-write, or not SQLite at all)\n"+
		"  your notes are safe: the index is derived from the markdown, so it is throwaway\n"+
		"  repair:  mesh index %s   (discards the corrupt file, then rebuilds)\n"+
		"  by hand: rm -f %s %s-wal %s-shm\n"+
		"  stored embeddings go with it, so re-run mesh embed if you use semantic search\n"+
		"  cause:   %w",
		ErrIndexCorrupt, abs, shellpath.Quote(root), shellpath.Quote(abs), shellpath.Quote(abs), shellpath.Quote(abs), cause)
}

// recoverCorruptIndex deletes an unreadable index so it can be rebuilt, and does so ONLY
// for ErrIndexCorrupt. Every other open failure returns untouched: a busy lock, a
// permission problem or a disk error all leave a database that is still perfectly good,
// and deleting on those would turn a wait-and-retry into permanent loss of the embed cache
// (a paid re-embed). The narrow condition is the whole point of the function, which is why
// it is separate from OpenRebuild and tested on its own.
func recoverCorruptIndex(dbPath string, openErr error) (bool, error) {
	if !errors.Is(openErr, ErrIndexCorrupt) {
		return false, openErr
	}
	for _, p := range indexFiles(dbPath) {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return false, errors.Join(openErr, err)
		}
	}
	return true, nil
}

// OpenRebuild is Open for the one command whose job is to REBUILD the index. `mesh index`
// reads the markdown and writes the database from scratch, so a database it cannot even
// open is not an obstacle, it is the thing being replaced. Anything else (the hub, the MCP
// server, search, doctor) must keep using Open and report the error instead.
//
// The bool reports whether a corrupt index was discarded, so the caller can say so out
// loud: silently deleting a file on the operator's disk is not acceptable even when the
// file is garbage.
func OpenRebuild(vaultRoot string) (*Store, bool, error) {
	s, err := Open(vaultRoot)
	if err == nil {
		return s, false, nil
	}
	dbPath := filepath.Join(vaultRoot, ".mesh", "mesh.db")
	recovered, rerr := recoverCorruptIndex(dbPath, err)
	if !recovered {
		return nil, false, rerr
	}
	s, err = Open(vaultRoot)
	if err != nil {
		return nil, false, err
	}
	return s, true, nil
}

// ensureSchemaChecked is ensureSchema with an error a human can act on.
//
// WHAT IS MEASURED, and what is not. The SQLITE_BUSY that operators hit on `mesh index`
// got three confident explanations here before the real one, and all three were wrong, so
// this records evidence rather than a story.
//
// RESOLVED: leaked read snapshots pinned the WAL. A `for rows.Next()` loop only closes its
// result set when it runs to exhaustion, so every early return inside the loop leaked an
// open *sql.Rows, and in WAL mode an open Rows holds a READ SNAPSHOT that no checkpoint can
// reclaim past. Seven sites in this package did that, DriftReport among them, which both
// watch daemons call on every reconcile. So the leaks accumulated for as long as the daemon
// lived, the WAL grew past journal_size_limit, and writers starved. Fixed by `defer
// rows.Close()` at all seven; TestWalPinnedByLeakedRows pins the mechanism and
// TestEveryQueryClosesItsRows stops it recurring. Note the shape of this bug: it was in the
// READ paths, and it only hurt a THIRD process, hours later. Symptom and cause shared
// neither a code path nor a process.
//
// Measured on the live vault, and still worth keeping because it rules out the wrong fixes:
//   - a plain `mesh index` against a held lock fails after 31s, i.e. exactly the DSN's
//     busy_timeout(30000). The busy handler runs; it loses. (So it was NOT "the timeout
//     is never applied", explanation one.)
//   - a full reindex holds the write lock for about 3 seconds, not minutes: the note
//     transaction is ~377ms for 894 notes and the code transaction ~2.6s for 3056 files
//     / 235k edges. (So it was NOT "one long transaction blocks everyone", explanation
//     two, and chunking that transaction would not have helped.)
//   - `Exec(SchemaSQL)` succeeds against a held write lock, so the schema check is not the
//     thing that blocks. (Explanation three.)
//   - the symptom tracked daemon UPTIME, not load: a 32-hour `mesh sync --watch` won 0 of
//     276 lock attempts in 45s with a 24MB WAL and failed `mesh index` 8 of 8, while a
//     freshly restarted one showed 0% busy and a 0MB WAL. That is what pointed at an
//     unbounded accumulation rather than a race.
//
// If this ever comes back, measure before theorising. Do not add a retry loop (tried,
// measured, it turned a 31s failure into a 247s hang because every attempt inherits the
// same timeout) and do not chunk the reindex (more transactions make starvation worse).
//
// The message below stays regardless: the error said "database is locked" and left the
// operator guessing, so it names the processes that plausibly hold it and what to do.
func ensureSchemaChecked(db *sql.DB) error {
	err := ensureSchema(db)
	if !isBusy(err) {
		return err
	}
	return fmt.Errorf("another mesh process holds the write lock (most likely a `mesh sync --watch` "+
		"or `mesh mcp --watch` daemon mid-write; a reindex itself only holds it for a few seconds). "+
		"Wait a moment and retry, or stop the daemon if it persists: %w", err)
}

func ensureSchema(db *sql.DB) error {
	var current, currentKeep int
	// meta may not exist yet; ignore the scan error in that case.
	_ = db.QueryRow(`SELECT CAST(value AS INTEGER) FROM meta WHERE key='schema_version'`).Scan(&current)
	_ = db.QueryRow(`SELECT CAST(value AS INTEGER) FROM meta WHERE key='keep_shape_version'`).Scan(&currentKeep)
	// A zero currentKeep is a database written before keep_shape_version existed, not a
	// shape change: adopt the current shape below rather than wiping the kept tables.
	rebuildKept := currentKeep != 0 && currentKeep != keepShapeVersion

	var carried map[string]string
	if current != 0 && current != SchemaVersion {
		var err error
		if carried, err = readMetaKeys(db, metaKeptWithVectors); err != nil {
			return err
		}
		for _, t := range dropOnVersionChange {
			if _, err := db.Exec("DROP TABLE IF EXISTS " + t); err != nil {
				return err
			}
		}
	}
	if rebuildKept {
		for _, t := range sortedKeys(schemaKeep) {
			if _, err := db.Exec("DROP TABLE IF EXISTS " + t); err != nil {
				return err
			}
		}
		// The vectors themselves are gone, so their descriptors must not come back:
		// a model/dim with no rows would misreport the embedding state.
		carried = nil
	}
	if _, err := db.Exec(SchemaSQL); err != nil {
		return err
	}
	if rebuildKept {
		if _, err := db.Exec(`DELETE FROM meta WHERE key IN ('vector_model','vector_dim')`); err != nil {
			return err
		}
	}
	for k, v := range carried {
		if _, err := db.Exec(
			`INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, k, v,
		); err != nil {
			return err
		}
	}
	if _, err := db.Exec(
		`INSERT INTO meta(key,value) VALUES('schema_version',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		fmt.Sprint(SchemaVersion),
	); err != nil {
		return err
	}
	_, err := db.Exec(
		`INSERT INTO meta(key,value) VALUES('keep_shape_version',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		fmt.Sprint(keepShapeVersion),
	)
	return err
}

// readMetaKeys reads the given meta keys, tolerating a missing meta table (a brand-new
// database) and absent keys. Returns only the keys that exist.
func readMetaKeys(db *sql.DB, keys []string) (map[string]string, error) {
	out := map[string]string{}
	for _, k := range keys {
		var v string
		err := db.QueryRow(`SELECT value FROM meta WHERE key=?`, k).Scan(&v)
		switch {
		case err == sql.ErrNoRows:
			continue
		case err != nil:
			// meta does not exist yet: nothing to carry.
			return out, nil
		}
		out[k] = v
	}
	return out, nil
}

// sortedKeys returns a map's keys in a deterministic order, so a DROP loop over a set
// is reproducible (and diffable in logs) rather than map-iteration random.
func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (s *Store) Path() string { return s.dbPath }

// MeshDir returns the vault's .mesh directory (where mesh.db and the solo
// config.toml live).
func (s *Store) MeshDir() string { return s.dir }

// ReadOnly reports whether this store was opened with OpenReadOnly, so a caller can
// take the non-writing path instead of discovering it by failing. Callers must branch
// on THIS, not on which constructor they think ran: the per-window `mesh mcp` server
// and the hub's own indexing server share one setup function, and the hub owns its
// index and must keep writing to it.
func (s *Store) ReadOnly() bool { return s.readOnly }

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
	// Telemetry counters accumulated by the read paths are flushed here, on the writer
	// itself, so they cost one batched transaction per interval instead of three
	// synchronous ones per request. runTx directly (not Write): this IS the writer, so
	// routing through the jobs channel would deadlock.
	telTicker := time.NewTicker(telemetryFlushInterval)
	defer telTicker.Stop()
	for {
		select {
		case <-s.done:
			return
		case j := <-s.jobs:
			j.reply <- s.runTx(j.fn)
		case <-telTicker.C:
			if fn, ok := s.drainTelemetry(); ok {
				if err := s.runTx(fn); err != nil {
					slog.Warn("mesh: telemetry flush failed; this batch of counters is lost", "err", err)
				}
			}
		case <-ticker.C:
			// PASSIVE first: it returns at once and never blocks the writer on a live
			// reader.
			_, _ = s.writeDB.Exec("PRAGMA wal_checkpoint(PASSIVE)")
			// PASSIVE alone does NOT bound the file, and the comment here used to claim
			// it did. journal_size_limit only applies when a checkpoint RESETS the WAL,
			// and a PASSIVE checkpoint that cannot fully drain (any reader mid-snapshot,
			// which is normal under a watch daemon) never resets it. Measured on the live
			// vault after 9 hours: 26.44MB against a 16MB limit, reclaimable to 0 the
			// moment a TRUNCATE ran. So escalate on the size we can actually see.
			//
			// Only when it is over the limit, because TRUNCATE waits for readers to
			// drain. checkpointTruncateBestEffort caps that on its own connection (2s
			// busy_timeout, 3s context) so a busy vault degrades to "not this tick"
			// instead of stalling the writer, which is the failure this whole area
			// already produced once.
			if s.walBytes() > walSizeLimit {
				s.checkpointTruncateBestEffort()
			}
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

// Write runs fn inside a transaction on the single writer goroutine. On a Store opened
// with OpenReadOnly this fails loudly with ErrReadOnly instead of blocking: s.jobs is
// nil on a read-only store (there is no writer goroutine to receive from it), and
// sending on a nil channel blocks forever, so this check must run BEFORE the select,
// not be left to fall out of it.
func (s *Store) Write(fn func(*sql.Tx) error) error {
	if s.readOnly {
		return ErrReadOnly
	}
	reply := make(chan error, 1)
	select {
	case s.jobs <- job{fn: fn, reply: reply}:
		return <-reply
	case <-s.done:
		return fmt.Errorf("store is closed")
	}
}

// recordTelemetry accumulates a pending counter increment and/or a reuse event without
// touching the database. It is the non-blocking half of the telemetry path: callers on
// a read path (mesh_search, mesh_fetch, the web search API) must never wait on the
// single writer just to bump a counter.
func (s *Store) recordTelemetry(key string, n int64, reuse *reuseEvent) {
	s.telMu.Lock()
	defer s.telMu.Unlock()
	if key != "" {
		if s.telCount == nil {
			s.telCount = map[string]int64{}
		}
		s.telCount[key] += n
	}
	if reuse != nil {
		s.telReuse = append(s.telReuse, *reuse)
	}
}

// drainTelemetry takes everything pending and returns the transaction that applies it,
// or ok=false when there is nothing to write. The pending state is cleared under the
// lock before the transaction runs, so a concurrent request never blocks on the flush;
// the trade is that a failed flush loses that batch, which is acceptable for counters
// that are best-effort at every call site anyway.
func (s *Store) drainTelemetry() (func(*sql.Tx) error, bool) {
	s.telMu.Lock()
	counts, reuses := s.telCount, s.telReuse
	s.telCount, s.telReuse = nil, nil
	s.telMu.Unlock()
	if len(counts) == 0 && len(reuses) == 0 {
		return nil, false
	}
	// Deterministic key order so the batched transaction touches rows in a stable
	// sequence (reproducible, and no lock-order surprises against another writer).
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return telemetryTx(keys, counts, reuses), true
}

// telemetryTx is the transaction that applies one batch of counters. Factored out
// because the batch now arrives by two routes: the writable store's own flush ticker,
// and a read-only store's batch forwarded through the owner's op queue. The two must
// apply identically, or the same fetch would count differently depending on which
// process observed it.
func telemetryTx(keys []string, counts map[string]int64, reuses []reuseEvent) func(*sql.Tx) error {
	return func(tx *sql.Tx) error {
		for _, k := range keys {
			if _, err := tx.Exec(
				`INSERT INTO metrics(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=value+excluded.value`,
				k, counts[k]); err != nil {
				return err
			}
		}
		for _, r := range reuses {
			if _, err := tx.Exec(
				`UPDATE note_reuse
				    SET reuse_count = reuse_count + 1,
				        first_reuse = COALESCE(first_reuse, ?),
				        last_reuse  = ?
				  WHERE note_id = ? AND (? - authored_at) >= ?`,
				r.at, r.at, r.noteID, r.at, r.gapSec); err != nil {
				return err
			}
		}
		return nil
	}
}

// forwardTelemetry is the read-only store's equivalent of the writer goroutine's flush
// ticker: it drains the in-memory counters onto the owning writer's op queue on the same
// cadence. One small file per interval per process, and only when something happened, so
// an idle window writes nothing at all.
func (s *Store) forwardTelemetry() {
	defer s.wg.Done()
	t := time.NewTicker(telemetryFlushInterval)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			s.queueTelemetry() // land the last batch on the way out
			return
		case <-t.C:
			s.queueTelemetry()
		}
	}
}

// queueTelemetry hands one batch of counters to the owning writer. Best effort by
// design: counters are the one thing in this index that is genuinely lossy (see
// walSizeLimit's neighbours), and a queue that is full means the owner is long gone,
// which the surfaces report on their own paths with a great deal more context than a
// counter could.
func (s *Store) queueTelemetry() {
	s.telMu.Lock()
	counts, reuses := s.telCount, s.telReuse
	s.telCount, s.telReuse = nil, nil
	s.telMu.Unlock()
	if len(counts) == 0 && len(reuses) == 0 {
		return
	}
	op := Op{Kind: OpTelemetry, Counts: counts}
	for _, r := range reuses {
		op.Reuse = append(op.Reuse, ReuseEvent{NoteID: r.noteID, GapSec: r.gapSec, At: r.at})
	}
	if _, err := s.EnqueueOp(op); err != nil {
		slog.Debug("mesh: could not queue usage counters for the owning writer", "err", err)
	}
}

// flushTelemetry writes any pending counters now, from the CALLER's goroutine (via the
// jobs channel). Read paths never call it; the reporting surfaces do, so a dashboard or
// a test reads a consistent picture instead of one that lags the flush ticker.
func (s *Store) flushTelemetry() {
	if s.readOnly {
		// Same intent, the only route available: hand the batch to the owning writer now
		// rather than at the next tick. Going through Write here would fail with
		// ErrReadOnly and log "this batch of counters is lost", which was true before the
		// queue existed and is not any more.
		s.queueTelemetry()
		return
	}
	fn, ok := s.drainTelemetry()
	if !ok {
		return
	}
	if err := s.Write(fn); err != nil {
		slog.Warn("mesh: telemetry flush failed; this batch of counters is lost", "err", err)
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
// A second Close must not panic. `close(s.done)` on an already-closed channel takes
// the whole process down, and Close sits on shutdown paths where a double call is
// easy to arrange: Server.Close calls it, and so does any caller that closed the
// store itself first. A panic during shutdown loses whatever the rest of the
// shutdown was going to flush, which is the worst possible moment for it.
// Idempotent instead, replaying the first result to every later caller.
func (s *Store) Close() error {
	s.closeOnce.Do(func() { s.closeErr = s.shutdown() })
	return s.closeErr
}

func (s *Store) shutdown() error {
	// A read-only store has no writer goroutine, no jobs channel and no writeDB. It must
	// also never open its own writable connection to run checkpointTruncateBestEffort,
	// which is exactly what the writable path below does: an OpenReadOnly caller (a
	// per-window `mesh mcp`) is not allowed to become a writer even for one PRAGMA. What
	// it does have is the telemetry forwarder, which lands its last batch on the op queue
	// when done closes; waiting for it is why a window that is closed right after a
	// search still contributes that search to the flywheel.
	if s.readOnly {
		close(s.done)
		s.wg.Wait()
		return s.readDB.Close()
	}
	// Land the last batch of in-memory telemetry while the writer is still serving:
	// after close(s.done) every Write returns "store is closed".
	s.flushTelemetry()
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
// walBytes is the size of mesh.db-wal on disk, or 0 if it cannot be read. The on-disk
// size is the only honest signal here: PRAGMA wal_checkpoint reports frames, not the file
// length, and the file is what fills a disk and what an operator sees.
func (s *Store) walBytes() int64 {
	fi, err := os.Stat(s.dbPath + "-wal")
	if err != nil {
		return 0
	}
	return fi.Size()
}

func (s *Store) checkpointTruncateBestEffort() {
	// Its OWN connection, never one out of writeDB. writeDB is capped at a single
	// connection, so `PRAGMA busy_timeout=2000` executed on it did not end with this
	// function: the connection went back to the pool carrying 2000 and every later write
	// transaction in the process inherited it, silently replacing the DSN's 30s. The
	// trigger is a WAL over walSizeLimit, which is itself a symptom of write contention,
	// so the daemon lost 28 seconds of patience exactly when a sibling watcher was
	// holding the lock. Reconciles then returned SQLITE_BUSY, the drift stayed, and the
	// next tick failed the same way, which is how the index stops picking up new notes.
	// The short timeout belongs in this connection's DSN instead.
	db, err := sql.Open("sqlite", dsnBusy(s.dbPath, checkpointBusyTimeoutMS))
	if err != nil {
		return
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _ = db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
}
