// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"slices"
	"testing"
)

func TestATXHeadingIndentAndTabDelimiter(t *testing.T) {
	pn := parse(t, "atx.md", "# Plain\n ## One space\n  ## Two spaces\n   ##\tThree spaces and tab\n    ## Four spaces is code\n")
	got := make([]string, len(pn.Headings))
	for i, heading := range pn.Headings {
		got[i] = heading.Anchor
	}
	want := []string{"plain", "one-space", "two-spaces", "three-spaces-and-tab"}
	if !slices.Equal(got, want) {
		t.Fatalf("heading anchors = %q, want %q", got, want)
	}
}

func TestHeadingAnchorUsesMarkdownLinkLabelNotDestination(t *testing.T) {
	pn := parse(t, "links.md", "## Read [Mesh docs](https://example.com/private/path?q=secret)\n")
	if len(pn.Headings) != 1 {
		t.Fatalf("headings = %+v, want one", pn.Headings)
	}
	if got, want := pn.Headings[0].Anchor, "read-mesh-docs"; got != want {
		t.Fatalf("heading anchor = %q, want %q", got, want)
	}
}
