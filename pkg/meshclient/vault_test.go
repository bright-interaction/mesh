// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package meshclient

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/syncproto"
)

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// TestCheckHomogeneity covers the join-time one-embedding-space guard across both
// axes (model name and dim) and the section-aware toml read.
func TestCheckHomogeneity(t *testing.T) {
	const toml = `vault_id = "v1"
gc_horizon_days = 90

[embedding]
model = "nomic-embed-text"
dim = 768
`
	cases := []struct {
		name          string
		model, dim    string // operator env (empty = unset)
		wantErr       bool
		wantErrSubstr string
	}{
		{name: "both match", model: "nomic-embed-text", dim: "768", wantErr: false},
		{name: "model unset is allowed", model: "", dim: "", wantErr: false},
		{name: "model mismatch fails closed", model: "bge-small", dim: "", wantErr: true, wantErrSubstr: "model mismatch"},
		{name: "dim mismatch fails closed (same model name, different width)", model: "nomic-embed-text", dim: "384", wantErr: true, wantErrSubstr: "dim mismatch"},
		{name: "dim 0 operator is treated as unset", model: "nomic-embed-text", dim: "0", wantErr: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MESH_EMBED_MODEL", tc.model)
			t.Setenv("MESH_EMBED_DIM", tc.dim)
			err := checkHomogeneity(toml)
			if tc.wantErr && err == nil {
				t.Fatalf("expected an error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
			if tc.wantErrSubstr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErrSubstr)) {
				t.Fatalf("error %v does not contain %q", err, tc.wantErrSubstr)
			}
		})
	}
}

// TestTomlSectionStringIsSectionAware proves a key in one section cannot shadow the
// same key in another (the latent section-blind bug the old tomlString had).
func TestTomlSectionStringIsSectionAware(t *testing.T) {
	const toml = `[rerank]
model = "cross-encoder"

[embedding]
model = "nomic-embed-text"
dim = 768
`
	if got := tomlSectionString(toml, "embedding", "model"); got != "nomic-embed-text" {
		t.Errorf("embedding model = %q, want nomic-embed-text (must not read the [rerank] model)", got)
	}
	if got := tomlSectionString(toml, "rerank", "model"); got != "cross-encoder" {
		t.Errorf("rerank model = %q, want cross-encoder", got)
	}
	if got := tomlSectionString(toml, "embedding", "dim"); got != "768" {
		t.Errorf("embedding dim = %q, want 768", got)
	}
}

// TestApplyDeltasExternalEditorGuard: a local edit made after the outbox was
// computed must not be clobbered; the incoming hub version is parked.
func TestApplyDeltasExternalEditorGuard(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x.md"), []byte("local unsynced edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sent := map[string]string{"x.md": contentHash([]byte("what we sent\n"))} // disk != sent => changed since send
	deltas := []syncproto.Delta{{Path: "x.md", Op: "upsert", ContentB64: b64("hub version\n")}}

	parked, err := applyDeltas(dir, deltas, sent)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "x.md")); string(got) != "local unsynced edit\n" {
		t.Errorf("local edit must be kept, got %q", got)
	}
	if len(parked) != 1 || parked[0].note != "x.md" {
		t.Fatalf("expected the incoming version parked for x.md, got %v", parked)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, parked[0].sibling)); string(got) != "hub version\n" {
		t.Errorf("incoming hub version must be parked in the sibling, got %q", got)
	}
}

// TestApplyDeltasDeleteRaceNoResurrect: a file deleted locally during the sync
// window must NOT be resurrected by an incoming upsert (the CRITICAL finding).
func TestApplyDeltasDeleteRaceNoResurrect(t *testing.T) {
	dir := t.TempDir()
	// File is absent on disk (user deleted it after the outbox was computed) but
	// was present at send time.
	sent := map[string]string{"x.md": contentHash([]byte("was here at send\n"))}
	deltas := []syncproto.Delta{{Path: "x.md", Op: "upsert", ContentB64: b64("hub version\n")}}

	parked, err := applyDeltas(dir, deltas, sent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "x.md")); err == nil {
		t.Error("a locally-deleted file must not be resurrected by an incoming upsert")
	}
	if len(parked) != 1 || parked[0].note != "x.md" {
		t.Fatalf("expected the incoming version parked, got %v", parked)
	}
}

// TestKeepParkedDirty: a parked path must be reset to its pre-sync base hash so
// the next outbox re-pushes the kept local change (no silent divergence).
func TestKeepParkedDirty(t *testing.T) {
	base := map[string]string{"edited.md": "BASE", "deleted.md": "BASE"}
	current := map[string]string{"edited.md": "DISKEDIT", "deleted.md": "SHOULD-NOT-MATTER", "new.md": "X"}
	parked := []park{{note: "edited.md", sibling: "edited.sync-conflict.md"}, {note: "gone.md", sibling: "gone.sync-conflict.md"}}

	keepParkedDirty(current, base, parked)

	if current["edited.md"] != "BASE" {
		t.Errorf("parked edited.md must revert to base hash, got %q", current["edited.md"])
	}
	if _, ok := current["gone.md"]; ok {
		t.Error("a parked path absent from base must be removed from current (so it re-pushes as new/delete)")
	}
	if current["new.md"] != "X" {
		t.Error("unrelated paths must be untouched")
	}
}

// TestApplyDeltasNormalOverwrite: a file unchanged since send takes the hub
// version (no parking).
func TestApplyDeltasNormalOverwrite(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x.md"), []byte("as sent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sent := map[string]string{"x.md": contentHash([]byte("as sent\n"))}
	deltas := []syncproto.Delta{{Path: "x.md", Op: "upsert", ContentB64: b64("hub version\n")}}

	parked, err := applyDeltas(dir, deltas, sent)
	if err != nil {
		t.Fatal(err)
	}
	if len(parked) != 0 {
		t.Errorf("no parking expected for an unchanged-since-send file, got %v", parked)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "x.md")); string(got) != "hub version\n" {
		t.Errorf("expected the hub version, got %q", got)
	}
}

// TestApplyDeltasDeleteGuard: a locally re-edited file is not deleted by an
// incoming delete delta.
func TestApplyDeltasDeleteGuard(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x.md"), []byte("re-created locally\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sent := map[string]string{"x.md": contentHash([]byte("old\n"))} // changed since send
	deltas := []syncproto.Delta{{Path: "x.md", Op: "delete"}}

	if _, err := applyDeltas(dir, deltas, sent); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "x.md")); err != nil {
		t.Error("a locally re-edited file must survive an incoming delete")
	}
}
