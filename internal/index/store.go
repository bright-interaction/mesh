package index

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

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
const SchemaVersion = 3

type job struct {
	fn    func(*sql.Tx) error
	reply chan error
}

func dsn(path string) string {
	return "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(on)"
}

// Open creates (or opens) <vaultRoot>/.mesh/mesh.db, applies the schema, and
// starts the writer goroutine.
func Open(vaultRoot string) (*Store, error) {
	meshDir := filepath.Join(vaultRoot, ".mesh")
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
	go s.writer()
	return s, nil
}

// ensureSchema applies the schema, dropping and rebuilding if the stored version
// differs. No data is lost that matters: everything is re-derivable from the
// markdown vault, so this replaces a migration tool.
func ensureSchema(db *sql.DB) error {
	var current int
	// meta may not exist yet; ignore the scan error in that case.
	_ = db.QueryRow(`SELECT CAST(value AS INTEGER) FROM meta WHERE key='schema_version'`).Scan(&current)
	if current != 0 && current != SchemaVersion {
		for _, t := range []string{"notes", "nodes", "edges", "vectors", "search_index", "corpus_stats", "meta", "code_files", "code_symbols", "code_edges", "code_search"} {
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

func (s *Store) writer() {
	for {
		select {
		case <-s.done:
			return
		case j := <-s.jobs:
			j.reply <- s.runTx(j.fn)
		}
	}
}

func (s *Store) runTx(fn func(*sql.Tx) error) (err error) {
	tx, err := s.writeDB.Begin()
	if err != nil {
		return err
	}
	defer func() {
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

// Close stops the writer goroutine and closes both pools. Callers must ensure no
// Write is in flight (M0 indexing is sequential; the watcher will own lifecycle).
func (s *Store) Close() error {
	close(s.done)
	errW := s.writeDB.Close()
	errR := s.readDB.Close()
	if errW != nil {
		return errW
	}
	return errR
}
