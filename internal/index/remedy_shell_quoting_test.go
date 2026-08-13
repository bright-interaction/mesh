// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"database/sql"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// shellArgs hands s to a real /bin/sh and returns the arguments the shell produced. The
// remedy lines are graded by a shell because a shell is what runs them.
func shellArgs(t *testing.T, s string) []string {
	t.Helper()
	script := "set -- " + s + "\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done"
	out, err := exec.Command("/bin/sh", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("/bin/sh could not parse the remedy Mesh printed: %v\n  command: %s\n  output: %s", err, s, out)
	}
	trimmed := strings.TrimSuffix(string(out), "\n")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

// lineAfter returns the remainder of the line that starts at marker.
func lineAfter(t *testing.T, text, marker string) string {
	t.Helper()
	i := strings.Index(text, marker)
	if i < 0 {
		t.Fatalf("no %q in:\n%s", marker, text)
	}
	rest := text[i+len(marker):]
	if j := strings.IndexByte(rest, '\n'); j >= 0 {
		rest = rest[:j]
	}
	return strings.TrimSpace(rest)
}

// TestCorruptIndexRemedySurvivesAVaultPathWithASpace is the nastiest half of the
// shell-quoting defect, because it fails SILENTLY.
//
// The by-hand repair used to render as `rm -f <db> <db>-wal <db>-shm` with the path
// interpolated raw. Under a vault like `~/Documents/My Notes` that is six operands, none
// of which exist. rm -f says nothing about a missing operand and exits 0, so the operator
// reads a clean run as "index cleared", re-runs Mesh, and meets the identical corruption
// error with no idea why the repair did not take.
//
// The assertion is what a shell makes of the line: exactly the three files Mesh itself
// would delete, no more and no fewer.
func TestCorruptIndexRemedySurvivesAVaultPathWithASpace(t *testing.T) {
	root := filepath.Join(t.TempDir(), "My Notes")
	dbPath := filepath.Join(root, ".mesh", "mesh.db")

	msg := corruptIndexError(root, dbPath, errors.New("file is not a database (26)")).Error()

	got := shellArgs(t, "rm -f "+lineAfter(t, msg, "by hand: rm -f "))
	want := indexFiles(dbPath)
	if len(got) != len(want)+2 { // "rm" and "-f" are arguments here too
		t.Fatalf("the shell read the repair as %d operands, not the %d files it names:\n  printed: %s\n  sh saw: %q",
			len(got)-2, len(want), lineAfter(t, msg, "by hand: "), got)
	}
	for i, w := range want {
		if got[i+2] != w {
			t.Errorf("operand %d: sh saw %q, want %q (printed: %s)", i, got[i+2], w, lineAfter(t, msg, "by hand: "))
		}
	}

	// The rebuild command on the line above has to survive the same shell.
	repair := lineAfter(t, msg, "repair:  mesh index ")
	if i := strings.Index(repair, "   ("); i >= 0 {
		repair = strings.TrimSpace(repair[:i])
	}
	args := shellArgs(t, repair)
	if len(args) != 1 || args[0] != root {
		t.Errorf("`mesh index` repair: sh split %q into %q, want the one argument %q", repair, args, root)
	}
}

// TestSchemaMismatchRemedySurvivesAVaultPathWithASpace is the twin remedy: the same
// "fix: mesh index <path>" line, built in a different function a few lines up, which is
// exactly the shape of miss this estate keeps repeating.
func TestSchemaMismatchRemedySurvivesAVaultPathWithASpace(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "My Notes")
	dbPath := filepath.Join(root, ".mesh", "mesh.db")

	// A database this Mesh never stamped: no meta table, so schemaVersionInIndex answers
	// 0 and the version comparison reports the mismatch. That is all this test needs; it
	// is the message it is grading, not the detection.
	db, err := sql.Open("sqlite", filepath.Join(dir, "unstamped.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TABLE placeholder (x)"); err != nil {
		t.Fatal(err)
	}

	mErr := checkReadOnlySchemaVersion(db, root, dbPath)
	if mErr == nil {
		t.Fatal("expected a schema mismatch for an unstamped database")
	}
	msg := mErr.Error()

	fix := lineAfter(t, msg, "fix: mesh index ")
	if i := strings.Index(fix, "   ("); i >= 0 {
		fix = strings.TrimSpace(fix[:i])
	}
	args := shellArgs(t, fix)
	if len(args) != 1 || args[0] != root {
		t.Errorf("schema-mismatch remedy: sh split %q into %q, want the one argument %q", fix, args, root)
	}
}

// TestNoOwnerRemedySurvivesAVaultPathWithASpace covers the third copy of the same
// pattern: NoOwnerRemedy builds `mesh mcp --vault <path> --watch`, `mesh watch <path>`
// and `mesh sync --watch <path>` from one interpolated argument, and it is the advice
// `mesh doctor`, `mesh status` and mesh_health all print.
func TestNoOwnerRemedySurvivesAVaultPathWithASpace(t *testing.T) {
	root := filepath.Join(t.TempDir(), "My Notes")
	msg := NoOwnerRemedy(root)

	for _, marker := range []string{"mesh mcp --vault ", "mesh watch ", "mesh sync --watch "} {
		i := strings.Index(msg, marker)
		if i < 0 {
			t.Fatalf("no %q in the no-owner remedy:\n%s", marker, msg)
		}
		rest := msg[i:]
		if j := strings.IndexByte(rest, '`'); j >= 0 {
			rest = rest[:j]
		}
		args := shellArgs(t, rest)
		found := false
		for _, a := range args {
			if a == root {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no-owner remedy %q: sh split %q into %q, none equal to %q", marker, rest, args, root)
		}
	}
}
