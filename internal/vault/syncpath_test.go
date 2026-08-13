// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSafeSyncPath pins the one rule the client, the hub and the curator all use.
// Everything on the reject list is something a hostile hub, or a teammate with the
// write role every member gets by default, could otherwise put on someone else's
// disk.
func TestSafeSyncPath(t *testing.T) {
	cases := []struct {
		name   string
		path   string
		wantOK bool
	}{
		{name: "note", path: "notes/a.md", wantOK: true},
		{name: "note at root", path: "a.md", wantOK: true},
		{name: "note with spaces", path: "notes/with space.md", wantOK: true},
		{name: "non-ascii note", path: "notes/café.md", wantOK: true},
		{name: "extension case does not matter", path: "notes/a.MD", wantOK: true},
		{name: "hub-authoritative config", path: "mesh.toml", wantOK: true},
		{name: "line-ending policy", path: ".gitattributes", wantOK: true},

		{name: "empty", path: "", wantOK: false},
		{name: "whitespace", path: "   ", wantOK: false},
		{name: "the vault itself", path: ".", wantOK: false},
		{name: "absolute", path: "/etc/passwd.md", wantOK: false},
		{name: "parent escape", path: "../evil.md", wantOK: false},
		{name: "escape hidden mid-path", path: "a/b/../../../evil.md", wantOK: false},
		{name: "backslash separator", path: `..\..\evil.md`, wantOK: false},

		// Reserved directories, and the two ways a byte-exact test misses them.
		{name: "mesh dir", path: ".mesh/config.toml", wantOK: false},
		{name: "mesh dir case-folded", path: ".MESH/config.toml", wantOK: false},
		{name: "mesh dir trailing dot", path: ".mesh./config.toml", wantOK: false},
		{name: "mesh dir trailing space", path: ".mesh /config.toml", wantOK: false},
		{name: "mesh dir trailing dot and space", path: ".Mesh. /config.toml", wantOK: false},
		{name: "note inside the mesh dir", path: ".mesh/x.md", wantOK: false},
		{name: "nested mesh dir", path: "notes/.mesh/sync.json", wantOK: false},
		{name: "git dir", path: ".git/config", wantOK: false},
		{name: "git dir case-folded", path: ".GIT/config", wantOK: false},
		{name: "nested git dir", path: "a/.git/config", wantOK: false},

		// Any hidden directory: the walker skips all of them, so a note landed under
		// one can never be indexed or pushed back, and these particular ones are
		// trusted by something else on the machine.
		{name: "agent hook config", path: ".claude/settings.json", wantOK: false},
		{name: "agent instructions", path: ".claude/CLAUDE.md", wantOK: false},
		{name: "editor tasks", path: ".vscode/tasks.json", wantOK: false},
		{name: "hidden dir with a note in it", path: ".config/x.md", wantOK: false},

		// Directories the walker prunes for noise.
		{name: "archive", path: "_archive/x.md", wantOK: false},
		{name: "node_modules", path: "node_modules/x.md", wantOK: false},
		{name: "node_modules case-folded", path: "NODE_MODULES/x.md", wantOK: false},

		// Not notes.
		{name: "direnv", path: ".envrc", wantOK: false},
		{name: "shell script", path: "notes/x.sh", wantOK: false},
		{name: "gitignore", path: ".gitignore", wantOK: false},
		{name: "root file allowlist is root only", path: "notes/mesh.toml", wantOK: false},
		{name: "root file allowlist is exact", path: "Mesh.toml", wantOK: false},
		{name: "note name with a trailing dot", path: "notes/a.md.", wantOK: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := SafeSyncPath(c.path)
			if ok != c.wantOK {
				t.Fatalf("SafeSyncPath(%q) ok = %v, want %v", c.path, ok, c.wantOK)
			}
			if ok && got == "" {
				t.Fatalf("SafeSyncPath(%q) accepted the path but returned no cleaned form", c.path)
			}
		})
	}
}

// TestSafeSyncNotePathTakesNotesOnly: the push direction is one step stricter than
// the read direction. Both hub-owned root files ride the delta stream out to every
// client, and neither may ever come back from one.
func TestSafeSyncNotePathTakesNotesOnly(t *testing.T) {
	for _, p := range []string{"notes/a.md", "a.md", "notes/a.MD"} {
		if _, ok := SafeSyncNotePath(p); !ok {
			t.Errorf("SafeSyncNotePath(%q) = false, want true", p)
		}
	}
	for _, p := range []string{"mesh.toml", ".gitattributes", ".mesh/config.toml", "../a.md"} {
		if _, ok := SafeSyncNotePath(p); ok {
			t.Errorf("SafeSyncNotePath(%q) = true, want false", p)
		}
	}
}

// TestSafeSyncPathAcceptsWhatWalkReturns is the anti-drift half: the guard and the
// indexer must agree. Any file Walk returns from a real vault has to be a path the
// sync protocol can carry, or the note is indexed locally and can never be shared.
func TestSafeSyncPathAcceptsWhatWalkReturns(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{
		"index.md",
		"notes/a.md",
		"notes/deep/nested/b.md",
		"notes/with space.md",
		"imported/jira/jira-1.md",
	} {
		mustWrite(t, root, rel)
	}
	files, err := Walk(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 5 {
		t.Fatalf("walk found %d files, want 5", len(files))
	}
	for _, abs := range files {
		rel, err := filepath.Rel(root, abs)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := SafeSyncPath(filepath.ToSlash(rel)); !ok {
			t.Errorf("Walk returns %q but SafeSyncPath refuses it: the note could be indexed "+
				"locally and never sync", rel)
		}
	}
}

func mustWrite(t *testing.T, root, rel string) {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nid: " + strings.ReplaceAll(rel, "/", "-") + "\n---\n# n\n"
	if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
