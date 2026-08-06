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
	"regexp"
	"sort"
	"strings"
	"testing"
)

// This file is the estate-wide guard for the temp-file + rename write, after the
// same durability bug was fixed on one twin and left on the others twice.
//
// History: the durability pass added f.Sync() plus a parent-directory fsync to
// pkg/meshclient/vault.go and internal/hub/repo.go, and each of those files grew a
// guard that pins ITSELF by name. Two more copies (cmd/mesh/conflicts.go
// writeFileAtomic, internal/curator/merge_note.go writeAtomic) were writing note
// bytes with no fsync at all, and neither self-pinning guard could see them, because
// a guard that names one file can only ever protect that file.
//
// So this one names no file. It DISCOVERS every function in the module that creates
// a temp file and renames it into place, and requires each to fsync the data before
// the rename and the directory after it. A fifth copy fails this test on the day it
// is written, which is the only version of this guard that ends the twin pattern.

// tempRenameWriter is one discovered writer: a function whose body writes bytes to a
// temp path (os.CreateTemp, os.WriteFile or os.OpenFile) and renames it into place.
//
// Residual limit, stated so the next reader does not over-trust this: a writer that
// takes an already-open *os.File from its caller and only renames it here would still
// be missed, because this function's body holds no byte-writing call. Nothing in the
// tree does that today.
type tempRenameWriter struct {
	rel  string // module-relative file path
	fn   string // function name
	body string // the function's CODE, comments stripped
}

// dirSyncCall matches the parent-directory fsync, however it is spelled. All four
// note writers call a local syncDir helper; matching the suffix keeps the guard
// working if one of them ever renames it (fsyncDir, syncParentDir, ...) without
// letting an unrelated identifier through.
var dirSyncCall = regexp.MustCompile(`\b\w*[sS]yncDir\(`)

// fileSyncCall matches an fsync on the temp file handle itself. Any receiver is
// accepted (tmp, f, fh, ...) so the guard pins the BEHAVIOUR, not one file's naming.
var fileSyncCall = regexp.MustCompile(`\b\w+\.Sync\(\)`)

// unguardedWriters are the temp+rename writers that are knowingly NOT durable yet,
// each with the reason it is acceptable. Anything not on this list must fsync.
//
// The list is checked in BOTH directions: an entry that has since been fixed fails
// the test as stale, so the exemption cannot outlive its reason and quietly cover a
// regression later.
var unguardedWriters = map[string]string{
	"internal/meshcfg/config.go:SaveConfig": "writes .mesh/config.toml, not note bytes. " +
		"Losing it costs a re-run of `mesh init`, and every field is re-derivable from env " +
		"or defaults, so it is not in the same class as the four note writers. Out of the " +
		"file group of the durability pass that added this guard; give it the same " +
		"treatment and delete this line.",
}

