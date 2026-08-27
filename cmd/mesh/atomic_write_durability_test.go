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

// This file is the estate-wide guard for durable note writes, after the same durability
// bug was fixed on one twin and left on the others, twice.
//
// History, in two chapters. First, the durability pass added f.Sync() plus a parent-
// directory fsync to pkg/meshclient/vault.go and internal/hub/repo.go, and each of those
// files grew a guard that pinned ITSELF by name. Two more copies (cmd/mesh/conflicts.go
// writeFileAtomic, internal/curator/merge_note.go writeAtomic) were writing note bytes
// with no fsync at all, and neither self-pinning guard could see them, because a guard
// that names one file can only ever protect that file. So this one names no file: it
// DISCOVERS the writers.
//
// Second chapter, and the reason this file was rewritten: the discovery anchored on
// os.Rename, so it could only ever find writers that ALREADY had the temp+rename shape.
// That is circular. A census of every function in the module that writes note bytes found
// seven, and the guard was watching four. The three it could not see were exactly the
// three that never renamed anything: internal/vault/migrate.go (the write path behind
// every bulk in-place `--apply` rewrite of a whole vault), internal/vault/scaffold.go
// CreateNote (the primary note-authoring path for the entire flywheel), and
// internal/ingest/ingest.go Run. Two of them carried more traffic than anything the guard
// did cover.
//
// So discovery no longer requires the fix to be present in order to notice the writer.
// It finds byte writes by ANY spelling (os.WriteFile, os.OpenFile + Write, os.CreateTemp
// + Rename) and decides in-scope from what the bytes ARE, not from how they are written.
// (This test was called TestEveryTempRenameWriterFsyncs before that rewrite; the name is
// recorded here so a reference to the old name still leads someone to this file.)

// noteByteWriter is one discovered writer: a function whose body writes bytes that land
// in a vault. How it writes them is not part of the definition, on purpose.
//
// Residual limit, stated so the next reader does not over-trust this: a writer that takes
// an already-open *os.File from its caller and only renames it here would still be
// missed, because this function's body holds no byte-writing call. Nothing in the tree
// does that today.
type noteByteWriter struct {
	rel     string   // module-relative file path
	fn      string   // function name
	body    string   // the function's CODE, comments stripped
	dests   []string // destination expression of each byte-writing call, as source text
	renames bool     // the body also renames a file into place
	why     string   // which signal put it in scope, for the failure message
}

// dirSyncCall matches the parent-directory fsync, however it is spelled. Every note
// writer calls a local syncDir helper; matching the suffix keeps the guard working if one
// of them ever renames it (fsyncDir, syncParentDir, ...) without letting an unrelated
// identifier through.
var dirSyncCall = regexp.MustCompile(`\b\w*[sS]yncDir\(`)

// fileSyncCall matches an fsync on the file handle itself. Any receiver is accepted (tmp,
// f, fh, ...) so the guard pins the BEHAVIOUR, not one file's naming.
var fileSyncCall = regexp.MustCompile(`\b\w+\.Sync\(\)`)

// mdPathLiteral matches a markdown path literal anywhere in a writer's code, which is how
// a function that builds its own note filename gives itself away (`id + ".md"`,
// "index.md"). Comments are stripped before this runs, so a path named only in prose does
// not count.
var mdPathLiteral = regexp.MustCompile(`"[^"\n]*\.md"`)

// vaultDest matches a destination expression built from a vault root
// (filepath.Join(vaultRoot, rel), credPath(vaultDir), ...). It is applied ONLY to the
// destination argument of the write call, never to the whole body: half the module takes
// a vaultAbs parameter it merely reads, and matching that would put every unrelated
// writer in scope.
var vaultDest = regexp.MustCompile(`(?i)vault`)

// frontmatterCode matches a writer that handles note frontmatter. Nothing but a note
// carries frontmatter in this module, so a function that both parses it and writes bytes
// is writing a note whatever its path expression looks like. This is the signal that
// covers internal/vault/migrate.go writeNoteChecked if a later change ever inlines a
// plain os.WriteFile back into it.
//
// It is a case-insensitive substring, NOT `\bFrontmatter\b`: every real call is
// SplitFrontmatter or ParseFrontmatter, where the F has a word character in front of it
// and the word-boundary form matches nothing at all. Verified by planting the regression
// this signal exists to catch; the anchored version stayed green on it.
var frontmatterCode = regexp.MustCompile(`(?i)frontmatter`)

