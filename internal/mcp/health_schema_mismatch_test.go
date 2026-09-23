// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/index"
)

// TestHealthRefusesToCallAVaultCleanItCannotRead: mesh_health read the dropped-note record
// through a table that an index written by an older Mesh does not have, swallowed the
// "no such table", and answered {"counts":{},"findings":null}. To the agent that is a
// clean vault, so every quarantined or unparseable note stayed invisible after an upgrade.
// It must fail with the rebuild command instead, and the message has to reach the AGENT:
// the generic internalErr leaves the cause on stderr, which the client hides.
func TestHealthRefusesToCallAVaultCleanItCannotRead(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "good.md"),
		[]byte("---\nid: good\ntype: note\nwhen: \"2026-01-01\"\n---\n# Good\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedIndex(t, dir)

	srv, err := NewServer(dir)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { srv.Close() })
	if err := srv.WaitReady(); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}

	// Take the table away underneath the live server: the same read failure an index
	// stamped with an older schema_version produced on every mesh_health call.
	// The sqlite driver is registered by internal/index, which this package imports.
	db, err := sql.Open("sqlite", filepath.Join(dir, ".mesh", "mesh.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE dropped_notes`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	out, rerr := srv.toolHealth(WithLocalOperator(context.Background()), json.RawMessage(`{}`))
	if rerr == nil {
		b, _ := json.Marshal(out)
		t.Fatalf("mesh_health answered %s although it could not read which notes the index dropped; that reads as a clean vault", b)
	}
	if !strings.Contains(rerr.Message, "mesh index") {
		t.Errorf("the error does not name the remedy, so the agent cannot act on it: %q", rerr.Message)
	}
	if rerr.Message == "internal error" {
		t.Errorf("the agent got the generic %q and the cause went to stderr, where the client hides it", rerr.Message)
	}
	if !strings.Contains(rerr.Message, "backup") || strings.Contains(rerr.Message, "the index is derived from the markdown") {
		t.Fatalf("schema repair advice treats database-only work as disposable: %s", rerr.Message)
	}
}

func TestHealthCorruptionAdviceDisclosesDatabaseOnlyLoss(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "good.md"), []byte("---\nid: good\ntype: note\n---\n# Good\nbody\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	seedIndex(t, dir)
	dbPath := filepath.Join(dir, ".mesh", "mesh.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var pageSize, pageNo int
	if err := db.QueryRow(`PRAGMA page_size`).Scan(&pageSize); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT pageno FROM dbstat WHERE name='notes' AND pagetype='leaf' ORDER BY pageno LIMIT 1`).Scan(&pageNo); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if pageNo <= 1 || pageSize <= 0 {
		t.Fatal("invalid fixture corruption offset")
	}
	f, err := os.OpenFile(dbPath, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteAt(make([]byte, pageSize), int64(pageNo-1)*int64(pageSize)); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := index.OpenReadOnly(dir)
	if err != nil {
		t.Fatalf("fixture schema must remain readable: %v", err)
	}
	defer store.Close()
	srv := &Server{store: store, vaultRoot: dir}
	_, rerr := srv.toolHealth(WithLocalOperator(context.Background()), json.RawMessage(`{}`))
	if rerr == nil {
		t.Fatal("corrupt fixture was reported healthy")
	}
	for _, want := range []string{"corrupt", "backup", "pending review notes", "usage/reuse history", "mesh index"} {
		if !strings.Contains(rerr.Message, want) {
			t.Errorf("agent-facing corruption advice missing %q: %s", want, rerr.Message)
		}
	}
	if strings.Contains(rerr.Message, dir) {
		t.Fatal("agent-facing corruption advice leaks the private vault path")
	}
}
