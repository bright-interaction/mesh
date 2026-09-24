// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package graph

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestTokenizePreservesVersionLiterals(t *testing.T) {
	for _, literal := range []string{"v0.41.2", "1.2.3", "V1.2.3-RC.1+Build.7"} {
		t.Run(literal, func(t *testing.T) {
			query := "Mesh " + literal + " activated for local MCP reader only"
			if got := TokenizeQuery(query); !slices.Contains(got, strings.ToLower(literal)) {
				t.Fatalf("version identity lost: TokenizeQuery(%q) = %q", query, got)
			}
		})
	}
	if slices.Equal(TokenizeQuery("v0.41.1"), TokenizeQuery("v0.41.2")) {
		t.Fatal("different patch versions collapse to identical queries")
	}
}

func TestVersionLiteralBoundariesAndQueryWork(t *testing.T) {
	for _, text := range []string{"xv0.41.2", "v0.41.20", "v0.41.2-rc.1", "v0.41.2+build.1", "127.0.0.1"} {
		if slices.Contains(Tokenize(text), "v0.41.2") {
			t.Fatalf("partial version identity extracted from %q", text)
		}
	}
	if !slices.Contains(Tokenize("(v0.41.2)."), "v0.41.2") {
		t.Fatal("sentence punctuation hid complete version")
	}
	for _, text := range []string{"v1.2.3-" + strings.Repeat("a", 200), "v1.2.3-rc" + strings.Repeat(".1", 10000)} {
		for _, term := range TokenizeQuery(text) {
			if strings.Contains(term, ".") {
				t.Fatalf("oversized literal retained/truncated: %q", term)
			}
		}
	}
	var b strings.Builder
	for i := 10; i < 30; i++ {
		fmt.Fprintf(&b, "v0.1.%d ", i)
	}
	terms := TokenizeQuery(b.String())
	work := 0
	compound := 0
	for _, term := range terms {
		work += literalTokenCount(term)
		if strings.Contains(term, ".") {
			compound++
		}
	}
	if work > MaxQueryTerms {
		t.Fatalf("phrase work %d exceeds cap %d", work, MaxQueryTerms)
	}
	if compound == 0 || compound == 20 {
		t.Fatalf("cap test did not retain and truncate phrases: %q", terms)
	}
	if got, want := TokenizeQuery(strings.Repeat("v0.41.2 ", 1000)), TokenizeQuery("v0.41.2"); !slices.Equal(got, want) {
		t.Fatalf("repeated versions must deduplicate: %q != %q", got, want)
	}
}

func BenchmarkTokenizeVersionedNote(b *testing.B) {
	for _, sample := range []struct{ name, text string }{
		{"versioned", "Mesh v0.41.2 activated for local MCP reader only. Keep rollback and verify the current release. "},
		{"ordinary", "Mesh was activated for local MCP reader only. Keep rollback and verify the current release. "},
	} {
		b.Run(sample.name, func(b *testing.B) {
			text := strings.Repeat(sample.text, 20)
			b.ReportAllocs()
			for b.Loop() {
				_ = Tokenize(text)
			}
		})
	}
}

func TestRankerDistinguishesRequestedPatchVersion(t *testing.T) {
	g := New()
	for _, pair := range []struct{ id, version string }{{"a-old", "v0.41.1"}, {"z-requested", "v0.41.2"}} {
		g.AddNode(note(pair.id, "Mesh "+pair.version+" activated for local MCP reader only", nil))
	}
	r := g.NewRanker()
	for _, query := range []string{"Mesh v0.41.2 activated for local MCP reader only", "v0.41.2"} {
		if hits := r.Score(query, 1); len(hits) != 1 || hits[0].Node.ID != "note:z-requested" {
			t.Fatalf("query %q: top hit = %+v, want requested patch version", query, hits)
		}
	}
	if hits := r.Score("Mesh v0.41.1 activated for local MCP reader only", 1); len(hits) != 1 || hits[0].Node.ID != "note:a-old" {
		t.Fatalf("historical version query must not become latest-only: %+v", hits)
	}
}
