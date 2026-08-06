// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixedNow(t *testing.T) {
	t.Helper()
	orig := Now
	Now = func() time.Time { return time.Date(2026, 6, 16, 9, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { Now = orig })
}

func TestCreateNoteGotchaLeavesFlywheelTODOs(t *testing.T) {
	fixedNow(t)
	dir := t.TempDir()
	res, err := CreateNote(dir, NewNoteSpec{Type: TypeGotcha, Title: "Modernc cannot load sqlite-vec"})
	if err != nil {
		t.Fatal(err)
	}
	if got := filepath.Base(filepath.Dir(res.Path)); got != "gotchas" {
		t.Errorf("placement = %s, want gotchas/", got)
	}
	if res.ID != "modernc-cannot-load-sqlite-vec" {
		t.Errorf("id = %q", res.ID)
	}
	if res.When != "2026-06-16" {
		t.Errorf("when = %q", res.When)
	}
	if len(res.TODOs) != 3 {
		t.Errorf("expected 3 flywheel TODOs, got %v", res.TODOs)
	}
	data, _ := os.ReadFile(res.Path)
	s := string(data)
	// yaml.v3 quotes date-like scalars so they stay strings; that round-trips fine.
	for _, want := range []string{
		"id: modernc-cannot-load-sqlite-vec",
		"type: gotcha",
		`when: "2026-06-16"`,
		`created: "2026-06-16"`,
		"do: TODO",
		"## Symptom",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("file missing %q\n---\n%s", want, s)
		}
	}
}

