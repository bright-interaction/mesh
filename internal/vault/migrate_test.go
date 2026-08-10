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

func TestBackfillBodyFile(t *testing.T) {
	const authored = "---\nid: g\ntype: gotcha\ntitle: G\ndo: RUN-THIS\ndont: AVOID-THIS\nwhy: BECAUSE\n---\n\n# G\n\n" +
		"## Symptom\n<!-- TODO: how the problem shows up -->\n\n" +
		"## Cause\n<!-- TODO: the root cause -->\n\n" +
		"## Fix\n<!-- TODO: the resolution or workaround -->\n"

	t.Run("fills TODO sections from frontmatter", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "n.md")
		os.WriteFile(p, []byte(authored), 0o644)
		res, err := BackfillBodyFile(p, false)
		if err != nil || !res.Changed {
			t.Fatalf("Changed=%v err=%v", res.Changed, err)
		}
		out, _ := os.ReadFile(p)
		for _, want := range []string{"RUN-THIS", "AVOID-THIS", "BECAUSE"} {
			if !strings.Contains(string(out), want) {
				t.Errorf("body missing %q:\n%s", want, out)
			}
		}
		// The frontmatter copy must survive: retrieval reads do/dont/why from there.
		fmStr, _, had := SplitFrontmatter(string(out))
		if !had || !strings.Contains(fmStr, "do:") {
			t.Errorf("frontmatter lost its fields, retrieval cards would degrade:\n%s", out)
		}
	})

	t.Run("is idempotent", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "n.md")
		os.WriteFile(p, []byte(authored), 0o644)
		BackfillBodyFile(p, false)
		first, _ := os.ReadFile(p)
		res, _ := BackfillBodyFile(p, false)
		second, _ := os.ReadFile(p)
		if res.Changed || string(first) != string(second) {
			t.Errorf("second run changed the file (Changed=%v)", res.Changed)
		}
	})

	t.Run("never overwrites an authored section", func(t *testing.T) {
		src := strings.Replace(authored, "## Fix\n<!-- TODO: the resolution or workaround -->", "## Fix\nHUMAN-WROTE-THIS", 1)
		p := filepath.Join(t.TempDir(), "n.md")
		os.WriteFile(p, []byte(src), 0o644)
		BackfillBodyFile(p, false)
		out, _ := os.ReadFile(p)
		// Scope the check to the BODY: `do: RUN-THIS` legitimately stays in the
		// frontmatter, so asserting over the whole file would fail on the copy we
		// deliberately keep.
		_, body, _ := SplitFrontmatter(string(out))
		if !strings.Contains(body, "HUMAN-WROTE-THIS") {
			t.Errorf("lost the authored section:\n%s", body)
		}
		if strings.Contains(body, "RUN-THIS") {
			t.Errorf("overwrote the authored Fix section with the `do` field:\n%s", body)
		}
	})

	t.Run("refuses a note with no frontmatter", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "n.md")
		os.WriteFile(p, []byte("# no frontmatter\n"), 0o644)
		if _, err := BackfillBodyFile(p, false); err == nil {
			t.Error("expected an error for a note with no frontmatter block")
		}
	})
}

