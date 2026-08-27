// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package graph

import (
	"context"
	"errors"
	"testing"
)

func TestNewRankerContextRejectsCanceledBuild(t *testing.T) {
	g := New()
	g.AddNode(note("a", "context cancellation", map[string]any{"why": "do not publish partial statistics"}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r, err := g.NewRankerContext(ctx)
	if !errors.Is(err, context.Canceled) || r != nil {
		t.Fatalf("NewRankerContext = (%v, %v), want (nil, context.Canceled)", r, err)
	}
	if legacy := g.NewRanker(); legacy == nil || len(legacy.Score("cancellation", 1)) != 1 {
		t.Fatal("legacy NewRanker wrapper no longer builds a usable ranker")
	}
}
