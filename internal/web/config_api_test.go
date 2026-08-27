// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func cfgServer(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "n.md"), []byte("---\nid: n\ntype: note\nwhen: 2026-01-01\n---\n# N\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedIndex(t, dir)
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
	t.Setenv("MESH_ALLOWED_ENDPOINT_HOSTS", "e.example.com")
	s, dir := cfgServer(t)
	h := s.Handler()

	// PUT a weight + endpoint.
	code, _ := doJSON(t, h, "PUT", "/api/config", `{"updates":{"retrieval.weight_fts":"0.5","embedding.endpoint":"https://e.example.com/v1"}}`)
	if code != 200 {
		t.Fatalf("PUT config = %d, want 200", code)
	}
	// It must land in config.toml on disk.
	b, err := os.ReadFile(filepath.Join(dir, ".mesh", "config.toml"))
	if err != nil || !strings.Contains(string(b), "weight_fts = 0.5") || !strings.Contains(string(b), "https://e.example.com/v1") {
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

func TestConfigHandlersHonorCanceledRequestContext(t *testing.T) {
	t.Run("GET", func(t *testing.T) {
		s, _ := cfgServer(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		req := httptest.NewRequest(http.MethodGet, "/api/config", nil).WithContext(ctx)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("cancelled GET /api/config = %d, want 500", rec.Code)
		}
	})

	t.Run("PUT", func(t *testing.T) {
		s, dir := cfgServer(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		req := httptest.NewRequest(http.MethodPut, "/api/config", strings.NewReader(`{"updates":{"retrieval.weight_fts":"0.5"}}`)).WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("cancelled PUT /api/config = %d, want 500", rec.Code)
		}
		if _, err := os.Stat(filepath.Join(dir, ".mesh", "config.toml")); !os.IsNotExist(err) {
			t.Fatalf("cancelled PUT wrote config.toml: %v", err)
		}
	})

}

func TestConfigUpdateWaitHonorsCancellation(t *testing.T) {
	s, _ := cfgServer(t)
	release, err := s.acquireConfigUpdate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := s.acquireConfigUpdate(ctx)
		errCh <- err
	}()
	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waiting config update cancellation = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("waiting config update ignored cancellation")
	}
	release()
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

// TestConfigEndpointURLsMustBeHTTPSAndAllowListed pins the three operator-settable URLs
// that are DESTINATIONS FOR CREDENTIALED TRAFFIC. secret_bridge.base_url receives the
// Dockyard API key in X-API-Key (the long-lived credential the whole capability design
// exists to keep out of Mesh), and rerank/embedding endpoints receive candidate note
// BODIES plus an Authorization bearer. All three are writable by a team admin over
// PUT /api/config and go live immediately, so an unvalidated write is credential
// exfiltration and vault-content egress in one request. They must be https, must name a
// host on the operator allow-list (which no HTTP surface can write, same shape as the
// key_env allow-list), and an unparseable URL must fail closed.
func TestConfigEndpointURLsMustBeHTTPSAndAllowListed(t *testing.T) {
	keys := []string{"embedding.endpoint", "rerank.endpoint", "secret_bridge.base_url"}
	cases := []struct {
		name     string
		allow    string
		value    string
		wantCode int
	}{
		{"plain http is refused", "llm.example.com", "http://llm.example.com/v1", http.StatusBadRequest},
		{"a host off the allow-list is refused", "llm.example.com", "https://evil.tld/v1", http.StatusBadRequest},
		{"an unparseable url fails closed", "llm.example.com", "https://exa mple.com/v1", http.StatusBadRequest},
		{"an empty allow-list refuses every host", "", "https://llm.example.com/v1", http.StatusBadRequest},
		{"an allow-listed https endpoint is accepted", "llm.example.com", "https://llm.example.com/v1", http.StatusOK},
	}
	for _, key := range keys {
		for _, tc := range cases {
			t.Run(key+"/"+tc.name, func(t *testing.T) {
				t.Setenv("MESH_ALLOWED_ENDPOINT_HOSTS", tc.allow)
				s, dir := cfgServer(t)
				body := fmt.Sprintf(`{"updates":{%q:%q}}`, key, tc.value)
				code, _ := doJSON(t, s.Handler(), "PUT", "/api/config", body)
				if code != tc.wantCode {
					t.Fatalf("PUT %s=%q = %d, want %d", key, tc.value, code, tc.wantCode)
				}
				// A refusal leaves config.toml unwritten entirely, so a missing file is
				// simply "not persisted".
				b, err := os.ReadFile(filepath.Join(dir, ".mesh", "config.toml"))
				if err != nil && !os.IsNotExist(err) {
					t.Fatalf("read config.toml: %v", err)
				}
				want := tc.wantCode == http.StatusOK
				if got := strings.Contains(string(b), tc.value); got != want {
					t.Fatalf("config.toml persisted %q = %v, want %v\n%s", tc.value, got, want, b)
				}
			})
		}
	}
}

// PUT /api/config is a read-modify-write of the whole file: it loads config.toml, sets
// the one field the caller sent, and writes the entire struct back. It used to discard
// the load error, so any read failure other than "file does not exist" handed the
// handler a ZERO Config and the save then persisted that: one PUT of one weight wiped
// both endpoint URLs, the secret-bridge URL and every other weight, and answered 200.
// The save writes a temp file and renames it, so it lands even when the file at that
// path cannot be read, which is what made the loss total rather than a failed write.
func TestConfigPutRefusesWhenLoadFails(t *testing.T) {
	t.Setenv("MESH_ALLOWED_ENDPOINT_HOSTS", "e.example.com")
	s, dir := cfgServer(t)
	h := s.Handler()

	seed := `{"updates":{"embedding.endpoint":"https://e.example.com/v1","retrieval.weight_fts":"0.5"}}`
	if code, _ := doJSON(t, h, "PUT", "/api/config", seed); code != http.StatusOK {
		t.Fatalf("seed PUT = %d, want 200", code)
	}
	// Make the read fail with something that is not os.IsNotExist. A self-referential
	// symlink does it portably (ELOOP) and without depending on file modes, which root
	// ignores.
	path := filepath.Join(dir, ".mesh", "config.toml")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path, path); err != nil {
		t.Fatal(err)
	}

	code, _ := doJSON(t, h, "PUT", "/api/config", `{"updates":{"retrieval.weight_graph":"0.3"}}`)
	if code != http.StatusInternalServerError {
		t.Errorf("PUT over an unreadable config.toml = %d, want 500", code)
	}
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		b, _ := os.ReadFile(path)
		t.Fatalf("the handler rewrote config.toml from a Config it never managed to read:\n%s", b)
	}
}
