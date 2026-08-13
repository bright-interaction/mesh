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
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// This file is the estate-wide guard for path validation on the sync protocol, after
// the same hole was fixed on one copy and left on the others.
//
// There were four private copies of one rule (client, hub, curator daemon, curator CLI)
// and they disagreed with each other. The client blocked the reserved ".mesh" component
// and the hub did not block it at all; none of them constrained the extension; all of
// them compared components byte for byte on filesystems that do not. So a path one side
// refused, another side landed: ".MESH/config.toml" reached <vault>/.mesh/config.toml on
// APFS, and ".claude/settings.json" reached a teammate's agent hook config.
//
// The rule now lives once, in vault.SafeSyncPath. This test exists so a fifth copy
// cannot be written by hand: it DISCOVERS path guards from the AST rather than naming
// them, and requires each one to reach the shared guard.
//
// Discovery uses two independent signals, so no single one of them can be the hole:
//
//   - the function is NAMED like a path guard (safeRelPath, safeNotePath, safeVaultPath,
//     SafeSyncPath, ...), which is the shape every copy so far has had
//   - the function COMPARES a path component against a reserved directory name, which is
//     the shape a hand-rolled copy has whatever it is called
//
// Comments never enter the AST here, so the prose in this file and in the guards
// themselves does not count as either signal.

// guardNameRe matches the name every copy of this rule has had so far.
var guardNameRe = regexp.MustCompile(`(?i)^safe[a-z0-9]*path$`)

// reservedComponents are the directory names a hand-rolled guard compares against. A
// function that tests a path component against one of these is deciding what may be
// written where, which is exactly the decision that must not be made twice.
var reservedComponents = map[string]bool{".git": true, ".mesh": true, ".claude": true}

// sharedGuardCall is how a delegating guard gives itself away, qualified
// (vault.SafeSyncPath) or not (inside package vault itself).
const sharedGuardCall = "SafeSyncPath("

// sharedGuardImpl is the one function allowed to make the decision itself.
const sharedGuardImpl = "internal/vault/syncpath.go:SafeSyncPath"

// nonSyncGuards are discovered functions that are knowingly NOT sync-path guards, each
// with the reason. Checked in BOTH directions: an entry that has since started
// delegating fails as stale, and an entry naming a function that no longer exists fails
// too, so an exemption cannot outlive its reason.
var nonSyncGuards = map[string]string{
	"cmd/mesh/conflicts.go:validateSibling": "validates a path the LOCAL USER typed at the " +
		"CLI (`mesh conflicts resolve <sibling>`), not one that arrived over the network. The " +
		"user may already read and delete their own files, so this is an ergonomics check, not " +
		"a trust boundary. It could still delegate; that is a deliberate, separate decision.",
	"internal/index/health.go:existsUnderRoots": "walks LOCAL source roots for the code " +
		"index and prunes .git/node_modules/vendor while walking. It decides what to READ off " +
		"a directory the user configured, never what to write from a wire path.",
}

// pathGuard is one discovered guard: a function that decides whether a path may be
// materialized in a vault.
type pathGuard struct {
	key    string // "<module-relative file>:<func>"
	pkgDir string // module-relative directory, for the same-package call closure
	fn     string
	why    string
}

