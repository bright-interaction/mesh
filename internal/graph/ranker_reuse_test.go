// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package graph

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func reuseTestGraph() *Graph {
	g := New()
	g.AddNode(note("a", "Mesh café deploy", map[string]any{"do": "deploy safely deploy", "scope": "public", "superseded_by": "classified"}))
	g.AddNode(note("b", "Retrieval shared knowledge", map[string]any{"why": "shared deploy knowledge", "scope": "dev"}))
	g.AddNode(&Node{ID: "heading:c", Kind: "heading", Label: "Invisible heading"})
	return g
}

func assertRankerEquivalent(t *testing.T, got, want *Ranker) {
	t.Helper()
	if got.n != want.n || got.avgLen != want.avgLen || !reflect.DeepEqual(got.df, want.df) || !reflect.DeepEqual(got.docs, want.docs) {
		t.Fatal("reused corpus statistics differ from full build")
	}
	for _, query := range []string{"deploy", "shared knowledge", "cafe\u0301", "classified", "replacement edited", "", "invisible heading"} {
		for _, allowed := range []map[string]bool{nil, {}, {"public": true}, {"dev": true}} {
			for _, limit := range []int{0, 1, 10} {
				if got, want := got.ScoreScoped(query, limit, allowed), want.ScoreScoped(query, limit, allowed); !reflect.DeepEqual(got, want) {
					t.Fatalf("ranker results differ for query=%q scopes=%v limit=%d", query, allowed, limit)
				}
			}
		}
	}
}

func TestRankerReuseMatchesFullBuild(t *testing.T) {
	for _, tc := range []struct {
		name   string
		edit   func(*Graph)
		reuseA bool
	}{
		{"unchanged", func(*Graph) {}, true},
		{"label", func(g *Graph) { g.nodes["note:a"].Label = "replacement edited" }, false},
		{"attribute", func(g *Graph) { g.SetNodeAttr("note:a", "do", "replacement edited") }, false},
		{"new_empty_attribute", func(g *Graph) { g.SetNodeAttr("note:a", "new", "") }, false},
		{"removed_attribute", func(g *Graph) { delete(g.nodes["note:a"].Attrs, "do") }, false},
		{"string_to_nonstring", func(g *Graph) { g.SetNodeAttr("note:a", "do", 42) }, false},
		{"renamed_attribute", func(g *Graph) {
			g.SetNodeAttr("note:a", "renamed", g.nodes["note:a"].Attrs["do"])
			delete(g.nodes["note:a"].Attrs, "do")
		}, false},
		{"scope", func(g *Graph) { g.SetNodeAttr("note:a", "scope", "dev") }, false},
		{"nonstring", func(g *Graph) { g.SetNodeAttr("note:a", "nested", map[string]any{"data": "ignored"}) }, true},
		{"metadata", func(g *Graph) {
			n := g.nodes["note:a"]
			n.NotePath, n.NoteID, n.Anchor, n.SourceLoc = "new/path.md", "new-note-id", "section", "L42"
			n.Community, n.Degree, n.KnowledgeDegree = 7, 9, 3
			g.SetNodeAttr(n.ID, "superseded_by", "new-classified")
		}, true},
		{"add_note", func(g *Graph) { g.AddNode(note("added", "deploy shared replacement", nil)) }, true},
		{"delete_note", func(g *Graph) { delete(g.nodes, "note:b") }, true},
		{"note_to_heading", func(g *Graph) { g.nodes["note:a"].Kind = "heading" }, false},
		{"heading_to_note", func(g *Graph) { g.nodes["heading:c"].Kind = "note" }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oldGraph, nextGraph := reuseTestGraph(), reuseTestGraph()
			old := oldGraph.NewRanker()
			oldCopy := oldGraph.NewRanker()
			tc.edit(nextGraph)
			got, err := nextGraph.NewRankerReusingContext(context.Background(), old)
			if err != nil {
				t.Fatal(err)
			}
			assertRankerEquivalent(t, got, nextGraph.NewRanker())
			assertRankerEquivalent(t, old, oldCopy)
			if reused := got.docs["note:a"] == old.docs["note:a"]; reused != tc.reuseA {
				t.Fatalf("reused document = %t, want %t", reused, tc.reuseA)
			}
			for id, n := range got.node {
				if n != nextGraph.nodes[id] {
					t.Fatalf("old node retained for %q", id)
				}
			}
		})
	}
}

func TestRankerReuseOwnsSearchableInputs(t *testing.T) {
	g := reuseTestGraph()
	old := g.NewRanker()
	oldDocument := old.docs["note:a"]
	oldTF := make(map[string]int, len(oldDocument.tf))
	for k, v := range oldDocument.tf {
		oldTF[k] = v
	}
	g.SetNodeAttr("note:a", "do", "replacement edited")
	got, err := g.NewRankerReusingContext(nil, old)
	if err != nil {
		t.Fatal(err)
	}
	assertRankerEquivalent(t, got, g.NewRanker())
	if got.docs["note:a"] == oldDocument || oldDocument.attrs["do"] != "deploy safely deploy" || !reflect.DeepEqual(oldDocument.tf, oldTF) {
		t.Fatal("mutable graph attributes corrupted saved ranker input or terms")
	}
}

