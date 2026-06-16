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
	"math"
	"net/http"
	"strings"
	"time"
)

// Embedder turns text into vectors. Implementations must be deterministic for a
// given model so a vault's vectors stay comparable.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	Model() string
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
}

func NewHTTP(baseURL, model, key string) *HTTP {
	return &HTTP{
		BaseURL: strings.TrimRight(baseURL, "/"),
		ModelID: model,
		Key:     key,
		Client:  &http.Client{Timeout: 60 * time.Second},
	}
}

func (h *HTTP) Model() string { return h.ModelID }

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
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if len(out.Data) != len(texts) {
		return nil, fmt.Errorf("embed: got %d vectors for %d inputs", len(out.Data), len(texts))
	}
	vecs := make([][]float32, len(texts))
	for _, d := range out.Data {
		if d.Index >= 0 && d.Index < len(vecs) {
			vecs[d.Index] = d.Embedding
		}
	}
	for i, v := range vecs {
		if v == nil {
			vecs[i] = out.Data[i].Embedding
		}
	}
	return vecs, nil
}

// Stub is a deterministic, network-free embedder for tests and offline demos.
// It hashes tokens into a fixed-dimension bag-of-words vector (L2-normalized).
// It is NOT semantic: it exists to exercise vector storage, cosine, and fusion
// plumbing, never to prove retrieval quality.
type Stub struct{ D int }

func (s Stub) Model() string { return "stub-bow" }

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
