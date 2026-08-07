// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package ingest

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// Every non-ASCII id here is spelled out by code point instead of typed as a literal, so
// the source of this file stays pure ASCII. A diacritic typed into the source is one
// careless editor round trip away from being normalised to its ASCII base, which would
// turn a collision case into an ordinary one and leave the test passing while testing
// nothing.
const (
	aRing    = string(rune(0x00E5)) // a with ring above
	aUmlaut  = string(rune(0x00E4)) // a with diaeresis
	eAcute   = string(rune(0x00E9)) // e with acute
	cjkJapan = string(rune(0x65E5)) + string(rune(0x672C))
	cjkChina = string(rune(0x4E2D)) + string(rune(0x56FD))

	aRingArende   = aRing + "rende-1"   // slugs to "arende-1" now, "rende-1" before the fold
	aUmlautArende = aUmlaut + "rende-1" // a DIFFERENT document that folds onto the same slug
	asciiCafe     = "cafe-1"
	acuteCafe     = "caf" + eAcute + "-1" // slugged to "caf-1" before the fold, "cafe-1" after
)

// runPull runs one pull over vaultRoot and returns the imported dir plus the note names in
// it, sorted.
func runPull(t *testing.T, root, connector string, docs []Doc) (dir string, names []string) {
	t.Helper()
	dir = filepath.Join(root, "imported", connector)
	if _, err := Run(context.Background(), root, &idConn{name: connector, docs: docs}, time.Time{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	return dir, mdNames(t, dir)
}

func mdNames(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var out []string
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".md") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// noteHolding returns the name of the single note under dir whose body contains marker.
// It fails when no note or more than one note carries it, which is exactly what a silent
// overwrite looks like from the outside: the losing document's body is simply gone.
func noteHolding(t *testing.T, dir, marker string) string {
	t.Helper()
	var hits []string
	for _, name := range mdNames(t, dir) {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), marker) {
			hits = append(hits, name)
		}
	}
	if len(hits) != 1 {
		t.Fatalf("%q appears in %d note(s) %v under %s, want exactly 1", marker, len(hits), hits, dir)
	}
	return hits[0]
}

// assertIDMatchesFilename pins that every note's frontmatter id still agrees with its
// filename; a disambiguated path with the old id would read as a duplicate node.
func assertIDMatchesFilename(t *testing.T, dir string, names []string) {
	t.Helper()
	for _, name := range names {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if want := "id: " + strings.TrimSuffix(name, ".md") + "\n"; !strings.Contains(string(b), want) {
			t.Errorf("%s: frontmatter id does not match the filename, want %q", name, want)
		}
	}
}

// TestBatchKeepsCollidingDocsDistinct is the regression for the aliasing the diacritic
// fold introduced. vault.Slugify folds a-ring, a-diaeresis and e-acute onto their ASCII
// base and drops everything it cannot fold, so two upstream ids that used to slug apart
// now slug onto ONE note. The CLI wrote both to that one path and the hub keys
// Change.Upserts by path in a map, so within a single pull the second document silently
// REPLACED the first while the run counted two written.
//
// Both documents must survive, on distinct paths, each carrying its own body. Every pair
// is driven in both orders, because the fix must not depend on which one arrives first.
func TestBatchKeepsCollidingDocsDistinct(t *testing.T) {
	cases := []struct {
		name      string
		connector string
		a, b      string
	}{
		{
			name:      "two non-ascii spellings of one word",
			connector: "slack",
			a:         aRingArende,
			b:         aUmlautArende,
		},
		{
			name:      "a non-ascii id collides with an ascii id",
			connector: "jira",
			a:         asciiCafe,
			b:         acuteCafe,
		},
		{
			name:      "ids that slug to nothing at all",
			connector: "linear",
			a:         cjkJapan,
			b:         cjkChina,
		},
	}
	for _, tc := range cases {
		for _, flipped := range []bool{false, true} {
			name := tc.name
			if flipped {
				name += " (reversed pull order)"
			}
			t.Run(name, func(t *testing.T) {
				docs := []Doc{
					{ExternalID: tc.a, Title: "first", Body: "body-of-a", CreatedAt: "2026-05-01"},
					{ExternalID: tc.b, Title: "second", Body: "body-of-b", CreatedAt: "2026-05-01"},
				}
				if flipped {
					docs[0], docs[1] = docs[1], docs[0]
				}
				dir, names := runPull(t, t.TempDir(), tc.connector, docs)
				if len(names) != 2 {
					t.Fatalf("a pull of 2 colliding ids wrote %d note(s) %v, want 2 (one document was silently replaced)",
						len(names), names)
				}
				if a, b := noteHolding(t, dir, "body-of-a"), noteHolding(t, dir, "body-of-b"); a == b {
					t.Fatalf("both documents landed on %s", a)
				}
				assertIDMatchesFilename(t, dir, names)
			})
		}
	}
}

