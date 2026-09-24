// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/bright-interaction/mesh/internal/graph"
)

func TestSearchDistinguishesRequestedVersionBeforeLimit(t *testing.T) {
	for _, versions := range [][2]string{{"v0.41.1", "v0.41.2"}, {"1.2.3", "1.2.4"}, {"v1.2.3-rc.1", "v1.2.3-rc.2"}, {"v0.41.20", "v0.41.2"}, {"v1.2.3+build.1", "v1.2.3+build.2"}} {
		t.Run(versions[1], func(t *testing.T) {
			var notes []*ParsedNote
			for i, id := range []string{"a-old", "z-requested"} {
				title := "Mesh " + versions[i] + " activated for local MCP reader only"
				pn, err := Parse(id+".md", []byte(fmt.Sprintf("---\nid: %s\ntitle: %s\ntype: note\n---\n# %s\n", id, title, title)))
				if err != nil {
					t.Fatal(err)
				}
				notes = append(notes, pn)
			}
			g, _ := BuildGraph(notes)
			s, err := Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { s.Close() })
			if _, err := s.IndexVault(notes, g); err != nil {
				t.Fatal(err)
			}
			for _, limit := range []int{1, 2} {
				hits, err := s.Search(context.Background(), "Mesh "+versions[1]+" activated for local MCP reader only", limit)
				if err != nil {
					t.Fatal(err)
				}
				if len(hits) != limit || hits[0].NodeID != "note:z-requested" {
					t.Fatalf("limit %d: got %+v; exact version must rank before truncation without hiding broader matches", limit, hits)
				}
			}
		})
	}
}

func TestVersionFTSQueryAndExcerpt(t *testing.T) {
	if got := buildFTS5Query(`v0.41.2" OR NOT (secret*)`); got != `"v0" OR "41" OR "or" OR "secret" OR "v0.41.2"` {
		t.Fatalf("version/FTS grammar not quoted safely: %s", got)
	}
	text := strings.Repeat("unrelated prose ", 30) + "release 1.2.3 now"
	excerpt := buildExcerpt(text, graph.TokenizeQuery("1.2.3"), 12)
	if !strings.Contains(excerpt, "[1.2.3]") {
		t.Fatalf("bare version missing from bounded excerpt: %q", excerpt)
	}
	if got := buildExcerpt("release 1.2.30 now", graph.TokenizeQuery("1.2.3"), 12); strings.Contains(got, "[") {
		t.Fatalf("excerpt highlighted a different patch version: %q", got)
	}
}

func TestVersionExcerptDoesNotInventCompleteLiteral(t *testing.T) {
	for _, version := range []string{"v1.2.3-rc.1", "v1.2.3+build.1", "v1.2.3.4", "4.v1.2.3", "prefix-v1.2.3", "v1.2.3-", "e\u0301v1.2.3"} {
		excerpt := buildExcerpt("Mesh "+version+" reader activation", graph.TokenizeQuery("Mesh v1.2.3 reader activation"), 24)
		if slices.Contains(graph.TokenizeQuery(excerpt), "v1.2.3") {
			t.Fatalf("highlighting manufactured a stable version from %q: %q", version, excerpt)
		}
	}
	if got := buildExcerpt("Mesh (v1.2.3). reader activation", graph.TokenizeQuery("v1.2.3"), 24); !strings.Contains(got, "[v1.2.3]") {
		t.Fatalf("sentence punctuation hid a complete version: %q", got)
	}
}

func TestVersionExcerptWindowDoesNotCutFields(t *testing.T) {
	query := graph.TokenizeQuery("reader 1.2.3")
	for _, text := range []string{
		"reader one two three four five six seven eight 1.2.3-rc.1 after",
		"reader one two three four five six seven eight 1.2.3+build.1 after",
		"reader one two three four five six seven eight 1.2.3.4 after",
		"before 9.1.2.3 reader after",
		"before -1.2.3 reader after",
		"reader 1.2.3-",
	} {
		for width := 1; width <= 24; width++ {
			excerpt := buildExcerpt(text, query, width)
			if slices.Contains(graph.TokenizeQuery(excerpt), "1.2.3") {
				t.Fatalf("window %d invented stable literal from %q: %q", width, text, excerpt)
			}
			if len(tokenSpans(excerpt)) > width {
				t.Fatalf("window %d grew beyond its bound: %q", width, excerpt)
			}
		}
	}
}

func TestVersionExcerptHighlightCannotSplitLongerField(t *testing.T) {
	query := graph.TokenizeQuery("reader 1.2.3 41")
	for _, text := range []string{"reader 1.2.3.41 after", "reader 41.1.2.3 after", "reader 1.2.3-41 after", "reader 1.2.3+41 after"} {
		for width := 1; width <= 24; width++ {
			excerpt := buildExcerpt(text, query, width)
			if slices.Contains(graph.TokenizeQuery(excerpt), "1.2.3") {
				t.Fatalf("window %d highlighting invented stable version from %q: %q", width, text, excerpt)
			}
		}
	}
}

func TestVersionSearchScanBoundaryCannotInventStableLiteral(t *testing.T) {
	for _, filler := range []string{"x", "å"} {
		t.Run(filler, func(t *testing.T) {
			const core = "1.2.3"
			body := strings.Repeat(filler, excerptScanChars-1-len(core)) + " " + core + "-rc.1 after"
			pn, err := Parse("release.md", []byte("---\nid: release\ntitle: Release notes\ntype: note\n---\n"+body))
			if err != nil {
				t.Fatal(err)
			}
			notes := []*ParsedNote{pn}
			g, _ := BuildGraph(notes)
			s, err := Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { s.Close() })
			if _, err := s.IndexVault(notes, g); err != nil {
				t.Fatal(err)
			}
			hits, err := s.Search(context.Background(), core, 1)
			if err != nil || len(hits) != 1 {
				t.Fatalf("search hits=%+v err=%v", hits, err)
			}
			if slices.Contains(graph.TokenizeQuery(hits[0].Snippet), core) {
				t.Fatalf("SQL prefix manufactured stable version: %.100q", hits[0].Snippet)
			}
		})
	}
	// SQLite caps characters, not bytes. A short non-ASCII body stays intact.
	short := strings.Repeat("å", excerptScanChars/2)
	if utf8.RuneCountInString(short) >= excerptScanChars || trimExcerptScanTail(short) != short {
		t.Fatal("scan-tail trimming confused bytes with characters")
	}
}