// unguardedWriters are the discovered writers that are knowingly NOT fsynced, each with
// the reason it is acceptable. Anything not on this list must fsync.
//
// The list is checked in BOTH directions: an entry that has since been fixed fails the
// test as stale, so the exemption cannot outlive its reason and quietly cover a
// regression later. An entry naming a function that no longer exists fails too.
//
// Every entry here is a file that is NOT a note: losing it costs a re-run, not knowledge.
// A note is the one thing in a vault that cannot be regenerated from anything else, which
// is the whole reason the fsync rule exists and the only reason these are out of it.
var unguardedWriters = map[string]string{
	// internal/meshcfg/config.go:SaveConfig used to sit here, exempted because "every
	// field is re-derivable from env or defaults and a lost config costs a re-run of
	// `mesh init`". Both halves were false. `mesh init` never writes config.toml (it
	// writes index.md and nothing else; the only writers are `mesh embed` and the web
	// Settings PUT), and the fields that matter are the ones the operator typed, which no
	// default reproduces: embedding endpoint/model/dim, code roots, the secret-bridge base
	// URL, the tuned weights. The file exists precisely so those do NOT live in env.
	// The failure was not "lost" either: LoadConfig reads a truncated file as a valid
	// EMPTY config, so semantic search, rerank, freshness decay, the code index and the
	// secret bridge all switch off silently, and both writers load-modify-write, so the
	// next single-field edit persists the zeroes for good. SaveConfig now fsyncs, so it is
	// covered by the checks below rather than excused here. Do not re-add it.
	// internal/hub/init.go:Bootstrap used to sit here, exempted because commitWorktree
	// runs immediately after it, so git was said to carry the bytes into the object
	// store. The entry itself asked for that argument to be re-checked, and it does not
	// survive: git's default is `core.fsync=committed,-loose-object` (git 2.39's manual
	// says so, and adds that it "risks losing recent work in the event of an unclean
	// system shutdown"), and a fresh bootstrap commit is nothing but loose objects, so
	// the file and the objects made from it are unhardened together. Bootstrap now lands
	// both starter files through internal/hub/repo.go:writeFileAtomic and no longer
	// writes bytes itself, which is why it is neither in this map nor in the discovered
	// set. Do not re-add it.
	// internal/ingest/state.go:saveState used to sit here, exempted because losing the
	// per-connector high-water mark only re-pulls a window that has already been pulled,
	// and every import is a deterministic upsert onto the same path, so the cost was said
	// to be a slower run and never a lost or duplicated note. That reasoning still holds
	// and is no longer the deciding factor: the same write had to become temp+rename
	// anyway to repair the file's MODE (it was 0644, beside .mesh/credentials, naming the
	// source systems this vault pulls from, and os.WriteFile's perm applies only on
	// create so an existing file would have stayed 0644 forever). Once a write is an
	// atomic rename, the fsync is one more line, and this map's own rule is that an
	// exemption cannot outlive its reason. saveState now fsyncs, so it is covered by the
	// checks below rather than excused here. Do not re-add it.
}

