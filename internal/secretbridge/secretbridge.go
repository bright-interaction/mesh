// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

// Package secretbridge is a thin HTTP client for the Dockyard capability-mode
// secrets bridge. It lets Mesh become the single hub an agent talks to: knowledge
// in plaintext AND, on request, a short-lived capability TOKEN for a secret the
// agent can never read. The real secret stays encrypted in the Dockyard vault and
// is injected server-side by Dockyard's /proxy at forward time.
//
// This package holds NO crypto, NO vault storage, and NO rotation: all of that
// lives in Dockyard (already audited). Mesh only ever handles secret NAMES and
// capability tokens, never a plaintext value, so a fetched credential never enters
// a tool result, a prompt, Mesh's process env, or a note. See the estate note
// "Dockyard Secrets Bridge (Capability Mode)".
//
// The base URL is NOT a trusted operator-only value: internal/web/config_api.go makes
// secret_bridge.base_url writable over PUT /api/config and the MCP tools re-read it per
// call, so a config-writing caller can aim this authenticated request (it carries the
// Dockyard API key in X-API-Key) at loopback, RFC1918, 169.254.169.254 or the tailnet.
// Every request therefore goes through internal/safehttp's SSRF guard. A self-hosted
// Dockyard on localhost / LAN / Tailscale is a supported deployment, so a private
// destination is still allowed on OPERATOR authority only: either the base URL is the
// one named in the MESH_SECRET_BRIDGE_URL env var (which outranks the config file and
// no HTTP surface can write), or MESH_ALLOW_PRIVATE_SECRET_BRIDGE=1 is set, the same
// shape as MESH_ALLOW_PRIVATE_LLM_ENDPOINT.
// Mesh only ever dials the configured base URL; the agent-supplied destination is a
// string forwarded to Dockyard for the token, never dialed here.
package secretbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/bright-interaction/mesh/internal/safehttp"
)

// Client talks to one Dockyard bridge as one agent identity. apiKey is the Dockyard
// API key (X-API-Key); it is only ever sent as a request header, never logged and
// never returned by any method.
type Client struct {
	baseURL string // e.g. https://dockyard.example.com (no trailing slash)
	apiKey  string
	agentID string
	hc      *http.Client
}

// AllowPrivateBridge reports whether the operator opted into a private bridge
// destination (a self-hosted Dockyard on localhost, the LAN, or the tailnet) via
// MESH_ALLOW_PRIVATE_SECRET_BRIDGE. The config API never writes environment variables,
// so a member who can PUT /api/config cannot flip this guard off.
func AllowPrivateBridge() bool {
	v := strings.TrimSpace(os.Getenv("MESH_ALLOW_PRIVATE_SECRET_BRIDGE"))
	return v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes")
}

// operatorConfiguredBase reports whether baseURL is exactly the one the OPERATOR named
// in MESH_SECRET_BRIDGE_URL. That variable outranks the config file and no HTTP surface
// can write the environment, so a matching value provably did not come from a
// PUT /api/config caller: a self-hosted Dockyard named there stays reachable, while the
// same private URL arriving through the writable config field does not.
func operatorConfiguredBase(baseURL string) bool {
	env := normalizeBase(os.Getenv("MESH_SECRET_BRIDGE_URL"))
	return env != "" && env == normalizeBase(baseURL)
}

func normalizeBase(s string) string { return strings.TrimRight(strings.TrimSpace(s), "/") }

// New builds a client for baseURL authenticating as agentID with apiKey. baseURL is
// normalized (trailing slash trimmed). A 20s timeout bounds a hung Dockyard.
func New(baseURL, apiKey, agentID string) *Client {
	// SSRF-guarded by default (base_url is config-writable, see the package doc): the
	// guard resolves the host and refuses loopback/private/link-local/CGNAT before
	// dialing, so a redirected base URL cannot probe internal services with the team's
	// Dockyard key attached. The operator opt-in restores the self-hosted case.
	hc := safehttp.Client(20 * time.Second)
	if AllowPrivateBridge() || operatorConfiguredBase(baseURL) {
		hc = safehttp.LoopbackAllowed(20 * time.Second)
	}
	// NEVER follow redirects. The only host Mesh should ever dial is the configured
	// base URL. Go copies non-Authorization custom headers (our X-API-Key) across a
	// redirect, so a single 3xx from a compromised, MITM'd (plaintext http on
	// LAN/Tailscale is a supported case), or open-redirecting Dockyard would otherwise
	// re-send the team API key to an attacker or internal host (an SSRF + key-egress
	// primitive). This is stricter than safehttp's own 3-redirect policy on purpose: a
	// real bridge endpoint never 3xx-redirects, so refusing them only closes the abuse
	// path. doJSON then sees the 3xx as a non-2xx status and returns an error.
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		apiKey:  strings.TrimSpace(apiKey),
		agentID: strings.TrimSpace(agentID),
		hc:      hc,
	}
}

