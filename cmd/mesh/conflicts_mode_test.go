// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestResolveWriteAtomicPreservesNoteMode is the `mesh conflicts resolve` half of the
// mode-preservation rule.
//
// See pkg/meshclient/vault_mode_test.go for the full reasoning. This writer is the one
// that restores the LOSING side of a true conflict over the note, so it runs on a file the
// user has already been editing by hand, which is exactly the file most likely to carry
// permissions somebody chose on purpose.
//
// Umask does not affect these assertions: it filters CREATE modes, not chmod(2), and both
// the setup and the writer chmod explicitly.
func TestResolveWriteAtomicPreservesNoteMode(t *testing.T) {
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
			why:      "resolving a conflict must not narrow the note it resolves",
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
			why:      "take-mine on a note deleted upstream recreates the path; it must not arrive private",
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
				if err := os.WriteFile(path, []byte("---\nid: n\n---\n# hub\n"), tc.existing); err != nil {
					t.Fatal(err)
				}
				// Explicit chmod, because WriteFile's mode is filtered by umask.
				if err := os.Chmod(path, tc.existing); err != nil {
					t.Fatal(err)
				}
			}

			if err := writeFileAtomic(path, []byte("---\nid: n\n---\n# mine\n")); err != nil {
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

// TestEveryTempRenameWriterRestoresTheMode is the estate-wide half, and the reason it
// exists is that the per-writer tests above cannot see a SIXTH copy of this writer.
//
// Every temp+rename writer in this module has the same latent bug the moment it is
// written: os.CreateTemp creates its file 0600, os.Rename installs the temp file's mode
// over the destination, so the destination's permissions become 0600 unless the writer
// says otherwise. The durability sweep converted four writers to temp+rename and three of
// them shipped without the chmod. That is the same "fixed one twin, shipped the others"
// shape this file's sibling guard already exists for, so it gets the same treatment:
// discover the writers from the AST, do not name them.
//
// Parsed WITHOUT comments on purpose. A body that only MENTIONS a chmod in prose contains
// no chmod at all, and a text scan that reads comments reports it as guarded. That exact
// decoy has defeated a source-text guard in this repo twice.
func TestEveryTempRenameWriterRestoresTheMode(t *testing.T) {
	root := moduleRoot(t)
	writers := findTempRenameWriters(t, root)
	// A discovery guard that discovers nothing passes vacuously, which is how the twins
	// survived last time. Pin the floor at the temp+rename writers findable in the OPEN
	// core, not in this monorepo checkout: the same test runs against the filtered public
	// mirror, where one of the five lives in a stripped pro path. See the equivalent note
	// in atomic_write_durability_test.go. The monorepo is a superset, so one number serves
	// both trees and a broken scanner still finds ~0.
	if len(writers) < 4 {
		t.Fatalf("found only %d temp+rename writer(s); the module has at least five, so the "+
			"scanner is broken rather than the code", len(writers))
	}
	for _, w := range writers {
		t.Run(w.key, func(t *testing.T) {
			chmodAt := strings.Index(w.body, ".Chmod(")
			if chmodAt < 0 {
				t.Errorf("%s creates a temp file and renames it into place without ever setting its "+
					"mode. os.CreateTemp makes the file 0600 and the rename installs that mode over "+
					"the destination, so every file this touches is silently narrowed to owner-only. "+
					"Stat the destination first and chmod the temp file to the mode it already has "+
					"(0644 when it does not exist yet), the way internal/vault/migrate.go "+
					"WriteNoteAtomic does.", w.key)
				return
			}
			if renameAt := strings.Index(w.body, "os.Rename("); renameAt >= 0 && chmodAt > renameAt {
				t.Errorf("%s chmods AFTER the rename, so the destination is briefly published at the "+
					"temp file's 0600 and the chmod races every reader in that window", w.key)
			}
		})
	}

	// Naming them is redundant with the discovery above, deliberately: if a refactor moves
	// or renames one so the scanner stops seeing it, this says so instead of the suite
	// going quietly green on four.
	seen := map[string]bool{}
	for _, w := range writers {
		seen[w.key] = true
	}
	for _, key := range []string{
		"pkg/meshclient/vault.go:writeFileAtomic",
		"internal/hub/repo.go:writeFileAtomic",
		"cmd/mesh/conflicts.go:writeFileAtomic",
		"internal/curator/merge_note.go:writeAtomic",
		"internal/vault/migrate.go:WriteNoteAtomic",
	} {
		if !seen[key] {
			t.Errorf("known temp+rename writer %s was not discovered; it moved, was renamed, or no "+
				"longer writes this way. Update this list in the same change, do not delete the entry.", key)
		}
	}
}

// tempRenameWriter is one discovered writer: a function whose body both creates a temp
// file and renames a file into place.
type tempRenameWriter struct {
	key  string // "<module-relative file>:<func>"
	body string // the function's CODE, comments stripped
}

// findTempRenameWriters parses every non-test .go file under root WITHOUT comments and
// returns each function that calls both os.CreateTemp and os.Rename.
//
// It reads files off disk rather than through the build system, so the pro-only hub writer
// is covered by the default-tag test run too. That twin is one of the writers this exists
// to cover.
func findTempRenameWriters(t *testing.T, root string) []tempRenameWriter {
	t.Helper()
	var out []tempRenameWriter
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "testdata", "vendor":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		parsed, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return perr
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			rel = path
		}
		rel = filepath.ToSlash(rel)
		for _, decl := range parsed.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			var creates, renames bool
			ast.Inspect(fd.Body, func(n ast.Node) bool {
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
				case "CreateTemp":
					creates = true
				case "Rename":
					renames = true
				}
				return true
			})
			if !creates || !renames {
				continue
			}
			var buf bytes.Buffer
			if err := printer.Fprint(&buf, fset, fd.Body); err != nil {
				return err
			}
			out = append(out, tempRenameWriter{key: rel + ":" + fd.Name.Name, body: buf.String()})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan module for temp+rename writers: %v", err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key < out[j].key })
	return out
}
