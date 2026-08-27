// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package embed

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

type blockingRoundTripper func(*http.Request) (*http.Response, error)

func (f blockingRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDimContextCancellationDoesNotPoisonProbe(t *testing.T) {
	started := make(chan struct{})
	h := &HTTP{
		BaseURL: "http://embed.invalid/v1",
		ModelID: "model",
		Client: &http.Client{Transport: blockingRoundTripper(func(r *http.Request) (*http.Response, error) {
			close(started)
			<-r.Context().Done()
			return nil, r.Context().Err()
		})},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := h.DimContext(ctx)
		done <- err
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("DimContext error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("DimContext did not return after cancellation")
	}
	h.mu.Lock()
	failed := h.probeFail
	h.mu.Unlock()
	if failed {
		t.Fatal("request cancellation poisoned the client's one-shot dim probe")
	}
}

func TestStubDimContextHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (Stub{D: 8}).DimContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Stub.DimContext error = %v, want context.Canceled", err)
	}
}
