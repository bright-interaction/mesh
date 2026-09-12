// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package retrieve

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bright-interaction/mesh/internal/graph"
	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/rerank"
)

func TestConfigInputsCaptureLocalPreferencesAndEnvironment(t *testing.T) {
	t.Setenv("MESH_RERANK_AGENT", "")
	t.Setenv("MESH_RERANK_MODEL", "")
	t.Setenv("MESH_RERANK_POLICY", "")
	t.Setenv("MESH_RERANK_CANDIDATES", "7")
	t.Setenv("MESH_WEIGHT_FTS", "0.4")
	s, err := index.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	current := rerank.SubscriptionConfig{Agent: "claude", Model: "captured-model", Policy: "auto"}
	loader := func(string) (rerank.SubscriptionConfig, bool, string, error) { return current, true, "", nil }
	a, err := loadConfigInputs(context.Background(), s.MeshDir(), loader)
	if err != nil {
		t.Fatal(err)
	}
	// Change inputs after capture. The constructor must consume A, including
	// subscription limits, rather than reread B and mislabel it with A's digest.
	current.Model = "new-model"
	t.Setenv("MESH_RERANK_CANDIDATES", "11")
	t.Setenv("MESH_WEIGHT_FTS", "0.8")
	r, err := NewFromInputsContext(context.Background(), s, graph.New(), a)
	if err != nil {
		t.Fatal(err)
	}
	if r.rr.Model() != "subscription/claude/captured-model" {
		t.Fatalf("reread preference: %s", r.rr.Model())
	}
	if r.rr.(*rerank.CLI).CandidateLimit() != 7 {
		t.Fatal("reread subscription environment")
	}
	if fts, _, _ := r.Weights(); fts != 0.4 {
		t.Fatalf("reread weights: %v", fts)
	}
	b, err := loadConfigInputs(context.Background(), s.MeshDir(), loader)
	if err != nil {
		t.Fatal(err)
	}
	af, aok := a.Fingerprint()
	bf, bok := b.Fingerprint()
	if !aok || !bok || af == bf {
		t.Fatal("changed preferences/environment did not invalidate")
	}
	current.Model = "only-local-config-changed"
	c, err := loadConfigInputs(context.Background(), s.MeshDir(), loader)
	if err != nil {
		t.Fatal(err)
	}
	cf, cok := c.Fingerprint()
	if !cok || cf == bf {
		t.Fatal("local preference-only edit did not invalidate")
	}
}

func TestConfigInputsLocalReadErrorDisablesReuseAndHonorsCancellation(t *testing.T) {
	t.Setenv("MESH_RERANK_AGENT", "")
	s, err := index.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	broken := func(string) (rerank.SubscriptionConfig, bool, string, error) {
		return rerank.SubscriptionConfig{}, false, "", errors.New("private invalid config")
	}
	in, err := loadConfigInputs(context.Background(), s.MeshDir(), broken)
	if err != nil {
		t.Fatal(err)
	}
	if _, reusable := in.Fingerprint(); reusable {
		t.Fatal("failed config read was cacheable")
	}
	r, err := NewFromInputsContext(context.Background(), s, graph.New(), in)
	if err != nil || r.rerankSetup == nil {
		t.Fatalf("lost configured fail-loud error: %v", err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	blocked := func(string) (rerank.SubscriptionConfig, bool, string, error) {
		close(started)
		<-release
		return rerank.SubscriptionConfig{}, false, "", nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := loadConfigInputs(ctx, s.MeshDir(), blocked); done <- err }()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("config read ignored cancellation")
	}
}

func TestOptionalVectorReadFailureCannotBeCachedAsHealthy(t *testing.T) {
	t.Setenv("MESH_RERANK_AGENT", "http")
	t.Setenv("MESH_EMBED_ENDPOINT", "http://127.0.0.1:1")
	t.Setenv("MESH_EMBED_MODEL", "unavailable-store")
	s, err := index.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	in, err := LoadConfigInputs(context.Background(), s.MeshDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := NewFromInputsContext(context.Background(), s, graph.New(), in)
	if err != nil {
		t.Fatalf("lost legacy lexical fallback: %v", err)
	}
	if r.RefreshReusable() {
		t.Fatal("optional vector read failure became a cacheable reader")
	}
}
