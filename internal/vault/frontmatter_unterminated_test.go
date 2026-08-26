// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The live Corpus vault carried exactly one of these: a decision whose frontmatter block
// was opened and never closed. It indexed as an untagged, unlinked `type: note` with an
// id-as-prose title, and nothing anywhere reported a problem, because "opener with no
// closer" was indistinguishable from "this file has no frontmatter".
func TestUnterminatedFrontmatter(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{"unterminated, the live vault's shape", "---\nid: x\ntype: decision\ntags:\n    - flare\n", true},
		{"unterminated with a trailing blank line", "---\nid: x\n\n", true},
		{"opener only", "---\n", true},
		{"properly closed", "---\nid: x\n---\n\n# Title\n", false},
		{"closed with an empty block", "---\n---\n\nbody\n", false},
		{"no frontmatter at all", "# Title\n\nbody\n", false},
		{"body contains --- but no opener", "# Title\n\n---\n\nbody\n", false},
		{"CRLF line endings, closed", "---\r\nid: x\r\n---\r\n\r\nbody\r\n", false},
		{"CRLF line endings, unterminated", "---\r\nid: x\r\n", true},
		{"UTF-8 BOM before opener, closed", "\ufeff---\nid: x\n---\nbody\n", false},
		{"UTF-8 BOM before opener, unterminated", "\ufeff---\nid: x\n", true},
		{"delimiter trailing horizontal whitespace, closed", "--- \t\nid: x\n---\t \nbody\n", false},
		{"spaced opener, unterminated", "--- \t\nid: x\n", true},
		{"empty file", "", false},
		{"a horizontal rule below a closed block does not reopen one", "---\nid: x\n---\n\ntext\n\n---\n\nmore\n", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := UnterminatedFrontmatter(tc.content); got != tc.want {
				t.Fatalf("UnterminatedFrontmatter() = %v, want %v", got, tc.want)
			}
			// The two readers share one scan and must never disagree: an unterminated
			// block is never also a present block.
			if _, _, had := SplitFrontmatter(tc.content); had && tc.want {
				t.Fatal("SplitFrontmatter reported a block present for content that is unterminated")
			}
		})
	}
}

func TestSplitFrontmatterAcceptsBOMAndDelimiterWhitespace(t *testing.T) {
	fm, body, had := SplitFrontmatter("\ufeff--- \t\nid: unicode\ntype: note\n---\t \n# Body\n")
	if !had {
		t.Fatal("frontmatter with BOM/delimiter whitespace was not detected")
	}
	if !strings.Contains(fm, "id: unicode") || body != "# Body\n" {
		t.Fatalf("split fm=%q body=%q", fm, body)
	}
}

// mesh lint told the operator to run `mesh migrate` on the unterminated note. Doing so
// prepended a second, closed block declaring `type: note`, demoted the author's real
// declaration to body prose, and reported "migrated 1 of 1 files (0 errored)". One
// missing line became a note that authoritatively lied about itself.
func TestMigrateRefusesUnterminatedFrontmatter(t *testing.T) {
	const unterminated = "---\nid: x-note\ntype: decision\ntitle: Real Title\ntags:\n    - flare\n"

	writers := []struct {
		name string
		run  func(path string) (*MigrateResult, error)
	}{
		{"MigrateFile", func(p string) (*MigrateResult, error) { return MigrateFile(filepath.Dir(p), p, false) }},
		{"BackfillScopeFile", func(p string) (*MigrateResult, error) { return BackfillScopeFile(p, "team", false) }},
	}
	for _, w := range writers {
		t.Run(w.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "x-note.md")
			if err := os.WriteFile(path, []byte(unterminated), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := w.run(path)
			if !errors.Is(err, ErrUnterminatedFrontmatter) {
				t.Fatalf("got err %v, want ErrUnterminatedFrontmatter", err)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != unterminated {
				t.Fatalf("refusing writer still rewrote the file:\n%s", after)
			}
			// The specific corruption: a second frontmatter block, so the file ends up
			// with three --- delimiters and a type the author never wrote.
			if n := strings.Count(string(after), "\n---"); n > 0 {
				t.Fatalf("a closing/second delimiter appeared, file is now double-fronted:\n%s", after)
			}
		})
	}
}
