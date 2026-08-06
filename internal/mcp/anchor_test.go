// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"strings"
	"testing"
)

const anchorDoc = "# Note\n\nintro text\n\n## Intro\n\nshort\n\n## Details\n\n" +
	"a very long section that stands in for a multi-megabyte note body\n"

// sectionByAnchor is a NARROWING function, and it used to return its whole input on a
// miss. One wrong character in an anchor - "introduction" for "intro" - turned a 27-char
// reply into the entire note, with no error and no flag, so the caller that did the
// token-frugal thing got the largest possible payload.
func TestSectionByAnchorMissDoesNotReturnWholeNote(t *testing.T) {
	if _, ok := sectionByAnchor(anchorDoc, "introduction"); ok {
		t.Fatal("a missing anchor reported success")
	}
	sec, ok := sectionByAnchor(anchorDoc, "intro")
	if !ok {
		t.Fatal("the real anchor did not resolve")
	}
	if strings.Contains(sec, "multi-megabyte") {
		t.Fatalf("section bled past its heading:\n%s", sec)
	}
	if len(sec) >= len(anchorDoc) {
		t.Fatalf("narrowing returned %d chars of a %d-char note", len(sec), len(anchorDoc))
	}
}

// The miss is only actionable if it can name the real options. Slugify is ASCII-only, so
// a heading like "## Atgarder" does not slug to anything a caller would guess.
func TestAnchorsOfListsRealAnchors(t *testing.T) {
	got := anchorsOf(anchorDoc)
	want := map[string]bool{"note": true, "intro": true, "details": true}
	for _, a := range got {
		if !want[a] {
			t.Errorf("unexpected anchor %q", a)
		}
		delete(want, a)
	}
	if len(want) != 0 {
		t.Errorf("missing anchors: %v (got %v)", want, got)
	}
}

// A '#' inside a fenced code block is not a heading, in either direction.
func TestAnchorsOfIgnoresFencedCode(t *testing.T) {
	doc := "## Real\n\n```sh\n# not a heading\n```\n\n## Also Real\n"
	for _, a := range anchorsOf(doc) {
		if strings.Contains(a, "not") {
			t.Fatalf("a comment inside a fence was read as a heading: %v", anchorsOf(doc))
		}
	}
	if len(anchorsOf(doc)) != 2 {
		t.Fatalf("got %v, want the two real headings", anchorsOf(doc))
	}
}
