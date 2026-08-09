// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/meshcfg"
)

// cfgField is one editable setting, with where its effective value comes from.
// source is "env" (an env var overrides the file, so editable is false), "file"
// (set in config.toml), or "default". Secrets are never a value here: key_env
// fields hold the NAME of the env var that holds the key, never the key.
type cfgField struct {
	Key      string `json:"key"`
	Label    string `json:"label"`
	Group    string `json:"group"`
	Kind     string `json:"kind"` // text | number | keyref
	Value    string `json:"value"`
	Source   string `json:"source"`
	Editable bool   `json:"editable"`
	Help     string `json:"help,omitempty"`
}

// envFor maps a config key to the env var that overrides it. key_env fields map to
// "" (the file holds the var name; nothing overrides the name itself).
var envFor = map[string]string{
	"embedding.endpoint":     "MESH_EMBED_ENDPOINT",
	"embedding.model":        "MESH_EMBED_MODEL",
	"embedding.dim":          "MESH_EMBED_DIM",
	"embedding.key_env":      "",
	"embedding.query_prefix": "MESH_EMBED_QUERY_PREFIX",
	"embedding.doc_prefix":   "MESH_EMBED_DOC_PREFIX",
	"retrieval.weight_fts":   "MESH_WEIGHT_FTS",
	"retrieval.weight_graph": "MESH_WEIGHT_GRAPH",
	"retrieval.weight_vec":   "MESH_WEIGHT_VEC",
	"rerank.endpoint":        "MESH_RERANK_ENDPOINT",
	"rerank.model":           "MESH_RERANK_MODEL",
	"rerank.key_env":         "",
	"rerank.blend":           "MESH_RERANK_BLEND",
	"ann.hnsw_threshold":     "MESH_HNSW_THRESHOLD",
	"secret_bridge.base_url": "MESH_SECRET_BRIDGE_URL",
	"secret_bridge.key_env":  "",
	"secret_bridge.agent_id": "MESH_SECRET_BRIDGE_AGENT_ID",
}