// TestEveryTempRenameWriterFsyncs walks the whole module, finds every temp+rename
// writer from the AST, and requires the two fsyncs in the right ORDER:
//
//	tmp.Sync()   BEFORE the rename, or the rename can publish blocks that were
//	             never written and the file comes back short or empty
//	syncDir(dir) AFTER  the rename, or the fsync does not cover the new directory
//	             entry and the rename itself can be lost
//
// For a note that means the local copy silently reverts while sync.json records the
// new hash, and the next sync round pushes the STALE bytes as an upsert that the hub
// fast-forwards, reverting the team's copy with no conflict and no sibling.
func TestEveryTempRenameWriterFsyncs(t *testing.T) {
	root := moduleRoot(t)
	writers := findTempRenameWriters(t, root)
	// A discovery guard that discovers nothing passes vacuously, which is the exact
	// failure mode that let the twins survive. Pin a floor.
	if len(writers) < 4 {
		t.Fatalf("found only %d temp+rename writer(s) in the module; the four note writers alone "+
			"should be found, so the scanner is broken, not the code", len(writers))
	}

	seen := map[string]bool{}
	for _, w := range writers {
		key := w.rel + ":" + w.fn
		seen[key] = true
		t.Run(key, func(t *testing.T) {
			renameAt := strings.Index(w.body, "os.Rename(")
			fileSyncAt := -1
			if loc := fileSyncCall.FindStringIndex(w.body); loc != nil {
				fileSyncAt = loc[0]
			}
			dirSyncAt := -1
			if loc := dirSyncCall.FindStringIndex(w.body); loc != nil {
				dirSyncAt = loc[0]
			}
			if why, exempt := unguardedWriters[key]; exempt {
				// Stale-exemption check: if it now fsyncs, the reason no longer applies
				// and the entry has to go, otherwise it silently covers a future regression.
				if fileSyncAt >= 0 && dirSyncAt >= 0 {
					t.Errorf("%s now fsyncs; remove it from unguardedWriters so it is guarded "+
						"from here on (recorded reason: %s)", key, why)
					return
				}
				t.Logf("knowingly not durable: %s", why)
				return
			}

			switch {
			case fileSyncAt < 0:
				t.Errorf("%s does not fsync the temp file before renaming it. A rename can be "+
					"durable while the data blocks it points at are not, so a power cut leaves the "+
					"file short or empty at a path everything else treats as written.", key)
			case fileSyncAt > renameAt:
				t.Errorf("%s fsyncs the temp file AFTER the rename; the rename can publish unwritten "+
					"blocks, so the ordering is the whole point", key)
			}
			switch {
			case dirSyncAt < 0:
				t.Errorf("%s does not fsync the parent directory after the rename. The directory "+
					"entry itself can be lost, leaving the previous file (or no file) behind even "+
					"though the data was fsynced.", key)
			case dirSyncAt < renameAt:
				t.Errorf("%s fsyncs the directory BEFORE the rename, which does not cover the new "+
					"directory entry", key)
			}
		})
	}

	// The four note writers are the ones this pass fixed. Naming them is redundant
	// with the discovery above and that is deliberate: if a refactor moves or renames
	// one so the scanner stops seeing it, this list says so out loud instead of the
	// suite going quietly green on three.
	for _, key := range []string{
		"pkg/meshclient/vault.go:writeFileAtomic",
		"internal/hub/repo.go:writeFileAtomic",
		"cmd/mesh/conflicts.go:writeFileAtomic",
		"internal/curator/merge_note.go:writeAtomic",
	} {
		if !seen[key] {
			t.Errorf("known note writer %s was not discovered; it moved, was renamed, or no longer "+
				"uses temp+rename. Update this list in the same change, do not delete the entry.", key)
		}
	}

	for key := range unguardedWriters {
		if !seen[key] {
			t.Errorf("unguardedWriters names %s, which no longer exists; drop the stale exemption", key)
		}
	}
}

// findTempRenameWriters parses every non-test .go file under root WITHOUT comments
// and returns each function that both creates a temp file and renames it.
//
// Parsing without parser.ParseComments is load-bearing, not tidiness. A body that
// merely MENTIONS tmp.Sync() in a comment while the call itself is gone contains no
// fsync at all, and a text scan that reads comments reports it as guarded. That
// exact false negative was produced deliberately during the last round's ablation:
// both calls deleted, "// tmp.Sync() removed, see history" left behind, guard green.
// Comments never enter the AST here, so printing a body yields code only.
//
// It reads files off disk rather than through the build system on purpose: build
// tags would hide the pro-only hub writer from the default-tag test run, and that
// twin is one of the four this exists to cover.
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
			var buf bytes.Buffer
			if err := printer.Fprint(&buf, fset, fd.Body); err != nil {
				return err
			}
			body := buf.String()
			// The shape being guarded is "write bytes somewhere, then rename them into
			// place", so the rename is the anchor and the byte write is any of the three
			// ways Go spells it. Requiring os.CreateTemp specifically was too narrow and
			// missed the other common idiom outright: a writer that does
			// os.WriteFile(path+".tmp", b) then os.Rename passed this guard green with no
			// fsync anywhere in it, verified by planting one. Checked with a probe again
			// after widening.
			if !strings.Contains(body, "os.Rename(") {
				continue
			}
			if !strings.Contains(body, "os.CreateTemp(") &&
				!strings.Contains(body, "os.WriteFile(") &&
				!strings.Contains(body, "os.OpenFile(") {
				continue
			}
			out = append(out, tempRenameWriter{rel: rel, fn: fd.Name.Name, body: body})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan module for temp+rename writers: %v", err)
	}
	return out
}

