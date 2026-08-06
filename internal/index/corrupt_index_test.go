// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// junkDB is enough non-SQLite bytes that the driver reads a header and rejects it. A
// zero-length file is a VALID empty database to SQLite, so the corruption has to be real
// content, not an absent one.
var junkDB = []byte(strings.Repeat("not a database, just bytes. ", 160))

func corruptVault(t *testing.T) (root, dbPath string) {
	t.Helper()
	root = t.TempDir()
	dbPath = filepath.Join(root, ".mesh", "mesh.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dbPath, junkDB, 0o644); err != nil {
		t.Fatal(err)
	}
	return root, dbPath
}

// TestIsUnreadableDBRecognisesBothCorruptionCodes pins the string match, exactly as
// TestIsBusyRecognisesSQLiteLockErrors does for the other operator-facing SQLite failure.
// SQLITE_BUSY got that treatment and neither corruption code did. Both codes are here
// because covering one of the two is how this defect class recurs: 26 is junk over the
// whole file, 11 is a half-written page, and only 11 happens by accident.
func TestIsUnreadableDBRecognisesBothCorruptionCodes(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil is not corruption", nil, false},
		{"the error the operator actually sees", errors.New("apply schema: file is not a database (26)"), true},
		{"symbolic code alone", errors.New("SQLITE_NOTADB"), true},
		{"a half-written page is corruption too", errors.New("apply schema: database disk image is malformed (11)"), true},
		{"symbolic code 11 alone", errors.New("SQLITE_CORRUPT"), true},
		{"a busy lock must NOT be treated as corruption", errors.New("database is locked (5) (SQLITE_BUSY)"), false},
		{"a disk error must NOT be treated as corruption", errors.New("disk I/O error"), false},
		{"a missing table must NOT be treated as corruption", errors.New("no such table: notes"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isUnreadableDB(tc.err); got != tc.want {
				t.Errorf("isUnreadableDB(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestOpenOnCorruptIndexIsActionable: the bare driver message was "apply schema: file is
// not a database (26)" from search, doctor, health AND index alike, which names no file,
// no remedy, and leaves the operator unsure whether their notes just died. Every element
// of the replacement is asserted, because each one is a question the old message left open.
func TestOpenOnCorruptIndexIsActionable(t *testing.T) {
	root, dbPath := corruptVault(t)

	s, err := Open(root)
	if err == nil {
		s.Close()
		t.Fatal("Open succeeded on a corrupt database")
	}
	if !errors.Is(err, ErrIndexCorrupt) {
		t.Fatalf("Open error does not match ErrIndexCorrupt: %v", err)
	}
	abs, aerr := filepath.Abs(dbPath)
	if aerr != nil {
		t.Fatal(aerr)
	}
	msg := err.Error()
	for _, want := range []string{
		abs,                      // which file
		"notes are safe",         // whether anything was lost
		"mesh index",             // the repair command
		"rm -f",                  // the manual repair
		"file is not a database", // the driver cause is still there
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("corrupt-index error is missing %q\n  got: %s", want, msg)
		}
	}
}

// TestRecoverCorruptIndexOnlyDeletesOnCorruption is the safety half of the auto-repair.
// `mesh index` is allowed to delete the database because it is about to rewrite it, but
// ONLY for the one error class where the file is already unreadable. A busy lock or a
// permission problem leaves a perfectly good index (and a vectors table that costs real
// money to rebuild), so those must come back untouched.
func TestRecoverCorruptIndexOnlyDeletesOnCorruption(t *testing.T) {
	tests := []struct {
		name        string
		openErr     error
		wantDeleted bool
	}{
		{"corrupt index is discarded", corruptIndexError("/v", "/v/.mesh/mesh.db", errors.New("file is not a database (26)")), true},
		{"a busy lock is NOT a reason to delete", errors.New("apply schema: database is locked (5) (SQLITE_BUSY)"), false},
		{"a permission error is NOT a reason to delete", os.ErrPermission, false},
		{"a disk error is NOT a reason to delete", errors.New("disk I/O error"), false},
		{"an unrelated failure is NOT a reason to delete", errors.New("no such table: notes"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			dbPath := filepath.Join(dir, "mesh.db")
			for _, p := range indexFiles(dbPath) {
				if err := os.WriteFile(p, junkDB, 0o644); err != nil {
					t.Fatal(err)
				}
			}

			deleted, err := recoverCorruptIndex(dbPath, tc.openErr)
			if deleted != tc.wantDeleted {
				t.Errorf("recoverCorruptIndex deleted = %v, want %v (err %v)", deleted, tc.wantDeleted, err)
			}
			for _, p := range indexFiles(dbPath) {
				_, statErr := os.Stat(p)
				if tc.wantDeleted && statErr == nil {
					t.Errorf("%s still exists after a corrupt-index recovery", filepath.Base(p))
				}
				if !tc.wantDeleted && statErr != nil {
					t.Errorf("%s was deleted on a non-corruption error (%v); that is data loss", filepath.Base(p), tc.openErr)
				}
			}
			if !tc.wantDeleted && !errors.Is(err, tc.openErr) {
				t.Errorf("the original open error must be returned untouched, got %v", err)
			}
		})
	}
}

// TestOpenRebuildRecoversAndOtherwiseLeavesTheIndexAlone: `mesh index` is the command that
// rebuilds, so it must not be blocked by the very file it replaces. The healthy case is
// asserted in the same test because "recovers" is worthless if it also throws away a good
// index (and its embed cache) on every ordinary run.
func TestOpenRebuildRecoversAndOtherwiseLeavesTheIndexAlone(t *testing.T) {
	t.Run("corrupt index is rebuilt", func(t *testing.T) {
		root, dbPath := corruptVault(t)

		s, recovered, err := OpenRebuild(root)
		if err != nil {
			t.Fatalf("OpenRebuild on a corrupt index: %v", err)
		}
		defer s.Close()
		if !recovered {
			t.Fatal("OpenRebuild did not report that it discarded the corrupt index")
		}
		if _, err := s.Count("notes"); err != nil {
			t.Fatalf("rebuilt index is not queryable: %v", err)
		}
		fi, err := os.Stat(dbPath)
		if err != nil {
			t.Fatalf("rebuilt database is missing: %v", err)
		}
		if fi.Size() == int64(len(junkDB)) {
			t.Fatal("the junk file is still in place; nothing was rebuilt")
		}
	})

	t.Run("a healthy index is opened, not discarded", func(t *testing.T) {
		root := t.TempDir()
		s, err := Open(root)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.IncrMetric("openrebuild_probe", 7); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Metric("openrebuild_probe"); err != nil { // forces the telemetry flush
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}

		s2, recovered, err := OpenRebuild(root)
		if err != nil {
			t.Fatal(err)
		}
		defer s2.Close()
		if recovered {
			t.Fatal("OpenRebuild discarded a healthy index")
		}
		got, err := s2.Metric("openrebuild_probe")
		if err != nil {
			t.Fatal(err)
		}
		if got != 7 {
			t.Fatalf("counter is %d, want 7: the healthy index lost its rows, so it was discarded rather than opened", got)
		}
	})
}

// TestPageCorruptIndexIsRecoveredToo is the SECOND corruption code. SQLITE_NOTADB (26)
// only fires when the file is not SQLite at all, which is what you get from `dd` over the
// whole file. The far more ordinary damage (a half-written page from a crash, a bad
// sector, a copy taken mid-write) leaves the 16-byte header intact and SQLite reports
// "database disk image is malformed" (SQLITE_CORRUPT, 11) instead. Covering only code 26
// left that case with the exact dead end the fix set out to remove: verified before the
// second half landed, a page-corrupt index gave
//
//	apply schema: database disk image is malformed (11)
//
// from Open AND from OpenRebuild, so `mesh index` was still blocked by the file it exists
// to replace.
func TestPageCorruptIndexIsRecoveredToo(t *testing.T) {
	root := t.TempDir()
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(root, ".mesh", "mesh.db")
	b, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	// Keep SQLite's 100-byte file header (so the magic still matches and this is NOT
	// code 26) and wreck the rest of page 1, where the schema lives.
	for i := 100; i < len(b) && i < 4096; i++ {
		b[i] = 0x41
	}
	if err := os.WriteFile(dbPath, b, 0o644); err != nil {
		t.Fatal(err)
	}

	_, openErr := Open(root)
	if openErr == nil {
		t.Fatal("Open succeeded on a page-corrupt database")
	}
	if !errors.Is(openErr, ErrIndexCorrupt) {
		t.Fatalf("a malformed page is corruption too, but the error does not match ErrIndexCorrupt: %v", openErr)
	}
	if !strings.Contains(openErr.Error(), "notes are safe") {
		t.Errorf("page-corrupt error is not the actionable one:\n%s", openErr)
	}

	s2, recovered, err := OpenRebuild(root)
	if err != nil {
		t.Fatalf("OpenRebuild on a page-corrupt index: %v", err)
	}
	defer s2.Close()
	if !recovered {
		t.Fatal("OpenRebuild did not report that it discarded the page-corrupt index")
	}
	if _, err := s2.Count("notes"); err != nil {
		t.Fatalf("rebuilt index is not queryable: %v", err)
	}
}