// TestAsciiIdKeepsItsPathAgainstADiacriticSibling pins WHICH side of a collision moves.
// An item imported for months under an all-ASCII id must not lose its note the day a
// diacritic spelling of the same id turns up in the pull, in either arrival order and
// whether or not the ASCII note is already on disk. The ambiguous spelling is the one
// that has to move: it is the one whose slug is not a faithful copy of itself.
func TestAsciiIdKeepsItsPathAgainstADiacriticSibling(t *testing.T) {
	const plain = "jira-cafe-1.md"
	ascii := Doc{ExternalID: asciiCafe, Title: "ascii", Body: "body-of-ascii", CreatedAt: "2026-05-01"}
	accented := Doc{ExternalID: acuteCafe, Title: "accented", Body: "body-of-accented", CreatedAt: "2026-05-01"}

	cases := []struct {
		name    string
		docs    []Doc
		preSeed bool // land the ascii note in its own earlier pull first
	}{
		{name: "ascii first", docs: []Doc{ascii, accented}},
		{name: "diacritic first", docs: []Doc{accented, ascii}},
		{name: "ascii already imported, diacritic first", docs: []Doc{accented, ascii}, preSeed: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if tc.preSeed {
				if _, names := runPull(t, root, "jira", []Doc{ascii}); len(names) != 1 || names[0] != plain {
					t.Fatalf("seed pull wrote %v, want [%s]", names, plain)
				}
			}
			dir, names := runPull(t, root, "jira", tc.docs)
			if len(names) != 2 {
				t.Fatalf("pull wrote %d note(s) %v, want 2", len(names), names)
			}
			if got := noteHolding(t, dir, "body-of-ascii"); got != plain {
				t.Fatalf("the ascii id was moved to %s; only the diacritic spelling may be disambiguated", got)
			}
			assertIDMatchesFilename(t, dir, names)
		})
	}
}

// TestPureASCIIBatchIsByteIdentical pins the compatibility half: a pull whose ids are all
// ASCII must produce exactly the paths and exactly the bytes it produced before batch
// resolution existed. RenderDoc, the unresolved renderer this work did not touch, is the
// oracle for both.
func TestPureASCIIBatchIsByteIdentical(t *testing.T) {
	docs := []Doc{
		{ExternalID: "o-r-issue-7", Title: "gh", Body: "b1", CreatedAt: "2026-05-01"},
		{ExternalID: "PROJ-42", Title: "jira", Body: "b2", CreatedAt: "2026-05-02"},
		{ExternalID: "8a1f2c3d-0000-4b5c-9d8e-0f1a2b3c4d5e", Title: "notion", Body: "b3", CreatedAt: "2026-05-03"},
		{ExternalID: "C0123456-1717171717.000100", Title: "slack", Body: "b4", CreatedAt: "2026-05-04"},
		{ExternalID: "ENG__123!!", Title: "linear", Body: "b5", CreatedAt: "2026-05-05"},
	}
	root := t.TempDir()
	_, names := runPull(t, root, "github", docs)
	if len(names) != len(docs) {
		t.Fatalf("pull wrote %d note(s) %v, want %d", len(names), names, len(docs))
	}
	for _, d := range docs {
		rel, want, err := RenderDoc("github", d)
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("%s: expected the note exactly where RenderDoc puts it: %v", d.ExternalID, err)
		}
		if string(got) != string(want) {
			t.Fatalf("%s landed different bytes than RenderDoc:\n got: %q\nwant: %q", d.ExternalID, got, want)
		}
	}
}

