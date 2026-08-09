// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// readOnlyCommands are the CLI commands that only READ the index. Each one is a
// function in this package that builds a *cobra.Command, and none of them may open a
// writable store.
//
// This is the CLI half of the single-writer split. The MCP server, the TUI and the web
// viewer were each moved to a read-only store one at a time, and every time the reason
// was the same: a process that only reads has no business taking the SQLite write lock,
// because opening a writable store applies the schema, and applying the schema against a
// live owning writer fails with SQLITE_BUSY on an index that is perfectly fresh.
//
// `mesh doctor` was the one left behind, and it failed exactly that way on the live vault
// on 2026-08-10 while the owner was reconciling: "apply schema: another mesh process
// holds the write lock". The command reads two counts and a drift report. It never wrote
// anything.
//
// A command that genuinely writes (index, embed, health, sync, watch, extract) simply
// does not belong on this list.
var readOnlyCommands = map[string]string{
	"doctorCmd":        "counts rows and reports drift",
	"statusCmd":        "counts rows, loads the graph, reads vector stats",
	"guardsListCmd":    "reads stored gotchas",
	"guardsSuggestCmd": "reads stored gotchas",
}

// TestReadOnlyCommandsDoNotTakeTheWriteLock walks the AST of this package and fails if a
// command on the list above calls index.Open / index.OpenAt / index.OpenRebuild.
//
// AST rather than a runtime check, because the failure it guards against is not
// observable without a second live writer: on an idle laptop a writable open succeeds
// and everything looks fine. It only bites when the owner is busy, which is exactly when
// someone is running doctor to find out what is wrong.
func TestReadOnlyCommandsDoNotTakeTheWriteLock(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", nil, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, pkg := range pkgs {
		for path, file := range pkg.Files {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Recv != nil {
					continue
				}
				why, watched := readOnlyCommands[fn.Name.Name]
				if !watched {
					continue
				}
				seen[fn.Name.Name] = true
				ast.Inspect(fn, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					pkgIdent, ok := sel.X.(*ast.Ident)
					if !ok || pkgIdent.Name != "index" {
						return true
					}
					switch sel.Sel.Name {
					case "Open", "OpenAt", "OpenRebuild":
						t.Errorf("%s (%s) calls index.%s at %s: it only reads, so it must use "+
							"index.OpenReadOnly. A writable open applies the schema, which fails with "+
							"SQLITE_BUSY against a live owning writer on an index that is perfectly fresh.",
							fn.Name.Name, why, sel.Sel.Name, fset.Position(call.Pos()))
					}
					return true
				})
			}
		}
	}
	// A renamed or deleted command must not silently drop off the guard.
	for name := range readOnlyCommands {
		if !seen[name] {
			t.Errorf("%s is on the read-only list but no such function exists in this package; "+
				"rename it here or remove the entry, do not leave the guard pointing at nothing", name)
		}
	}
}
