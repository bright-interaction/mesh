// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
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
		found, err := unclosedQueryCalls(rel, raw)
		missing = append(missing, found...)
		return err
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

// Inspect the call whose results are assigned, not arbitrary Query text nested
// inside its arguments. URL.Query in store.Inspect(..., r.URL.Query().Get(...))
// does not return SQL rows. Parsing also catches multiline assignments and error
// variables with names other than err, without treating comments as call sites.
func unclosedQueryCalls(path string, raw []byte) ([]string, error) {
	positions := token.NewFileSet()
	file, err := parser.ParseFile(positions, path, raw, 0)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(raw), "\n")
	var missing []string
	ast.Inspect(file, func(node ast.Node) bool {
		assignment, ok := node.(*ast.AssignStmt)
		if !ok || len(assignment.Lhs) != 2 || len(assignment.Rhs) != 1 {
			return true
		}
		call, ok := assignment.Rhs[0].(*ast.CallExpr)
		if !ok {
			return true
		}
		method, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || (method.Sel.Name != "Query" && method.Sel.Name != "QueryContext") {
			return true
		}
		rows, ok := assignment.Lhs[0].(*ast.Ident)
		if !ok {
			return true
		}
		i := positions.Position(assignment.Pos()).Line - 1
		// Preserve the existing close requirement and bounded look-ahead past the
		// multiline query and its error check; only call identification changes.
		end := i + 20
		if end > len(lines) {
			end = len(lines)
		}
		if !strings.Contains(strings.Join(lines[i:end], "\n"), "defer "+rows.Name+".Close()") {
			missing = append(missing, path+":"+itoa(i+1)+" ("+rows.Name+")")
		}
		return true
	})
	return missing, nil
}

func TestQueryCloseGuardClassifiesAssignedCall(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		leaks      int
	}{
		{"unclosed SQL query", `rows, err := db.Query("SELECT 1")`, 1},
		{"unclosed context query", "rows, queryErr :=\n db.QueryContext(ctx, \"SELECT 1\")", 1},
		{"reassigned query", `rows, err = db.Query("SELECT 1")`, 1},
		{"closed SQL query", "rows, err := db.Query(\"SELECT 1\")\n if err != nil { return }; defer rows.Close()", 0},
		{"nested URL query is not SQL", `req, err := store.Inspect(r.Context(), r.URL.Query().Get("user_code"), publicURL)`, 0},
		{"URL query has one result", `values := r.URL.Query()`, 0},
		{"comment is not a call", `// rows, err := db.Query("SELECT 1")`, 0},
		{"separate unclosed SQL remains visible", "req, err := store.Inspect(r.URL.Query())\n rows, err := db.Query(\"SELECT 1\")", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			found, err := unclosedQueryCalls("fixture.go", []byte("package fixture\nfunc check() {\n"+tc.body+"\n}\n"))
			if err != nil {
				t.Fatal(err)
			}
			if len(found) != tc.leaks {
				t.Fatalf("got %v, want %d unclosed queries", found, tc.leaks)
			}
		})
	}
	if _, err := unclosedQueryCalls("broken.go", []byte("not Go syntax")); err == nil {
		t.Fatal("invalid source must fail the guard, not silently skip it")
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
