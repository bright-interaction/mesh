// Package meshcfg reads and writes a solo vault's local embedding config at
// <vault>/.mesh/config.toml. It is the solo counterpart to a team vault's
// hub-authoritative mesh.toml: it pins the embedding endpoint/model/dim so
// `mesh search`, `mesh mcp`, and friends work without re-exporting env vars every
// session. Environment variables always override the file (the file is a fallback,
// never an authority), and secrets are never stored: key_env names the env var
// that holds the bearer key, it does not hold the key itself.
package meshcfg

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Embedding is the [embedding] section of a solo config.toml.
type Embedding struct {
	Endpoint    string
	Model       string
	Dim         int
	KeyEnv      string // env var NAME holding the bearer key (never the key itself)
	QueryPrefix string // e.g. "search_query: " for nomic-style asymmetric models
	DocPrefix   string // e.g. "search_document: "
}

// Retrieval is the [retrieval] + [rerank] + [ann] sections of config.toml: the
// solo fallback for fusion weights, the cross-encoder rerank stage, and the ANN
// gate. As with [embedding], the matching env vars (MESH_WEIGHT_*, MESH_RERANK_*,
// MESH_HNSW_THRESHOLD) override these; keys live in env vars named by *KeyEnv.
type Retrieval struct {
	WeightFTS      float64
	WeightGraph    float64
	WeightVec      float64
	RerankEndpoint string
	RerankModel    string
	RerankKeyEnv   string
	RerankBlend    float64
	HNSWThreshold  int
	// FreshnessHalfLifeDays decays non-institutional notes in ranking by age (0 =
	// off, the default, so nothing changes silently). Env MESH_FRESHNESS_HALFLIFE_DAYS
	// wins. Tier-0 (decisions/gotchas/post-mortems) + entities/concepts/maps never
	// decay; only note/status notes do, floored so an old note is demoted, not buried.
	FreshnessHalfLifeDays int
}

// Code is the [code] section: the opt-in source-code index (the graphify
// replacement). Index gates it on; Roots are the repos to walk (separate from the
// note vault, since source lives elsewhere); Languages is an allowlist of language
// tags (empty = all supported). Env MESH_CODE_INDEX / MESH_CODE_ROOTS override.
type Code struct {
	Index     bool
	Roots     []string
	Languages []string
}

// Config is the full solo config.toml (embedding + retrieval + code).
type Config struct {
	Embedding Embedding
	Retrieval Retrieval
	Code      Code
}

// configName is the file under <vault>/.mesh.
const configName = "config.toml"

// Load reads <meshDir>/config.toml. A missing file is not an error: it returns a
// zero Embedding so callers can treat "no config" and "empty config" the same.
func Load(meshDir string) (Embedding, error) {
	b, err := os.ReadFile(filepath.Join(meshDir, configName))
	if err != nil {
		if os.IsNotExist(err) {
			return Embedding{}, nil
		}
		return Embedding{}, err
	}
	body := string(b)
	e := Embedding{
		Endpoint:    sectionString(body, "embedding", "endpoint"),
		Model:       sectionString(body, "embedding", "model"),
		KeyEnv:      sectionString(body, "embedding", "key_env"),
		QueryPrefix: sectionString(body, "embedding", "query_prefix"),
		DocPrefix:   sectionString(body, "embedding", "doc_prefix"),
	}
	if d := sectionString(body, "embedding", "dim"); d != "" {
		e.Dim, _ = strconv.Atoi(d)
	}
	return e, nil
}

// LoadConfig reads the full config.toml (embedding + retrieval). A missing file
// returns a zero Config, like Load.
func LoadConfig(meshDir string) (Config, error) {
	emb, err := Load(meshDir)
	if err != nil {
		return Config{}, err
	}
	c := Config{Embedding: emb}
	b, err := os.ReadFile(filepath.Join(meshDir, configName))
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return Config{}, err
	}
	body := string(b)
	c.Retrieval = Retrieval{
		WeightFTS:      sectionFloat(body, "retrieval", "weight_fts"),
		WeightGraph:    sectionFloat(body, "retrieval", "weight_graph"),
		WeightVec:      sectionFloat(body, "retrieval", "weight_vec"),
		RerankEndpoint: sectionString(body, "rerank", "endpoint"),
		RerankModel:    sectionString(body, "rerank", "model"),
		RerankKeyEnv:   sectionString(body, "rerank", "key_env"),
		RerankBlend:           sectionFloat(body, "rerank", "blend"),
		HNSWThreshold:         int(sectionFloat(body, "ann", "hnsw_threshold")),
		FreshnessHalfLifeDays: int(sectionFloat(body, "retrieval", "freshness_half_life_days")),
	}
	c.Code = Code{
		Index:     sectionBool(body, "code", "index"),
		Roots:     sectionList(body, "code", "roots"),
		Languages: sectionList(body, "code", "languages"),
	}
	return c, nil
}

