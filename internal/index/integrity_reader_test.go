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

// A long-running reader must not mistake another connection's committed FTS
// updates for corruption. This uses only a disposable database, never a vault
// from the operator's environment.
func TestCheckIntegrityAfterExternalFTSChanges(t *testing.T) {
	for _, readOnly := range []bool{true, false} {
		t.Run(fmt.Sprintf("read_only_%t", readOnly), func(t *testing.T) {
			testIntegrityAfterExternalFTSChanges(t, readOnly)
		})
	}
}

func testIntegrityAfterExternalFTSChanges(t *testing.T, readOnly bool) {
	t.Helper()
	root := t.TempDir()
	writer, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	write := func(generation int) {
		t.Helper()
		if err := writer.Write(func(tx *sql.Tx) error {
			if _, err := tx.Exec(`DELETE FROM search_index`); err != nil {
				return err
			}
			if _, err := tx.Exec(`DELETE FROM code_search`); err != nil {
				return err
			}
			for i := 0; i < 32; i++ {
				_, err := tx.Exec(`INSERT INTO search_index(node_id,kind,anchor,title,body) VALUES(?,?,?,?,?)`,
					fmt.Sprintf("note:fixture-%d", i), "note", "", "Fixture",
					fmt.Sprintf("generation%d fixture%d words about changing searchable knowledge", generation, i))
				if err != nil {
					return err
				}
				if _, err := tx.Exec(`INSERT INTO code_search(symbol_id,name,signature,doc) VALUES(?,?,?,?)`,
					fmt.Sprintf("code:fixture-%d", i), "Fixture", "func Fixture()",
					fmt.Sprintf("generation%d indexed source comment", generation)); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	write(0)
	reader := writer
	if readOnly {
		reader, err = OpenReadOnly(root)
		if err != nil {
			t.Fatal(err)
		}
		defer reader.Close()
	}
	reader.readDB.SetMaxOpenConns(1)
	reader.readDB.SetMaxIdleConns(1)
	if err := reader.CheckIntegrity(root); err != nil {
		t.Fatalf("initial integrity: %v", err)
	}
	for generation := 1; generation <= 5; generation++ {
		write(generation)
		if err := reader.CheckIntegrity(root); err != nil {
			fresh, openErr := OpenReadOnly(root)
			if openErr != nil {
				t.Fatalf("fresh open: %v; reused reader: %v", openErr, err)
			}
			freshErr := fresh.CheckIntegrity(root)
			fresh.Close()
			t.Fatalf("generation %d: reused reader integrity: %v; fresh reader integrity: %v", generation, err, freshErr)
		}
		// Normal FTS reads still have to see the new generation. A diagnostic fix
		// must not close or otherwise disrupt the shared retrieval pool.
		for _, table := range []string{"search_index", "code_search"} {
			var hits int
			if err := reader.readDB.QueryRow(`SELECT count(*) FROM `+table+` WHERE `+table+` MATCH ?`,
				fmt.Sprintf("generation%d", generation)).Scan(&hits); err != nil || hits != 32 {
				t.Fatalf("generation %d %s: hits=%d err=%v", generation, table, hits, err)
			}
		}
	}
}

func TestCheckIntegrityDetectsFTSCorruptionWithoutRepair(t *testing.T) {
	root := t.TempDir()
	writer, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if err := writer.Write(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO search_index(rowid,node_id,kind,title,body) VALUES(1,'note:fixture','note','Fixture','original searchable words')`); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO pending_notes(id,type,title,do_text,created_at) VALUES('review-fixture','decision','Keep this review','Unpublished work',1)`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	reader, err := OpenReadOnly(root)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if err := reader.CheckIntegrity(root); err != nil {
		t.Fatalf("healthy fixture: %v", err)
	}
	// Deliberately damage only a disposable FTS shadow table. This changes the
	// stored content without maintaining its inverted index: genuine corruption.
	if err := writer.Write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE search_index_content SET c4='tampered without index maintenance' WHERE id=1`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := reader.CheckIntegrity(root); !errors.Is(err, ErrIndexCorrupt) {
		t.Fatalf("corrupt FTS: got %v, want ErrIndexCorrupt", err)
	}
	var body, pending string
	if err := writer.readDB.QueryRow(`SELECT c4 FROM search_index_content WHERE id=1`).Scan(&body); err != nil {
		t.Fatal(err)
	}
	if err := writer.readDB.QueryRow(`SELECT do_text FROM pending_notes WHERE id='review-fixture'`).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if body != "tampered without index maintenance" || pending != "Unpublished work" {
		t.Fatalf("diagnostic changed data: body=%q pending=%q", body, pending)
	}
}

func TestCheckIntegrityMissingDatabaseDoesNotCreateOne(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "absent.db")
	store := &Store{dbPath: dbPath}
	err := store.CheckIntegrity(root)
	if err == nil || errors.Is(err, ErrIndexCorrupt) {
		t.Fatalf("missing database must be an open error, not corruption: %v", err)
	}
	if _, err := os.Stat(dbPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("diagnostic created a database: %v", err)
	}
}

func TestCheckIntegrityReadFailureIsNotCorruption(t *testing.T) {
	root := t.TempDir()
	// A directory cannot be opened as a SQLite file, without relying on Unix
	// permissions (which a privileged test runner could bypass).
	store := &Store{dbPath: root}
	err := store.CheckIntegrity(root)
	if err == nil || errors.Is(err, ErrIndexCorrupt) || !strings.Contains(err.Error(), "check index integrity") {
		t.Fatalf("I/O/open failure misclassified: %v", err)
	}
}
