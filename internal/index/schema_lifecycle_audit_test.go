// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A schema replacement is one operation. Before the explicit transaction, a failure in
// the last DROP left every earlier table gone and also lost vector metadata from meta.
func TestSchemaUpgradeRollsBackEveryDDLStep(t *testing.T) {
	dir := seedIndexedVault(t)
	dbPath := filepath.Join(dir, ".mesh", "mesh.db")
	db, err := sql.Open("sqlite", dsn(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO meta(key,value) VALUES
		('vector_model','audit-model'),('vector_dim','3')
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE meta SET value=? WHERE key='schema_version'`, fmt.Sprint(SchemaVersion-1)); err != nil {
		db.Close()
		t.Fatal(err)
	}
	// Append a deliberately invalid final statement. Every real schema statement and
	// every DROP has executed by then, so only a transaction around the whole lifecycle
	// can preserve the old database.
	if err := ensureSchemaSQL(db, true, true, SchemaSQL+`
		INSERT INTO audit_table_that_does_not_exist(value) VALUES(1);`); err == nil {
		db.Close()
		t.Fatal("schema upgrade unexpectedly succeeded despite the late DDL trap")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	check, err := sql.Open("sqlite", dsn(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	var notes int
	if err := check.QueryRow(`SELECT count(*) FROM notes`).Scan(&notes); err != nil {
		t.Fatalf("failed upgrade did not roll back the earlier DROP statements: %v", err)
	}
	if notes != 1 {
		t.Fatalf("notes after failed upgrade = %d, want original 1", notes)
	}
	for key, want := range map[string]string{
		"schema_version": fmt.Sprint(SchemaVersion - 1),
		"vector_model":   "audit-model",
		"vector_dim":     "3",
	} {
		var got string
		if err := check.QueryRow(`SELECT value FROM meta WHERE key=?`, key).Scan(&got); err != nil {
			t.Fatalf("failed upgrade lost meta.%s: %v", key, err)
		}
		if got != want {
			t.Errorf("meta.%s after rollback = %q, want %q", key, got, want)
		}
	}
}

func TestCurrentStampDoesNotAcceptAViewInPlaceOfATable(t *testing.T) {
	dir := seedIndexedVault(t)
	dbPath := filepath.Join(dir, ".mesh", "mesh.db")
	db, err := sql.Open("sqlite", dsn(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE dropped_notes`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	// Its columns deliberately match the probe. Type validation, not column selection,
	// must be what rejects it.
	if _, err := db.Exec(`CREATE VIEW dropped_notes AS
		SELECT '' AS path, '' AS err, 0 AS duplicate, 0 AS detected_at`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	if store, err := OpenReadOnly(dir); err == nil || !errors.Is(err, ErrSchemaMismatch) {
		if store != nil {
			store.Close()
		}
		t.Fatalf("read-only open accepted a view under a table name: %v", err)
	}
	if store, err := OpenCurrent(dir); err == nil || !errors.Is(err, ErrSchemaMismatch) {
		if store != nil {
			store.Close()
		}
		t.Fatalf("current-only open accepted a view under a table name: %v", err)
	}

	store, recovered, err := OpenRebuild(dir)
	if err != nil {
		t.Fatalf("rebuild could not replace the impostor view: %v", err)
	}
	defer store.Close()
	if recovered {
		t.Fatal("schema-shape repair was reported as file-corruption deletion")
	}
	if _, _, err := ReindexFull(store, dir); err != nil {
		t.Fatal(err)
	}
	var objectType string
	if err := store.readDB.QueryRow(`SELECT type FROM sqlite_schema WHERE name='dropped_notes'`).Scan(&objectType); err != nil {
		t.Fatal(err)
	}
	if objectType != "table" {
		t.Fatalf("dropped_notes object type after rebuild = %q, want table", objectType)
	}
}

// A schema from the future is not a rebuildable old derived index. It may contain kept
// state unknown to this binary, so every opener must fail before changing any row/stamp.
func TestEveryOpenerRefusesAFutureIndexWithoutMutation(t *testing.T) {
	dir := seedIndexedVault(t)
	dbPath := filepath.Join(dir, ".mesh", "mesh.db")
	db, err := sql.Open("sqlite", dsn(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO metrics(key,value) VALUES('future-proof',41)`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO pending_notes(id,type,title,created_at)
		VALUES('future-pending','gotcha','Must survive',1)`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE meta SET value=? WHERE key='schema_version'`, fmt.Sprint(SchemaVersion+1)); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE meta SET value=? WHERE key='keep_shape_version'`, fmt.Sprint(keepShapeVersion+1)); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	openers := []struct {
		name string
		open func() error
	}{
		{"read-only", func() error {
			store, err := OpenReadOnly(dir)
			if store != nil {
				_ = store.Close()
			}
			return err
		}},
		{"writable owner", func() error {
			store, err := Open(dir)
			if store != nil {
				_ = store.Close()
			}
			return err
		}},
		{"current-only writer", func() error {
			store, err := OpenCurrent(dir)
			if store != nil {
				_ = store.Close()
			}
			return err
		}},
		{"explicit rebuild", func() error {
			store, _, err := OpenRebuild(dir)
			if store != nil {
				_ = store.Close()
			}
			return err
		}},
	}
	for _, tc := range openers {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.open()
			if !errors.Is(err, ErrSchemaTooNew) {
				t.Fatalf("got %v, want ErrSchemaTooNew", err)
			}
			if !errors.Is(err, ErrSchemaMismatch) {
				t.Errorf("future error should also match ErrSchemaMismatch: %v", err)
			}
			if msg := err.Error(); !strings.Contains(msg, "upgrade Mesh") || !strings.Contains(msg, "untouched") {
				t.Errorf("future-index refusal is not actionable: %v", err)
			}
		})
	}

	check, err := sql.Open("sqlite", dsn(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	for query, want := range map[string]int{
		`SELECT count(*) FROM notes`:                                   1,
		`SELECT value FROM metrics WHERE key='future-proof'`:           41,
		`SELECT count(*) FROM pending_notes WHERE id='future-pending'`: 1,
	} {
		var got int
		if err := check.QueryRow(query).Scan(&got); err != nil {
			t.Fatalf("future index was mutated (%s): %v", query, err)
		}
		if got != want {
			t.Errorf("after refused downgrade, %s = %d, want %d", query, got, want)
		}
	}
	for key, want := range map[string]string{
		"schema_version":     fmt.Sprint(SchemaVersion + 1),
		"keep_shape_version": fmt.Sprint(keepShapeVersion + 1),
	} {
		var got string
		if err := check.QueryRow(`SELECT value FROM meta WHERE key=?`, key).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("refused downgrade restamped %s=%q, want %q", key, got, want)
		}
	}
}

func TestCurrentStampDoesNotHideMissingTables(t *testing.T) {
	dir := seedIndexedVault(t)
	dbPath := filepath.Join(dir, ".mesh", "mesh.db")
	db, err := sql.Open("sqlite", dsn(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE nodes`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	if store, err := OpenReadOnly(dir); err == nil || !errors.Is(err, ErrSchemaMismatch) {
		if store != nil {
			store.Close()
		}
		t.Fatalf("read-only open over a missing current table = %v, want ErrSchemaMismatch", err)
	}
	if store, err := OpenCurrent(dir); err == nil || !errors.Is(err, ErrSchemaMismatch) {
		if store != nil {
			store.Close()
		}
		t.Fatalf("current-only writer over a missing table = %v, want ErrSchemaMismatch", err)
	}
	check, err := sql.Open("sqlite", dsn(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	var nodes, notes int
	if err := check.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE type='table' AND name='nodes'`).Scan(&nodes); err != nil {
		check.Close()
		t.Fatal(err)
	}
	if err := check.QueryRow(`SELECT count(*) FROM notes`).Scan(&notes); err != nil {
		check.Close()
		t.Fatal(err)
	}
	if err := check.Close(); err != nil {
		t.Fatal(err)
	}
	if nodes != 0 || notes != 1 {
		t.Fatalf("refused current-only open mutated the index: nodes table=%d, notes=%d", nodes, notes)
	}

	rebuilt, recovered, err := OpenRebuild(dir)
	if err != nil {
		t.Fatalf("rebuild could not repair a correctly stamped partial schema: %v", err)
	}
	defer rebuilt.Close()
	if recovered {
		t.Fatal("shape repair was reported as file-corruption deletion")
	}
	if _, _, err := ReindexFull(rebuilt, dir); err != nil {
		t.Fatal(err)
	}
	if notes, err := rebuilt.Count("notes"); err != nil || notes != 1 {
		t.Fatalf("notes after repair = %d, err=%v; want 1", notes, err)
	}
}

func TestMalformedAndUnstampedSchemasAreRebuiltNotBlessed(t *testing.T) {
	t.Run("malformed current-looking stamp", func(t *testing.T) {
		dir := seedIndexedVault(t)
		dbPath := filepath.Join(dir, ".mesh", "mesh.db")
		db, err := sql.Open("sqlite", dsn(dbPath))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`DROP TABLE notes`); err != nil {
			db.Close()
			t.Fatal(err)
		}
		if _, err := db.Exec(`CREATE TABLE notes(id TEXT)`); err != nil {
			db.Close()
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE meta SET value='6garbage' WHERE key='schema_version'`); err != nil {
			db.Close()
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}

		if store, err := OpenReadOnly(dir); err == nil || !errors.Is(err, ErrSchemaMismatch) {
			if store != nil {
				store.Close()
			}
			t.Fatalf("malformed stamp bypassed read-only schema validation: %v", err)
		}
		store, _, err := OpenRebuild(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		if _, _, err := ReindexFull(store, dir); err != nil {
			t.Fatal(err)
		}
		if n, err := store.Count("notes"); err != nil || n != 1 {
			t.Fatalf("repaired malformed-stamp index has notes=%d, err=%v", n, err)
		}
	})

	t.Run("unstamped incompatible table", func(t *testing.T) {
		dir := t.TempDir()
		writeFixture(t, dir, "good.md", "id: good\ntype: note\nwhen: \"2026-01-01\"\n", "# Good\nbody\n")
		meshDir := filepath.Join(dir, ".mesh")
		if err := os.MkdirAll(meshDir, 0o700); err != nil {
			t.Fatal(err)
		}
		db, err := sql.Open("sqlite", dsn(filepath.Join(meshDir, "mesh.db")))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`CREATE TABLE notes(id TEXT); INSERT INTO notes VALUES('ghost')`); err != nil {
			db.Close()
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		if store, err := OpenCurrent(dir); err == nil || !errors.Is(err, ErrSchemaMismatch) {
			if store != nil {
				store.Close()
			}
			t.Fatalf("current-only writer blessed an unstamped incompatible table: %v", err)
		}
		store, _, err := OpenRebuild(dir)
		if err != nil {
			t.Fatalf("rebuild failed to repair unstamped table: %v", err)
		}
		defer store.Close()
		if _, _, err := ReindexFull(store, dir); err != nil {
			t.Fatal(err)
		}
		if n, err := store.Count("notes"); err != nil || n != 1 {
			t.Fatalf("unstamped repair kept ghost or lost Markdown note: notes=%d err=%v", n, err)
		}
	})
}

// Damage a notes b-tree leaf while preserving page 1. Ping and schema checks therefore
// succeed; only a whole-database integrity pass can find the corruption reliably.
func corruptNotesLeafPage(t *testing.T, dir string) {
	t.Helper()
	dbPath := filepath.Join(dir, ".mesh", "mesh.db")
	db, err := sql.Open("sqlite", dsn(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	var pageSize, pageNo int
	if err := db.QueryRow(`PRAGMA page_size`).Scan(&pageSize); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT pageno FROM dbstat WHERE name='notes' AND pagetype='leaf' ORDER BY pageno LIMIT 1`).Scan(&pageNo); err != nil {
		db.Close()
		t.Fatalf("locate notes leaf page: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if pageNo <= 1 || pageSize <= 0 {
		t.Fatalf("unsafe corruption target: page=%d size=%d", pageNo, pageSize)
	}
	f, err := os.OpenFile(dbPath, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := f.WriteAt(make([]byte, pageSize), int64(pageNo-1)*int64(pageSize))
	syncErr := f.Sync()
	closeErr := f.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		t.Fatalf("damage notes page: %v", err)
	}
}

func TestOpenRebuildRecoversCorruptionOutsideTheSchemaPage(t *testing.T) {
	dir := seedIndexedVault(t)
	corruptNotesLeafPage(t, dir)

	reader, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatalf("precondition: schema-only open should succeed, got %v", err)
	}
	if err := reader.CheckIntegrity(dir); !errors.Is(err, ErrIndexCorrupt) {
		reader.Close()
		t.Fatalf("quick_check got %v, want ErrIndexCorrupt", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}

	rebuilt, recovered, err := OpenRebuild(dir)
	if err != nil {
		t.Fatalf("OpenRebuild could not recover a corrupt data page: %v", err)
	}
	defer rebuilt.Close()
	if !recovered {
		t.Fatal("OpenRebuild left the corrupt data-page database in place")
	}
	if _, _, err := ReindexFull(rebuilt, dir); err != nil {
		t.Fatalf("reindex after recovery: %v", err)
	}
	if n, err := rebuilt.Count("notes"); err != nil || n != 1 {
		t.Fatalf("recovered index notes = %d, err=%v; want 1", n, err)
	}
}
