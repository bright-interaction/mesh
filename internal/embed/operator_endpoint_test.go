// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package embed

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// The embedding half of BLOCKER 5. README.md tells the reader to export
// MESH_EMBED_ENDPOINT=http://localhost:11434/v1 and run `mesh embed`; that died with a
// bare SSRF refusal and exit 1, because every BYOAI client was guarded regardless of who
// supplied the URL. NewOperatorHTTP is the endpoint the operator handed us on the
// command line or in the environment; NewHTTP is the one a member could have written
// through PUT /api/config, and it stays guarded.

// localEmbeddingsServer serves the OpenAI-compatible /embeddings shape on 127.0.0.1 and
// counts the requests it actually received.
func localEmbeddingsServer(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		var req struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		type row struct {
			Embedding []float32 `json:"embedding"`
		}
		out := struct {
			Data []row `json:"data"`
		}{}
		for range req.Input {
			out.Data = append(out.Data, row{Embedding: []float32{0.1, 0.2, 0.3}})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestOperatorEndpointReachesLocalEmbeddingServer(t *testing.T) {
	t.Setenv("MESH_ALLOW_PRIVATE_LLM_ENDPOINT", "")
	srv, hits := localEmbeddingsServer(t)

	h := NewOperatorHTTP(srv.URL, "nomic-embed-text", "")
	vecs, err := h.Embed(context.Background(), []string{"storage engine"})
	if err != nil {
		t.Fatalf("an endpoint from a flag or the process env must be dialed: %v", err)
	}
	if len(vecs) != 1 || len(vecs[0]) != 3 {
		t.Fatalf("got %d vectors, want 1 of width 3", len(vecs))
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("the local embedding server received %d requests, want 1", n)
	}
}

func TestConfigSuppliedEndpointStaysGuardedAndNamesTheOptIn(t *testing.T) {
	t.Setenv("MESH_ALLOW_PRIVATE_LLM_ENDPOINT", "")
	srv, hits := localEmbeddingsServer(t)

	h := NewHTTP(srv.URL, "nomic-embed-text", "")
	_, err := h.Embed(context.Background(), []string{"storage engine"})
	if err == nil {
		t.Fatal("a config-writable endpoint pointing at loopback must be refused")
	}
	if !strings.Contains(err.Error(), "MESH_ALLOW_PRIVATE_LLM_ENDPOINT") {
		t.Errorf("the refusal must name its remedy, got: %v", err)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("the guard let %d requests through to loopback", n)
	}
}