func TestCreateNoteCompleteDecisionLintsClean(t *testing.T) {
	fixedNow(t)
	dir := t.TempDir()
	res, err := CreateNote(dir, NewNoteSpec{
		Type:    TypeDecision,
		Title:   "Use Syncthing for sync",
		Do:      "use Syncthing plus a hub autocommit daemon",
		Dont:    "build a bespoke git hub first",
		Why:     "deletes the riskiest subsystem in the spec",
		Related: []string{"mesh", "[[codeindex]]"},
		Tags:    []string{"#Sync", "sync"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.TODOs) != 0 {
		t.Fatalf("expected clean note, got TODOs %v", res.TODOs)
	}
	data, _ := os.ReadFile(res.Path)
	s := string(data)
	if !strings.Contains(s, "codeindex") {
		t.Errorf("related link not normalized/rendered:\n%s", s)
	}
	if c := strings.Count(s, "- sync\n"); c != 1 {
		t.Errorf("tags not deduped/normalized: %d occurrences of sync\n%s", c, s)
	}
}

// A note whose judgment fields contain colon-space (the exact shape that produced
// invalid YAML and silently dropped real notes) must still round-trip cleanly,
// because yaml.Marshal quotes such values. CreateNote proves this before writing.
func TestCreateNoteRoundTripsColonValues(t *testing.T) {
	fixedNow(t)
	dir := t.TempDir()
	res, err := CreateNote(dir, NewNoteSpec{
		Type:  TypeGotcha,
		Title: "Mollie: no HMAC on webhooks, so re-fetch by id",
		Do:    "re-fetch GET /v2/payments/{id}: the webhook body is not signed",
		Dont:  "trust the webhook payload: it can be forged (self-upgrade hole)",
		Why:   "reason: the callback carries no signature to verify",
	})
	if err != nil {
		t.Fatalf("CreateNote rejected a note it should have round-tripped: %v", err)
	}
	data, err := os.ReadFile(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	fmStr, _, had := SplitFrontmatter(string(data))
	if !had {
		t.Fatal("written note has no frontmatter block")
	}
	parsed, _, err := ParseFrontmatter([]byte(fmStr))
	if err != nil {
		t.Fatalf("written note does not re-parse (it would be dropped by the index): %v\n%s", err, data)
	}
	if parsed.ID != res.ID {
		t.Errorf("id did not round-trip: parsed %q, created %q", parsed.ID, res.ID)
	}
	if parsed.Type != TypeGotcha {
		t.Errorf("type did not round-trip: %q", parsed.Type)
	}
}

func TestValidateRoundTrip(t *testing.T) {
	valid := "---\nid: ok-note\ntype: gotcha\ntitle: \"a: b\"\n---\n# body\n"
	if err := validateRoundTrip(valid, "ok-note"); err != nil {
		t.Errorf("valid note rejected: %v", err)
	}
	// A colon-space in an unquoted value is invalid YAML: the guard must reject it so
	// such a note is never written (it would vanish from search and the graph).
	broken := "---\nid: bad-note\ntitle: foo: bar\n---\n# body\n"
	if err := validateRoundTrip(broken, "bad-note"); err == nil {
		t.Error("invalid frontmatter should be rejected, would be silently dropped by the index")
	}
	// Valid YAML but the id changed under us: still a round-trip failure.
	mismatch := "---\nid: other\ntype: gotcha\n---\n# body\n"
	if err := validateRoundTrip(mismatch, "expected"); err == nil {
		t.Error("id mismatch should be rejected")
	}
	// No frontmatter at all is a failure too.
	if err := validateRoundTrip("# just a body\n", "x"); err == nil {
		t.Error("missing frontmatter should be rejected")
	}
}

func TestCreateNoteCollisionSuffixes(t *testing.T) {
	fixedNow(t)
	dir := t.TempDir()
	a, err := CreateNote(dir, NewNoteSpec{Type: TypeNote, Title: "Same Title"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := CreateNote(dir, NewNoteSpec{Type: TypeNote, Title: "Same Title"})
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == b.ID {
		t.Fatalf("expected unique ids, both %q", a.ID)
	}
	if b.ID != "same-title-2" {
		t.Errorf("collision id = %q, want same-title-2", b.ID)
	}
}

// A note authored with do/dont/why must carry that content in its BODY, under the
// type's real headings, not leave a TODO skeleton while the only copy of the content
// sits in frontmatter. Every agent write-back had that shape, so a reader saw a wall of
// YAML followed by four empty sections.
func TestRenderedBodyCarriesAuthoredContent(t *testing.T) {
	for _, tc := range []struct {
		name     string
		noteType NoteType
		wantPair [][2]string // heading -> content that must appear under it
	}{
		{"decision", TypeDecision, [][2]string{{"## Context", "BECAUSE-WHY"}, {"## Decision", "THE-DO"}, {"## Consequences", "THE-DONT"}}},
		{"gotcha", TypeGotcha, [][2]string{{"## Symptom", "THE-DONT"}, {"## Cause", "BECAUSE-WHY"}, {"## Fix", "THE-DO"}}},
		{"post-mortem", TypePostMortem, [][2]string{{"## Impact", "THE-DONT"}, {"## Root cause", "BECAUSE-WHY"}, {"## Follow-ups", "THE-DO"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := CreateNote(t.TempDir(), NewNoteSpec{
				Type: tc.noteType, Title: "A note", Do: "THE-DO", Dont: "THE-DONT", Why: "BECAUSE-WHY",
			})
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(res.Path)
			if err != nil {
				t.Fatal(err)
			}
			_, body, had := SplitFrontmatter(string(data))
			if !had {
				t.Fatal("note has no frontmatter block")
			}
			for _, hp := range tc.wantPair {
				heading, content := hp[0], hp[1]
				i := strings.Index(body, heading)
				if i < 0 {
					t.Fatalf("body missing heading %q:\n%s", heading, body)
				}
				rest := body[i:]
				if j := strings.Index(rest[len(heading):], "\n## "); j >= 0 {
					rest = rest[:len(heading)+j]
				}
				if !strings.Contains(rest, content) {
					t.Errorf("section %q does not carry %q (the content is stranded in frontmatter):\n%s", heading, content, rest)
				}
				if strings.Contains(rest, "<!-- TODO") {
					t.Errorf("section %q still holds a TODO stub despite authored content:\n%s", heading, rest)
				}
			}
		})
	}
}

// An unauthored note must still scaffold its TODO skeleton, so `mesh new` keeps
// prompting a human for what to write.
func TestRenderedBodyKeepsSkeletonWhenUnauthored(t *testing.T) {
	res, err := CreateNote(t.TempDir(), NewNoteSpec{Type: TypeGotcha, Title: "Empty one"})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(res.Path)
	for _, want := range []string{"## Symptom", "## Cause", "## Fix", "<!-- TODO"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("scaffold lost %q for an unauthored note:\n%s", want, data)
		}
	}
}