// BaseURL returns the configured Dockyard base URL (never the key).
func (c *Client) BaseURL() string { return c.baseURL }

// AgentID returns the agent identity Mesh presents to Dockyard.
func (c *Client) AgentID() string { return c.agentID }

// ProxyBase is the root the agent sends its capability-authorized request to.
func (c *Client) ProxyBase() string { return c.baseURL + "/proxy" }

// ProxyURL builds the exact URL an agent should call for a destination host+path,
// carrying its capability token. Built from the configured base URL (not the issue
// response's proxy_url, which Dockyard derives from the request Host and can be wrong
// behind a misconfigured proxy).
func (c *Client) ProxyURL(host, path string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if path != "" && !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return c.baseURL + "/proxy/" + host + path
}

// SecretMeta is the non-sensitive metadata for one vault entry. It NEVER carries a
// secret value (the Dockyard list endpoint does not return values, and this projects
// only name/provider/category/rotation fields).
type SecretMeta struct {
	Name           string `json:"name"`
	Provider       string `json:"provider,omitempty"`
	Category       string `json:"category,omitempty"`
	LastRotatedAt  string `json:"last_rotated_at,omitempty"`
	AutoRotate     bool   `json:"auto_rotate"`
	RotationStatus string `json:"rotation_status,omitempty"`
}

// vaultEntry mirrors the fields Dockyard's GET /api/vault/ returns per entry. Only
// the non-sensitive subset is decoded; there is no value field in the response.
type vaultEntry struct {
	Name           string `json:"name"`
	Provider       string `json:"provider"`
	Category       string `json:"category"`
	LastRotatedAt  string `json:"last_rotated_at"`
	AutoRotate     bool   `json:"auto_rotate"`
	RotationStatus string `json:"rotation_status"`
}

// ListSecrets returns the names + rotation metadata of the vault entries this agent's
// key can see. It calls GET /api/vault/ (the trailing slash matters: chi's index
// route). No secret value is ever returned.
func (c *Client) ListSecrets(ctx context.Context) ([]SecretMeta, error) {
	var entries []vaultEntry
	if err := c.doJSON(ctx, http.MethodGet, "/api/vault/", nil, &entries); err != nil {
		return nil, err
	}
	out := make([]SecretMeta, 0, len(entries))
	for _, e := range entries {
		out = append(out, SecretMeta{
			Name: e.Name, Provider: e.Provider, Category: e.Category,
			LastRotatedAt: e.LastRotatedAt, AutoRotate: e.AutoRotate, RotationStatus: e.RotationStatus,
		})
	}
	return out, nil
}

// IssueRequest asks Dockyard to mint a capability token. Give SecretName to pin a
// vault entry, or leave it empty to auto-route by Destination. Destination is the
// upstream "host/path" the token will be bound to. Method defaults to "*".
type IssueRequest struct {
	SecretName  string
	Destination string
	Method      string
	TTLSeconds  int
}

// issueBody is the wire shape of POST /api/secrets/issue.
type issueBody struct {
	AgentID     string `json:"agent_id"`
	Secret      string `json:"secret,omitempty"`
	Destination string `json:"destination,omitempty"`
	Method      string `json:"method,omitempty"`
	TTLSeconds  int    `json:"ttl_seconds,omitempty"`
}

// IssueResponse is what Dockyard returns: a capability token (NOT the secret), the
// proxy base, the resolved secret NAME, and the expiry. The token is bearer-
// equivalent, single-use, and destination+method bound for a short TTL.
type IssueResponse struct {
	Token       string `json:"token"`
	ProxyURL    string `json:"proxy_url"`
	Secret      string `json:"secret"` // the entry NAME that was resolved, never a value
	ExpiresAt   string `json:"expires_at"`
	NonceLength int    `json:"nonce_length"`
}

