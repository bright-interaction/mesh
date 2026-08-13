// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/index"
)

// The automatic onboarding path, end to end: `mesh install` builds the first index and
// the SessionStart hook it writes reads it. Both used to fail silently, and silence is
// the whole defect: the user is a stranger on their first run, the next thing they see
// is their agent failing to connect to an MCP server with no message at all.

// TestOrientNeverCreatesAnIndex pins the SessionStart hook against index.Open's
// create-on-absence behaviour.
//
// corruptIndexError prints its own repair ("rm -f mesh.db mesh.db-wal mesh.db-shm"), and
// the operator who followed it got an empty 225KB database back on their very next
// session, because SessionStart -> `mesh orient` opened writable and index.Open CREATES.
// From then on the vault was indistinguishable from an indexed one: `mesh search` said
// "no matches" rather than "no index at <path>", and every read-only surface opened the
// empty file happily. So orient must leave a vault with no index exactly as it found it,
// and say which command builds one.
func TestOrientNeverCreatesAnIndex(t *testing.T) {
	vault := t.TempDir()
	writeNote(t, vault, "one.md", "# One\n\nA note.\n")
	dbPath := filepath.Join(vault, ".mesh", "mesh.db")

	out, err := runCLI(t, orientCmd(), vault, "--hook")
	if err != nil {
		t.Fatalf("orient over an unindexed vault must still orient the session, got: %v", err)
	}
	if _, statErr := os.Stat(dbPath); !os.IsNotExist(statErr) {
		t.Errorf("orient created %s: the SessionStart hook must never mint an index, or the "+
			"documented rm-and-rebuild repair leaves an empty mesh that every surface serves as real",
			dbPath)
	}
	if _, statErr := os.Stat(filepath.Join(vault, ".mesh")); !os.IsNotExist(statErr) {
		t.Errorf("orient created %s/.mesh over a vault with no index", vault)
	}

	var env struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if jsonErr := json.Unmarshal([]byte(strings.TrimSpace(out)), &env); jsonErr != nil {
		t.Fatalf("--hook must emit the SessionStart envelope even with no index, got %q: %v", out, jsonErr)
	}
	if env.HookSpecificOutput.HookEventName != "SessionStart" {
		t.Errorf("hookEventName = %q, want SessionStart", env.HookSpecificOutput.HookEventName)
	}
	if !strings.Contains(env.HookSpecificOutput.AdditionalContext, "mesh index") {
		t.Errorf("the orientation for an unindexed vault must name the command that builds one "+
			"(`mesh index`), got:\n%s", env.HookSpecificOutput.AdditionalContext)
	}
}

// TestOrientReadsAnExistingIndex is the other half: the read-only switch must not have
// cost the feature its content.
func TestOrientReadsAnExistingIndex(t *testing.T) {
	vault := t.TempDir()
	writeNote(t, vault, "hub.md", "# Hub\n\nLinks to [[leaf]].\n")
	writeNote(t, vault, "leaf.md", "# Leaf\n\nA leaf.\n")
	store, err := index.Open(vault)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := index.Reindex(store, vault); err != nil {
		t.Fatal(err)
	}
	store.Close()

	out, err := runCLI(t, orientCmd(), vault)
	if err != nil {
		t.Fatalf("orient over an indexed vault: %v", err)
	}
	if !strings.Contains(out, "Entry points") || !strings.Contains(out, "Hub") {
		t.Errorf("orient did not render the indexed vault, got:\n%s", out)
	}
}

// TestInstallFailsLoudlyWhenTheIndexCannotBeBuilt is BLOCKER 6.
//
// indexVault used to discard the open error, the reindex error AND the count error, so
// `mesh install` registered the MCP server over a vault whose index cannot open, armed a
// welcome that can never fire, printed "Done. Start a new agent session" and exited 0.
// The user restarts, the MCP server dies at startup, and Claude Code reports that as a
// bare connect timeout: first-run setup is the one place a failure must be loud.
//
// The vault here is writable (so arming the welcome would succeed, as it used to) but
// .mesh/mesh.db is a directory, which is unopenable and is NOT the corrupt-file class
// OpenRebuild discards. That is what separates "install refused" from "install could not
// write anything at all".
func TestInstallFailsLoudlyWhenTheIndexCannotBeBuilt(t *testing.T) {
	vault := t.TempDir()
	proj := t.TempDir()
	writeNote(t, vault, "one.md", "# One\n\nA note.\n")
	blocked := filepath.Join(vault, ".mesh", "mesh.db")
	if err := os.MkdirAll(blocked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blocked, "keep"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := runCLI(t, installCmd(), vault, "--dir", proj)
	if err == nil {
		t.Fatalf("install returned success over a vault whose index cannot be built; output:\n%s", out)
	}
	if !strings.Contains(err.Error(), "mesh index") {
		t.Errorf("the install failure must name the remedy (`mesh index <vault>`), got: %v", err)
	}
	if strings.Contains(out, "Done.") {
		t.Errorf("install printed Done. over a vault with no working index:\n%s", out)
	}
	if _, statErr := os.Stat(filepath.Join(vault, ".mesh", "onboard")); statErr == nil {
		t.Error("install armed the first-run welcome over a vault with no working index: " +
			"the welcome fires from the SessionStart hook, which cannot run without an index")
	}
}

// TestInstallRebuildsACorruptIndex is the other half of BLOCKER 6: install IS the first
// build, so an unreadable index file is the thing being replaced, not a dead end. Before
// this, install silently left the corrupt file in place and reported success.
func TestInstallRebuildsACorruptIndex(t *testing.T) {
	vault := t.TempDir()
	proj := t.TempDir()
	writeNote(t, vault, "one.md", "# One\n\nA note.\n")
	if err := os.MkdirAll(filepath.Join(vault, ".mesh"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vault, ".mesh", "mesh.db"), []byte("this is not a sqlite database"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := runCLI(t, installCmd(), vault, "--dir", proj)
	if err != nil {
		t.Fatalf("install must discard a corrupt index and rebuild it, got: %v\n%s", err, out)
	}
	if !strings.Contains(out, "corrupt") {
		t.Errorf("install discarded a file on the user's disk without saying so:\n%s", out)
	}
	store, err := index.OpenReadOnly(vault)
	if err != nil {
		t.Fatalf("install reported success but the index still does not open: %v", err)
	}
	defer store.Close()
	n, err := store.Count("notes")
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Error("install reported success over an index holding no notes")
	}
}