// TestEveryNoteByteWriterFsyncs walks the whole module, finds every function that writes
// note bytes from the AST, and requires the two fsyncs in the right ORDER:
//
//	f.Sync()     BEFORE the rename (or simply before the directory fsync, for a writer
//	             that claims its final name directly), because a rename can publish
//	             blocks that were never written and the file comes back short or empty
//	syncDir(dir) AFTER  the write, or the fsync does not cover the new directory entry
//	             and the rename (or the freshly created file) itself can be lost
//
// For a note that means the local copy silently reverts while sync.json records the new
// hash, and the next sync round pushes the STALE bytes as an upsert that the hub
// fast-forwards, reverting the team's copy with no conflict and no sibling.
func TestEveryNoteByteWriterFsyncs(t *testing.T) {
	root := moduleRoot(t)
	writers := findNoteByteWriters(t, root)
	// A discovery guard that discovers nothing passes vacuously, which is the exact
	// failure mode that let the twins survive. Pin a floor at the writers that are
	// findable in the OPEN core, not in this monorepo checkout.
	//
	// The distinction is load-bearing: this test also runs against the filtered
	// public mirror, where split-public-repo.sh has stripped the pro paths and one
	// of the writers counted here goes with them. A floor set from the monorepo's
	// census therefore fails in the mirror for a reason that has nothing to do with
	// durability, and it fails AFTER the publish transform, where it reads as
	// "publishing would hand contributors a red suite". The monorepo satisfies this
	// floor with room to spare (it is a superset of the open core), so one number
	// serves both trees, and a genuinely broken scanner still finds ~0 and trips it.
	if len(writers) < 8 {
		t.Fatalf("found only %d note-byte writer(s) in the module; at least the seven note writers "+
			"and the exempted non-note writers should be found, so the scanner is broken, not the code", len(writers))
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
				t.Errorf("%s writes note bytes (%s) and does not fsync the file. The bytes can sit "+
					"in the page cache while everything downstream treats the write as done, so a "+
					"power cut leaves a short or empty file at a path the index, sync.json and the "+
					"caller's receipt all name as written.", key, w.why)
			case renameAt >= 0 && fileSyncAt > renameAt:
				t.Errorf("%s fsyncs the file AFTER the rename; the rename can publish unwritten "+
					"blocks, so the ordering is the whole point", key)
			}
			switch {
			case dirSyncAt < 0:
				t.Errorf("%s writes note bytes (%s) and does not fsync the parent directory. The "+
					"directory entry itself can be lost, leaving the previous file (or no file) "+
					"behind even though the data was fsynced.", key, w.why)
			case renameAt >= 0 && dirSyncAt < renameAt:
				t.Errorf("%s fsyncs the directory BEFORE the rename, which does not cover the new "+
					"directory entry", key)
			case renameAt < 0 && fileSyncAt >= 0 && dirSyncAt < fileSyncAt:
				t.Errorf("%s fsyncs the directory before the file's own data; the entry would be "+
					"durable while the bytes it points at are not", key)
			}
		})
	}

	// The durable writers. Naming them is redundant with the discovery above and that is
	// deliberate: if a refactor moves or renames one so the scanner stops seeing it, this
	// list says so out loud instead of the suite going quietly green on five.
	//
	// The last entry is the config writer, not a note writer. It is here because every
	// other writer in the module is pinned by name either in this list or in
	// unguardedWriters, and the config writer left unguardedWriters, so without an entry
	// it would be the one writer nothing names. The gap is not hypothetical for this one:
	// its ONLY discovery signal is the temp+rename shape (its destination expression says
	// meshDir, and it carries no .md literal and no frontmatter), so a refactor back to a
	// plain os.WriteFile drops it out of the census entirely. Measured: rewritten that
	// way, the census found 9 writers, the floor above tolerates 9, and every check
	// passed on a config writer that fsynced nothing.
	for _, key := range []string{
		"pkg/meshclient/vault.go:writeFileAtomic",
		"internal/hub/repo.go:writeFileAtomic",
		"cmd/mesh/conflicts.go:writeFileAtomic",
		"cmd/mesh/main.go:writeStarterIndex",
		"internal/curator/merge_note.go:writeAtomic",
		"internal/vault/migrate.go:WriteNoteAtomic",
		"internal/vault/scaffold.go:createNoteContext",
		"internal/meshcfg/config.go:saveConfigContextWith",
	} {
		if knownWriterStripped(root, key) {
			continue
		}
		if !seen[key] {
			t.Errorf("known durable writer %s was not discovered; it moved, was renamed, or no longer "+
				"writes bytes the scanner can see. Update this list in the same change, do not delete "+
				"the entry.", key)
		}
	}

	for key := range unguardedWriters {
		if !seen[key] {
			t.Errorf("unguardedWriters names %s, which no longer exists; drop the stale exemption", key)
		}
	}
}

// findNoteByteWriters parses every non-test .go file under root WITHOUT comments and
// returns each function that writes bytes into a vault.
//
// Parsing without parser.ParseComments is load-bearing, not tidiness. A body that merely
// MENTIONS tmp.Sync() in a comment while the call itself is gone contains no fsync at all,
// and a text scan that reads comments reports it as guarded. That exact false negative was
// produced deliberately by an earlier round while checking this guard: both calls deleted,
// "tmp.Sync() removed, see history" left behind as a comment, guard green. Comments never
// enter the AST here, so printing a body yields code only.
//
// It reads files off disk rather than through the build system on purpose: build tags
// would hide the pro-only hub writer from the default-tag test run, and that twin is one
// of the writers this exists to cover.
func findNoteByteWriters(t *testing.T, root string) []noteByteWriter {
	t.Helper()
	var out []noteByteWriter
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
			w := noteByteWriter{rel: rel, fn: fd.Name.Name}
			var ierr error
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				// CreateNote's context tests inject the O_EXCL opener as openFile. It is
				// still the byte-publication call in production, and allowing that narrow
				// spelling keeps the census from losing the primary note writer merely
				// because its filesystem boundary became testable.
				if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == "openFile" {
					if len(call.Args) > 0 {
						txt, err := exprText(fset, call.Args[0])
						if err != nil {
							ierr = err
							return false
						}
						w.dests = append(w.dests, txt)
					}
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
				// The three ways this module spells "put these bytes on disk". Requiring
				// os.CreateTemp specifically was the old shape and it missed the other two
				// outright, which is how three note writers stayed invisible.
				case "WriteFile", "OpenFile", "CreateTemp":
					if len(call.Args) > 0 {
						txt, err := exprText(fset, call.Args[0])
						if err != nil {
							ierr = err
							return false
						}
						w.dests = append(w.dests, txt)
					}
				case "Rename":
					w.renames = true
				}
				return true
			})
			if ierr != nil {
				return ierr
			}
			if len(w.dests) == 0 {
				continue
			}
			var buf bytes.Buffer
			if err := printer.Fprint(&buf, fset, fd.Body); err != nil {
				return err
			}
			w.body = buf.String()
			w.why = vaultWriteSignal(w)
			if w.why == "" {
				continue
			}
			out = append(out, w)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan module for note-byte writers: %v", err)
	}
	return out
}

