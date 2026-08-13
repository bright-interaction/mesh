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
}
