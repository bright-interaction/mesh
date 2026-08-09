// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package tui

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bright-interaction/mesh/internal/index"
)

// TestLocalBackendNeverWritesTheIndex. The TUI is a browser: it must not be a second
// writer against a vault that already has an owning one. It used to open a writable store
// and run a FULL reindex at startup, so opening the window took the write lock for the
// length of a whole-vault pass and then held a writable store for as long as the window
// stayed up.
//
// Asserted on the files rather than on the code, so it stays true however the backend is
// built: after browsing, mesh.db must be byte-for-byte what the owner left, and the WAL
// must hold no frames. (Opening a WAL database creates an EMPTY -wal file even read-only,
// which is not a write; frames in it are. The full-reindex version of this backend left
// megabytes of them.)
func TestLocalBackendNeverWritesTheIndex(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.md"),
		[]byte("---\nid: alpha\ntype: note\nwhen: 2026-01-01\n---\n# Alpha\nsearchable body text\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedIndex(t, dir) // the owning writer builds the index, then leaves

	before := indexFingerprint(t, dir)

	b, closeFn, err := NewLocalBackend(dir)
	if err != nil {
		t.Fatalf("the TUI could not open an index the owner had already built: %v", err)
	}
	if n := b.Notes(); len(n) != 1 || n[0].ID != "alpha" {
		t.Fatalf("the backend read %v, want the one indexed note", n)
	}
	if _, err := b.Search("searchable body"); err != nil {
		t.Fatalf("search: %v", err)
	}
	if _, err := b.Note("alpha"); err != nil {
		t.Fatalf("note: %v", err)
	}
	if err := closeFn(); err != nil {
		t.Fatal(err)
	}

	if after := indexFingerprint(t, dir); after != before {
		t.Fatalf("browsing modified the index:\n before %s\n after  %s", before, after)
	}
	if fi, err := os.Stat(filepath.Join(dir, ".mesh", "mesh.db-wal")); err == nil && fi.Size() > 0 {
		t.Fatalf("browsing left %d bytes of WAL frames, so it ran a write transaction", fi.Size())
	}
}

// TestLocalBackendRefusesAVaultWithNoIndex: the trade that comes with not indexing. It
// must be a clear refusal naming what to run, not an empty window that looks like an
// empty vault.
func TestLocalBackendRefusesAVaultWithNoIndex(t *testing.T) {
	_, _, err := NewLocalBackend(t.TempDir())
	if err == nil {
		t.Fatal("the TUI opened a vault with no index at all; it must say so instead of showing nothing")
	}
	if !containsAll(err.Error(), "no index", "mesh") {
		t.Fatalf("the refusal does not name what to run: %v", err)
	}
}

// seedIndex plays the part of the single owning writer (`mesh index`, `mesh watch`): it
// builds the index once and closes. The TUI reads what it left.
func seedIndex(t *testing.T, vaultRoot string) {
	t.Helper()
	owner, err := index.Open(vaultRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if _, _, err := index.ReindexFull(owner, vaultRoot); err != nil {
		t.Fatal(err)
	}
}

func indexFingerprint(t *testing.T, vaultRoot string) string {
	t.Helper()
	p := filepath.Join(vaultRoot, ".mesh", "mesh.db")
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return "mesh.db=" + itoa(fi.Size()) + "/" + itoa(int64(checksum(b)))
}

func checksum(b []byte) uint32 {
	var h uint32 = 2166136261
	for _, c := range b {
		h ^= uint32(c)
		h *= 16777619
	}
	return h
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var d []byte
	for n > 0 {
		d = append([]byte{byte('0' + n%10)}, d...)
		n /= 10
	}
	return string(d)
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		found := false
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
