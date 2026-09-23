// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package web

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNoteRouteBoundsRawRead(t *testing.T) {
	s, dir := cfgServer(t)
	if err := os.WriteFile(filepath.Join(dir, "n.md"), make([]byte, maxWebNoteBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _ := doJSON(t, s.Handler(), http.MethodGet, "/api/note/n", "")
	if code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize note response = %d, want 413", code)
	}
}

func TestNoteRouteRejectsNonRegularEntry(t *testing.T) {
	s, dir := cfgServer(t)
	path := filepath.Join(dir, "n.md")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	code, _ := doJSON(t, s.Handler(), http.MethodGet, "/api/note/n", "")
	if code != http.StatusInternalServerError {
		t.Fatalf("directory note response = %d, want 500", code)
	}
}

func TestNoteRouteRejectsSymlink(t *testing.T) {
	s, dir := cfgServer(t)
	target := filepath.Join(dir, "target.md")
	const outsideMarker = "outside-file-must-not-cross-symlink-boundary"
	if err := os.WriteFile(target, []byte(outsideMarker), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "n.md")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	code, body := doJSON(t, s.Handler(), http.MethodGet, "/api/note/n", "")
	if code == http.StatusOK || strings.Contains(fmt.Sprint(body), outsideMarker) {
		t.Fatalf("symlink note leaked target: status=%d body=%v", code, body)
	}
}

func TestNoteRouteRechecksCurrentScope(t *testing.T) {
	for name, body := range map[string]string{
		"private":      "---\nid: n\ntype: note\nscope: [private]\n---\n# Secret\nnewly restricted bytes\n",
		"malformed":    "---\nid: n\nscope: [private\n---\n# Secret\nnewly restricted bytes\n",
		"unterminated": "---\nid: n\nscope: [dev]\n# Secret\nnewly restricted bytes\n",
	} {
		t.Run(name, func(t *testing.T) {
			s, dir := cfgServer(t) // indexed with the default dev scope
			s.SetScopeResolver(func(*http.Request) map[string]bool { return map[string]bool{"dev": true} })
			if err := os.WriteFile(filepath.Join(dir, "n.md"), []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			code, reply := doJSON(t, s.Handler(), http.MethodGet, "/api/note/n", "")
			if code != http.StatusNotFound || strings.Contains(fmt.Sprint(reply), "newly restricted") {
				t.Fatalf("changed file did not receive opaque denial: status=%d", code)
			}
		})
	}
}

func TestNoteRouteRejectsReplacedParentDirectory(t *testing.T) {
	dir := t.TempDir()
	group := filepath.Join(dir, "group")
	if err := os.Mkdir(group, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(group, "n.md"), []byte("---\nid: n\ntype: note\n---\n# Original\n"), 0600); err != nil {
		t.Fatal(err)
	}
	seedIndex(t, dir)
	s, err := NewServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "n.md"), []byte("# Outside\noutside-directory-marker\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(group, group+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, group); err != nil {
		t.Fatal(err)
	}
	code, reply := doJSON(t, s.Handler(), http.MethodGet, "/api/note/n", "")
	if code == http.StatusOK || strings.Contains(fmt.Sprint(reply), "outside-directory-marker") {
		t.Fatalf("parent symlink escaped the vault: status=%d", code)
	}
}