func (s *Server) effectiveConfig() []cfgField {
	c, _ := meshcfg.LoadConfig(s.store.MeshDir())
	e, rv, sb := c.Embedding, c.Retrieval, c.SecretBridge
	num := func(f float64) string {
		if f == 0 {
			return ""
		}
		return strconv.FormatFloat(f, 'g', -1, 64)
	}
	ival := func(i int) string {
		if i == 0 {
			return ""
		}
		return strconv.Itoa(i)
	}
	defs := []struct {
		key, label, group, kind, file, help string
	}{
		{"embedding.endpoint", "Endpoint", "Semantic search (optional)", "text", e.Endpoint, "URL of an embedding service (OpenAI-compatible /v1). Adds \"find by meaning\" on top of keyword search. Leave blank to use fast keyword + graph search only."},
		{"embedding.model", "Model", "Semantic search (optional)", "text", e.Model, "The embedding model your endpoint serves, e.g. nomic-embed-text."},
		{"embedding.dim", "Dimensions", "Semantic search (optional)", "number", ival(e.Dim), "The vector size the model outputs, e.g. 768. Must match the model exactly."},
		{"embedding.key_env", "Key env var", "Semantic search (optional)", "keyref", e.KeyEnv, "Name of an environment variable that holds the API key. Mesh reads the key from there; it is never typed or stored here."},
		{"embedding.query_prefix", "Query prefix", "Semantic search (optional)", "text", e.QueryPrefix, "Some models need text prepended to a search query (e.g. \"search_query: \"). Leave blank unless the model's docs say so."},
		{"embedding.doc_prefix", "Doc prefix", "Semantic search (optional)", "text", e.DocPrefix, "Like the query prefix, but for the notes being indexed. Leave blank unless required."},
		{"retrieval.weight_fts", "Keyword weight", "Ranking (advanced)", "number", num(rv.WeightFTS), "How much exact keyword matches count toward ranking. Blank = the tuned default. Most people never change these."},
		{"retrieval.weight_graph", "Link weight", "Ranking (advanced)", "number", num(rv.WeightGraph), "How much a note's links and closeness to your query count toward ranking. Blank = default."},
		{"retrieval.weight_vec", "Meaning weight", "Ranking (advanced)", "number", num(rv.WeightVec), "How much meaning-based similarity counts. Only has an effect when semantic search is on. Blank = default."},
		{"rerank.endpoint", "Endpoint", "Reranker (optional)", "text", rv.RerankEndpoint, "URL of a reranker service that re-scores the top hits for a sharper #1 result. Leave blank to skip reranking."},
		{"rerank.model", "Model", "Reranker (optional)", "text", rv.RerankModel, "The rerank model your endpoint serves."},
		{"rerank.key_env", "Key env var", "Reranker (optional)", "keyref", rv.RerankKeyEnv, "Name of the environment variable holding the reranker's API key."},
		{"rerank.blend", "Blend", "Reranker (optional)", "number", num(rv.RerankBlend), "0 to 1. How strongly the reranker overrides the base ranking (1 = trust it fully). Blank = default."},
		{"ann.hnsw_threshold", "Approx-index threshold", "Large-vault scale (pro)", "number", ival(rv.HNSWThreshold), "Pro builds only. Above this many chunks, switch to a faster approximate index. Blank or 0 = exact search, which is fine for most vaults."},
		{"secret_bridge.base_url", "Dockyard URL", "Secret vault (optional)", "text", sb.BaseURL, "URL of a Dockyard instance running the capability-mode secrets vault. When set, agents can fetch short-lived, use-once tokens for your stored API keys (which stay encrypted and auto-rotate) without ever seeing the key. Leave blank to disable."},
		{"secret_bridge.key_env", "Key env var", "Secret vault (optional)", "keyref", sb.KeyEnv, "Name of the environment variable holding the Dockyard API key. Mesh reads the key from there; it is never typed or stored here. Blank = MESH_SECRET_BRIDGE_KEY."},
		{"secret_bridge.agent_id", "Agent id", "Secret vault (optional)", "text", sb.AgentID, "The identity Mesh presents to Dockyard (for the audit log + access grants). Blank = mesh-<hostname>."},
	}
	out := make([]cfgField, 0, len(defs))
	for _, d := range defs {
		f := cfgField{Key: d.key, Label: d.label, Group: d.group, Kind: d.kind, Help: d.help, Value: d.file, Source: "default", Editable: true}
		if d.file != "" {
			f.Source = "file"
		}
		if env := envFor[d.key]; env != "" {
			if v := os.Getenv(env); v != "" {
				f.Value = v // env wins
				f.Source = "env"
				f.Editable = false // cannot edit an env-overridden value from the UI
				f.Help = d.help + " (set by " + env + ")"
			}
		}
		out = append(out, f)
	}
	return out
}

// checkKeyEnv validates a *.key_env value against the closed allow-list in
// meshcfg (see internal/meshcfg/keyenv.go, which is also what the retrieval and
// secret-bridge READ sites resolve through, so the write gate and the read gate cannot
// drift). key_env is a POINTER to a secret: the retrieval layer dereferences it with
// os.Getenv and sends the result as an Authorization: Bearer header to a
// caller-configured endpoint, so validating only the identifier's SHAPE would turn a
// config write into an arbitrary read of the whole process environment. Empty is allowed:
// it clears the field back to the per-stage built-in default.
func checkKeyEnv(key, v string) error {
	if meshcfg.KeyEnvAllowed(v) {
		return nil
	}
	return fmt.Errorf("%s must be one of %s", key, meshcfg.AllowedKeyEnvList)
}

