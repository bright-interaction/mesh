// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
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
func TestEveryQueryClosesItsRows(t *testing.T) {
	queryRe := regexp.MustCompile(`(\w+), err :?= [^\n]*\.Query(Context)?\(`)
	var missing []string

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatal(err)
		}
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
				missing = append(missing, name+":"+itoa(i+1)+" ("+v+")")
			}
		}
	}
	if len(missing) > 0 {
		t.Errorf("these Query call sites never close their rows, which pins a WAL read snapshot "+
			"for the life of the process and starves other processes' writes into SQLITE_BUSY:\n  %s\n"+
			"Add `defer <rows>.Close()` immediately after the error check.",
			strings.Join(missing, "\n  "))
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
