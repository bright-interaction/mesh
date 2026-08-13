// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CreateNote has been guarded by validateRoundTrip since a note with unparseable
// frontmatter was found to vanish from search and the graph with no warning. The
// migrate writers were not: MigrateFile and BackfillScopeFile both prepended a
// hand-built YAML block with a bare os.WriteFile, so `mesh migrate` could still write a
// note whose frontmatter does not re-parse, making it silently invisible. Both must now
// refuse the write and leave the file exactly as it was.
//
// The two cases here used to be a quote inside a synthesized `when: "<value>"` and an
// operator scope of `[unclosed`. Neither breaks the block any more: values go through the
// yaml encoder now rather than into a formatted line, and those two inputs are asserted to
// survive intact in TestMigratedValuesAreEncodedNotFormatted. What a writer still cannot
// control is the block it is prepending TO, so that is what these cases plant: a note whose
// mapping is indented under the document root parses perfectly well on its own and cannot
// take a column-0 key in front of it.
func TestMigrateWritersRefuseUnparseableFrontmatter(t *testing.T) {
	cases := []struct {
		name     string
		original string
		write    func(path string) error
		wantErr  string
	}{
		{
			name:     "MigrateFile prepending to an indented block",
			original: "---\n  type: entity\n  updated: 2026-04-10\n---\n# Note\nbody\n",
			write: func(path string) error {
				_, err := MigrateFile(filepath.Dir(path), path, false)
				return err
			},
			wantErr: "invalid YAML",
		},
		{
			name:     "BackfillScopeFile prepending to an indented block",
			original: "---\n  id: n\n  type: note\n  when: \"2026-01-01\"\n---\n# Note\nbody\n",
			write: func(path string) error {
				_, err := BackfillScopeFile(path, "sales", false)
				return err
			},
			wantErr: "invalid YAML",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, "note.md")
			if err := os.WriteFile(p, []byte(tc.original), 0o644); err != nil {
				t.Fatal(err)
			}

			err := tc.write(p)
			if err == nil {
				t.Fatal("writing a note whose frontmatter does not re-parse must fail; it would be invisible to search and the graph")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to mention %q", err, tc.wantErr)
			}

			// The file on disk is untouched, and still parses.
			got, readErr := os.ReadFile(p)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if string(got) != tc.original {
				t.Errorf("a refused migration must not modify the file:\n got %q\nwant %q", got, tc.original)
			}
			fmText, _, had := SplitFrontmatter(string(got))
			if !had {
				t.Fatal("the original frontmatter block is gone")
			}
			if _, _, err := ParseFrontmatter([]byte(fmText)); err != nil {
				t.Errorf("the file left on disk no longer parses: %v", err)
			}
		})
	}
}

// The guard must not get in the way of the normal path: a migration that produces valid
// frontmatter still writes, and re-running stays idempotent.
func TestMigrateWritersStillWriteValidNotes(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "codeindex.md")
	if err := os.WriteFile(p, []byte("---\ntype: entity\nupdated: 2026-04-10\n---\n# Codeindex\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := MigrateFile(dir, p, false); err != nil {
		t.Fatalf("a valid migration must still write: %v", err)
	}
	if _, err := BackfillScopeFile(p, "sales", false); err != nil {
		t.Fatalf("a valid scope backfill must still write: %v", err)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	fmText, _, _ := SplitFrontmatter(string(data))
	parsed, _, err := ParseFrontmatter([]byte(fmText))
	if err != nil {
		t.Fatalf("the migrated note must re-parse: %v", err)
	}
	if parsed.ID != "codeindex" {
		t.Errorf("id = %q, want codeindex", parsed.ID)
	}
	if got := parsed.EffectiveScopes(); len(got) != 1 || got[0] != "sales" {
		t.Errorf("scope = %v, want [sales]", got)
	}
}
