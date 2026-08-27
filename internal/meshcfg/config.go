// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

// Package meshcfg reads and writes a solo vault's local embedding config at
// <vault>/.mesh/config.toml. It is the solo counterpart to a team vault's
// hub-authoritative mesh.toml: it pins the embedding endpoint/model/dim so
// `mesh search`, `mesh mcp`, and friends work without re-exporting env vars every
// session. Environment variables always override the file (the file is a fallback,
// never an authority), and secrets are never stored: key_env names the env var
// that holds the bearer key, it does not hold the key itself.
package meshcfg

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/bright-interaction/mesh/internal/vault"
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

// Code is the [code] section: the opt-in source-code index. Index gates it on; Roots are the repos to walk (separate from the
// note vault, since source lives elsewhere); Languages is an allowlist of language
// tags (empty = all supported). Env MESH_CODE_INDEX / MESH_CODE_ROOTS override.
type Code struct {
	Index     bool
	Roots     []string
	Languages []string
}

// SecretBridge is the [secret_bridge] section: an optional attached Dockyard vault
// (capability mode) that Mesh brokers secrets from. When BaseURL is set, the MCP
// server exposes mesh_secret_list / mesh_secret_use so an agent can fetch a short-
// lived capability token for a secret it can never read; the real value stays
// encrypted in Dockyard and is injected server-side by its proxy. Mesh stores NO
// secret: KeyEnv NAMES the env var that holds the Dockyard API key, exactly like the
// embedding key_env indirection. AgentID is the identity Mesh presents to Dockyard
// (empty = mesh-<hostname>). Env MESH_SECRET_BRIDGE_URL / MESH_SECRET_BRIDGE_KEY /
// MESH_SECRET_BRIDGE_AGENT_ID override these.
type SecretBridge struct {
	BaseURL string
	KeyEnv  string // env var NAME holding the Dockyard API key (never the key itself)
	AgentID string
}

// Config is the full solo config.toml (embedding + retrieval + code + secret bridge).
type Config struct {
	Embedding    Embedding
	Retrieval    Retrieval
	Code         Code
	SecretBridge SecretBridge
}

// configName is the file under <vault>/.mesh.
const configName = "config.toml"

// Load reads <meshDir>/config.toml. A missing file is not an error: it returns a
// zero Embedding so callers can treat "no config" and "empty config" the same.
func Load(meshDir string) (Embedding, error) {
	return LoadContext(context.Background(), meshDir)
}

// LoadContext is Load with caller-controlled cancellation of the config read.
func LoadContext(ctx context.Context, meshDir string) (Embedding, error) {
	b, err := vault.ReadFileContext(ctx, filepath.Join(meshDir, configName))
	if err != nil {
		if os.IsNotExist(err) {
			return Embedding{}, nil
		}
		return Embedding{}, err
	}
	return parseEmbedding(string(b)), nil
}

func parseEmbedding(body string) Embedding {
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
	return e
}

// LoadConfig reads the full config.toml (embedding + retrieval). A missing file
// returns a zero Config, like Load.
func LoadConfig(meshDir string) (Config, error) {
	return LoadConfigContext(context.Background(), meshDir)
}

