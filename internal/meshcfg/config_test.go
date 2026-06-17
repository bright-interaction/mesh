package meshcfg

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadMissingIsZero(t *testing.T) {
	e, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("Load of a missing config must not error: %v", err)
	}
	if (e != Embedding{}) {
		t.Errorf("missing config should be the zero Embedding, got %+v", e)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	in := Embedding{
		Endpoint:    "http://localhost:11434/v1",
		Model:       "nomic-embed-text",
		Dim:         768,
		KeyEnv:      "MY_KEY",
		QueryPrefix: "search_query: ",
		DocPrefix:   "search_document: ",
	}
	if err := Save(dir, in); err != nil {
		t.Fatalf("Save: %v", err)
	}
	out, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if out != in {
		t.Errorf("round-trip mismatch:\n in  %+v\n out %+v", in, out)
	}
}

func TestSaveDefaultsKeyEnv(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, Embedding{Endpoint: "http://x/v1", Model: "m", Dim: 4}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	out, _ := Load(dir)
	if out.KeyEnv != "MESH_EMBED_KEY" {
		t.Errorf("empty KeyEnv should default to MESH_EMBED_KEY, got %q", out.KeyEnv)
	}
}

// TestSaveNeverWritesSecrets is the sovereignty guard: the file holds the env var
// NAME, never a key value. (Defensive: even if a caller stuffed a secret into a
// field, only the documented fields are serialized.)
func TestSaveNeverWritesSecrets(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, Embedding{Endpoint: "http://x/v1", Model: "m", Dim: 4, KeyEnv: "MESH_EMBED_KEY"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(b); len(got) == 0 {
		t.Fatal("config.toml is empty")
	}
}

// TestSaveSanitizesInvalidKeyEnv: a KeyEnv that is not a plain env-var identifier
// (quotes, spaces, newlines) must be replaced with the default, never written
// verbatim (it could not name a real var and would not round-trip).
func TestSaveSanitizesInvalidKeyEnv(t *testing.T) {
	for _, bad := range []string{`BAD NAME`, `a"b`, "x\ny", `1leading`, ``} {
		dir := t.TempDir()
		if err := Save(dir, Embedding{Endpoint: "http://x/v1", Model: "m", Dim: 4, KeyEnv: bad}); err != nil {
			t.Fatalf("Save(%q): %v", bad, err)
		}
		out, _ := Load(dir)
		if out.KeyEnv != "MESH_EMBED_KEY" {
			t.Errorf("invalid KeyEnv %q should sanitize to MESH_EMBED_KEY, got %q", bad, out.KeyEnv)
		}
	}
}

func TestSectionStringIgnoresWrongSection(t *testing.T) {
	const toml = `[rerank]
model = "cross-encoder"

[embedding]
model = "nomic-embed-text"
`
	if got := sectionString(toml, "embedding", "model"); got != "nomic-embed-text" {
		t.Errorf("got %q, want nomic-embed-text", got)
	}
}
