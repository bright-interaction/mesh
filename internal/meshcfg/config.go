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

const configTemplate = `# Mesh solo vault config. Local, not synced (a team vault uses the
# hub-authoritative mesh.toml instead). Pins your embedding setup so mesh search /
# mesh mcp work without re-exporting env vars each session. Environment variables
# (MESH_EMBED_ENDPOINT / MESH_EMBED_MODEL / the key_env var below / ...) OVERRIDE
# these values. Secrets are never stored here: key_env only names the env var that
# holds the bearer key.

[embedding]
endpoint = %q
model = %q
dim = %d
key_env = %q
query_prefix = %q
doc_prefix = %q
`

// Save writes <meshDir>/config.toml atomically (temp file + rename), 0644.
func Save(meshDir string, e Embedding) error {
	// key_env is the NAME of an env var, never a secret. Reject anything that is not a
	// plain env-var identifier: it could not name a real var anyway, and a value with
	// quotes/newlines would not round-trip through the simple TOML reader.
	if !validEnvName(e.KeyEnv) {
		e.KeyEnv = "MESH_EMBED_KEY"
	}
	body := fmt.Sprintf(configTemplate, e.Endpoint, e.Model, e.Dim, e.KeyEnv, e.QueryPrefix, e.DocPrefix)
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