// LoadConfigContext is LoadConfig with caller-controlled cancellation. It reads
// config.toml once, so one request cannot observe two different versions of the
// embedding and retrieval sections during a concurrent atomic config update.
func LoadConfigContext(ctx context.Context, meshDir string) (Config, error) {
	b, err := vault.ReadFileContext(ctx, filepath.Join(meshDir, configName))
	if err != nil {
		if os.IsNotExist(err) {
			return Config{}, nil
		}
		return Config{}, err
	}
	body := string(b)
	c := Config{Embedding: parseEmbedding(body)}
	c.Retrieval = Retrieval{
		WeightFTS:             sectionFloat(body, "retrieval", "weight_fts"),
		WeightGraph:           sectionFloat(body, "retrieval", "weight_graph"),
		WeightVec:             sectionFloat(body, "retrieval", "weight_vec"),
		RerankEndpoint:        sectionString(body, "rerank", "endpoint"),
		RerankModel:           sectionString(body, "rerank", "model"),
		RerankKeyEnv:          sectionString(body, "rerank", "key_env"),
		RerankBlend:           sectionFloat(body, "rerank", "blend"),
		HNSWThreshold:         int(sectionFloat(body, "ann", "hnsw_threshold")),
		FreshnessHalfLifeDays: int(sectionFloat(body, "retrieval", "freshness_half_life_days")),
	}
	c.Code = Code{
		Index:     sectionBool(body, "code", "index"),
		Roots:     sectionList(body, "code", "roots"),
		Languages: sectionList(body, "code", "languages"),
	}
	c.SecretBridge = SecretBridge{
		BaseURL: sectionString(body, "secret_bridge", "base_url"),
		KeyEnv:  sectionString(body, "secret_bridge", "key_env"),
		AgentID: sectionString(body, "secret_bridge", "agent_id"),
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
freshness_half_life_days = %d

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
# Source-code index  Opt-in. index=true walks the roots
# below and lets mesh_code_search / mesh_code_neighbors locate functions, types, and
# the Go call graph. Roots are SEPARATE from the note vault (they are other repos);
# comma-separated. languages is a comma list of tags (go,ts,tsx,js,jsx,svelte,astro,
# py); empty = all supported. Env MESH_CODE_INDEX / MESH_CODE_ROOTS override.
index = %v
roots = %q
languages = %q

[secret_bridge]
# Optional attached Dockyard vault (capability mode). When base_url is set, Mesh
# exposes mesh_secret_list / mesh_secret_use so an agent can fetch a SHORT-LIVED
# capability token for a secret it can never read: the real value stays encrypted in
# the Dockyard vault and is injected server-side by Dockyard's /proxy at forward time.
# Mesh stores NO secret here - key_env only NAMES the env var holding the Dockyard API
# key. Leave base_url empty to disable. Env MESH_SECRET_BRIDGE_URL /
# MESH_SECRET_BRIDGE_KEY / MESH_SECRET_BRIDGE_AGENT_ID override these.
base_url = %q
key_env = %q
agent_id = %q
`

// Save writes the [embedding] section, preserving any other sections already in the
// file. Kept for the `mesh embed` caller; new callers should use SaveConfig.
func Save(meshDir string, e Embedding) error {
	cfg, _ := LoadConfig(meshDir)
	cfg.Embedding = e
	return SaveConfig(meshDir, cfg)
}

// SaveConfig writes the full <meshDir>/config.toml durably: temp + rename at 0644, with
// the data fsynced before the rename publishes the name and the directory fsynced after,
// exactly like the note writers.
//
// It used to skip both fsyncs, exempted from the census guard in
// cmd/mesh/atomic_write_durability_test.go on the grounds that "every field is
// re-derivable from env or defaults and a lost config costs a re-run of `mesh init`".
// Neither half of that was true. `mesh init` does not write this file at all (the only
// two writers are `mesh embed` and the web Settings PUT), and the fields that matter are
// exactly the ones the operator TYPED and that no default can reproduce: the embedding
// endpoint/model/dim, the code roots, the secret-bridge base URL, the tuned fusion
// weights and the rerank stage. The whole point of the file is that the env vars do NOT
// have to be re-exported, so "re-derivable from env" describes the case where the file
// is not needed in the first place.
//
// The failure it leaves is also worse than "lost". An unsynced rename can publish a
// ZERO-LENGTH config.toml, and this parser reads a truncated file as a valid empty
// config with no error, so semantic search, rerank, freshness decay, the code index and
// the secret bridge all switch themselves off silently. Both writers then do
// load-modify-write (Save and the web handler each LoadConfig, mutate, SaveConfig), so
// the next single-field edit persists the zeroes and the operator's setup is gone for
// good with nothing having reported an error.
func SaveConfig(meshDir string, c Config) error {
	return SaveConfigContext(context.Background(), meshDir, c)
}

// SaveConfigContext is SaveConfig with cooperative cancellation before publication.
// Filesystem mutations remain synchronous: abandoning a blocked write/sync worker could
// let it mutate or strand a temp file after this function returned. Cancellation is
// checked between every pre-publication syscall and every bounded write chunk instead.
// The final check is immediately before rename. Once rename publishes config.toml, the
// directory fsync is completed synchronously and cancellation no longer changes the
// committed result.
func SaveConfigContext(ctx context.Context, meshDir string, c Config) error {
	return saveConfigContextWith(ctx, meshDir, c, nil)
}

// configSaveHooks is a narrow deterministic test seam around the two publication
// boundaries. Injected operations are still called synchronously, so the safety
// property under test is the same one production relies on: no mutating worker can
// outlive SaveConfigContext.
type configSaveHooks struct {
	write        func(*os.File, []byte) (int, error)
	beforeRename func()
	rename       func(string, string) error
	syncDir      func(string)
}

func saveConfigContextWith(ctx context.Context, meshDir string, c Config, hooks *configSaveHooks) (retErr error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// key_env vars are NAMES of env vars, never secrets, and the set of names they may
	// point at is closed (see keyenv.go). Anything outside the allow-list is reset to the
	// field's default rather than persisted, so SaveConfig can never write a config.toml
	// that aims a key_env at an unrelated process secret. This subsumes the old
	// plain-identifier shape check: every allow-listed name is a valid identifier.
	// Empty stays empty for the optional sections (it means "unset"), except for
	// embedding, whose template has always written the concrete default name.
	if c.Embedding.KeyEnv == "" || !KeyEnvAllowed(c.Embedding.KeyEnv) {
		c.Embedding.KeyEnv = "MESH_EMBED_KEY"
	}
	if !KeyEnvAllowed(c.Retrieval.RerankKeyEnv) {
		c.Retrieval.RerankKeyEnv = "MESH_RERANK_KEY"
	}
	if !KeyEnvAllowed(c.SecretBridge.KeyEnv) {
		c.SecretBridge.KeyEnv = "MESH_SECRET_BRIDGE_KEY"
	}
	e, rv := c.Embedding, c.Retrieval
	body := fmt.Sprintf(configTemplate,
		e.Endpoint, e.Model, e.Dim, e.KeyEnv, e.QueryPrefix, e.DocPrefix,
		rv.WeightFTS, rv.WeightGraph, rv.WeightVec, rv.FreshnessHalfLifeDays,
		rv.RerankEndpoint, rv.RerankModel, rv.RerankKeyEnv, rv.RerankBlend,
		rv.HNSWThreshold,
		c.Code.Index, strings.Join(c.Code.Roots, ","), strings.Join(c.Code.Languages, ","),
		c.SecretBridge.BaseURL, c.SecretBridge.KeyEnv, c.SecretBridge.AgentID,
	)
	if err := ctx.Err(); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(meshDir, ".config-*.toml")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	closed := false
	published := false
	defer func() {
		if published {
			return
		}
		// Every pre-publication exit removes the private name. Cleanup is joined before
		// return rather than delegated, so neither the descriptor nor a remover can race
		// a replacement process after shutdown.
		if !closed {
			if err := tmp.Close(); err != nil {
				retErr = errors.Join(retErr, fmt.Errorf("close config temp file: %w", err))
			}
			closed = true
		}
		if err := os.Remove(tmpName); err != nil && !os.IsNotExist(err) {
			retErr = errors.Join(retErr, fmt.Errorf("remove config temp file: %w", err))
		}
	}()
	if err := ctx.Err(); err != nil {
		return err
	}

	// Bound the amount of work between cancellation points. A single filesystem Write
	// can itself block and cannot safely be detached because it mutates the temp file;
	// once that syscall returns, cancellation wins before another chunk is attempted.
	data := []byte(body)
	const writeChunkSize = 32 << 10
	for len(data) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		chunk := data
		if len(chunk) > writeChunkSize {
			chunk = chunk[:writeChunkSize]
		}
		var n int
		if hooks != nil && hooks.write != nil {
			n, err = hooks.write(tmp, chunk)
		} else {
			n, err = tmp.Write(chunk)
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			return err
		}
		if n != len(chunk) {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	// os.CreateTemp makes its file 0600 and a rename installs that mode over the
	// destination, so the chmod is what keeps config.toml at the 0644 it has always
	// landed with. It runs BEFORE the fsync so the mode is part of what gets flushed,
	// and the mode itself is deliberately unchanged by this durability fix: a fix that
	// quietly narrows a file is worse than the gap it closes.
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Flush the data to the device BEFORE the rename publishes the name; a rename can
	// otherwise publish blocks that were never written.
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		closed = true
		return err
	}
	closed = true
	if hooks != nil && hooks.beforeRename != nil {
		hooks.beforeRename()
	}
	// This is the publication linearization point. If cancellation wins here, the old
	// config remains in place and the deferred cleanup removes the temp file. If rename
	// starts, its result decides whether the new config was committed.
	if err := ctx.Err(); err != nil {
		return err
	}
	dst := filepath.Join(meshDir, configName)
	if hooks != nil && hooks.rename != nil {
		err = hooks.rename(tmpName, dst)
	} else {
		err = os.Rename(tmpName, dst)
	}
	if err != nil {
		return err
	}
	published = true
	// Publication succeeded. Finish durability synchronously even if shutdown arrives
	// now; returning context.Canceled would falsely imply that the update did not land.
	if hooks != nil && hooks.syncDir != nil {
		hooks.syncDir(meshDir)
	} else {
		syncDir(meshDir)
	}
	return nil
}

// syncDir fsyncs a directory so the rename that just landed in it survives a power cut.
// Best effort on purpose, matching every other writer in the tree: opening or fsyncing a
// directory handle is a no-op or an error on some platforms and network filesystems
// (Windows cannot open one at all), and that is not a failed write, so the bytes still
// count as written and the refusal is only logged. The data itself is already fsynced.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		slog.Debug("meshcfg: cannot open the config directory to fsync it", "dir", dir, "err", err)
		return
	}
	if err := d.Sync(); err != nil {
		slog.Debug("meshcfg: cannot fsync the config directory", "dir", dir, "err", err)
	}
	d.Close()
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
