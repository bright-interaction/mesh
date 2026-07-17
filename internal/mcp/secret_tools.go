// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"time"

	"github.com/bright-interaction/mesh/internal/meshcfg"
	"github.com/bright-interaction/mesh/internal/secretbridge"
)

// The secret-broker tools make Mesh the single hub an agent talks to: knowledge in
// plaintext AND, on request, a short-lived capability TOKEN for a secret the agent can
// never read. They proxy to an attached Dockyard vault (capability mode) configured in
// [secret_bridge] of .mesh/config.toml (or MESH_SECRET_BRIDGE_* env). The plaintext
// secret NEVER crosses into Mesh: mesh_secret_list returns only names/metadata,
// mesh_secret_use returns a capability token + the proxy URL, and Dockyard injects the
// real value server-side at forward time. So no tool result, prompt, Mesh env, claude
// -p child env, or note ever carries a secret value. All crypto/rotation stays in
// Dockyard; Mesh holds only a thin HTTP client (internal/secretbridge). See the note
// "Dockyard Secrets Bridge (Capability Mode)".

// secretBridge resolves the attached-vault config the same way retrieval resolves its
// keys: the config file names the setup, env always overrides, and the API key is read
// from the env var the config NAMES (never stored). Returns (client, true) only when a
// base URL AND a key are present, so the tools stay dormant until configured.
func (s *Server) secretBridge() (*secretbridge.Client, bool) {
	base, keyEnv, agentID := "", "MESH_SECRET_BRIDGE_KEY", ""
	if cfg, err := meshcfg.LoadConfig(s.store.MeshDir()); err == nil {
		base = cfg.SecretBridge.BaseURL
		if cfg.SecretBridge.KeyEnv != "" {
			keyEnv = cfg.SecretBridge.KeyEnv
		}
		agentID = cfg.SecretBridge.AgentID
	}
	// Env overrides the file (the universal Mesh contract).
	if v := strings.TrimSpace(os.Getenv("MESH_SECRET_BRIDGE_URL")); v != "" {
		base = v
	}
	if v := strings.TrimSpace(os.Getenv("MESH_SECRET_BRIDGE_AGENT_ID")); v != "" {
		agentID = v
	}
	base = strings.TrimSpace(base)
	key := strings.TrimSpace(os.Getenv(keyEnv))
	if base == "" || key == "" {
		return nil, false
	}
	if agentID == "" {
		agentID = defaultAgentID()
	}
	return secretbridge.New(base, key, agentID), true
}

// defaultAgentID is the identity Mesh presents to Dockyard when none is configured.
func defaultAgentID() string {
	h, _ := os.Hostname()
	if strings.TrimSpace(h) == "" {
		h = "unknown"
	}
	return "mesh-" + h
}

// notConfigured is the graceful "no vault attached" result (same shape as
// mesh_code_search on an empty index): a note, not an error, so the tool is advertised
// but harmless until an operator wires the bridge.
func notConfigured() any {
	return textResult(map[string]any{
		"configured": false,
		"note": "no secret vault is attached. Set [secret_bridge] base_url in .mesh/config.toml " +
			"(or MESH_SECRET_BRIDGE_URL) and put the Dockyard API key in the env var it names " +
			"(default MESH_SECRET_BRIDGE_KEY) to enable brokered secrets.",
	})
}

// toolSecretStatus reports whether a vault is attached and how to use it, without any
// network call and without ever echoing the key. It is the whoami of the broker.
func (s *Server) toolSecretStatus(_ context.Context) (any, *rpcError) {
	c, ok := s.secretBridge()
	if !ok {
		return notConfigured(), nil
	}
	return textResult(map[string]any{
		"configured": true,
		"agent_id":   c.AgentID(),
		"base_url":   c.BaseURL(),
		"proxy_base": c.ProxyBase(),
		"how": "Call mesh_secret_list to see available secrets, then mesh_secret_use with the " +
			"destination you will call to mint a capability token. You never receive the real " +
			"secret; the vault injects it server-side at forward time.",
	}), nil
}

// toolSecretList returns the NAMES + rotation metadata of the attached vault's secrets
// (never a value). Read-only metadata, so it is not write-gated.
func (s *Server) toolSecretList(ctx context.Context, _ json.RawMessage) (any, *rpcError) {
	// Enumerating the vault inventory (names/providers of brokerable secrets) is a
	// privileged action: a read-only hosted viewer must not see it. Gate it the same way
	// as mesh_secret_use. Unset capability (local solo binary) owns the vault.
	if can, set := writeAllowed(ctx); set && !can {
		return nil, &rpcError{Code: codeInvalidParams, Message: "forbidden: your role is read-only"}
	}
	c, ok := s.secretBridge()
	if !ok {
		return notConfigured(), nil
	}
	cctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	metas, err := c.ListSecrets(cctx)
	if err != nil {
		return textResult(map[string]any{"error": bridgeErr(err)}), nil
	}
	return textResult(map[string]any{"secrets": metas, "count": len(metas), "base_url": c.BaseURL()}), nil
}

// toolSecretUse mints a short-lived, single-use, destination-bound capability token for
// a secret the agent can never read. Minting spends the team's credential, so it is
// gated behind the write role (a read-only hosted viewer cannot broker secrets); a solo
// binary with no role policy is unrestricted. Returns a token + the proxy URL; the real
// secret is injected by Dockyard's proxy at forward time, never returned here.
func (s *Server) toolSecretUse(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	if can, set := writeAllowed(ctx); set && !can {
		return nil, &rpcError{Code: codeInvalidParams, Message: "forbidden: your role is read-only; you cannot broker secrets"}
	}
	var a struct {
		Destination string `json:"destination"`
		SecretName  string `json:"secret_name"`
		Method      string `json:"method"`
		TTLSeconds  int    `json:"ttl_seconds"`
	}
	json.Unmarshal(raw, &a)
	if strings.TrimSpace(a.Destination) == "" {
		return nil, &rpcError{Code: codeInvalidParams, Message: "destination is required (the host+path you will call, e.g. api.openai.com/v1/chat/completions)"}
	}
	c, ok := s.secretBridge()
	if !ok {
		return notConfigured(), nil
	}
	cctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	res, err := c.Issue(cctx, secretbridge.IssueRequest{
		SecretName: a.SecretName, Destination: a.Destination, Method: a.Method, TTLSeconds: a.TTLSeconds,
	})
	if err != nil {
		return textResult(map[string]any{"error": bridgeErr(err)}), nil
	}
	host, path := secretbridge.SplitHostPath(a.Destination)
	return textResult(map[string]any{
		"proxy_url":   c.ProxyURL(host, path),
		"token":       res.Token,
		"secret_used": res.Secret, // the NAME of the resolved vault entry, never a value
		"expires_at":  res.ExpiresAt,
		"usage": "Send your request to proxy_url with header `Authorization: Capability <token>` " +
			"(the token field above). The Dockyard vault injects the real credential server-side, so " +
			"you never see it. The token is single-use and short-lived: mint a fresh one per request, " +
			"and never store or log it.",
	}), nil
}

// bridgeErr returns a client error string safe to hand back to the agent. secretbridge
// only ever carries Dockyard status codes (secret_not_found, agent_not_granted,
// ambiguous_destination, ...) or transport errors, never the API key or a secret value.
func bridgeErr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
