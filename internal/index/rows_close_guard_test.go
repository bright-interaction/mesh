// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestEveryQueryClosesItsRows is the guard for the WAL-pinning leak proven in
// TestWalPinnedByLeakedRows. A `for rows.Next()` loop only closes the result set when it
// runs to exhaustion; any early return inside the loop leaks it, and a leaked Rows pins a
// WAL read snapshot for the life of the process. `defer rows.Close()` is correct at every
// call site regardless, so this requires it rather than trying to reason about which
// loops can return early.
//
// It walks the WHOLE MODULE, not just this package. The original leak happened to live in
// internal/index, but nothing about the failure is specific to it: internal/hub is a
// long-running server on its own SQLite database, and its own store.go says its
// concurrency "mirrors internal/index". A package-local guard would have let the identical
// bug land there and would have looked green while doing it.
func TestEveryQueryClosesItsRows(t *testing.T) {
	queryRe := regexp.MustCompile(`(\w+), err :?= [^\n]*\.Query(Context)?\(`)
	var missing []string

	root, err := moduleRoot()
	if err != nil {
		t.Fatal(err)
	}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Vendored and generated trees are not ours to fix.
			if n := d.Name(); n == "vendor" || n == "node_modules" || n == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		lines := strings.Split(string(raw), "\n")
		for i, l := range lines {
			m := queryRe.FindStringSubmatch(l)
			if m == nil {
				continue
			}
			v := m[1]
			// Look ahead past the multi-line query text and its error check.
			end := i + 20
			if end > len(lines) {
				end = len(lines)
			}
			if !strings.Contains(strings.Join(lines[i:end], "\n"), "defer "+v+".Close()") {
				missing = append(missing, rel+":"+itoa(i+1)+" ("+v+")")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) > 0 {
		t.Errorf("these Query call sites never close their rows, which pins a WAL read snapshot "+
			"for the life of the process and starves other processes' writes into SQLITE_BUSY:\n  %s\n"+
			"Add `defer <rows>.Close()` immediately after the error check.",
			strings.Join(missing, "\n  "))
	}
}

// moduleRoot walks up from the test's working directory to the directory holding go.mod,
// so the guard covers the module wherever the test happens to be run from.
func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("no go.mod found above the test working directory")
		}
		dir = parent
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
