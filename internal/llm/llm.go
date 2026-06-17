// Package llm is the Mesh BYOAI chat boundary for the sync-curator (S2.1). It is
// stdlib-only (no SDK) and speaks two backends behind one Complete interface:
// Anthropic Messages (the default, best reconciliation quality) and any
// OpenAI-compatible /v1/chat/completions endpoint (e.g. a local Ollama, so a
// sovereign vault never egresses). Keys and vault content are never logged.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// ErrRateLimited (429) is transient: the caller should back off and retry the
// whole pass later, not hammer the rest of the batch.
var ErrRateLimited = errors.New("llm: rate limited")

// ErrTruncated means the model hit its output budget (stop_reason max_tokens /
// finish_reason length), so the completion is partial and must not be trusted as
// a complete merged note.
var ErrTruncated = errors.New("llm: output truncated at max_tokens")

// ErrAuth (401/403) is an operator config problem (bad/expired key), not a poison
// job: the caller should wait for the key to be fixed, NOT burn the attempt cap.
var ErrAuth = errors.New("llm: authentication failed (check the API key)")

// Client turns a (system, user) prompt into completion text.
type Client interface {
	Complete(ctx context.Context, system, user string) (string, error)
	Describe() string // agent + model, no secrets; for status/logging
}

// Func adapts a function to a Client (used by tests + the e2e stub curator).
type Func func(ctx context.Context, system, user string) (string, error)

func (f Func) Complete(ctx context.Context, system, user string) (string, error) {
	return f(ctx, system, user)
}
func (f Func) Describe() string { return "stub" }

const (
	defaultAnthropicBase  = "https://api.anthropic.com"
	defaultAnthropicModel = "claude-sonnet-4-6"
	anthropicVersion      = "2023-06-01"
	defaultMaxTokens      = 8192
)

// NewFromEnv builds the curator's LLM client from MESH_CURATOR_* env:
//
//	MESH_CURATOR_AGENT   anthropic (default) | local
//	MESH_CURATOR_MODEL   model id (anthropic default claude-sonnet-4-6; required for local)
//	MESH_CURATOR_MAXTOK  max output tokens (default 4096)
//	anthropic: MESH_ANTHROPIC_KEY (fallback ANTHROPIC_API_KEY), MESH_ANTHROPIC_BASE
//	local:     MESH_CURATOR_ENDPOINT (e.g. http://localhost:11434/v1), MESH_CURATOR_KEY
func NewFromEnv() (Client, error) {
	agent := strings.ToLower(strings.TrimSpace(os.Getenv("MESH_CURATOR_AGENT")))
	if agent == "" {
		agent = "anthropic"
	}
	maxTok := defaultMaxTokens
	if v, err := strconv.Atoi(os.Getenv("MESH_CURATOR_MAXTOK")); err == nil && v > 0 {
		maxTok = v
	}
	model := strings.TrimSpace(os.Getenv("MESH_CURATOR_MODEL"))
	hc := &http.Client{Timeout: 120 * time.Second}

	switch agent {
	case "anthropic":
		key := firstNonEmpty(os.Getenv("MESH_ANTHROPIC_KEY"), os.Getenv("ANTHROPIC_API_KEY"))
		if key == "" {
			return nil, fmt.Errorf("anthropic agent needs MESH_ANTHROPIC_KEY (or ANTHROPIC_API_KEY)")
		}
		if model == "" {
			model = defaultAnthropicModel
		}
		base := firstNonEmpty(os.Getenv("MESH_ANTHROPIC_BASE"), defaultAnthropicBase)
		return &anthropic{base: strings.TrimRight(base, "/"), key: key, model: model, maxTok: maxTok, hc: hc}, nil
	case "local":
		ep := strings.TrimRight(strings.TrimSpace(os.Getenv("MESH_CURATOR_ENDPOINT")), "/")
		if ep == "" {
			return nil, fmt.Errorf("local agent needs MESH_CURATOR_ENDPOINT (e.g. http://localhost:11434/v1)")
		}
		if model == "" {
			return nil, fmt.Errorf("local agent needs MESH_CURATOR_MODEL")
		}
		return &openaiCompat{endpoint: ep, key: os.Getenv("MESH_CURATOR_KEY"), model: model, maxTok: maxTok, hc: hc}, nil
	default:
		return nil, fmt.Errorf("unknown MESH_CURATOR_AGENT %q (want anthropic|local)", agent)
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// ---- Anthropic Messages backend ----

type anthropic struct {
	base, key, model string
	maxTok           int
	hc               *http.Client
	baseOverride     string // tests
}

func (a *anthropic) Describe() string { return "anthropic/" + a.model }

func (a *anthropic) Complete(ctx context.Context, system, user string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"model":      a.model,
		"max_tokens": a.maxTok,
		"system":     system,
		"messages":   []map[string]any{{"role": "user", "content": user}},
	})
	base := a.base
	if a.baseOverride != "" {
		base = a.baseOverride
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", a.key)
	req.Header.Set("anthropic-version", anthropicVersion)
	resp, err := a.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("anthropic: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", statusError("anthropic", resp.StatusCode)
	}
	var out struct {
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("anthropic decode: %w", err)
	}
	if out.StopReason == "max_tokens" {
		return "", fmt.Errorf("anthropic: %w", ErrTruncated)
	}
	var sb strings.Builder
	for _, c := range out.Content {
		if c.Type == "text" {
			sb.WriteString(c.Text)
		}
	}
	if sb.Len() == 0 {
		return "", fmt.Errorf("anthropic: empty completion")
	}
	return sb.String(), nil
}

// ---- OpenAI-compatible (local/sovereign) backend ----

type openaiCompat struct {
	endpoint, key, model string
	maxTok               int
	hc                   *http.Client
}

func (o *openaiCompat) Describe() string { return "local/" + o.model }

func (o *openaiCompat) Complete(ctx context.Context, system, user string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"model":      o.model,
		"max_tokens": o.maxTok,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.endpoint+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if o.key != "" {
		req.Header.Set("Authorization", "Bearer "+o.key)
	}
	resp, err := o.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("local llm: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", statusError("local llm", resp.StatusCode)
	}
	var out struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("local llm decode: %w", err)
	}
	if len(out.Choices) == 0 || strings.TrimSpace(out.Choices[0].Message.Content) == "" {
		return "", fmt.Errorf("local llm: empty completion")
	}
	if out.Choices[0].FinishReason == "length" {
		return "", fmt.Errorf("local llm: %w", ErrTruncated)
	}
	return out.Choices[0].Message.Content, nil
}

func statusError(who string, code int) error {
	switch code {
	case http.StatusTooManyRequests:
		return fmt.Errorf("%s: %w", who, ErrRateLimited)
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%s: %w", who, ErrAuth)
	}
	return fmt.Errorf("%s: unexpected status %d", who, code)
}
