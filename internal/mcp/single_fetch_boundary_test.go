// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/index"
)

func singleFetchHTTP(t *testing.T, s *Server, ctx context.Context) response {
	t.Helper()
	batchE2EReady(s)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"mesh_fetch","arguments":{"id":"n0"}}}`
	r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body)).WithContext(ctx)
	w := httptest.NewRecorder()
	s.HandleHTTP(w, r)
	var reply response
	if err := json.Unmarshal(w.Body.Bytes(), &reply); err != nil {
		t.Fatal(err)
	}
	return reply
}

func assertSingleFetchUnavailable(t *testing.T, s *Server, reply response) {
	t.Helper()
	if reply.Error == nil || reply.Error.Code != codeInvalidParams || reply.Error.Message != "unknown note id" || reply.Result != nil {
		t.Fatal("single fetch did not give the same opaque denial as an unknown note")
	}
	if got, err := s.store.Metric("fetches"); err != nil || got != 0 {
		t.Fatalf("denied fetch counted as reuse: %d (%v)", got, err)
	}
}

func TestSingleFetchEnforcesCurrentScopeThroughHTTP(t *testing.T) {
	for name, body := range map[string]string{
		"restricted":   "---\nid: n0\ntype: note\nscope: [private]\n---\n# Private\nrestricted fixture bytes\n",
		"malformed":    "---\nid: n0\nscope: [private\n---\n# Private\nrestricted fixture bytes\n",
		"unterminated": "---\nid: n0\nscope: [public]\n# Private\nrestricted fixture bytes\n",
	} {
		t.Run(name, func(t *testing.T) {
			s := batchFixture(t, 1) // indexed while public; deliberately do not reindex
			if err := os.WriteFile(filepath.Join(s.vaultRoot, "n0.md"), []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			ctx := WithScopeFilter(context.Background(), &ScopeFilter{AllowedRead: map[string]bool{"public": true}})
			batch := batchCall(t, s, ctx, []batchFetchItem{{"n0", ""}}, 8000)
			if len(batch.Results) != 1 || batch.Results[0].Status != "unavailable" || batch.Results[0].Text != "" {
				t.Fatal("batch fetch did not deny the same changed file")
			}
			assertSingleFetchUnavailable(t, s, singleFetchHTTP(t, s, ctx))
		})
	}
}

func TestSingleFetchRequiresIndexedScopeToo(t *testing.T) {
	s := batchFixture(t, 1)
	path := filepath.Join(s.vaultRoot, "n0.md")
	private := "---\nid: n0\ntype: note\nscope: [private]\n---\n# Private\n"
	if err := os.WriteFile(path, []byte(private), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := index.ReindexFull(s.store, s.vaultRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(private, "[private]", "[public]")), 0600); err != nil {
		t.Fatal(err)
	}
	ctx := WithScopeFilter(context.Background(), &ScopeFilter{AllowedRead: map[string]bool{"public": true}})
	assertSingleFetchUnavailable(t, s, singleFetchHTTP(t, s, ctx))
}

func TestSingleFetchRefusesReplacedDirectoryThroughHTTP(t *testing.T) {
	s := batchFixture(t, 1)
	dir := filepath.Join(s.vaultRoot, "group")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(s.vaultRoot, "n0.md"), filepath.Join(dir, "n0.md")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := index.ReindexFull(s.store, s.vaultRoot); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "n0.md"), []byte("# Outside\nprivate directory fixture\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(dir, dir+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, dir); err != nil {
		t.Fatal(err)
	}
	assertSingleFetchUnavailable(t, s, singleFetchHTTP(t, s, context.Background()))
}

func TestSingleFetchPreservesWholeNoteSizeCompatibility(t *testing.T) {
	s := batchFixture(t, 1)
	path := filepath.Join(s.vaultRoot, "n0.md")
	body := "---\nid: n0\ntype: note\nscope: [public]\n---\n# Large\n" + strings.Repeat("x", batchFetchFileBytes)
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	ctx := WithScopeFilter(context.Background(), &ScopeFilter{AllowedRead: map[string]bool{"public": true}})
	result, rerr := s.toolFetch(ctx, json.RawMessage(`{"id":"n0"}`))
	if rerr != nil || rawContent(t, result) != body {
		t.Fatal("single fetch silently inherited the batch file cap or changed content")
	}
	batch := batchCall(t, s, ctx, []batchFetchItem{{"n0", ""}}, 8000)
	if len(batch.Results) != 1 || batch.Results[0].Status != "too_large" {
		t.Fatal("batch file cap changed")
	}
	if got, err := s.store.Metric("fetches"); err != nil || got != 1 {
		t.Fatalf("successful fetch attribution changed: %d (%v)", got, err)
	}
}

func TestSingleFetchRefusesReplacedSymlinkThroughHTTP(t *testing.T) {
	s := batchFixture(t, 1)
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(outside, []byte("---\nid: n0\nscope: [public]\n---\n# Outside\nnot in the vault\n"), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.vaultRoot, "n0.md")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	assertSingleFetchUnavailable(t, s, singleFetchHTTP(t, s, context.Background()))
}

func TestSingleFetchCancelledBeforeReadDoesNotAttribute(t *testing.T) {
	s := batchFixture(t, 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, rerr := s.toolFetch(ctx, json.RawMessage(`{"id":"n0"}`))
	if result != nil || rerr == nil {
		t.Fatal("cancelled single fetch returned content")
	}
	if got, err := s.store.Metric("fetches"); err != nil || got != 0 {
		t.Fatalf("cancelled fetch counted as reuse: %d (%v)", got, err)
	}
}
