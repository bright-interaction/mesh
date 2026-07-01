// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Bright Interaction AB

package embed

import (
	"context"
	"encoding/json"
	"math"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestCosine(t *testing.T) {
	if c := Cosine([]float32{1, 0, 0}, []float32{1, 0, 0}); math.Abs(c-1) > 1e-9 {
		t.Errorf("identical vectors cosine = %v, want 1", c)
	}
	if c := Cosine([]float32{1, 0}, []float32{0, 1}); math.Abs(c) > 1e-9 {
		t.Errorf("orthogonal cosine = %v, want 0", c)
	}
	if c := Cosine([]float32{1, 1}, []float32{2, 2}); math.Abs(c-1) > 1e-9 {
		t.Errorf("parallel cosine = %v, want 1", c)
	}
	if c := Cosine([]float32{1}, []float32{1, 2}); c != 0 {
		t.Errorf("mismatched lengths cosine = %v, want 0", c)
	}
}

func TestStubDeterministicAndNormalized(t *testing.T) {
	s := Stub{D: 32}
	a, _ := s.Embed(context.Background(), []string{"modernc sqlite storage"})
	b, _ := s.Embed(context.Background(), []string{"modernc sqlite storage"})
	if len(a) != 1 || len(b) != 1 {
		t.Fatal("expected one vector each")
	}
	for i := range a[0] {
		if a[0][i] != b[0][i] {
			t.Fatalf("stub not deterministic at %d", i)
		}
	}
	if c := Cosine(a[0], a[0]); math.Abs(c-1) > 1e-6 {
		t.Errorf("self-cosine of a normalized vector should be 1, got %v", c)
	}
	// related text should be closer than unrelated text
	rel, _ := s.Embed(context.Background(), []string{"sqlite storage engine"})
	unrel, _ := s.Embed(context.Background(), []string{"marketing copy newsletter"})
	if Cosine(a[0], rel[0]) <= Cosine(a[0], unrel[0]) {
		t.Errorf("shared-token text should score higher than unrelated")
	}
}

func TestStubDim(t *testing.T) {
	if (Stub{D: 256}).Dim() != 256 {
		t.Fatal("Stub.Dim should reflect D")
	}
	if (Stub{}).Dim() != 64 {
		t.Fatal("Stub.Dim default should be 64")
	}
}

func TestHTTPDimProbeCache(t *testing.T) {
	t.Setenv("MESH_ALLOW_PRIVATE_LLM_ENDPOINT", "1") // the test server is on loopback
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		var req struct {
			Input []string `json:"input"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		var out struct {
			Data []struct {
				Embedding []float32 `json:"embedding"`
				Index     int       `json:"index"`
			} `json:"data"`
		}
		for i := range req.Input {
			out.Data = append(out.Data, struct {
				Embedding []float32 `json:"embedding"`
				Index     int       `json:"index"`
			}{Embedding: make([]float32, 384), Index: i})
		}
		json.NewEncoder(w).Encode(out)
	}))
	defer srv.Close()

	h := NewHTTP(srv.URL, "test-model", "")
	if d := h.Dim(); d != 384 { // first call probes
		t.Fatalf("Dim probe = %d, want 384", d)
	}
	if d := h.Dim(); d != 384 { // cached, no new call
		t.Fatalf("cached Dim = %d, want 384", d)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("Dim should probe exactly once then cache, got %d calls", n)
	}
	// A real Embed after the probe must not re-probe (dim already cached).
	h.Embed(context.Background(), []string{"hi"})
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("one probe + one embed = 2 calls, got %d", n)
	}
}

// TestHTTPRejectsDuplicateIndex: a non-compliant endpoint that returns the right
// COUNT of vectors but duplicate indices must error, not silently mis-pair vectors
// to the wrong input (which the content-hash cache would then lock in).
func TestHTTPRejectsDuplicateIndex(t *testing.T) {
	t.Setenv("MESH_ALLOW_PRIVATE_LLM_ENDPOINT", "1") // the test server is on loopback
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		var out struct {
			Data []struct {
				Embedding []float32 `json:"embedding"`
				Index     int       `json:"index"`
			} `json:"data"`
		}
		// Two items but both claim index 0.
		for range req.Input {
			out.Data = append(out.Data, struct {
				Embedding []float32 `json:"embedding"`
				Index     int       `json:"index"`
			}{Embedding: []float32{1, 0}, Index: 0})
		}
		json.NewEncoder(w).Encode(out)
	}))
	defer srv.Close()

	h := NewHTTP(srv.URL, "m", "")
	if _, err := h.Embed(context.Background(), []string{"a", "b"}); err == nil {
		t.Fatal("Embed must error on a duplicate response index")
	}
}

// randVec makes a deterministic L2-ish random vector for benchmarks.
func randVec(rng *rand.Rand, d int) []float32 {
	v := make([]float32, d)
	for i := range v {
		v[i] = float32(rng.NormFloat64())
	}
	return v
}

func BenchmarkCosine(b *testing.B) {
	for _, dim := range []int{256, 768, 1536} {
		rng := rand.New(rand.NewSource(1))
		a, c := randVec(rng, dim), randVec(rng, dim)
		b.Run("dim"+itoa(dim), func(b *testing.B) {
			b.ReportAllocs()
			var sink float64
			for i := 0; i < b.N; i++ {
				sink += Cosine(a, c)
			}
			_ = sink
		})
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
