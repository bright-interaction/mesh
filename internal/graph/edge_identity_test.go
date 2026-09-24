// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package graph

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestEdgeIdentityIsAnExactTuple(t *testing.T) {
	for name, makeGraph := range map[string]func() *Graph{
		"empty":    New,
		"sized":    func() *Graph { return NewSized(6) },
		"capacity": func() *Graph { return NewWithCapacity(6, 5) },
	} {
		t.Run(name, func(t *testing.T) {
			g := makeGraph()
			// Both the source/target and target/relation boundaries used to be
			// ambiguous when tuple fields were joined with an unescaped NUL.
			edges := []Edge{
				{Source: "a\x00b", Target: "c", Relation: "d", Weight: 1, Confidence: ConfExtracted},
				{Source: "a", Target: "b\x00c", Relation: "d", Weight: 2, Confidence: ConfInferred},
				{Source: "a", Target: "b", Relation: "c\x00d", Weight: 3, Confidence: ConfAmbiguous},
				{Source: "", Target: "", Relation: "", Weight: 4},
				{Source: "a", Target: "a", Relation: "self", Weight: 5},
				{Source: "c", Target: "a\x00b", Relation: "d", Weight: 6},
				{Source: "a", Target: "b", Relation: "different", Weight: 7},
			}
			out, in := map[string][]Edge{}, map[string][]Edge{}
			for _, e := range edges {
				for _, id := range []string{e.Source, e.Target} {
					g.AddNode(&Node{ID: id})
				}
				g.AddEdge(e)
				out[e.Source] = append(out[e.Source], e)
				in[e.Target] = append(in[e.Target], e)
				duplicate := e
				duplicate.Weight, duplicate.SourceLoc = 99, "later payload must not overwrite first"
				g.AddEdge(duplicate)
			}
			if g.EdgeCount() != len(edges) {
				t.Fatalf("distinct tuples were collapsed: %d edges, want %d", g.EdgeCount(), len(edges))
			}
			g.RecomputeDegrees()
			for _, n := range g.Nodes() {
				if !reflect.DeepEqual(g.Neighbors(n.ID), out[n.ID]) || !reflect.DeepEqual(g.RefsTo(n.ID), in[n.ID]) {
					t.Fatalf("adjacency or first-edge payload changed for %q", n.ID)
				}
				if want := len(out[n.ID]) + len(in[n.ID]); n.Degree != want {
					t.Fatalf("degree for %q = %d, want %d", n.ID, n.Degree, want)
				}
			}
		})
	}
}

// Benchmark real construction, including adjacency, exact edge membership and
// degree calculation. Synthetic short/long identifiers expose representation
// tradeoffs; this is not an end-to-end acknowledgement or resident-memory SLO.
func BenchmarkGraphEdgeIdentity(b *testing.B) {
	const size = 12000
	for _, padding := range []int{0, 80, 160} {
		b.Run(fmt.Sprintf("id_padding=%d", padding), func(b *testing.B) {
			ids := make([]string, size)
			for i := range ids {
				ids[i] = fmt.Sprintf("note:%05d-%s", i, strings.Repeat("x", padding))
			}
			b.ReportAllocs()
			b.ResetTimer()
			for round := 0; round < b.N; round++ {
				g := NewWithCapacity(size, size*2)
				for _, id := range ids {
					g.AddNode(&Node{ID: id, Kind: "note"})
				}
				for i, id := range ids {
					for j := 1; j <= 2; j++ {
						g.AddEdge(Edge{Source: id, Target: ids[(i+j)%size], Relation: RelReferences})
					}
				}
				g.RecomputeDegrees()
				if g.EdgeCount() != size*2 || g.NodeCount() != size {
					b.Fatal("incomplete fixture graph")
				}
			}
		})
	}
}

// An in-process map-only control isolates key representation from SQL, graph
// adjacency and degree costs. Both orders are measured to expose order effects.
// The legacy arm is valid only for these NUL-free fixture IDs, never production.
func BenchmarkGraphEdgeKeyMap(b *testing.B) {
	const size = 12000
	for _, padding := range []int{0, 80, 160} {
		ids := make([]string, size)
		for i := range ids {
			ids[i] = fmt.Sprintf("note:%05d-%s", i, strings.Repeat("x", padding))
		}
		for _, tupleFirst := range []bool{false, true} {
			for _, tuple := range []bool{tupleFirst, !tupleFirst} {
				b.Run(fmt.Sprintf("id_padding=%d/tuple_first=%t/tuple=%t", padding, tupleFirst, tuple), func(b *testing.B) {
					b.ReportAllocs()
					for round := 0; round < b.N; round++ {
						if tuple {
							keys := make(map[edgeKey]struct{}, size*2)
							for i, id := range ids {
								for j := 1; j <= 2; j++ {
									key := edgeKey{id, ids[(i+j)%size], RelReferences}
									if _, exists := keys[key]; !exists {
										keys[key] = struct{}{}
									}
								}
							}
							if len(keys) != size*2 {
								b.Fatal("tuple membership incomplete")
							}
						} else {
							keys := make(map[string]bool, size*2)
							for i, id := range ids {
								for j := 1; j <= 2; j++ {
									key := id + "\x00" + ids[(i+j)%size] + "\x00" + RelReferences
									if !keys[key] {
										keys[key] = true
									}
								}
							}
							if len(keys) != size*2 {
								b.Fatal("legacy membership incomplete")
							}
						}
					}
				})
			}
		}
	}
}
