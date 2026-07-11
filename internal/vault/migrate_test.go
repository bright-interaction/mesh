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
	p := filepath.Join(dir, "graphify.md")
	original := "---\ntype: entity\nupdated: 2026-04-10\n---\n# Graphify\nbody\n\n## Related\n- [[mesh]]\n- [[dockyard|the platform]]\n"
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
		"id: graphify", `when: "2026-04-10"`, "related:", "  - mesh", "  - dockyard",
		"type: entity", "updated: 2026-04-10", // existing keys preserved
	} {
		if !strings.Contains(s, want) {
			t.Errorf("after migrate missing %q\n---\n%s", want, s)
		}
	}

	fm, _, _ := ParseFrontmatter([]byte(mustFM(s)))
	if fm.ID != "graphify" || fm.When != "2026-04-10" || len(fm.Related) != 2 {
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
