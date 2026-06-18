package web

import (
	"encoding/json"
	"fmt"
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
}

func (s *Server) effectiveConfig() []cfgField {
	c, _ := meshcfg.LoadConfig(s.store.MeshDir())
	e, rv := c.Embedding, c.Retrieval
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

func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"fields": s.effectiveConfig()})
}

func (s *Server) handlePutConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Updates map[string]string `json:"updates"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	// editable set, so an env-overridden field is rejected (writing the file would
	// have no effect while the env var is set).
	editable := map[string]bool{}
	for _, f := range s.effectiveConfig() {
		editable[f.Key] = f.Editable
	}
	cfg, _ := meshcfg.LoadConfig(s.store.MeshDir())
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
	default:
		return fmt.Errorf("unknown field: %s", key)
	}
	return nil
}

// handleReindex runs an authoritative reconcile and swaps in the fresh graph, the
// browser equivalent of `mesh index`. Returns what changed.
func (s *Server) handleReindex(w http.ResponseWriter, r *http.Request) {
	g, err := index.Reindex(s.store, s.vaultRoot)
	if err != nil {
		http.Error(w, "reindex failed", http.StatusInternalServerError)
		return
	}
	s.mu.Lock()
	s.graph = g
	s.mu.Unlock()
	notes, _ := s.store.Count("notes")
	nodes, _ := s.store.Count("nodes")
	edges, _ := s.store.Count("edges")
	writeJSON(w, map[string]any{"reindexed": true, "counts": map[string]int{"notes": notes, "nodes": nodes, "edges": edges}})
}
