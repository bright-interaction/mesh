// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"slices"
	"strings"
	"testing"
)

func TestSectionByAnchorPrefersCurrentSlugOverEarlierLegacySlug(t *testing.T) {
	doc := "## Åtgärder\n\nlegacy fallback body\n\n## tg rder\n\nexact current body\n"
	section, ok := sectionByAnchor(doc, "tg-rder")
	if !ok {
		t.Fatal("current anchor did not resolve")
	}
	if !strings.Contains(section, "exact current body") || strings.Contains(section, "legacy fallback body") {
		t.Fatalf("legacy fallback shadowed an exact current anchor:\n%s", section)
	}
}

func TestSectionByAnchorNormalizesRequestedAnchorNFC(t *testing.T) {
	doc := "## \ud55c\uae00\n\nnormalized body\n"
	// The request is the canonically equivalent decomposed Jamo form of "한글".
	section, ok := sectionByAnchor(doc, "\u1112\u1161\u11ab\u1100\u1173\u11af")
	if !ok || !strings.Contains(section, "normalized body") {
		t.Fatalf("decomposed anchor did not resolve: ok=%v section=%q", ok, section)
	}
}

func TestMCPAnchorsUseCommonATXIndentAndDelimiter(t *testing.T) {
	doc := "   ##\tTabbed heading\n\nbody\n\n    ## Four spaces is code\n"
	if got, want := anchorsOf(doc), []string{"tabbed-heading"}; !slices.Equal(got, want) {
		t.Fatalf("anchorsOf = %q, want %q", got, want)
	}
	if _, ok := sectionByAnchor(doc, "tabbed-heading"); !ok {
		t.Fatal("valid indented ATX heading with a tab delimiter did not resolve")
	}
}

func TestMCPHeadingAnchorUsesMarkdownLinkLabelNotDestination(t *testing.T) {
	doc := "## Read [Mesh docs](https://example.com/private/path?q=secret)\n\nbody\n"
	if got, want := anchorsOf(doc), []string{"read-mesh-docs"}; !slices.Equal(got, want) {
		t.Fatalf("anchorsOf = %q, want %q", got, want)
	}
	if _, ok := sectionByAnchor(doc, "read-mesh-docs"); !ok {
		t.Fatal("anchor made from the visible link label did not resolve")
	}
}