// TestBackfillBodyFileRepairsReferencePages covers the shape that produced 18 dead bodies
// in the live vault: an agent writes an entity through mesh_write_entity, the page's whole
// substance lands in frontmatter `why`, and the body stays a TODO skeleton whose Related
// section DESCRIBES the links instead of listing them. Nothing was empty, so lint stayed
// at zero notices and nothing ever pointed at them.
func TestBackfillBodyFileRepairsReferencePages(t *testing.T) {
	entity := "---\nid: e\ntype: entity\ntitle: E\nrelated:\n  - other-note\nwhy: WHAT-THIS-IS\n---\n\n# E\n\n" + tmplEntity

	t.Run("fills an entity from why and renders its links", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "e.md")
		os.WriteFile(p, []byte(entity), 0o644)
		res, err := BackfillBodyFile(p, false)
		if err != nil || !res.Changed {
			t.Fatalf("Changed=%v err=%v", res.Changed, err)
		}
		out, _ := os.ReadFile(p)
		_, body, _ := SplitFrontmatter(string(out))
		if !strings.Contains(body, "## What it does\nWHAT-THIS-IS") {
			t.Errorf("entity body did not take its prose from why:\n%s", body)
		}
		if !strings.Contains(body, "- [[other-note]]") {
			t.Errorf("entity Related still describes its links instead of listing them:\n%s", body)
		}
		// The sections with no field behind them keep their prompt rather than a guess.
		if !strings.Contains(body, "## Key facts\n<!-- TODO") {
			t.Errorf("a section with no source field should keep its TODO prompt:\n%s", body)
		}
	})

	t.Run("create-time and repair-time agree", func(t *testing.T) {
		// The two paths had no reason to produce the same bytes, and a projection that
		// only ran at create time is why the live pages never healed.
		fm := &Frontmatter{ID: "e", Type: TypeEntity, Title: "E", Why: "WHAT-THIS-IS", Related: StringList{"other-note"}}
		created := renderBody(fm)

		p := filepath.Join(t.TempDir(), "e.md")
		os.WriteFile(p, []byte(entity), 0o644)
		BackfillBodyFile(p, false)
		out, _ := os.ReadFile(p)
		_, withTitle, _ := SplitFrontmatter(string(out))
		// SplitFrontmatter's body keeps the "# Title" heading; renderBody starts below it.
		repaired := strings.TrimPrefix(strings.TrimSpace(withTitle), "# E")

		if strings.TrimSpace(created) != strings.TrimSpace(repaired) {
			t.Errorf("create-time and repair-time bodies differ:\ncreated:\n%s\nrepaired:\n%s", created, repaired)
		}
	})

	t.Run("fills a plain note from why/do/dont", func(t *testing.T) {
		note := "---\nid: n\ntype: note\ntitle: N\ndo: DO-THIS\ndont: NOT-THIS\nwhy: BECAUSE\n---\n\n# N\n\n" + tmplNote
		p := filepath.Join(t.TempDir(), "n.md")
		os.WriteFile(p, []byte(note), 0o644)
		if _, err := BackfillBodyFile(p, false); err != nil {
			t.Fatal(err)
		}
		out, _ := os.ReadFile(p)
		_, body, _ := SplitFrontmatter(string(out))
		for _, want := range []string{"## Overview\nBECAUSE", "## Do\nDO-THIS", "## Don't\nNOT-THIS"} {
			if !strings.Contains(body, want) {
				t.Errorf("note body missing %q:\n%s", want, body)
			}
		}
	})

	t.Run("added sections land before the closing Related list", func(t *testing.T) {
		// tmplNote ends with Related, so a naive append put the note's own Do and Don't
		// AFTER its link list: Overview, Related, Do, Don't.
		note := "---\nid: n\ntype: note\ntitle: N\nrelated:\n  - other-note\ndo: DO-THIS\ndont: NOT-THIS\nwhy: BECAUSE\n---\n\n# N\n\n" + tmplNote
		p := filepath.Join(t.TempDir(), "n.md")
		os.WriteFile(p, []byte(note), 0o644)
		if _, err := BackfillBodyFile(p, false); err != nil {
			t.Fatal(err)
		}
		out, _ := os.ReadFile(p)
		var order []string
		for _, line := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(line, "## ") {
				order = append(order, strings.TrimPrefix(line, "## "))
			}
		}
		want := []string{"Overview", "Do", "Don't", "Related"}
		if strings.Join(order, ",") != strings.Join(want, ",") {
			t.Errorf("section order is %v, want %v:\n%s", order, want, out)
		}
	})

	t.Run("is idempotent on a reference page", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "e.md")
		os.WriteFile(p, []byte(entity), 0o644)
		BackfillBodyFile(p, false)
		first, _ := os.ReadFile(p)
		res, _ := BackfillBodyFile(p, false)
		second, _ := os.ReadFile(p)
		if res.Changed || string(first) != string(second) {
			t.Errorf("second run changed the file (Changed=%v)", res.Changed)
		}
	})

	t.Run("leaves an empty scaffold alone", func(t *testing.T) {
		// `mesh new entity` writes no why at all. There is nothing to project, and
		// inventing one is the failure this whole area exists to avoid.
		empty := "---\nid: e\ntype: entity\ntitle: E\n---\n\n# E\n\n" + tmplEntity
		p := filepath.Join(t.TempDir(), "e.md")
		os.WriteFile(p, []byte(empty), 0o644)
		res, _ := BackfillBodyFile(p, false)
		out, _ := os.ReadFile(p)
		if res.Changed || string(out) != empty {
			t.Errorf("an unauthored scaffold must be left exactly as written (Changed=%v)", res.Changed)
		}
	})
}
