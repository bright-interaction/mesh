package llm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTruncationDetected(t *testing.T) {
	// Anthropic stop_reason=max_tokens -> ErrTruncated.
	ats := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"stop_reason":"max_tokens","content":[{"type":"text","text":"partial"}]}`))
	}))
	defer ats.Close()
	a := &anthropic{key: "k", model: "m", maxTok: 10, hc: &http.Client{Timeout: 5 * time.Second}, baseOverride: ats.URL}
	if _, err := a.Complete(context.Background(), "s", "u"); !errors.Is(err, ErrTruncated) {
		t.Fatalf("anthropic truncation: want ErrTruncated, got %v", err)
	}
	// OpenAI-compat finish_reason=length -> ErrTruncated.
	ots := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"finish_reason":"length","message":{"content":"partial"}}]}`))
	}))
	defer ots.Close()
	o := &openaiCompat{endpoint: ots.URL, model: "m", maxTok: 10, hc: &http.Client{Timeout: 5 * time.Second}}
	if _, err := o.Complete(context.Background(), "s", "u"); !errors.Is(err, ErrTruncated) {
		t.Fatalf("local truncation: want ErrTruncated, got %v", err)
	}
}

func TestAnthropicComplete(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Header.Get("x-api-key") != "sekret" {
			t.Errorf("missing x-api-key")
		}
		if r.Header.Get("anthropic-version") == "" {
			t.Errorf("missing anthropic-version")
		}
		w.Write([]byte(`{"content":[{"type":"text","text":"merged "},{"type":"text","text":"note"}]}`))
	}))
	defer ts.Close()
	c := &anthropic{base: "https://unused", key: "sekret", model: "claude-sonnet-4-6", maxTok: 1024, hc: &http.Client{Timeout: 5 * time.Second}, baseOverride: ts.URL}
	got, err := c.Complete(context.Background(), "sys", "user")
	if err != nil {
		t.Fatal(err)
	}
	if got != "merged note" {
		t.Fatalf("got %q", got)
	}
	if c.Describe() != "anthropic/claude-sonnet-4-6" {
		t.Fatalf("describe = %q", c.Describe())
	}
}

func TestAnthropicRateLimited(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer ts.Close()
	c := &anthropic{key: "k", model: "m", maxTok: 10, hc: &http.Client{Timeout: 5 * time.Second}, baseOverride: ts.URL}
	_, err := c.Complete(context.Background(), "s", "u")
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("want ErrRateLimited (errors.Is), got %v", err)
	}
}

func TestOpenAICompatComplete(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"local merged"}}]}`))
	}))
	defer ts.Close()
	c := &openaiCompat{endpoint: ts.URL, model: "llama", maxTok: 1024, hc: &http.Client{Timeout: 5 * time.Second}}
	got, err := c.Complete(context.Background(), "sys", "user")
	if err != nil {
		t.Fatal(err)
	}
	if got != "local merged" {
		t.Fatalf("got %q", got)
	}
}

func TestNewFromEnv(t *testing.T) {
	t.Setenv("MESH_CURATOR_AGENT", "anthropic")
	t.Setenv("MESH_ANTHROPIC_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	if _, err := NewFromEnv(); err == nil {
		t.Fatal("anthropic without a key should error")
	}
	t.Setenv("ANTHROPIC_API_KEY", "abc")
	c, err := NewFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(c.Describe(), "anthropic/") {
		t.Fatalf("describe = %q", c.Describe())
	}

	t.Setenv("MESH_CURATOR_AGENT", "local")
	t.Setenv("MESH_CURATOR_ENDPOINT", "")
	if _, err := NewFromEnv(); err == nil {
		t.Fatal("local without an endpoint should error")
	}
	t.Setenv("MESH_CURATOR_ENDPOINT", "http://localhost:11434/v1")
	t.Setenv("MESH_CURATOR_MODEL", "nomic")
	c, err = NewFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(c.Describe(), "local/") {
		t.Fatalf("describe = %q", c.Describe())
	}
}