// Issue mints a capability token by POSTing /api/secrets/issue. The returned token is
// safe to hand to the model: it cannot be decrypted client-side and the real secret
// is injected only by Dockyard's proxy at forward time.
func (c *Client) Issue(ctx context.Context, req IssueRequest) (IssueResponse, error) {
	if strings.TrimSpace(req.Destination) == "" && strings.TrimSpace(req.SecretName) == "" {
		return IssueResponse{}, fmt.Errorf("secretbridge: destination or secret_name is required")
	}
	method := strings.TrimSpace(req.Method)
	if method == "" {
		method = "*"
	}
	body := issueBody{
		AgentID:     c.agentID,
		Secret:      strings.TrimSpace(req.SecretName),
		Destination: strings.TrimSpace(req.Destination),
		Method:      method,
		TTLSeconds:  req.TTLSeconds,
	}
	var out IssueResponse
	if err := c.doJSON(ctx, http.MethodPost, "/api/secrets/issue", body, &out); err != nil {
		return IssueResponse{}, err
	}
	return out, nil
}

// SplitHostPath splits an agent-supplied "host/path" destination into host + path.
// A bare host yields an empty path. A leading scheme (https://) is tolerated and
// stripped so the agent can paste a full-ish URL.
func SplitHostPath(dest string) (host, path string) {
	dest = strings.TrimSpace(dest)
	// Strip only a genuine LEADING scheme (https://), not a "://" that appears later
	// inside a path or query value (e.g. api.example.com/cb?next=https://x), which would
	// otherwise mis-parse the host and produce a broken proxy URL. A real scheme has no
	// /, ?, or # before the "://".
	if i := strings.Index(dest, "://"); i > 0 && !strings.ContainsAny(dest[:i], "/?#") {
		dest = dest[i+3:]
	}
	dest = strings.TrimPrefix(dest, "/")
	if i := strings.IndexByte(dest, '/'); i >= 0 {
		return strings.ToLower(dest[:i]), dest[i:]
	}
	return strings.ToLower(dest), ""
}

// doJSON performs one authenticated request. It sets X-API-Key (never Authorization/
// Bearer, matching Dockyard's JWTOrAPIKeyAuth), and on a non-2xx it returns a compact
// error carrying Dockyard's own error code (safe: those bodies never contain the key
// or a secret value). The API key is only ever a header.
func (c *Client) doJSON(ctx context.Context, method, path string, reqBody any, out any) error {
	if c.baseURL == "" {
		return fmt.Errorf("secretbridge: no base URL configured")
	}
	if c.apiKey == "" {
		return fmt.Errorf("secretbridge: no API key configured")
	}
	if u, err := url.Parse(c.baseURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("secretbridge: base URL must be http(s)")
	}
	var rdr io.Reader
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if err != nil {
		return err
	}
	httpReq.Header.Set("X-API-Key", c.apiKey)
	httpReq.Header.Set("Accept", "application/json")
	if reqBody != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(httpReq)
	if err != nil {
		return fmt.Errorf("secretbridge: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("secretbridge: %s %s: %s: %s", method, path, resp.Status, briefError(data))
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("secretbridge: decode %s: %w", path, err)
		}
	}
	return nil
}

// briefError pulls Dockyard's error code/message out of a JSON error body (either
// {"error":"..."} or {"code":"...","message":"..."}), falling back to the raw text
// trimmed. These strings are Dockyard status codes (secret_not_found,
// agent_not_granted, ambiguous_destination, ...), never a credential.
func briefError(data []byte) string {
	var e struct {
		Error   string `json:"error"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(data, &e) == nil {
		switch {
		case e.Error != "":
			return e.Error
		case e.Code != "" && e.Message != "":
			return e.Code + ": " + e.Message
		case e.Code != "":
			return e.Code
		case e.Message != "":
			return e.Message
		}
	}
	s := strings.TrimSpace(string(data))
	if len(s) > 200 {
		s = s[:200]
	}
	if s == "" {
		return "request failed"
	}
	return s
}