const configTemplate = `# Mesh solo vault config. Local, not synced (a team vault uses the
# hub-authoritative mesh.toml instead). Pins your setup so mesh search / mesh mcp
# work without re-exporting env vars each session. Environment variables
# (MESH_EMBED_* / MESH_WEIGHT_* / MESH_RERANK_* / MESH_HNSW_THRESHOLD) OVERRIDE
# these values. Secrets are never stored here: key_env only names the env var that
# holds the bearer key.

[embedding]
endpoint = %q
model = %q
dim = %d
key_env = %q
query_prefix = %q
doc_prefix = %q

[retrieval]
# Fusion weights. 0 means "use the built-in default". Env MESH_WEIGHT_* wins.
weight_fts = %g
weight_graph = %g
weight_vec = %g
# Age-decay non-institutional notes in ranking (0 = off). Tier-0 + entities/concepts
# never decay. Env MESH_FRESHNESS_HALFLIFE_DAYS wins.
freshness_half_life_days = 0

[rerank]
# Cross-encoder rerank (BYOAI). Empty endpoint/model = off. Env MESH_RERANK_* wins.
endpoint = %q
model = %q
key_env = %q
blend = %g

[ann]
# HNSW approximate-nearest-neighbour gate: build the index past this many chunks
# (0 = brute force). Only acts in the pro build. Env MESH_HNSW_THRESHOLD wins.
hnsw_threshold = %d

[code]
# Source-code index (the graphify replacement). Opt-in. index=true walks the roots
# below and lets mesh_code_search / mesh_code_neighbors locate functions, types, and
# the Go call graph. Roots are SEPARATE from the note vault (they are other repos);
# comma-separated. languages is a comma list of tags (go,ts,tsx,js,jsx,svelte,astro,
# py); empty = all supported. Env MESH_CODE_INDEX / MESH_CODE_ROOTS override.
index = %v
roots = %q
languages = %q
`

// Save writes the [embedding] section, preserving any other sections already in the
// file. Kept for the `mesh embed` caller; new callers should use SaveConfig.
func Save(meshDir string, e Embedding) error {
	cfg, _ := LoadConfig(meshDir)
	cfg.Embedding = e
	return SaveConfig(meshDir, cfg)
}

// SaveConfig writes the full <meshDir>/config.toml atomically (temp + rename), 0644.
func SaveConfig(meshDir string, c Config) error {
	// key_env vars are NAMES of env vars, never secrets. Reject anything that is not
	// a plain identifier so a bad value cannot break the simple TOML round-trip.
	if !validEnvName(c.Embedding.KeyEnv) {
		c.Embedding.KeyEnv = "MESH_EMBED_KEY"
	}
	if c.Retrieval.RerankKeyEnv != "" && !validEnvName(c.Retrieval.RerankKeyEnv) {
		c.Retrieval.RerankKeyEnv = "MESH_RERANK_KEY"
	}
	e, rv := c.Embedding, c.Retrieval
	body := fmt.Sprintf(configTemplate,
		e.Endpoint, e.Model, e.Dim, e.KeyEnv, e.QueryPrefix, e.DocPrefix,
		rv.WeightFTS, rv.WeightGraph, rv.WeightVec,
		rv.RerankEndpoint, rv.RerankModel, rv.RerankKeyEnv, rv.RerankBlend,
		rv.HNSWThreshold,
		c.Code.Index, strings.Join(c.Code.Roots, ","), strings.Join(c.Code.Languages, ","),
	)
	tmp, err := os.CreateTemp(meshDir, ".config-*.toml")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.WriteString(body); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, filepath.Join(meshDir, configName))
}

// validEnvName reports whether s is a plain environment-variable identifier
// ([A-Za-z_][A-Za-z0-9_]*). Empty is invalid.
func validEnvName(s string) bool {
	if s == "" {
		return false
	}
	for i, c := range s {
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c == '_':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// sectionBool reads a boolean key from a section: true for true/1/yes/on (any
// case), false otherwise (including absent).
func sectionBool(toml, section, key string) bool {
	switch strings.ToLower(sectionString(toml, section, key)) {
	case "true", "1", "yes", "on":
		return true
	}
	return false
}

// sectionList reads a comma-separated key into a trimmed, empty-dropped slice; nil
// when the key is absent or blank.
func sectionList(toml, section, key string) []string {
	raw := sectionString(toml, section, key)
	if raw == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// sectionFloat reads a numeric key from a section, 0 when absent or unparseable.
func sectionFloat(toml, section, key string) float64 {
	s := sectionString(toml, section, key)
	if s == "" {
		return 0
	}
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

// sectionString pulls a simple `key = "value"` (or bare key = value) from inside a
// named [section]. Section-aware so a future section reusing a key name cannot
// shadow another's. Not a general TOML parser.
func sectionString(toml, section, key string) string {
	cur := ""
	for _, line := range strings.Split(toml, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			cur = strings.TrimSpace(line[1 : len(line)-1])
			continue
		}
		if cur != section {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(k) != key {
			continue
		}
		return strings.Trim(strings.TrimSpace(v), `"`)
	}
	return ""
}
