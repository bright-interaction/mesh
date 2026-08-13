// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package retrieve

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bright-interaction/mesh/internal/graph"
	"github.com/bright-interaction/mesh/internal/index"
)

// These tests are the regression for BLOCKER 5: every documented local BYOAI endpoint
// was blocked by the SSRF guard, and the rerank half reported success while doing
// nothing. A stranger exported MESH_RERANK_ENDPOINT=http://127.0.0.1:8787/rerank exactly
// as README.md and tools/rerank-server/README.md say to, ran mesh search, got byte-
// identical unreranked results with exit 0, and not one request ever reached the server
// that was running right there on their machine.
//
// They assert the two halves that have to hold TOGETHER:
//   - an endpoint from the PROCESS ENVIRONMENT (operator authority, unwritable from any
//     HTTP surface) is dialed for real, loopback included, and the local server receives
//     the request;
//   - the same loopback URL arriving through .mesh/config.toml (member-writable via
//     PUT /api/config) is still refused by the guard, the request count stays at zero,
//     and the refusal names the operator opt-in instead of dead-ending the user.
//
// The request COUNTER is the load-bearing assertion. A rerank that "succeeded" without
// sending anything is precisely the bug, so a test that only checks the error is nil
// would have passed over it.

// localRerankServer starts a rerank endpoint on 127.0.0.1 speaking the Cohere/Jina wire
// format, and returns it with a counter of the requests it actually served.
func localRerankServer(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		var req struct {
			Documents []string `json:"documents"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		type res struct {
			Index int     `json:"index"`
			Score float64 `json:"relevance_score"`
		}
		out := struct {
			Results []res `json:"results"`
		}{}
		// Descending scores in reverse document order, so a reranked head is
		// distinguishable from the fused order it replaced.
		for i := range req.Documents {
			out.Results = append(out.Results, res{Index: i, Score: float64(len(req.Documents) - i)})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// buildVaultStore indexes the shared three-note fixture and hands back the store plus
// its graph, so a test can go through NewFromEnv (which is where the BYOAI endpoints are
// resolved and the client is chosen) rather than wiring a Retriever by hand.
func buildVaultStore(t *testing.T) (*index.Store, *graph.Graph) {
	t.Helper()
	dir := t.TempDir()
	mk := func(path, body string) *index.ParsedNote {
		pn, err := index.Parse(path, []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		return pn
	}
	notes := []*index.ParsedNote{
		mk("a.md", "---\nid: a\ntype: decision\nwhen: 2026-01-01\ndo: x\ndont: y\nwhy: use modernc sqlite for storage\nrelated: [b]\n---\n# Storage engine\n"),
		mk("b.md", "---\nid: b\ntype: gotcha\nwhen: 2026-01-01\ndo: x\ndont: y\nwhy: modernc cannot load sqlite vec extensions\n---\n# Modernc extensions\n"),
		mk("c.md", "---\nid: c\ntype: note\nwhen: 2026-01-01\n---\n# Unrelated\nsomething about marketing copy\n"),
	}
	g, _ := index.BuildGraph(notes)
	g.DetectCommunities(0)
	s, err := index.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := s.IndexVault(notes, g); err != nil {
		t.Fatal(err)
	}
	lg, err := s.LoadGraph()
	if err != nil {
		t.Fatal(err)
	}
	return s, lg
}

// clearBYOAIEnv unsets everything that could leak in from the developer's own shell and
// make these tests lie in either direction.
func clearBYOAIEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"MESH_ALLOW_PRIVATE_LLM_ENDPOINT",
		"MESH_RERANK_ENDPOINT", "MESH_RERANK_MODEL", "MESH_RERANK_BLEND",
		"MESH_EMBED_ENDPOINT", "MESH_EMBED_MODEL",
	} {
		t.Setenv(k, "")
	}
}

// The documented local setup must work with no extra opt-in: the endpoint came from the
// process environment, which is operator authority.
func TestRerankEndpointFromEnvReachesLocalServer(t *testing.T) {
	clearBYOAIEnv(t)
	srv, hits := localRerankServer(t)
	t.Setenv("MESH_RERANK_ENDPOINT", srv.URL+"/rerank")
	t.Setenv("MESH_RERANK_MODEL", "local-test-cross-encoder")

	store, g := buildVaultStore(t)
	r := NewFromEnv(store, g)
	if !r.RerankActive() {
		t.Fatal("rerank should be configured from MESH_RERANK_ENDPOINT + MESH_RERANK_MODEL")
	}
	if err := r.RerankProbe(context.Background()); err != nil {
		t.Fatalf("RerankProbe could not reach the local server: %v", err)
	}
	if _, err := r.Retrieve(context.Background(), "sqlite storage", Options{Limit: 10}); err != nil {
		t.Fatalf("Retrieve against a live local reranker: %v", err)
	}
	if n := hits.Load(); n < 2 {
		t.Fatalf("the local rerank server received %d requests, want at least 2 (one probe, one query); "+
			"an env-configured loopback endpoint is operator input and must be dialed, not refused by the SSRF guard", n)
	}
}

// The security half: the same loopback URL, written into .mesh/config.toml (which
// PUT /api/config rewrites), stays guarded, sends nothing, and says how to allow it.
func TestRerankEndpointFromConfigFileStaysGuarded(t *testing.T) {
	clearBYOAIEnv(t)
	srv, hits := localRerankServer(t)
	store, g := buildVaultStore(t)
	cfg := "[rerank]\nendpoint = \"" + srv.URL + "/rerank\"\nmodel = \"local-test-cross-encoder\"\n"
	if err := os.WriteFile(filepath.Join(store.MeshDir(), "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	r := NewFromEnv(store, g)
	if !r.RerankActive() {
		t.Fatal("rerank should be configured from config.toml")
	}
	_, err := r.Retrieve(context.Background(), "sqlite storage", Options{Limit: 10})
	if err == nil {
		t.Fatal("a config-file loopback reranker must be refused, not silently skipped")
	}
	if !errors.Is(err, ErrRerankUnavailable) {
		t.Fatalf("error should wrap ErrRerankUnavailable, got %v", err)
	}
	if !strings.Contains(err.Error(), "MESH_ALLOW_PRIVATE_LLM_ENDPOINT") {
		t.Errorf("the SSRF refusal must name its remedy, got %v", err)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("the guard let %d requests through to a loopback endpoint written via the config file", n)
	}
}

// A reranker the operator asked for and that cannot be reached fails the query. It used
// to return the fused order with a nil error, so `mesh search` looked reranked, exit 0,
// and nothing anywhere said the endpoint had never answered.
func TestRerankSurfacesEndpointFailure(t *testing.T) {
	r := buildVault(t)
	r.EnableRerank(errReranker{})
	_, err := r.Retrieve(context.Background(), "sqlite storage", Options{Limit: 10})
	if err == nil {
		t.Fatal("a configured reranker that fails must fail the query, not degrade silently")
	}
	if !errors.Is(err, ErrRerankUnavailable) {
		t.Fatalf("error should wrap ErrRerankUnavailable, got %v", err)
	}
	for _, want := range []string{"MESH_RERANK_ENDPOINT", "tools/rerank-server"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must name the remedy %q, got %v", want, err)
		}
	}
}

// RerankProbe is what `mesh status` asks instead of printing "active" off configuration
// alone. A configured-but-dead endpoint must come back as an error.
func TestRerankProbeReportsUnreachableEndpoint(t *testing.T) {
	r := buildVault(t)
	if err := r.RerankProbe(context.Background()); err != nil {
		t.Fatalf("no reranker configured means nothing to probe, got %v", err)
	}
	r.EnableRerank(errReranker{})
	err := r.RerankProbe(context.Background())
	if err == nil {
		t.Fatal("RerankProbe must report a dead endpoint; mesh status printed \"active\" for one")
	}
	if !errors.Is(err, ErrRerankUnavailable) {
		t.Fatalf("probe error should wrap ErrRerankUnavailable, got %v", err)
	}
}
