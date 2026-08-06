// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package ingest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// idConn is a Connector that returns one doc with a caller-chosen ExternalID, so a
// test can drive the upsert path without an HTTP fixture.
type idConn struct {
	name string
	docs []Doc
}

func (c *idConn) Name() string { return c.name }
func (c *idConn) Key() string  { return c.name + ":test" }
func (c *idConn) Pull(_ context.Context, _ time.Time) ([]Doc, bool, error) {
	return c.docs, false, nil
}

// TestRenderDocPathIsUnchangedForASCII pins the compatibility half of the legacy
// fallback: for an all-ASCII ExternalID the legacy slug and the current slug are the
// same string, so no fallback can fire and the path is exactly what it always was.
func TestRenderDocPathIsUnchangedForASCII(t *testing.T) {
	cases := []struct {
		name       string
		connector  string
		externalID string
		want       string
	}{
		{"github issue", "github", "o-r-issue-7", "imported/github/github-o-r-issue-7.md"},
		{"slack ts", "slack", "C0123456-1717171717.000100", "imported/slack/slack-c0123456-1717171717-000100.md"},
		{"jira key", "jira", "PROJ-42", "imported/jira/jira-proj-42.md"},
		{"notion uuid", "notion", "8a1f2c3d-0000-4b5c-9d8e-0f1a2b3c4d5e", "imported/notion/notion-8a1f2c3d-0000-4b5c-9d8e-0f1a2b3c4d5e.md"},
		{"punctuation runs collapse", "linear", "ENG__123!!", "imported/linear/linear-eng-123.md"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := Doc{ExternalID: tc.externalID, Title: "T"}
			plain, _, err := RenderDoc(tc.connector, d)
			if err != nil {
				t.Fatal(err)
			}
			if plain != tc.want {
				t.Fatalf("RenderDoc path = %q, want %q", plain, tc.want)
			}
			// Every path is claimed to exist, which is the state most likely to make a
			// buggy fallback pick the wrong file. It must still not move.
			resolved, _, err := RenderDocResolved(tc.connector, d, func(string) bool { return true })
			if err != nil {
				t.Fatal(err)
			}
			if resolved != tc.want {
				t.Fatalf("RenderDocResolved path = %q, want %q (ASCII ids must never move)", resolved, tc.want)
			}
		})
	}
}

// TestIngestUpsertsNoteWrittenUnderLegacySlug is the regression this fallback exists
// for: a non-ASCII ExternalID that was ingested BEFORE Slugify learned to fold
// diacritics sits on disk under the old, letter-dropping slug. Re-pulling it must
// update that note in place, not mint a second one under the new slug.
func TestIngestUpsertsNoteWrittenUnderLegacySlug(t *testing.T) {
	// "arende-1" with a-ring; written as an escape so an editor round trip that
	// normalises the file cannot quietly turn this into the ASCII case.
	const externalID = "\u00e5rende-1"
	const legacyRel = "imported/slack/slack-rende-1.md" // pre-fold: the a-ring was dropped
	const newRel = "imported/slack/slack-arende-1.md"   // post-fold

	cases := []struct {
		name        string
		preexisting string // legacy note to plant, empty = none
		wantPath    string
		wantFiles   int
	}{
		{"legacy note exists, update in place", legacyRel, legacyRel, 1},
		{"no legacy note, mint the new slug", "", newRel, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "imported", "slack")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if tc.preexisting != "" {
				plant := filepath.Join(root, filepath.FromSlash(tc.preexisting))
				if err := os.WriteFile(plant, []byte("---\nid: slack-rende-1\n---\n\n# old\n\nstale body\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			conn := &idConn{name: "slack", docs: []Doc{{
				ExternalID: externalID,
				Title:      "[slack] status",
				Body:       "fresh body",
				SourceURL:  "slack://C1/1",
				CreatedAt:  "2026-05-01",
			}}}
			if _, err := Run(context.Background(), root, conn, time.Time{}); err != nil {
				t.Fatal(err)
			}
			if got := countMD(t, dir); got != tc.wantFiles {
				names, _ := os.ReadDir(dir)
				var have []string
				for _, n := range names {
					have = append(have, n.Name())
				}
				t.Fatalf("imported/slack holds %d note(s) %v, want %d (a duplicate means the upsert key moved)", got, have, tc.wantFiles)
			}
			b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(tc.wantPath)))
			if err != nil {
				t.Fatalf("expected the note at %s: %v", tc.wantPath, err)
			}
			body := string(b)
			if !strings.Contains(body, "fresh body") {
				t.Errorf("%s was not refreshed by the pull", tc.wantPath)
			}
			// The frontmatter id must agree with the filename, or lint reports the note
			// as a duplicate/unresolvable node.
			wantID := strings.TrimSuffix(filepath.Base(tc.wantPath), ".md")
			if !strings.Contains(body, "id: "+wantID+"\n") {
				t.Errorf("%s: frontmatter id does not match the filename, want id: %s", tc.wantPath, wantID)
			}
		})
	}
}

