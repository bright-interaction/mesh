// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// The index holds the full text of every note. Both the directory and the files inside
// it must be owner-only, on every entry point that can CREATE them.
//
// This test enumerates the constructors instead of asserting the invariant once, which
// is the specific reason the previous guard missed this: the 0700 constant landed on
// Open and OpenAt kept its own MkdirAll(0755), so a test that opened a vault the normal
// way was green over a hub index that was world-traversable. A new constructor that
// creates the index without narrowing it has to be added to this table, and a reviewer
// who does not add it is the one case this cannot catch.
func TestIndexCreationPathsAreOwnerOnly(t *testing.T) {
	openers := []struct {
		name string
		open func(t *testing.T, vault string) (*Store, string)
	}{
		{
			// The path every CLI command takes: <vault>/.mesh.
			name: "Open",
			open: func(t *testing.T, vault string) (*Store, string) {
				s, err := Open(vault)
				if err != nil {
					t.Fatal(err)
				}
				return s, filepath.Join(vault, ".mesh")
			},
		},
		{
			// The path the hub takes: an index directory OUTSIDE the served vault,
			// created by this call and by nothing else, so nothing else can have
			// narrowed it first.
			name: "OpenAt",
			open: func(t *testing.T, vault string) (*Store, string) {
				dir := filepath.Join(t.TempDir(), "hub-index")
				s, err := OpenAt(vault, dir)
				if err != nil {
					t.Fatal(err)
				}
				return s, dir
			},
		},
	}

	for _, tc := range openers {
		t.Run(tc.name, func(t *testing.T) {
			store, dir := tc.open(t, t.TempDir())
			defer store.Close()

			fi, err := os.Stat(dir)
			if err != nil {
				t.Fatal(err)
			}
			if perm := fi.Mode().Perm(); perm&0o077 != 0 {
				t.Errorf("index dir %s is %v, want no group/other bits", dir, perm)
			}

			// mesh.db must be narrow in its own right, not merely sheltered by the
			// directory: the bytes outlive the directory the moment they are copied,
			// synced or backed up.
			db := filepath.Join(dir, "mesh.db")
			assertOwnerOnly(t, db)
			// -wal holds the newest note bodies until a checkpoint folds them back.
			assertOwnerOnly(t, db+"-wal")
			assertOwnerOnly(t, db+"-shm")
		})
	}
}

// assertOwnerOnly fails when p exists with group or other bits. A file that does not
// exist yet is not a failure: SQLite creates -wal/-shm lazily, and when it does create
// them it copies the main database's mode, which this test has already pinned.
func assertOwnerOnly(t *testing.T, p string) {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("%s is %v, want no group/other bits", p, perm)
	}
}

// An index created by an older mesh is already on disk at 0644 in a 0755 directory.
// Opening it must REPAIR that, not just avoid repeating it, which is the same reasoning
// ensureVaultMeshDir documents for the directory: MkdirAll and the file-create mode both
// apply only on creation, so tightening a constant alone leaves every existing vault
// exposed forever.
func TestOpenNarrowsAnIndexLeftWideByAnOlderMesh(t *testing.T) {
	vault := t.TempDir()
	store, err := Open(vault)
	if err != nil {
		t.Fatal(err)
	}
	store.Close()

	dir := filepath.Join(vault, ".mesh")
	db := filepath.Join(dir, "mesh.db")
	// Put it back the way an older mesh left it.
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(db, 0o644); err != nil {
		t.Fatal(err)
	}
	// Prove the premise: the test must start from the wide state it claims to repair.
	if fi, err := os.Stat(db); err != nil {
		t.Fatal(err)
	} else if fi.Mode().Perm()&0o077 == 0 {
		t.Fatalf("fixture broken: mesh.db is %v, so this proves nothing", fi.Mode().Perm())
	}

	reopened, err := Open(vault)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()

	for _, p := range []string{dir, db} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if perm := fi.Mode().Perm(); perm&0o077 != 0 {
			t.Errorf("reopening left %s at %v; an existing wide index must be narrowed, not tolerated", p, fs.FileMode(perm))
		}
	}
}
