// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package retrieve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bright-interaction/mesh/internal/index"
)

func TestRetrieverSetupDoesNotInferAndQueriesGuardWidth(t *testing.T) {
	for _, width := range []int{2, 3} {
		t.Run(string(rune('0'+width)), func(t *testing.T) {
			clearBYOAIEnv(t)
			var hits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				vec := make([]float32, width)
				vec[0] = 1
				_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"embedding": vec, "index": 0}}})
			}))
			defer srv.Close()
			t.Setenv("MESH_EMBED_ENDPOINT", srv.URL)
			t.Setenv("MESH_EMBED_MODEL", "lazy-test")
			store, g := buildVaultStore(t)
			hash, err := store.NoteRetrievalHash("note:a")
			if err != nil {
				t.Fatal(err)
			}
			if err := store.ReplaceVectors("lazy-test", []index.VectorRow{{NodeID: "note:a", Vec: []float32{1, 0}, NoteHash: hash}}); err != nil {
				t.Fatal(err)
			}
			var r *Retriever
			for i := 0; i < 4; i++ {
				r = NewFromEnv(store, g)
				if !r.VectorsActive() {
					t.Fatal("configured vectors disappeared")
				}
			}
			if hits.Load() != 0 {
				t.Fatalf("graph setup spent model calls: %d", hits.Load())
			}
			_, err = r.queryVec(context.Background(), "actual query")
			if width == 2 && err != nil {
				t.Fatal(err)
			}
			if width == 3 && (err == nil || !strings.Contains(err.Error(), "does not match stored width")) {
				t.Fatalf("mismatched query accepted: %v", err)
			}
			if hits.Load() != 1 {
				t.Fatalf("query made %d calls, want 1", hits.Load())
			}
			err = r.EmbedderProbe()
			if width == 2 && err != nil {
				t.Fatal(err)
			}
			if width == 3 && (err == nil || !strings.Contains(err.Error(), "dimension mismatch")) {
				t.Fatalf("health probe falsely healthy: %v", err)
			}
		})
	}
}
