// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package web

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func cfgServer(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "n.md"), []byte("---\nid: n\ntype: note\nwhen: 2026-01-01\n---\n# N\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := NewServer(dir)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, dir
}

func doJSON(t *testing.T, h http.Handler, method, path, body string) (int, map[string]any) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = bytes.NewReader([]byte(body))
	}
	req, _ := http.NewRequest(method, path, r)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	return rec.Code, m
}

func TestConfigRoundTrip(t *testing.T) {
	s, dir := cfgServer(t)
	h := s.Handler()

	// PUT a weight + endpoint.
	code, _ := doJSON(t, h, "PUT", "/api/config", `{"updates":{"retrieval.weight_fts":"0.5","embedding.endpoint":"http://e/v1"}}`)
	if code != 200 {
		t.Fatalf("PUT config = %d, want 200", code)
	}
	// It must land in config.toml on disk.
	b, err := os.ReadFile(filepath.Join(dir, ".mesh", "config.toml"))
	if err != nil || !strings.Contains(string(b), "weight_fts = 0.5") || !strings.Contains(string(b), "http://e/v1") {
		t.Fatalf("config.toml did not persist the update: %v\n%s", err, b)
	}
	// GET reflects it with source=file.
	code, got := doJSON(t, h, "GET", "/api/config", "")
	if code != 200 {
		t.Fatalf("GET config = %d", code)
	}
	fields, _ := got["fields"].([]any)
	found := false
	for _, fi := range fields {
		f := fi.(map[string]any)
		if f["key"] == "retrieval.weight_fts" {
			found = true
			if f["value"] != "0.5" || f["source"] != "file" || f["editable"] != true {
				t.Errorf("weight_fts = %+v, want value 0.5 source file editable true", f)
			}
		}
		// secrets must never appear: keyref fields hold the var NAME, never a key.
		if f["kind"] == "keyref" && strings.Contains(f["value"].(string), "sk-") {
			t.Errorf("keyref field leaked a secret-looking value: %v", f["value"])
		}
	}
	if !found {
		t.Fatal("weight_fts field missing from GET")
	}
}

func TestConfigEnvLock(t *testing.T) {
	s, _ := cfgServer(t)
	h := s.Handler()
	t.Setenv("MESH_WEIGHT_GRAPH", "0.7")

	// GET: the env-set field is source=env, not editable.
	_, got := doJSON(t, h, "GET", "/api/config", "")
	for _, fi := range got["fields"].([]any) {
		f := fi.(map[string]any)
		if f["key"] == "retrieval.weight_graph" {
			if f["source"] != "env" || f["editable"] != false || f["value"] != "0.7" {
				t.Errorf("env-locked field = %+v, want source env editable false value 0.7", f)
			}
		}
	}
	// PUT of an env-locked field is refused (409).
	code, _ := doJSON(t, h, "PUT", "/api/config", `{"updates":{"retrieval.weight_graph":"0.1"}}`)
	if code != http.StatusConflict {
		t.Errorf("PUT env-locked field = %d, want 409", code)
	}
}

func TestConfigValidationAndReindex(t *testing.T) {
	s, _ := cfgServer(t)
	h := s.Handler()
	// invalid blend (>1) is rejected.
	if code, _ := doJSON(t, h, "PUT", "/api/config", `{"updates":{"rerank.blend":"2"}}`); code != http.StatusBadRequest {
		t.Errorf("blend>1 = %d, want 400", code)
	}
	// unknown field rejected.
	if code, _ := doJSON(t, h, "PUT", "/api/config", `{"updates":{"nope.x":"1"}}`); code != http.StatusBadRequest {
		t.Errorf("unknown field = %d, want 400", code)
	}
	// reindex works.
	code, got := doJSON(t, h, "POST", "/api/reindex", "")
	if code != 200 || got["reindexed"] != true {
		t.Errorf("reindex = %d %+v", code, got)
	}
}
