// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package meshclient

import (
	"os"
	"path/filepath"
	"testing"
)

// TestWriteFileAtomicPreservesNoteMode pins the permission bits the durable writer lands
// a note with.
//
// The durability pass moved this writer from os.WriteFile to os.CreateTemp + rename. A
// rename INSTALLS the temp file, mode and all, and os.CreateTemp makes its file 0600, so
// every note this writer touched came back 0600 no matter what it was before: a note that
// was 0644 was silently narrowed on the first sync that pulled a delta for it. Nothing
// fails visibly when that happens. It shows up later as another user, another process, an
// editor, a backup agent or a container running as a different uid no longer being able to
// read a note it could read yesterday.
//
// Both directions are asserted on purpose. Restoring the destination mode has to be
// exactly that, restoration: a note the author deliberately chmod'd 0600 must NOT be
// widened back to 0644 by a helpful default.
//
// Umask does not affect these assertions. It filters the mode passed to a CREATE
// (os.WriteFile, os.OpenFile, os.CreateTemp), never an explicit chmod(2). The setup below
// chmods after creating, and the writer chmods the temp file, so both sides are exact
// under any umask. Measured here at umask 022.
func TestWriteFileAtomicPreservesNoteMode(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root's umask and permission handling make the observed bits unreliable")
	}
	cases := []struct {
		name     string
		existing os.FileMode // 0 means the note does not exist yet
		want     os.FileMode
		why      string
	}{
		{
			name:     "world readable note stays world readable",
			existing: 0o644,
			want:     0o644,
			why:      "a synced note narrowed to 0600 stops being readable by the indexer, a backup agent, or a container on another uid",
		},
		{
			name:     "deliberately locked note stays locked",
			existing: 0o600,
			want:     0o600,
			why:      "restoring the mode must not WIDEN a note the author locked down",
		},
		{
			name:     "group readable note keeps its group bit",
			existing: 0o640,
			want:     0o640,
			why:      "the destination's own bits are the contract, not one of two hard-coded modes",
		},
		{
			name:     "new note lands at the estate note mode",
			existing: 0,
			want:     0o644,
			why:      "a note pulled fresh from the hub must not arrive private when every other note is not",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "notes", "n.md")
			if tc.existing != 0 {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("---\nid: n\n---\n# old\n"), tc.existing); err != nil {
					t.Fatal(err)
				}
				// Explicit chmod, because WriteFile's mode is filtered by umask.
				if err := os.Chmod(path, tc.existing); err != nil {
					t.Fatal(err)
				}
			}

			if err := writeFileAtomic(path, []byte("---\nid: n\n---\n# new\n"), 0o644); err != nil {
				t.Fatalf("writeFileAtomic: %v", err)
			}

			fi, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if got := fi.Mode().Perm(); got != tc.want {
				t.Errorf("mode = %04o, want %04o. %s", got, tc.want, tc.why)
			}
		})
	}
}

// TestSyncStateStaysPrivate pins the mode of .mesh/sync.json, which is NOT a note and must
// not inherit the note default.
//
// writeState was os.WriteFile(..., 0o600) from the commit that created it, next to the
// 0600 credentials file. The durability pass routed it through writeFileAtomic, which kept
// 0600 by accident because os.CreateTemp creates at 0600. Restoring the note mode on top of
// that would have turned the accident into a 0644 default and published the file on the
// next fresh join, which is the same "a fix quietly changed the permissions" failure the
// mode work exists to prevent, one directory over.
//
// It matters because of what is in the file: HeadSHA plus a vault-relative path and a
// content hash for EVERY note. That is the full table of contents of a private vault, and
// the hashes let anyone who can read it confirm a guess about a note's contents. .mesh is
// only 0700 when meshclient created it; internal/ingest/state.go creates it 0755, so the
// containing directory cannot be relied on to hide anything.
func TestSyncStateStaysPrivate(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root's umask and permission handling make the observed bits unreliable")
	}
	cases := []struct {
		name    string
		dirPerm os.FileMode // mode .mesh is pre-created with; 0 means let writeState create it
		want    os.FileMode
		why     string
	}{
		{
			name:    "fresh vault, mesh dir created by writeState",
			dirPerm: 0,
			want:    0o600,
			why:     "a new sync.json must land at the mode its os.WriteFile used, not the note default",
		},
		{
			name:    "vault already indexed, so .mesh is the 0755 internal/index made it",
			dirPerm: 0o755,
			want:    0o600,
			why:     "with a world-traversable .mesh, 0644 here publishes every note path in the vault",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.dirPerm != 0 {
				if err := os.MkdirAll(filepath.Join(dir, ".mesh"), tc.dirPerm); err != nil {
					t.Fatal(err)
				}
				// Explicit chmod, because MkdirAll's mode is filtered by umask.
				if err := os.Chmod(filepath.Join(dir, ".mesh"), tc.dirPerm); err != nil {
					t.Fatal(err)
				}
			}
			st := syncState{HeadSHA: "abc123", Hashes: map[string]string{"notes/private.md": "deadbeef"}}
			if err := writeState(dir, st); err != nil {
				t.Fatalf("writeState: %v", err)
			}
			fi, err := os.Stat(statePath(dir))
			if err != nil {
				t.Fatal(err)
			}
			if got := fi.Mode().Perm(); got != tc.want {
				t.Errorf("sync.json mode = %04o, want %04o. %s", got, tc.want, tc.why)
			}
		})
	}
}

// TestSyncStateRewriteKeepsOperatorMode is the other direction: once sync.json exists, its
// own bits win, so an operator who widened or narrowed it is not overridden every round.
func TestSyncStateRewriteKeepsOperatorMode(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root's umask and permission handling make the observed bits unreliable")
	}
	for _, perm := range []os.FileMode{0o600, 0o640, 0o644} {
		t.Run(perm.String(), func(t *testing.T) {
			dir := t.TempDir()
			if err := writeState(dir, syncState{HeadSHA: "a"}); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(statePath(dir), perm); err != nil {
				t.Fatal(err)
			}
			if err := writeState(dir, syncState{HeadSHA: "b"}); err != nil {
				t.Fatal(err)
			}
			fi, err := os.Stat(statePath(dir))
			if err != nil {
				t.Fatal(err)
			}
			if got := fi.Mode().Perm(); got != perm {
				t.Errorf("sync.json mode = %04o, want %04o; a rewrite must keep the bits the file already has", got, perm)
			}
		})
	}
}