// scrubKeyEnv clears every *.key_env on a loaded config that is not allow-listed,
// logging what it dropped. Used before rewriting config.toml so a value that got in
// before the allow-list existed (or by hand-editing the file) is not re-persisted.
func scrubKeyEnv(c *meshcfg.Config) {
	drop := func(field string, v *string) {
		if err := checkKeyEnv(field, *v); err != nil {
			slog.Warn("mesh ui: dropping a disallowed key_env from config.toml", "field", field, "value", *v)
			*v = ""
		}
	}
	drop("embedding.key_env", &c.Embedding.KeyEnv)
	drop("rerank.key_env", &c.Retrieval.RerankKeyEnv)
	drop("secret_bridge.key_env", &c.SecretBridge.KeyEnv)
}

// handleGetConfig returns the effective settings. Admin-gated: the values include the
// internal embedding/rerank endpoints and the Dockyard secret-bridge URL plus agent id,
// which is reconnaissance a read-only viewer has no reason to hold. A no-op in
// standalone loopback mode, where the single token is already the gate.
func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	writeJSON(w, map[string]any{"fields": s.effectiveConfig()})
}

func (s *Server) handlePutConfig(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var req struct {
		Updates map[string]string `json:"updates"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	// Serialize the whole load-modify-save: it is a read-modify-write of one file, so
	// two concurrent PUTs (member mode shares one .mesh dir) would otherwise each load
	// the same base and the second SaveConfig would clobber the first's field.
	s.configMu.Lock()
	defer s.configMu.Unlock()
	// editable set, so an env-overridden field is rejected (writing the file would
	// have no effect while the env var is set).
	editable := map[string]bool{}
	for _, f := range s.effectiveConfig() {
		editable[f.Key] = f.Editable
	}
	cfg, _ := meshcfg.LoadConfig(s.store.MeshDir())
	// Defence in depth on the READ side: the file on disk may predate the allow-list or
	// have been hand-edited, and this handler is about to rewrite the whole struct back
	// out. Drop any key_env that names something outside allowedKeyEnv rather than
	// re-persisting it, so the allow-list is not just an input filter.
	scrubKeyEnv(&cfg)
	for k, v := range req.Updates {
		if _, known := envFor[k]; !known {
			http.Error(w, "unknown field: "+k, http.StatusBadRequest)
			return
		}
		if !editable[k] {
			http.Error(w, "field "+k+" is set by an environment variable and cannot be edited here", http.StatusConflict)
			return
		}
		if err := applyConfigField(&cfg, k, strings.TrimSpace(v)); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	if err := meshcfg.SaveConfig(s.store.MeshDir(), cfg); err != nil {
		http.Error(w, "save failed", http.StatusInternalServerError)
		return
	}
	// Retrieval weights / embedding endpoint may have changed; drop the cached
	// retriever so the next search reflects the new config.
	s.invalidateRetriever()
	writeJSON(w, map[string]any{"fields": s.effectiveConfig(), "saved": true})
}

// applyConfigField sets one validated field on the Config.
func applyConfigField(c *meshcfg.Config, key, v string) error {
	pf := func() (float64, error) {
		if v == "" {
			return 0, nil
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < 0 {
			return 0, fmt.Errorf("%s must be a non-negative number", key)
		}
		return f, nil
	}
	pi := func() (int, error) {
		if v == "" {
			return 0, nil
		}
		i, err := strconv.Atoi(v)
		if err != nil || i < 0 {
			return 0, fmt.Errorf("%s must be a non-negative integer", key)
		}
		return i, nil
	}
	switch key {
	case "embedding.endpoint":
		c.Embedding.Endpoint = v
	case "embedding.model":
		c.Embedding.Model = v
	case "embedding.dim":
		i, err := pi()
		if err != nil {
			return err
		}
		c.Embedding.Dim = i
	case "embedding.key_env":
		if err := checkKeyEnv(key, v); err != nil {
			return err
		}
		c.Embedding.KeyEnv = v
	case "embedding.query_prefix":
		c.Embedding.QueryPrefix = v
	case "embedding.doc_prefix":
		c.Embedding.DocPrefix = v
	case "retrieval.weight_fts":
		f, err := pf()
		if err != nil {
			return err
		}
		c.Retrieval.WeightFTS = f
	case "retrieval.weight_graph":
		f, err := pf()
		if err != nil {
			return err
		}
		c.Retrieval.WeightGraph = f
	case "retrieval.weight_vec":
		f, err := pf()
		if err != nil {
			return err
		}
		c.Retrieval.WeightVec = f
	case "rerank.endpoint":
		c.Retrieval.RerankEndpoint = v
	case "rerank.model":
		c.Retrieval.RerankModel = v
	case "rerank.key_env":
		if err := checkKeyEnv(key, v); err != nil {
			return err
		}
		c.Retrieval.RerankKeyEnv = v
	case "rerank.blend":
		f, err := pf()
		if err != nil {
			return err
		}
		if f > 1 {
			return fmt.Errorf("rerank.blend must be between 0 and 1")
		}
		c.Retrieval.RerankBlend = f
	case "ann.hnsw_threshold":
		i, err := pi()
		if err != nil {
			return err
		}
		c.Retrieval.HNSWThreshold = i
	case "secret_bridge.base_url":
		c.SecretBridge.BaseURL = v
	case "secret_bridge.key_env":
		if err := checkKeyEnv(key, v); err != nil {
			return err
		}
		c.SecretBridge.KeyEnv = v
	case "secret_bridge.agent_id":
		c.SecretBridge.AgentID = v
	default:
		return fmt.Errorf("unknown field: %s", key)
	}
	return nil
}

// handleReindex brings the served graph up to date with the vault, the browser
// equivalent of `mesh index`. Returns what changed.
//
// A read-only viewer does not reindex anything, because it cannot: the owning writer
// does that. It measures what the vault has drifted by, waits for the owner to absorb
// it, and re-reads the result. Same observable contract ("after this returns, what is on
// disk is queryable"), reached the only way a process without the write lock can reach
// it, and with the same loud owner_down when the owner is not running. Rounding that up
// to a plain success would make the button a lie in exactly the case it exists for.
func (s *Server) handleReindex(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	out := map[string]any{"reindexed": true}
	if s.store.ReadOnly() {
		werr := s.store.AwaitOwnerCaughtUp(r.Context(), s.vaultRoot, s.ownerWait)
		stale := errors.Is(werr, index.ErrOwnerNotIndexing)
		if werr != nil && !stale {
			slog.Error("mesh ui: waiting for the owning writer failed", "error", werr)
			http.Error(w, "reindex failed", http.StatusInternalServerError)
			return
		}
		if err := s.refresh(); err != nil {
			slog.Error("mesh ui: reloading the graph failed", "error", err)
			http.Error(w, "reindex failed", http.StatusInternalServerError)
			return
		}
		if stale {
			out["index_stale"] = true
			out["owner_down"] = true
			out["warning"] = ownerDownNote
		}
	} else {
		// This server owns the index: apply anything a reader queued, then reindex.
		if _, err := s.store.DrainOps(); err != nil {
			slog.Warn("mesh ui: could not drain the owner op queue", "error", err)
		}
		g, err := index.Reindex(s.store, s.vaultRoot)
		if err != nil {
			http.Error(w, "reindex failed", http.StatusInternalServerError)
			return
		}
		// Swap the graph and drop the cached retriever in ONE exclusive critical section,
		// so a retriever build that is in flight cannot publish a retriever over the old
		// graph after we cleared the cache (see Server.retriever).
		s.mu.Lock()
		s.graph = g
		s.cachedRetriever.Store(nil) // rebuild over the fresh graph on the next search
		s.mu.Unlock()
	}
	notes, _ := s.store.Count("notes")
	nodes, _ := s.store.Count("nodes")
	edges, _ := s.store.Count("edges")
	out["counts"] = map[string]int{"notes": notes, "nodes": nodes, "edges": edges}
	writeJSON(w, out)
}