func TestRankerReuseConcurrentQueriesAndCancellation(t *testing.T) {
	g := reuseTestGraph()
	old := g.NewRanker()
	oldCopy := g.NewRanker()
	ctx, cancel := context.WithCancelCause(context.Background())
	cause := errors.New("stop ranker refresh")
	cancel(cause)
	if got, err := g.NewRankerReusingContext(ctx, old); got != nil || !errors.Is(err, cause) {
		t.Fatalf("canceled reuse = %v, %v", got, err)
	}
	// Cancel after entry while assembling private statistics from reused terms.
	midBuild := newCancelAfterChecksContext(5, cause)
	if got, err := g.NewRankerReusingContext(midBuild, old); got != nil || !errors.Is(err, cause) {
		t.Fatalf("mid-build cancellation published partial statistics: %v, %v", got, err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			old.ScoreScoped("shared deploy", 3, map[string]bool{"public": true})
		}
	}()
	for i := 0; i < 25; i++ {
		next := reuseTestGraph()
		next.SetNodeAttr("note:b", "why", fmt.Sprintf("replacement edited %d", i))
		got, err := next.NewRankerReusingContext(context.Background(), old)
		if err != nil {
			t.Error(err)
			break
		}
		assertRankerEquivalent(t, got, next.NewRanker())
	}
	wg.Wait()
	assertRankerEquivalent(t, old, oldCopy)
}

func TestRankerReuseDropsDeletedDocumentsAndAcceptsMissingBaseline(t *testing.T) {
	g := reuseTestGraph()
	old := g.NewRanker()
	empty, err := New().NewRankerReusingContext(context.Background(), old)
	if err != nil {
		t.Fatal(err)
	}
	if len(empty.docs) != 0 || len(empty.node) != 0 || len(empty.df) != 0 || empty.n != 0 || empty.avgLen != 1 {
		t.Fatal("empty graph retained deleted documents or corpus statistics")
	}
	got, err := g.NewRankerReusingContext(context.Background(), &Ranker{})
	if err != nil {
		t.Fatal(err)
	}
	assertRankerEquivalent(t, got, old)
}

// Both arm orders use identical in-memory corpora; this isolates ranker build
// cost, not graph loading, owner wait, vectors, model tokens or resident memory.
func BenchmarkRankerDocumentReuse(b *testing.B) {
	g := rankerReuseBenchmarkGraph()
	previous := g.NewRanker()
	g.SetNodeAttr("note:n0", "do", "changed searchable document")
	for _, reuseFirst := range []bool{false, true} {
		for _, reuse := range []bool{reuseFirst, !reuseFirst} {
			b.Run(fmt.Sprintf("reuse_first=%t/reuse=%t", reuseFirst, reuse), func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					var baseline *Ranker
					if reuse {
						baseline = previous
					}
					r, err := g.NewRankerReusingContext(context.Background(), baseline)
					if err != nil || int(r.n) != 4200 {
						b.Fatalf("incomplete ranker: %v", err)
					}
				}
			})
		}
	}
}

func rankerReuseBenchmarkGraph() *Graph {
	const size = 4200
	g := NewWithCapacity(size, 0)
	for i := 0; i < size; i++ {
		g.AddNode(note(fmt.Sprintf("n%d", i), fmt.Sprintf("Mesh release safety %d", i), map[string]any{
			"do":  strings.Repeat("verify exact versions and preserve current scope knowledge ", 12),
			"why": "Shared retrieval saves model tokens through reuse", "scope": "dev",
		}))
	}
	return g
}

// The source-equivalent pre-reuse algorithm is a fixture-only control for the
// extra cold-build/retained-input cost; it is never a production fallback.
type legacyRankerBuild struct {
	node      map[string]*Node
	tf        map[string]map[string]int
	docLen    map[string]int
	df        map[string]int
	n, avgLen float64
}

func buildLegacyRanker(ctx context.Context, g *Graph) (*legacyRankerBuild, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	r := &legacyRankerBuild{node: map[string]*Node{}, tf: map[string]map[string]int{}, docLen: map[string]int{}, df: map[string]int{}}
	total := 0
	for id, n := range g.nodes {
		if err := contextCause(ctx); err != nil {
			return nil, err
		}
		if n.Kind != "note" {
			continue
		}
		tokens := nodeText(n)
		tf := termFreq(tokens)
		r.node[id], r.tf[id], r.docLen[id] = n, tf, len(tokens)
		total += len(tokens)
		for term := range tf {
			if err := contextCause(ctx); err != nil {
				return nil, err
			}
			r.df[term]++
		}
		r.n++
	}
	if r.n > 0 {
		r.avgLen = float64(total) / r.n
	}
	if r.avgLen == 0 {
		r.avgLen = 1
	}
	return r, contextCause(ctx)
}

func TestRankerReuseColdStatisticsMatchLegacy(t *testing.T) {
	g := reuseTestGraph()
	legacy, err := buildLegacyRanker(context.Background(), g)
	if err != nil {
		t.Fatal(err)
	}
	got := g.NewRanker()
	if got.n != legacy.n || got.avgLen != legacy.avgLen || !reflect.DeepEqual(got.df, legacy.df) || !reflect.DeepEqual(got.node, legacy.node) {
		t.Fatal("cold corpus statistics changed")
	}
	for id, doc := range got.docs {
		if doc.length != legacy.docLen[id] || !reflect.DeepEqual(doc.tf, legacy.tf[id]) {
			t.Fatalf("cold terms changed for %q", id)
		}
	}
}

func BenchmarkRankerColdBuild(b *testing.B) {
	g := rankerReuseBenchmarkGraph()
	for _, candidateFirst := range []bool{false, true} {
		for _, candidate := range []bool{candidateFirst, !candidateFirst} {
			b.Run(fmt.Sprintf("candidate_first=%t/candidate=%t", candidateFirst, candidate), func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					if candidate {
						r, err := g.NewRankerContext(context.Background())
						if err != nil || int(r.n) != 4200 {
							b.Fatalf("incomplete candidate: %v", err)
						}
					} else {
						r, err := buildLegacyRanker(context.Background(), g)
						if err != nil || int(r.n) != 4200 {
							b.Fatalf("incomplete control: %v", err)
						}
					}
				}
			})
		}
	}
}
