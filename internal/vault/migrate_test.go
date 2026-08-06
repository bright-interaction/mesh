// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mustFM(s string) string {
	fm, _, _ := SplitFrontmatter(s)
	return fm
}

func TestMigrateFileAddsIdWhenRelatedIdempotent(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "codeindex.md")
	original := "---\ntype: entity\nupdated: 2026-04-10\n---\n# Codeindex\nbody\n\n## Related\n- [[mesh]]\n- [[dockyard|the platform]]\n"
	if err := os.WriteFile(p, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := MigrateFile(p, false)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed {
		t.Fatal("expected changes")
	}

	data, _ := os.ReadFile(p)
	s := string(data)
	for _, want := range []string{
		"id: codeindex", `when: "2026-04-10"`, "related:", "  - mesh", "  - dockyard",
		"type: entity", "updated: 2026-04-10", // existing keys preserved
	} {
		if !strings.Contains(s, want) {
			t.Errorf("after migrate missing %q\n---\n%s", want, s)
		}
	}

	fm, _, _ := ParseFrontmatter([]byte(mustFM(s)))
	if fm.ID != "codeindex" || fm.When != "2026-04-10" || len(fm.Related) != 2 {
		t.Errorf("reparse mismatch: id=%q when=%q related=%v", fm.ID, fm.When, fm.Related)
	}

	res2, err := MigrateFile(p, false)
	if err != nil {
		t.Fatal(err)
	}
	if res2.Changed {
		t.Errorf("second migrate should be a no-op, got actions %v", res2.Actions)
	}
}

func TestMigrateFileNoFrontmatter(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "orphan.md")
	if err := os.WriteFile(p, []byte("# Orphan\njust text\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := MigrateFile(p, false)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed {
		t.Fatal("expected changes")
	}
	s, _ := os.ReadFile(p)
	for _, want := range []string{"id: orphan", "type: note", "when:", "# Orphan", "just text"} {
		if !strings.Contains(string(s), want) {
			t.Errorf("missing %q\n%s", want, string(s))
		}
	}
}

func TestMigrateReportsFlywheelTODOs(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "some-decision.md")
	if err := os.WriteFile(p, []byte("---\nid: some-decision\ntype: decision\nwhen: 2026-01-01\n---\n# D\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := MigrateFile(p, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Changed {
		t.Error("already-keyed file should not change")
	}
	if len(res.Issues) != 3 {
		t.Errorf("expected 3 flywheel issues for a decision missing do/dont/why, got %v", res.Issues)
	}
}

func TestBackfillRelatedFile(t *testing.T) {
	for _, tc := range []struct {
		name       string
		content    string
		related    []string
		wantChange bool
		wantHas    []string
	}{
		{
			name:       "adds links to a note with none",
			content:    "---\nid: alpha\ntype: gotcha\ntags:\n    - mesh\n---\n# Alpha\nbody\n",
			related:    []string{"beta", "gamma"},
			wantChange: true,
			wantHas:    []string{"related:", "- beta", "- gamma"},
		},
		{
			name:       "never overwrites an author's own links",
			content:    "---\nid: alpha\nrelated:\n    - mine\n---\n# Alpha\n",
			related:    []string{"derived"},
			wantChange: false,
			wantHas:    []string{"- mine"},
		},
		{
			name:       "deduplicates and drops blanks",
			content:    "---\nid: alpha\n---\n# Alpha\n",
			related:    []string{"beta", "  ", "beta"},
			wantChange: true,
			wantHas:    []string{"- beta"},
		},
		{
			name:       "no links means no rewrite",
			content:    "---\nid: alpha\n---\n# Alpha\n",
			related:    nil,
			wantChange: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "n.md")
			if err := os.WriteFile(p, []byte(tc.content), 0o644); err != nil {
				t.Fatal(err)
			}
			res, err := BackfillRelatedFile(p, tc.related, false)
			if err != nil {
				t.Fatal(err)
			}
			if res.Changed != tc.wantChange {
				t.Fatalf("Changed = %v, want %v", res.Changed, tc.wantChange)
			}
			out, _ := os.ReadFile(p)
			for _, want := range tc.wantHas {
				if !strings.Contains(string(out), want) {
					t.Errorf("output missing %q:\n%s", want, out)
				}
			}
			// Whatever we wrote must still parse, or the note vanishes from the index.
			fmStr, _, had := SplitFrontmatter(string(out))
			if !had {
				t.Fatal("rewritten note lost its frontmatter block")
			}
			if _, _, err := ParseFrontmatter([]byte(fmStr)); err != nil {
				t.Fatalf("rewritten frontmatter does not parse (index would drop the note): %v", err)
			}
		})
	}
}
