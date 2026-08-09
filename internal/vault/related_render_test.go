// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every flywheel type must end with its links, RENDERED. A note whose last section is a
// comment explaining that its links live in the frontmatter is useless to both readers
// that matter: an agent reading it through mesh_fetch, and a human reading the raw file.
func TestEveryFlywheelTypeRendersItsRelatedLinks(t *testing.T) {
	for _, ty := range []NoteType{TypeDecision, TypeGotcha, TypePostMortem} {
		t.Run(string(ty), func(t *testing.T) {
			sections := bodySections(ty)
			if len(sections) == 0 {
				t.Fatalf("%s has no body sections", ty)
			}
			last := sections[len(sections)-1]
			if last.heading != "Related" {
				t.Fatalf("%s ends with %q, not Related: a note has to end with where to go next", ty, last.heading)
			}
			if last.field == nil {
				t.Fatalf("%s's Related section renders no field, so it can only ever be a placeholder", ty)
			}
			fm := &Frontmatter{Related: []string{"alpha", "beta"}}
			got := renderSections(fm, sections)
			for _, want := range []string{"[[alpha]]", "[[beta]]"} {
				if !strings.Contains(got, want) {
					t.Errorf("%s body does not contain %s:\n%s", ty, want, got)
				}
			}
		})
	}
}

// An empty related: list must fall back to the placeholder rather than emitting an empty
// heading or, worse, a broken link.
func TestRelatedSectionWithNoLinksStaysAPlaceholder(t *testing.T) {
	got := renderSections(&Frontmatter{}, bodySections(TypeGotcha))
	if !strings.Contains(got, relatedPlaceholder) {
		t.Fatalf("an empty related: list did not fall back to the placeholder:\n%s", got)
	}
	if strings.Contains(got, "[[") {
		t.Fatalf("an empty related: list emitted a link:\n%s", got)
	}
}

// The backfill has to reach the notes that predate a heading, and it has to leave
// authored prose alone. Both in one fixture, because the danger is a pass that gets one
// right by getting the other wrong.
func TestBackfillAddsMissingRelatedAndKeepsAuthoredProse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "g.md")
	// A gotcha written before Related existed: Symptom is authored, Cause is still the
	// placeholder, and there is no Related heading at all.
	original := `---
id: g
type: gotcha
title: A gotcha
when: "2026-01-01"
related:
    - alpha
do: run the thing
dont: do not run the other thing
why: because the other thing eats the index
---

# A gotcha

## Symptom
The operator's own words about how this shows up.

## Cause
<!-- TODO: the root cause -->

## Fix
<!-- TODO: the resolution or workaround -->

<!-- authored by claude-code -->
`
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := BackfillBodyFile(path, false)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed {
		t.Fatal("the backfill reported no change on a note missing Related and holding two placeholders")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body := string(got)
	if !strings.Contains(body, "## Related\n- [[alpha]]") {
		t.Errorf("Related was not added with its rendered link:\n%s", body)
	}
	if !strings.Contains(body, "The operator's own words about how this shows up.") {
		t.Errorf("authored prose was destroyed:\n%s", body)
	}
	if !strings.Contains(body, "because the other thing eats the index") {
		t.Errorf("the Cause placeholder was not filled from why:\n%s", body)
	}
	// The provenance trailer stays last: a section bolted past the signature reads as an
	// afterthought.
	if !strings.HasSuffix(strings.TrimSpace(body), "<!-- authored by claude-code -->") {
		t.Errorf("the provenance comment is no longer last:\n%s", body)
	}

	// Idempotent. This pass rewrites every note in a 1200-note vault in place with no
	// backup, so a second run doing anything at all is a data-integrity bug, not a
	// cosmetic one.
	second, err := BackfillBodyFile(path, false)
	if err != nil {
		t.Fatal(err)
	}
	if second.Changed {
		t.Fatalf("second run changed the note again: %v", second.Actions)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != body {
		t.Fatal("a second backfill produced different bytes; the pass is not idempotent")
	}
}

// A note carrying the placeholder text this template USED to emit must still be
// upgradable, or the backfill silently skips the oldest notes: the ones with the most
// accumulated links and the most to gain.
func TestBackfillUpgradesARetiredPlaceholder(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "d.md")
	original := `---
id: d
type: decision
title: A decision
when: "2026-01-01"
related:
    - alpha
do: do the thing
dont: do not do the other
why: because
---

# A decision

## Context
because

## Decision
do the thing

## Consequences
do not do the other

## Related
<!-- linked notes from the related: field render in the graph -->
`
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := BackfillBodyFile(path, false); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "- [[alpha]]") {
		t.Fatalf("the retired Related placeholder was not upgraded to real links:\n%s", got)
	}
	if strings.Contains(string(got), "render in the graph") {
		t.Fatalf("the retired placeholder text survived:\n%s", got)
	}
}
