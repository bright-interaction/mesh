// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package web

import (
	"testing"

	"github.com/bright-interaction/mesh/internal/meshcfg"
)

// The endpoint allow-list must not eat an operator's existing configuration.
//
// scrubEndpoints exists so a redirect that landed before the allow-list existed cannot
// survive the next config write. That is worth having, but CheckEndpointURL refuses
// EVERY host when MESH_ALLOWED_ENDPOINT_HOSTS is unset, and unset is the state of every
// deployment that has not opted in yet. Running the scrub unconditionally therefore
// blanked all three endpoints on the next unrelated config write, silently, with a 200:
// the upgrade itself was the data loss, and it took the localhost sovereign setup with
// it, since a config.toml written by `mesh embed` is refused on scheme before the host
// is even considered.
//
// The write path is the one that faces the untrusted admin, and it is unchanged: PUT
// still cannot set an endpoint that is not allow-listed. config.toml is the operator's
// own file and is not the threat here, so the scrub is opt-in with the allow-list.
func TestScrubEndpoints_LeavesConfigAloneWhenNoAllowListIsSet(t *testing.T) {
	t.Setenv(meshcfg.AllowedEndpointHostsEnv, "")

	cfg := &meshcfg.Config{}
	cfg.Embedding.Endpoint = "http://localhost:11434/v1/embeddings"
	cfg.Retrieval.RerankEndpoint = "https://rerank.internal.example/v1/rerank"
	cfg.SecretBridge.BaseURL = "https://vault.example/api"

	scrubEndpoints(cfg)

	if got := cfg.Embedding.Endpoint; got != "http://localhost:11434/v1/embeddings" {
		t.Errorf("embedding.endpoint was wiped with no allow-list configured: got %q.\n"+
			"This is the sovereign localhost setup `mesh embed` writes, and the operator never "+
			"asked for it to be validated.", got)
	}
	if got := cfg.Retrieval.RerankEndpoint; got != "https://rerank.internal.example/v1/rerank" {
		t.Errorf("rerank.endpoint was wiped with no allow-list configured: got %q", got)
	}
	if got := cfg.SecretBridge.BaseURL; got != "https://vault.example/api" {
		t.Errorf("secret_bridge.base_url was wiped with no allow-list configured: got %q", got)
	}
}

// With an allow-list configured the operator HAS opted in, so the scrub does its job:
// a host outside the list is dropped rather than re-persisted.
func TestScrubEndpoints_DropsOffAllowListHostsWhenAllowListIsSet(t *testing.T) {
	t.Setenv(meshcfg.AllowedEndpointHostsEnv, "good.example")

	cfg := &meshcfg.Config{}
	cfg.Embedding.Endpoint = "https://good.example/v1/embeddings"
	cfg.Retrieval.RerankEndpoint = "https://evil.tld/v1/rerank"
	cfg.SecretBridge.BaseURL = "http://good.example/api" // right host, wrong scheme

	scrubEndpoints(cfg)

	if got := cfg.Embedding.Endpoint; got != "https://good.example/v1/embeddings" {
		t.Errorf("an allow-listed https endpoint was dropped: got %q", got)
	}
	if got := cfg.Retrieval.RerankEndpoint; got != "" {
		t.Errorf("a host off the allow-list survived the scrub: got %q. This is the redirect the "+
			"scrub exists to remove, and it carries note bodies plus a bearer token.", got)
	}
	if got := cfg.SecretBridge.BaseURL; got != "" {
		t.Errorf("a plain-http endpoint survived the scrub: got %q", got)
	}
}
