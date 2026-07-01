// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Bright Interaction AB

// Package embed is the BYOAI embedding layer. Mesh never runs inference itself;
// it calls a configured endpoint (Ollama, OpenAI, Voyage, any /v1/embeddings
// server) so vectors stay on the user's sovereign infrastructure. Embeddings
// are a mechanical transform, not the reasoning AI - the agent is still the
// librarian.
package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/bright-interaction/mesh/internal/safehttp"
)

// maxEmbedResponseBytes bounds an embedding endpoint's response. A batch of vectors
// is large (N texts x dim floats as JSON), so this is generous, but it still stops an
// unbounded/hostile body from OOMing the process (the client timeout bounds time only).
const maxEmbedResponseBytes = 256 << 20

// Embedder turns text into vectors. Implementations must be deterministic for a
// given model so a vault's vectors stay comparable. Dim() is the vector width;
// it pins the space so a same-named-but-different-width model is caught, not
// silently cosined across incompatible dimensions.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	Model() string
	Dim() int
}

// HTTP is an OpenAI-compatible embeddings client. It speaks the
// POST {BaseURL}/embeddings {model, input:[...]} -> {data:[{embedding:[...]}]}
// shape that Ollama (via /v1), OpenAI, Voyage, LiteLLM, and most local servers
// expose. BaseURL should include the version prefix, e.g.
// http://localhost:11434/v1 or https://api.openai.com/v1.
type HTTP struct {
	BaseURL string
	ModelID string
	Key     string // bearer token; empty for keyless local endpoints
	Client  *http.Client

	mu  sync.Mutex
	dim int // cached vector width; 0 = unknown until first Embed or a probe
}

func NewHTTP(baseURL, model, key string) *HTTP {
	return &HTTP{
		BaseURL: strings.TrimRight(baseURL, "/"),
		ModelID: model,
		Key:     key,
		Client:  safehttp.LLMClient(60 * time.Second),
	}
}

func (h *HTTP) Model() string { return h.ModelID }

// Dim returns the embedding width. It is cached from the first Embed; if Dim is
// asked before any Embed it probes once with a sentinel string (no /embeddings
// endpoint exposes dim metadata, so a probe is the only honest option). Returns
// 0 if the endpoint is unreachable.
func (h *HTTP) Dim() int {
	h.mu.Lock()
	d := h.dim
	h.mu.Unlock()
	if d > 0 {
		return d
	}
	vecs, err := h.Embed(context.Background(), []string{"dim probe"})
	if err != nil || len(vecs) == 0 {
		return 0
	}
	return len(vecs[0]) // Embed recorded h.dim
}

func (h *HTTP) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	body, err := json.Marshal(map[string]any{"model": h.ModelID, "input": texts})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.BaseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if h.Key != "" {
		req.Header.Set("Authorization", "Bearer "+h.Key)
	}
	resp, err := h.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embed endpoint %s: status %d", h.BaseURL, resp.StatusCode)
	}
	var out struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		} `json:"data"`
	}
	// Cap the response read: the endpoint is operator/config-controlled (and editable
	// at runtime via the web config API), so an unbounded body could OOM the process.
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxEmbedResponseBytes)).Decode(&out); err != nil {
		return nil, err
	}
	if len(out.Data) != len(texts) {
		return nil, fmt.Errorf("embed: got %d vectors for %d inputs", len(out.Data), len(texts))
	}
	// Place each embedding by its declared index. Reject an out-of-range or duplicate
	// index rather than guessing positionally: a hybrid (index-then-position) recovery
	// could silently mis-pair a vector to the wrong chunk, and the content-hash cache
	// would then lock that wrong vector in under a correct hash.
	vecs := make([][]float32, len(texts))
	for _, d := range out.Data {
		if d.Index < 0 || d.Index >= len(vecs) {
			return nil, fmt.Errorf("embed: response index %d out of range [0,%d)", d.Index, len(vecs))
		}
		if vecs[d.Index] != nil {
			return nil, fmt.Errorf("embed: duplicate response index %d", d.Index)
		}
		vecs[d.Index] = d.Embedding
	}
	for i := range vecs {
		if vecs[i] == nil {
			return nil, fmt.Errorf("embed: no vector returned for input %d", i)
		}
	}
	if len(vecs) > 0 && len(vecs[0]) > 0 {
		h.mu.Lock()
		if h.dim == 0 {
			h.dim = len(vecs[0])
		}
		h.mu.Unlock()
	}
	return vecs, nil
}

// Stub is a deterministic, network-free embedder for tests and offline demos.
// It hashes tokens into a fixed-dimension bag-of-words vector (L2-normalized).
// It is NOT semantic: it exists to exercise vector storage, cosine, and fusion
// plumbing, never to prove retrieval quality.
type Stub struct{ D int }

func (s Stub) Model() string { return "stub-bow" }

func (s Stub) Dim() int {
	if s.D > 0 {
		return s.D
	}
	return 64
}

func (s Stub) Embed(_ context.Context, texts []string) ([][]float32, error) {
	dim := s.D
	if dim <= 0 {
		dim = 64
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, dim)
		for _, tok := range strings.Fields(strings.ToLower(t)) {
			v[fnv(tok)%uint32(dim)]++
		}
		var n float64
		for _, x := range v {
			n += float64(x) * float64(x)
		}
		if n > 0 {
			inv := float32(1 / math.Sqrt(n))
			for j := range v {
				v[j] *= inv
			}
		}
		out[i] = v
	}
	return out, nil
}

func fnv(s string) uint32 {
	h := uint32(2166136261)
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}

// Cosine returns the cosine similarity of two equal-length vectors. Returns 0
// when either is zero or lengths differ.
func Cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