// TestBatchCollisionSurvivesReorderedPulls is the stability half. The first pull that sees
// a colliding pair decides which of the two keeps the plain slug; every later pull must
// reach the SAME answer even when the connector returns them in the other order. A
// resolver that only looked at the contested path would hand it to whoever arrived first,
// swap the two notes on the next pull, and leave a third, stale copy behind.
func TestBatchCollisionSurvivesReorderedPulls(t *testing.T) {
	docA := Doc{ExternalID: aRingArende, Title: "first", Body: "body-of-a", CreatedAt: "2026-05-01"}
	docB := Doc{ExternalID: aUmlautArende, Title: "second", Body: "body-of-b", CreatedAt: "2026-05-01"}

	root := t.TempDir()
	conn := &idConn{name: "slack", docs: []Doc{docA, docB}}
	if _, err := Run(context.Background(), root, conn, time.Time{}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "imported", "slack")
	first := mdNames(t, dir)
	wantA, wantB := noteHolding(t, dir, "body-of-a"), noteHolding(t, dir, "body-of-b")

	// Same two docs, opposite order, twice, so a resolver that only settles on the second
	// visit is caught too.
	for round := 1; round <= 2; round++ {
		conn.docs = []Doc{docB, docA}
		if _, err := Run(context.Background(), root, conn, time.Time{}); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		if got := mdNames(t, dir); len(got) != len(first) {
			t.Fatalf("round %d: reordering the pull left %v, want the original %v", round, got, first)
		}
		if got := noteHolding(t, dir, "body-of-a"); got != wantA {
			t.Fatalf("round %d: doc A moved from %s to %s", round, wantA, got)
		}
		if got := noteHolding(t, dir, "body-of-b"); got != wantB {
			t.Fatalf("round %d: doc B moved from %s to %s", round, wantB, got)
		}
	}
}

// TestBatchDoesNotShareOneLegacyNote covers the pre-fold vault. Both ids legacy-slugged to
// "rende-1", so a single note sits there from before the upgrade. The fallback that
// re-renders that note in place must hand it to ONE of them; the other still has to
// survive somewhere.
func TestBatchDoesNotShareOneLegacyNote(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "imported", "slack")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(dir, "slack-rende-1.md")
	if err := os.WriteFile(legacy, []byte("---\nid: slack-rende-1\n---\n\n# old\n\nstale body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	docs := []Doc{
		{ExternalID: aRingArende, Title: "first", Body: "body-of-a", CreatedAt: "2026-05-01"},
		{ExternalID: aUmlautArende, Title: "second", Body: "body-of-b", CreatedAt: "2026-05-01"},
	}
	if _, err := Run(context.Background(), root, &idConn{name: "slack", docs: docs}, time.Time{}); err != nil {
		t.Fatal(err)
	}
	names := mdNames(t, dir)
	if len(names) != 2 {
		t.Fatalf("a pull over a pre-fold vault left %v, want 2 notes (one document was folded into the other)", names)
	}
	if a, b := noteHolding(t, dir, "body-of-a"), noteHolding(t, dir, "body-of-b"); a == b {
		t.Fatalf("both documents landed on %s", a)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatalf("the legacy note is gone, so the in-place re-render was lost: %v", err)
	}
}

// TestBatchSeparatesCollisionWithoutOnDiskFeedback is the hub-shaped case. The hub renders
// a whole pull against what is at git HEAD and only commits afterwards, so its existence
// predicate never sees the notes the same pull is producing. Batch must therefore keep the
// two apart from its own claim bookkeeping, not from the filesystem answering differently
// on the second doc.
func TestBatchSeparatesCollisionWithoutOnDiskFeedback(t *testing.T) {
	atHead := func(string) bool { return false } // nothing committed yet, like a fresh hub pull
	docs := []Doc{
		{ExternalID: aRingArende, Title: "first", Body: "body-of-a"},
		{ExternalID: aUmlautArende, Title: "second", Body: "body-of-b"},
		{ExternalID: aRingArende, Title: "first again", Body: "body-of-a-v2"}, // the same doc pulled twice
	}
	OrderForResolution(docs)
	b := NewBatch("jira", atHead)
	owners := map[string]string{}
	for _, d := range docs {
		rel, content, err := b.Render(d)
		if err != nil {
			t.Fatalf("%q: %v", d.ExternalID, err)
		}
		if owner, taken := owners[rel]; taken && owner != d.ExternalID {
			t.Fatalf("%q reused the path already claimed by %q (%s); the hub's upsert map would drop one of them",
				d.ExternalID, owner, rel)
		}
		owners[rel] = d.ExternalID
		want := "id: " + strings.TrimSuffix(filepath.Base(rel), ".md") + "\n"
		if !strings.Contains(string(content), want) {
			t.Errorf("%s: frontmatter id does not match the path, want %q", rel, want)
		}
	}
	if len(owners) != 2 {
		t.Fatalf("three renders of two distinct ids produced %d path(s) %v, want 2", len(owners), owners)
	}
}
