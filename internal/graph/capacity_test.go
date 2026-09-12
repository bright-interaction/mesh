// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package graph

import (
	"fmt"
	"reflect"
	"testing"
)

func TestCapacityHintsPreserveGraphSemanticsAndAllowGrowth(t *testing.T) {
	for _, hint := range [][2]int{{-1, -1}, {0, 0}, {1, 1}, {100, 200}} {
		t.Run(fmt.Sprint(hint), func(t *testing.T) {
			want, got := New(), NewWithCapacity(hint[0], hint[1])
			for _, g := range []*Graph{want, got} {
				for i := 0; i < 10; i++ {
					g.AddNode(&Node{ID: fmt.Sprintf("note:%d", i), Kind: "note"})
				}
				for i := 0; i < 10; i++ {
					e := Edge{Source: fmt.Sprintf("note:%d", i), Target: fmt.Sprintf("note:%d", i+1), Relation: RelReferences, Weight: 1}
					g.AddEdge(e) // final target is deliberately absent
					g.AddEdge(e) // duplicate must still be suppressed
				}
				g.RecomputeDegrees()
			}
			if !reflect.DeepEqual(got.nodes, want.nodes) || !reflect.DeepEqual(got.adj, want.adj) || !reflect.DeepEqual(got.rev, want.rev) || !reflect.DeepEqual(got.edgeSet, want.edgeSet) {
				t.Fatal("capacity changed graph values or duplicate handling")
			}
		})
	}
}