// TestIngestSecondPullDoesNotDuplicate walks the whole upgrade sequence: pull under
// the old slug (simulated by planting), re-pull twice under the new binary. Two runs
// catch a fallback that only matches once.
func TestIngestSecondPullDoesNotDuplicate(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "imported", "jira")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// "OKR-7" with an o-umlaut in the project key.
	const externalID = "\u00d6KR-7"
	legacy := filepath.Join(dir, "jira-kr-7.md")
	if err := os.WriteFile(legacy, []byte("---\nid: jira-kr-7\n---\n\n# old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	conn := &idConn{name: "jira", docs: []Doc{{ExternalID: externalID, Title: "Q3", Body: "b"}}}
	for i := 0; i < 2; i++ {
		if _, err := Run(context.Background(), root, conn, time.Time{}); err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
		if got := countMD(t, dir); got != 1 {
			t.Fatalf("after run %d imported/jira holds %d notes, want 1", i+1, got)
		}
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatalf("the legacy note is gone after two pulls: %v", err)
	}
}

// TestLegacySlugifyIsFrozen pins the historical algorithm itself. It must keep
// dropping non-ASCII letters; the day it starts folding them it stops recognising
// the files it exists to recognise.
func TestLegacySlugifyIsFrozen(t *testing.T) {
	cases := []struct{ in, want string }{
		{"o-r-issue-7", "o-r-issue-7"},
		{"\u00e5rende-1", "rende-1"}, // a-ring dropped
		{"\u00d6KR-7", "kr-7"},       // O-umlaut dropped
		{"caf\u00e9-2", "caf-2"},     // e-acute dropped
		{"\u00e5\u00e4\u00f6", ""},   // all non-ASCII collapses to nothing
		{"--lead-and-trail--", "lead-and-trail"},
	}
	for _, tc := range cases {
		if got := legacySlugify(tc.in); got != tc.want {
			t.Errorf("legacySlugify(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestAllNonASCIILegacySlugIsNotAdopted covers the deliberate exclusion: an
// ExternalID that legacy-slugged to nothing shared one collision bucket with every
// other such doc, so the fallback must not adopt it.
func TestAllNonASCIILegacySlugIsNotAdopted(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "imported", "slack")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	bucket := filepath.Join(dir, "slack-.md")
	if err := os.WriteFile(bucket, []byte("---\nid: slack-\n---\n\n# bucket\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	conn := &idConn{name: "slack", docs: []Doc{{ExternalID: "\u00e5\u00e4\u00f6", Title: "T", Body: "b"}}}
	if _, err := Run(context.Background(), root, conn, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "slack-aao.md")); err != nil {
		t.Fatalf("expected a fresh note at slack-aao.md, not an adoption of the collision bucket: %v", err)
	}
}

// TestRenderDocResolvedExistsMatrix pins the fallback over every combination of
// which note is already present, so the "only when the current note is ABSENT"
// precondition cannot be dropped in a later refactor without a test noticing. The
// both-exist row is the one that matters: a vault that already ran the new binary
// once holds both files, and adopting the legacy one there would abandon the note
// the previous run just wrote and start appending history to a stale sibling.
func TestRenderDocResolvedExistsMatrix(t *testing.T) {
	// "arende-1" with a-ring, written as an escape for the same reason as above.
	const externalID = "\u00e5rende-1"
	const legacyRel = "imported/slack/slack-rende-1.md"
	const newRel = "imported/slack/slack-arende-1.md"

	cases := []struct {
		name         string
		currentThere bool
		legacyThere  bool
		want         string
	}{
		{"neither exists, mint the new slug", false, false, newRel},
		{"only the legacy note exists, adopt it", false, true, legacyRel},
		{"only the current note exists, keep it", true, false, newRel},
		{"both exist, keep the current note", true, true, newRel},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			exists := func(rel string) bool {
				switch rel {
				case newRel:
					return tc.currentThere
				case legacyRel:
					return tc.legacyThere
				default:
					t.Fatalf("unexpected lookup for %q", rel)
					return false
				}
			}
			got, content, err := RenderDocResolved("slack", Doc{ExternalID: externalID, Title: "T", Body: "b"}, exists)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("RenderDocResolved path = %q, want %q", got, tc.want)
			}
			wantID := strings.TrimSuffix(filepath.Base(tc.want), ".md")
			if !strings.Contains(string(content), "id: "+wantID+"\n") {
				t.Errorf("frontmatter id does not match %s, want id: %s", tc.want, wantID)
			}
		})
	}
}
