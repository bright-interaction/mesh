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
