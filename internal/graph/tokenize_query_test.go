// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package graph

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestTokenizeQueryCapsAndDeduplicates pins the structural half of the uncapped query
// defect: the cap lives in the tokenizer, so it holds on every surface that turns
// user text into search terms, the CLI and the TUI included, not only on the two
// entry points that also refuse an over-long request.
func TestTokenizeQueryCapsAndDeduplicates(t *testing.T) {
	t.Run("distinct terms are capped", func(t *testing.T) {
		var b strings.Builder
		for i := 0; i < MaxQueryTerms*100; i++ {
			fmt.Fprintf(&b, "deploy%d ", i)
		}
		got := TokenizeQuery(b.String())
		if len(got) != MaxQueryTerms {
			t.Fatalf("TokenizeQuery over %d distinct words returned %d terms, want %d: both keyword signals cost time linear in the term count",
				MaxQueryTerms*100, len(got), MaxQueryTerms)
		}
	})

	t.Run("a repeated word collapses to one term", func(t *testing.T) {
		got := TokenizeQuery(strings.Repeat("deploy ", 10000))
		if len(got) != 1 || got[0] != "deploy" {
			t.Fatalf("TokenizeQuery of one word repeated 10000 times = %v, want [deploy]: repeating a phrase in an OR expression never changed which notes match, only how long the match takes", got)
		}
	})

	t.Run("an ordinary question is untouched", func(t *testing.T) {
		// Compared against Tokenize rather than a literal list, so this stays true when
		// the corpus-side tokenizer or the stopword set changes: the cap must change
		// nothing about how a real query is read.
		const q = "how do we deploy the vault index"
		got, want := TokenizeQuery(q), Tokenize(q)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("TokenizeQuery = %v, want the same terms Tokenize gives (%v): the cap must not change ordinary retrieval", got, want)
		}
	})
}

// TestScoreScopedBoundsAnUnboundedQuery is the graph-BM25 twin of the FTS regression
// in internal/index. ScoreScoped walks the whole corpus once per query term, so an
// uncapped query text was uncapped CPU here too, and this path is reached by the same
// mesh_search call.
func TestScoreScopedBoundsAnUnboundedQuery(t *testing.T) {
	g := New()
	// Corpus size matters to what this test can see: the cost is termCount x
	// corpusSize, so a handful of notes stays fast even uncapped and the assertion
	// would prove nothing. This is a plausible mid-size vault.
	for i := 0; i < 20000; i++ {
		id := fmt.Sprintf("note:n%03d", i)
		g.AddNode(&Node{ID: id, Kind: "note", Label: fmt.Sprintf("Deploy note %d", i),
			Attrs: map[string]any{"body": "deploy pipeline retention window rollback vault index"}})
	}
	r := g.NewRanker()

	var qb strings.Builder
	for i := 0; qb.Len() < 1<<20; i++ {
		fmt.Fprintf(&qb, "deploy%d ", i)
	}

	done := make(chan int, 1)
	start := time.Now()
	go func() { done <- len(r.ScoreScoped(qb.String(), 10, nil)) }()
	select {
	case <-done:
		t.Logf("a %d-byte query scored in %s", qb.Len(), time.Since(start))
	case <-time.After(5 * time.Second):
		t.Fatalf("ScoreScoped did not return within 5s for a %d-byte query: it walks the corpus once per query term", qb.Len())
	}
}
