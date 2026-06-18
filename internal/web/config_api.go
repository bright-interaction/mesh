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
		{"embedding.endpoint", "Endpoint", "Embedding", "text", e.Endpoint, "BYOAI embedding endpoint (OpenAI-compatible /v1)"},
		{"embedding.model", "Model", "Embedding", "text", e.Model, "Embedding model id"},
		{"embedding.dim", "Dimensions", "Embedding", "number", ival(e.Dim), "Vector width (must match the model)"},
		{"embedding.key_env", "Key env var", "Embedding", "keyref", e.KeyEnv, "NAME of the env var holding the bearer key (never the key itself)"},
		{"embedding.query_prefix", "Query prefix", "Embedding", "text", e.QueryPrefix, "Prefix for query embeds (asymmetric models)"},
		{"embedding.doc_prefix", "Doc prefix", "Embedding", "text", e.DocPrefix, "Prefix for document embeds"},
		{"retrieval.weight_fts", "FTS weight", "Retrieval", "number", num(rv.WeightFTS), "Full-text fusion weight (0 = default)"},
		{"retrieval.weight_graph", "Graph weight", "Retrieval", "number", num(rv.WeightGraph), "Graph-proximity fusion weight (0 = default)"},
		{"retrieval.weight_vec", "Vector weight", "Retrieval", "number", num(rv.WeightVec), "Semantic fusion weight (0 = default)"},
		{"rerank.endpoint", "Endpoint", "Rerank", "text", rv.RerankEndpoint, "Cross-encoder rerank endpoint (empty = off)"},
		{"rerank.model", "Model", "Rerank", "text", rv.RerankModel, "Rerank model id"},
		{"rerank.key_env", "Key env var", "Rerank", "keyref", rv.RerankKeyEnv, "NAME of the env var holding the rerank key"},
		{"rerank.blend", "Blend", "Rerank", "number", num(rv.RerankBlend), "How much the reranker overrides fusion (0..1)"},
		{"ann.hnsw_threshold", "HNSW threshold", "Scale (pro)", "number", ival(rv.HNSWThreshold), "Build the ANN index past this many chunks (pro build only; 0 = brute force)"},
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