// moduleRoot walks up from the test's working directory to the directory holding
// go.mod, so the scan covers the whole module no matter which package runs it.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod found above %s", dir)
		}
		dir = parent
	}
}

// TestConflictResolveWriteLandsAndCleansUp is the behavioural half for the
// conflicts writer: it must still produce exactly the right bytes, leave no temp
// file behind for the vault walker to pick up as a note, and report no error on a
// platform where the directory fsync is refused. A power-loss repro is not possible
// in a unit test, so the fsync calls themselves are pinned above.
func TestConflictResolveWriteLandsAndCleansUp(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "new file", body: "---\nid: n\n---\n# n\n"},
		{name: "overwrite", body: "---\nid: n\n---\n# n\n\nA longer second body.\n"},
		{name: "empty body", body: ""},
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "notes", "n.md")
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := writeFileAtomic(path, []byte(tc.body)); err != nil {
				t.Fatalf("writeFileAtomic: %v", err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.body {
				t.Errorf("content = %q, want %q", got, tc.body)
			}
			ents, err := os.ReadDir(filepath.Dir(path))
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range ents {
				if strings.HasPrefix(e.Name(), ".mesh-resolve-") {
					t.Errorf("temp file %s left behind", e.Name())
				}
			}
		})
	}
}

// syncHeadlineFormat is the one format string that renders a completed sync round.
// It is matched against CODE only (the AST scan below strips comments), because a
// guard that reads comments is satisfied by a comment quoting the literal, and that
// is the exact false negative this file already exists to avoid.
const syncHeadlineFormat = `synced: pushed %d, pulled %d, %d conflict(s) (HEAD %s)`

// TestSyncHeadlineHasOneRenderer pins the sync headline to a single function.
//
// Three CLI paths call SyncVault and report the round: `mesh sync`, its --watch loop,
// and `mesh conflicts resolve --take-mine`. All three had their own copy of this
// format string, and the copies drifted, which is how the deferred-remainder line
// (Summary.Remaining) reached one of them and none of the others: a bounded round
// looked complete everywhere except the one path that had been updated. The count is
// pinned at exactly one so a fourth path cannot start a fourth copy.
func TestSyncHeadlineHasOneRenderer(t *testing.T) {
	root := moduleRoot(t)
	owners := functionsContaining(t, root, syncHeadlineFormat)
	want := []string{"cmd/mesh/main.go:syncHeadlineLines"}
	if len(owners) != 1 || owners[0] != want[0] {
		t.Errorf("the sync headline is rendered by %v, want exactly %v. Every caller must go "+
			"through the shared renderer, or the next line added to one copy silently misses "+
			"the others (that is how Summary.Remaining stayed invisible).", owners, want)
	}
}

// functionsContaining returns "<rel>:<func>" for every function whose CODE contains
// lit. Comments are not parsed, so a comment quoting lit does not count.
func functionsContaining(t *testing.T, root, lit string) []string {
	t.Helper()
	var out []string
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
			var buf bytes.Buffer
			if err := printer.Fprint(&buf, fset, fd.Body); err != nil {
				return err
			}
			if strings.Contains(buf.String(), lit) {
				out = append(out, rel+":"+fd.Name.Name)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan module for %q: %v", lit, err)
	}
	sort.Strings(out)
	return out
}
