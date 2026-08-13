// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Anything that creates or writes inside <vault>/.mesh must do so owner-only.
//
// The comment on vaultMeshDirMode said "three packages create it"; there were five, and
// two were still on 0755 with 0644 files inside (.mesh/extract.log carries the LLM
// extraction subprocess's stdout over a session transcript, .mesh/onboard and
// .mesh/sync.json sit beside the credentials file). A counted-set comment cannot notice a
// sixth copy, and neither could TestVaultMeshDirModeAgreesWithOtherWriters, which
// compares one constant to another and names no call site.
//
// This enumerates the call sites instead: any MkdirAll/WriteFile/OpenFile whose path
// expression mentions .mesh must carry an owner-only mode.
func TestEveryMeshDirWriterIsOwnerOnly(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var bad []string
	seen := 0

	err = filepath.Walk(root, func(path string, info os.FileInfo, werr error) error {
		if werr != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			if info != nil && info.IsDir() && (info.Name() == ".git" || info.Name() == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil
		}
		src, _ := os.ReadFile(path)
		rel, _ := filepath.Rel(root, path)
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "os" {
				return true
			}
			switch sel.Sel.Name {
			case "MkdirAll", "WriteFile", "OpenFile":
			default:
				return true
			}
			// The mode is the last argument in all three signatures.
			if len(call.Args) < 2 {
				return true
			}
			start := fset.Position(call.Pos()).Offset
			end := fset.Position(call.End()).Offset
			if start < 0 || end > len(src) {
				return true
			}
			text := string(src[start:end])
			// Only the .mesh writers. A note directory is the user's markdown and stays
			// world-readable on purpose.
			if !strings.Contains(text, ".mesh") && !strings.Contains(text, "meshDir") && !strings.Contains(text, "MeshDir") {
				return true
			}
			seen++
			mode, _ := call.Args[len(call.Args)-1].(*ast.BasicLit)
			if mode == nil {
				return true // a named constant; vaultMeshDirMode and friends are checked elsewhere
			}
			if v := mode.Value; v != "0o700" && v != "0o600" {
				bad = append(bad, rel+": "+sel.Sel.Name+" mode "+v+" -> "+firstLine(text))
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen == 0 {
		t.Fatal("matched no .mesh writers at all; this guard has rotted and is checking nothing")
	}
	if len(bad) > 0 {
		t.Errorf("%d writer(s) into <vault>/.mesh use a world-readable mode. "+
			".mesh holds the index, the credentials file and the extraction log; use 0o700 for "+
			"directories and 0o600 for files.\n  %s", len(bad), strings.Join(bad, "\n  "))
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