// TestEverySyncPathGuardDelegates requires every discovered path guard to reach
// vault.SafeSyncPath, directly or through a helper in its own package.
func TestEverySyncPathGuardDelegates(t *testing.T) {
	root := moduleRoot(t)
	guards, bodies := findPathGuards(t, root)

	// A discovery guard that discovers nothing passes vacuously, which is the exact
	// failure mode that let the copies drift. Pin a floor at what is findable in the
	// OPEN core: this test also runs against the filtered public mirror, where the hub
	// and curator-daemon guards are stripped, so the floor must not count them.
	if len(guards) < 5 {
		t.Fatalf("found only %d path guard(s) in the module; the open core alone has the shared "+
			"guard, three in pkg/meshclient and one in cmd/mesh, so the scanner is broken, not the code", len(guards))
	}

	seen := map[string]bool{}
	for _, g := range guards {
		seen[g.key] = true
		t.Run(g.key, func(t *testing.T) {
			delegates := g.key == sharedGuardImpl || reachesSharedGuard(bodies[g.pkgDir], g.fn)
			if why, exempt := nonSyncGuards[g.key]; exempt {
				if delegates {
					t.Errorf("%s now reaches %s; drop it from nonSyncGuards so it is held to the "+
						"shared rule from here on (recorded reason: %s)", g.key, sharedGuardCall, why)
					return
				}
				t.Logf("knowingly not a sync-path guard: %s", why)
				return
			}
			if !delegates {
				t.Errorf("%s decides a vault path for itself (%s) instead of calling "+
					"vault.SafeSyncPath. Four private copies of this rule disagreed with each "+
					"other, and a path one refused another landed: .MESH/config.toml onto the "+
					"client's own config, .claude/settings.json onto a teammate's agent hooks. "+
					"Delegate, or add an entry to nonSyncGuards saying why this one is not a "+
					"wire path.", g.key, g.why)
			}
		})
	}

	for key := range nonSyncGuards {
		if !seen[key] {
			t.Errorf("nonSyncGuards names %s, which the scanner no longer finds; drop the stale entry", key)
		}
	}
	// The known guards. Naming them is redundant with the discovery above and that is
	// deliberate: if a refactor renames one so the scanner stops seeing it, this list
	// says so out loud instead of the suite going quietly green on the rest.
	for _, key := range []string{
		sharedGuardImpl,
		"pkg/meshclient/safepath.go:safeRelPath",
		"pkg/meshclient/safepath.go:safeNotePath",
		"pkg/meshclient/safepath.go:safeSiblingPath",
		"cmd/mesh/curator.go:safeRelPath",
		"internal/hub/repo.go:safeRelPath",
		"internal/curator/merge_note.go:safeVaultPath",
	} {
		if knownWriterStripped(root, key) { // the pro guards are absent from the public mirror
			continue
		}
		if !seen[key] {
			t.Errorf("known path guard %s was not discovered; it moved or was renamed. Update this "+
				"list in the same change, do not delete the entry.", key)
		}
	}
}

// reachesSharedGuard reports whether fn, or anything it calls in its own package,
// contains a call to the shared guard. The hop matters: safeNotePath validates through
// resolveInVault, which is the function that actually calls it.
func reachesSharedGuard(pkg map[string]string, fn string) bool {
	seen := map[string]bool{}
	var walk func(string) bool
	walk = func(name string) bool {
		if seen[name] {
			return false
		}
		seen[name] = true
		body, ok := pkg[name]
		if !ok {
			return false
		}
		if strings.Contains(body, sharedGuardCall) {
			return true
		}
		for callee := range pkg {
			if callee != name && strings.Contains(body, callee+"(") && walk(callee) {
				return true
			}
		}
		return false
	}
	return walk(fn)
}

// findPathGuards parses every non-test .go file under root WITHOUT comments and returns
// the discovered guards plus, per package directory, every function body in it (the
// call closure reachesSharedGuard walks).
//
// It reads files off disk rather than through the build system on purpose: build tags
// would hide the pro-only hub and curator guards from the default-tag run, and those are
// two of the four copies this exists to cover.
func findPathGuards(t *testing.T, root string) ([]pathGuard, map[string]map[string]string) {
	t.Helper()
	var guards []pathGuard
	bodies := map[string]map[string]string{}
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
		pkgDir := filepath.ToSlash(filepath.Dir(rel))
		if bodies[pkgDir] == nil {
			bodies[pkgDir] = map[string]string{}
		}
		for _, decl := range parsed.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			var buf bytes.Buffer
			if err := printer.Fprint(&buf, fset, fd.Body); err != nil {
				return err
			}
			bodies[pkgDir][fd.Name.Name] = buf.String()
			why := ""
			switch {
			case guardNameRe.MatchString(fd.Name.Name):
				why = "named like a path guard"
			case comparesReservedComponent(fd):
				why = "compares a path component against a reserved directory name"
			default:
				continue
			}
			guards = append(guards, pathGuard{
				key: rel + ":" + fd.Name.Name, pkgDir: pkgDir, fn: fd.Name.Name, why: why,
			})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan module for path guards: %v", err)
	}
	return guards, bodies
}

// comparesReservedComponent reports whether fd tests something against ".git", ".mesh"
// or ".claude" with == / != or in a switch case. A map or slice LITERAL holding the same
// names is not a comparison and does not count: those are the walker prune lists the
// shared guard itself reads, and flagging them would put the data next to the rule in
// scope instead of the rule.
func comparesReservedComponent(fd *ast.FuncDecl) bool {
	hit := false
	isReserved := func(e ast.Expr) bool {
		lit, ok := e.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return false
		}
		s, err := strconv.Unquote(lit.Value)
		return err == nil && reservedComponents[s]
	}
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.BinaryExpr:
			if (v.Op == token.EQL || v.Op == token.NEQ) && (isReserved(v.X) || isReserved(v.Y)) {
				hit = true
			}
		case *ast.CaseClause:
			for _, e := range v.List {
				if isReserved(e) {
					hit = true
				}
			}
		}
		return !hit
	})
	return hit
}
