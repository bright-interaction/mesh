// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package rerank

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStubScoresByOverlap(t *testing.T) {
	res, err := Stub{}.Rerank(context.Background(), "alpha beta", []string{
		"alpha beta gamma", // both query terms -> 1.0
		"delta epsilon",    // none -> 0.0
		"beta only",        // one of two -> 0.5
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 3 {
		t.Fatalf("want 3 results in input order, got %d", len(res))
	}
	if res[0].Score <= res[2].Score || res[2].Score <= res[1].Score {
		t.Errorf("overlap order wrong: %v", res)
	}
	if res[1].Score != 0 {
		t.Errorf("no-overlap doc should score 0, got %v", res[1].Score)
	}
}

func TestHTTPContractAndOrdering(t *testing.T) {
	t.Setenv("MESH_ALLOW_PRIVATE_LLM_ENDPOINT", "1") // the test server is on loopback
	var gotQuery string
	var gotDocs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k3y" {
			t.Errorf("missing/wrong auth header: %q", r.Header.Get("Authorization"))
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model     string   `json:"model"`
			Query     string   `json:"query"`
			Documents []string `json:"documents"`
		}
		_ = json.Unmarshal(body, &req)
		gotQuery, gotDocs = req.Query, req.Documents
		// Respond Cohere/Jina-style: sorted desc by score, index points back into docs.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{
				{"index": 2, "relevance_score": 9.5},
				{"index": 0, "relevance_score": 1.0},
				{"index": 1, "relevance_score": -4.2},
			},
		})
	}))
	defer srv.Close()

	rr := NewHTTP(srv.URL+"/rerank", "test-rerank", "k3y")
	if rr.Model() != "test-rerank" {
		t.Errorf("model id not preserved")
	}
	res, err := rr.Rerank(context.Background(), "the query", []string{"a", "b", "c"})
	if err != nil {
		t.Fatal(err)
	}
	if gotQuery != "the query" || len(gotDocs) != 3 {
		t.Errorf("request payload wrong: q=%q docs=%v", gotQuery, gotDocs)
	}
	// Results must be normalized back to request order so index i maps to docs[i].
	if len(res) != 3 {
		t.Fatalf("want 3 results, got %d", len(res))
	}
	for i, r := range res {
		if r.Index != i {
			t.Errorf("result %d not in request order: index=%d", i, r.Index)
		}
	}
	if res[2].Score != 9.5 || res[0].Score != 1.0 || res[1].Score != -4.2 {
		t.Errorf("scores not mapped to the right docs: %v", res)
	}
}

func TestHTTPRejectsMalformedResultSets(t *testing.T) {
	t.Setenv("MESH_ALLOW_PRIVATE_LLM_ENDPOINT", "1") // the test servers are on loopback
	cases := []struct {
		name    string
		results []map[string]any
		docs    []string
	}{
		{"out-of-range", []map[string]any{{"index": 99, "relevance_score": 1.0}}, []string{"only-one"}},
		{"duplicate-index", []map[string]any{{"index": 0, "relevance_score": 1.0}, {"index": 0, "relevance_score": 2.0}}, []string{"a", "b"}},
		{"partial-set", []map[string]any{{"index": 0, "relevance_score": 1.0}}, []string{"a", "b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"results": tc.results})
			}))
			defer srv.Close()
			if _, err := NewHTTP(srv.URL, "m", "").Rerank(context.Background(), "q", tc.docs); err == nil {
				t.Errorf("%s: expected error, got nil (would silently corrupt caller scoring)", tc.name)
			}
		})
	}
}