// vaultWriteSignal reports why a byte writer counts as writing into a vault, or "" when
// it does not. Four independent signals, so no single one of them can be the hole:
//
//   - it renames a temp file into place. In this module that shape exists only for vault
//     files and the config, and it is the generic helper shape (writeFileAtomic takes a
//     path, so the destination expression says nothing about what it holds).
//   - it names a .md path in its own code, which is how a function that builds a note
//     filename itself gives itself away.
//   - it handles frontmatter, which nothing but a note has.
//   - it writes to a destination built from a vault root.
//
// Deliberately NOT in scope: Claude Code hook config files, the extraction log, and
// .gitattributes. None of them is inside a note tree and none of them holds knowledge.
func vaultWriteSignal(w noteByteWriter) string {
	if w.renames {
		return "temp file renamed into place"
	}
	if mdPathLiteral.MatchString(w.body) {
		return "names a .md path"
	}
	if frontmatterCode.MatchString(w.body) {
		return "handles note frontmatter"
	}
	for _, d := range w.dests {
		if vaultDest.MatchString(d) {
			return "writes to a vault path (" + d + ")"
		}
	}
	return ""
}

// exprText renders one AST expression back to source text.
func exprText(fset *token.FileSet, e ast.Expr) (string, error) {
	var buf bytes.Buffer
	if err := printer.Fprint(&buf, fset, e); err != nil {
		return "", err
	}
	return buf.String(), nil
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
// Several CLI paths run a sync round and report it, and they each had their own copy
// of this format string. The copies drifted, which is how the deferred-remainder line
// (Summary.Remaining) reached one of them and none of the others: a bounded round
// looked complete everywhere except the one path that had been updated. The count is
// pinned at exactly one so the next path cannot start another copy.
//
// The renderer lives in pkg/meshclient, next to Summary, because cmd/mesh-curator
// runs rounds too and cannot import a cmd/mesh function.
//
// This test on its own is NOT enough, and that is worth stating here because it was
// believed to be: it pins where the string lives, not who uses it. `mesh join` was the
// uncounted caller that printed a two-field receipt of its own and never touched this
// string at all, so nothing here could see it. That is what the test below covers.
func TestSyncHeadlineHasOneRenderer(t *testing.T) {
	root := moduleRoot(t)
	owners := functionsContaining(t, root, syncHeadlineFormat)
	want := []string{"pkg/meshclient/summary.go:HeadlineLines"}
	if len(owners) != 1 || owners[0] != want[0] {
		t.Errorf("the sync headline is rendered by %v, want exactly %v. Every caller must go "+
			"through the shared renderer, or the next line added to one copy silently misses "+
			"the others (that is how Summary.Remaining stayed invisible).", owners, want)
	}
}

// syncEntryPoints are the two calls that RUN a sync round and hand back a Summary
// describing what needs a decision. A function that calls one of these owes the
// operator a report of what it got back.
var syncEntryPoints = map[string]bool{"SyncVault": true, "JoinVault": true}

// TestEveryCLISyncRoundRendersThroughTheSharedRenderer DISCOVERS, from the AST, every
// function in the command-line binaries that runs a sync round, and fails on any one
// that cannot reach the shared renderer.
//
// The subject list is discovered rather than written down, and that is the whole point.
// The previous guard stated its subjects in a comment ("three CLI paths call SyncVault")
// and asserted only that one format string had one owner. `mesh join` was a fourth
// caller: JoinVault ends in SyncVault and returns the full Summary, and joinCmd read
// Head and Pulled off it and printed nothing else. So a stranger who ran `mesh init`,
// wrote an index.md and then joined a hub had their file replaced and their bytes parked
// in a sibling, and the terminal said "5 files pulled". A viewer-role join reported no
// refusals; a join with a deferred remainder reported none of it either. Every one of
// those facts was in the Summary and every one was dropped, and the guard that existed
// to catch exactly this was green over it, because a hand-written list cannot notice a
// caller nobody added to it.
//
// Reachability rather than a direct call, because `mesh conflicts resolve --take-mine`
// legitimately owns its own receipt wording for the note it just rescued; it must still
// route the ROUND through the shared headline, which it does two calls down.
func TestEveryCLISyncRoundRendersThroughTheSharedRenderer(t *testing.T) {
	root := moduleRoot(t)
	// cmd/ is where the operator-facing binaries live; pkg/meshclient is scanned too so
	// the renderer and the entry points themselves resolve during the walk.
	funcs := scanFuncs(t, root, []string{"cmd", filepath.Join("pkg", "meshclient")})

	var renderers []string
	for q, f := range funcs {
		if strings.Contains(f.body, syncHeadlineFormat) {
			renderers = append(renderers, q)
		}
	}
	sort.Strings(renderers)
	if len(renderers) != 1 {
		t.Fatalf("expected exactly one function to own the sync headline, found %v", renderers)
	}
	renderer := renderers[0]

	var callers, offenders []string
	for q, f := range funcs {
		if !strings.HasPrefix(q, "cmd/") {
			continue // internal daemons report through their own logs, not a receipt
		}
		if !anyOf(f.calls, syncEntryPoints) {
			continue
		}
		callers = append(callers, q)
		if !reachesFunc(funcs, q, renderer) {
			offenders = append(offenders, q)
		}
	}
	sort.Strings(callers)
	sort.Strings(offenders)
	if len(callers) == 0 {
		t.Fatal("found no CLI function that calls SyncVault or JoinVault; the discovery is " +
			"broken, and a broken discovery is a guard that passes over anything")
	}
	if len(offenders) > 0 {
		t.Errorf("these CLI functions run a sync round but never reach %s: %v\n"+
			"Each one gets a Summary carrying Conflicts, ConflictSiblings, Protected, Rejected "+
			"and Remaining, and a receipt that omits them tells the user their notes are on the "+
			"hub when they are not. Render through Summary.Lines (cmd/mesh: syncSummaryLines) "+
			"instead of hand-rolling a headline.", renderer, offenders)
	}
	t.Logf("sync-round callers checked: %v", callers)
}

// scannedFunc is one function body plus the plain names of everything it calls, which
// is enough to walk the call graph inside the packages we scanned.
type scannedFunc struct {
	body  string
	calls map[string]bool
}

// scanFuncs parses every non-test .go file under the given module-relative directories
// and returns "<rel>:<func>" -> its body and call set. Methods are keyed by their own
// name (there is no receiver in the key), which is enough resolution here and keeps the
// call-graph walk name-based.
func scanFuncs(t *testing.T, root string, dirs []string) map[string]scannedFunc {
	t.Helper()
	out := map[string]scannedFunc{}
	for _, d := range dirs {
		err := filepath.WalkDir(filepath.Join(root, d), func(path string, de fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if de.IsDir() {
				switch de.Name() {
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
				calls := map[string]bool{}
				ast.Inspect(fd.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					switch fn := call.Fun.(type) {
					case *ast.Ident:
						calls[fn.Name] = true
					case *ast.SelectorExpr:
						calls[fn.Sel.Name] = true
					}
					return true
				})
				out[rel+":"+fd.Name.Name] = scannedFunc{body: buf.String(), calls: calls}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("scan %s: %v", d, err)
		}
	}
	return out
}

func anyOf(have map[string]bool, want map[string]bool) bool {
	for n := range want {
		if have[n] {
			return true
		}
	}
	return false
}

// reachesFunc walks the call graph from start and reports whether target is reachable.
// The sync entry points are dead ends on purpose: SyncVault BUILDS the Summary (its body
// mentions every field), so traversing into it would make every caller trivially
// "reach" any renderer and the guard would pass over the exact thing it is watching for.
func reachesFunc(funcs map[string]scannedFunc, start, target string) bool {
	byName := map[string][]string{}
	for q := range funcs {
		byName[q[strings.LastIndex(q, ":")+1:]] = append(byName[q[strings.LastIndex(q, ":")+1:]], q)
	}
	seen := map[string]bool{start: true}
	queue := []string{start}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur == target {
			return true
		}
		for name := range funcs[cur].calls {
			if syncEntryPoints[name] {
				continue
			}
			for _, next := range byName[name] {
				if !seen[next] {
					seen[next] = true
					queue = append(queue, next)
				}
			}
		}
	}
	return false
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
