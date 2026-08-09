// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seedOwnedIndex plays the single owning writer: it builds an index once and closes it,
// which is the precondition OpenReadOnly requires (and the ordering production has).
func seedOwnedIndex(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "decisions"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "decisions", "seed.md"),
		[]byte("---\nid: seed\ntype: decision\nwhen: 2026-01-01\ndo: x\ndont: y\nwhy: seed the index\n---\n# Seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	owner, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if _, _, err := ReindexFull(owner, dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

// A read-only store must REFUSE writes, not crash on them. Write() is guarded by its own
// readOnly check, but a handful of methods reach s.writeDB directly, and writeDB is nil
// on a read-only store, so an unguarded one is a nil-pointer panic inside database/sql
// instead of an error. setCodeRoots was exactly that: it took down the whole test binary
// (and would take down a long-running `mesh mcp` server) from mesh_code_search, a path
// that looks read-only from the outside. Without the guard in setCodeRoots this test
// panics rather than fails.
func TestReadOnlyStoreRefusesDirectWriteDBUseInsteadOfPanicking(t *testing.T) {
	dir := seedOwnedIndex(t)
	ro, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()

	if err := ro.setCodeRoots([]string{"/some/root"}); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("setCodeRoots on a read-only store: want ErrReadOnly, got %v", err)
	}
}

// The refusal must be a sentinel, never something that reads like contention. A caller
// (and a human reading a log) has to be able to tell "this server does not write" apart
// from "another process holds the write lock", because the first is by design and the
// second is the symptom this whole split exists to remove.
func TestReadOnlyRefusalIsDistinguishableFromContention(t *testing.T) {
	dir := seedOwnedIndex(t)
	ro, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()

	err = ro.Write(func(*sql.Tx) error { return nil })
	if !errors.Is(err, ErrReadOnly) {
		t.Fatalf("Write on a read-only store: want ErrReadOnly, got %v", err)
	}
	for _, contention := range []string{"database is locked", "SQLITE_BUSY"} {
		if strings.Contains(err.Error(), contention) {
			t.Fatalf("read-only refusal must not read like contention (%q), got %q", contention, err.Error())
		}
	}
}

// OpenReadOnly must fail closed when no owner has ever indexed the vault, rather than
// serving an empty graph that looks like a vault with no notes.
func TestOpenReadOnlyRefusesAVaultWithNoIndex(t *testing.T) {
	dir := t.TempDir()
	if _, err := OpenReadOnly(dir); err == nil {
		t.Fatal("OpenReadOnly on a vault with no index: want an error, got nil")
	}
}
