// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package retrieve

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bright-interaction/mesh/internal/graph"
)

type blockingDimEmbedder struct{ started chan struct{} }

func (e blockingDimEmbedder) Embed(context.Context, []string) ([][]float32, error) { return nil, nil }
func (e blockingDimEmbedder) Model() string                                        { return "blocking" }
func (e blockingDimEmbedder) Dim() int                                             { return 2 }
func (e blockingDimEmbedder) DimContext(ctx context.Context) (int, error) {
	close(e.started)
	<-ctx.Done()
	return 0, ctx.Err()
}

func TestEnableVectorsContextCancelsWithoutPublishing(t *testing.T) {
	r := New(nil, graph.New())
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := r.EnableVectorsContext(ctx, blockingDimEmbedder{started}, "blocking", 2,
			map[string][][]float32{"note:a": {{1, 0}}})
		done <- err
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("EnableVectorsContext error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("EnableVectorsContext did not return after cancellation")
	}
	if r.VectorsActive() || r.emb != nil || r.vecs != nil || r.ann != nil {
		t.Fatal("canceled vector activation published partial receiver state")
	}
}

func TestNewContextRejectsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r, err := NewContext(ctx, nil, graph.New())
	if !errors.Is(err, context.Canceled) || r != nil {
		t.Fatalf("NewContext = (%v, %v), want (nil, context.Canceled)", r, err)
	}
}
