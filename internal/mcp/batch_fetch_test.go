// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/retrieve"
)

func batchFixture(t testing.TB, count int) *Server {
	t.Helper()
	root := t.TempDir()
	for i := 0; i < count; i++ {
		body := fmt.Sprintf("---\nid: n%d\ntype: note\nscope: [public]\nstatus: retired\ndont: Do not deploy.\n---\n# Note\n## Parent\nparent answer\n### Child\nchild answer\n## Other\nother answer\n", i)
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("n%d.md", i)), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	store, err := index.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = index.ReindexFull(store, root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return &Server{vaultRoot: root, store: store}
}

func batchCall(t *testing.T, s *Server, ctx context.Context, items []batchFetchItem, budget int) batchFetchResponse {
	t.Helper()
	args, _ := json.Marshal(map[string]any{"items": items, "budget": budget})
	out, err := s.toolFetchMany(ctx, args)
	if err != nil {
		t.Fatal(err)
	}
	var response batchFetchResponse
	if err := json.Unmarshal([]byte(rawContent(t, out)), &response); err != nil {
		t.Fatal(err)
	}
	wire, _ := json.Marshal(out)
	actual := retrieve.EstimateTokens(string(wire))
	if actual > budget || response.Tokens < actual || response.Tokens > budget {
		t.Fatalf("wire=%d receipt=%d budget=%d", actual, response.Tokens, budget)
	}
	return response
}

func TestBatchFetchWorkerSettingAppliesToNextRequest(t *testing.T) {
	t.Setenv("MESH_FETCH_WORKERS", "")
	s := batchFixture(t, 4)
	items := []batchFetchItem{{"n0", "parent"}, {"n1", "parent"}, {"n2", "parent"}, {"n3", "parent"}}
	if r := batchCall(t, s, context.Background(), items, 8000); r.Workers != 2 {
		t.Fatal("default must use two workers")
	}
	config := filepath.Join(s.store.MeshDir(), "config.toml")
	if err := os.WriteFile(config, []byte("[retrieval]\nfetch_workers = 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if r := batchCall(t, s, context.Background(), items, 8000); r.Workers != 1 {
		t.Fatal("file setting was not applied")
	}
	if err := os.WriteFile(config, []byte("[retrieval]\nfetch_workers = 4\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if r := batchCall(t, s, context.Background(), items, 8000); r.Workers != 4 {
		t.Fatal("changed file setting requires an unwanted restart")
	}
	t.Setenv("MESH_FETCH_WORKERS", "16")
	if r := batchCall(t, s, context.Background(), items, 8000); r.Workers != 4 {
		t.Fatal("worker count must not exceed unique notes")
	}
	t.Setenv("MESH_FETCH_WORKERS", "1")
	if r := batchCall(t, s, context.Background(), items, 8000); r.Workers != 1 {
		t.Fatal("environment did not override file")
	}
	for _, bad := range []string{"0", "17", "-1", "not-an-integer"} {
		t.Setenv("MESH_FETCH_WORKERS", bad)
		if out, err := s.toolFetchMany(context.Background(), json.RawMessage(`{"items":[{"id":"n0"}]}`)); out != nil || err == nil {
			t.Fatal("invalid worker setting accepted")
		}
	}
}

func TestBatchFetchCombinesDeduplicatesAndAttributes(t *testing.T) {
	s := batchFixture(t, 2)
	ctx := context.Background()
	s.rememberSearch(ctx, []retrieve.Card{{NoteID: "n1"}, {NoteID: "n0"}}, retrieve.Economics{})
	r := batchCall(t, s, ctx, []batchFetchItem{{"n0", "parent"}, {"n1", "other"}, {"n0", "child"}, {"n0", "parent"}}, 8000)
	if len(r.Results) != 2 || len(r.Omitted) != 0 || r.Results[0].ID != "n0" || !reflect.DeepEqual(r.Results[0].Indices, []int{0, 2, 3}) {
		t.Fatalf("bad grouping: %+v", r)
	}
	text := r.Results[0].Text
	if strings.Count(text, "child answer") != 1 || strings.Count(text, sectionContextNotice) != 1 || !strings.Contains(text, "Do not deploy.") {
		t.Fatal("duplicated sections/context or lost safety warning")
	}
	for metric, want := range map[string]int64{"fetches": 2, "fetch:n0": 1, "fetch:n1": 1, "retrieval:selected_rank:2": 1, "retrieval:selected_rank:1": 0} {
		got, err := s.store.Metric(metric)
		if err != nil || got != want {
			t.Fatalf("%s=%d want %d err=%v", metric, got, want, err)
		}
	}
	full := batchCall(t, s, ctx, []batchFetchItem{{"n0", "child"}, {"n0", ""}}, 8000)
	original, err := os.ReadFile(filepath.Join(s.vaultRoot, "n0.md"))
	if err != nil {
		t.Fatal(err)
	}
	if len(full.Results) != 1 || full.Results[0].Text != string(original) {
		t.Fatal("explicit full note must subsume sections")
	}
}

func TestBatchFetchBudgetNeverSlicesSafety(t *testing.T) {
	s := batchFixture(t, 4)
	items := []batchFetchItem{{"n0", "parent"}, {"n1", "parent"}, {"n2", "parent"}, {"n3", "parent"}}
	for _, budget := range []int{256, 350, 500, 8000} {
		r := batchCall(t, s, context.Background(), items, budget)
		covered := map[int]bool{}
		for _, result := range r.Results {
			for _, i := range result.Indices {
				if covered[i] {
					t.Fatal("duplicate index")
				}
				covered[i] = true
			}
			if result.Status == "ok" && (!strings.Contains(result.Text, "Do not deploy.") || !strings.Contains(result.Text, "child answer")) {
				t.Fatal("budget sliced content")
			}
		}
		for _, i := range r.Omitted {
			if covered[i] {
				t.Fatal("included index also omitted")
			}
			covered[i] = true
		}
		if len(covered) != len(items) {
			t.Fatal("lost input bookkeeping")
		}
	}
	// A large result is not reuse merely because workers fetched it.
	root := s.vaultRoot
	large := "---\nid: n0\ntype: note\nscope: [public]\n---\n# Large\n" + strings.Repeat("expensive distinct words ", 10000)
	if err := os.WriteFile(filepath.Join(root, "n0.md"), []byte(large), 0600); err != nil {
		t.Fatal(err)
	}
	before, _ := s.store.Metric("fetch:n0")
	r := batchCall(t, s, context.Background(), []batchFetchItem{{"n0", ""}, {"n1", "other"}}, 256)
	after, _ := s.store.Metric("fetch:n0")
	if after != before {
		t.Fatal("too-large or omitted note counted as reused")
	}
	if len(r.Results) == 0 && len(r.Omitted) == 0 {
		t.Fatal("empty receipt")
	}
	// This one is a successful read under the byte cap, omitted ONLY by tokens.
	large = "---\nid: n0\ntype: note\nscope: [public]\n---\n# Large\n" + strings.Repeat("expensive distinct words ", 500)
	if err := os.WriteFile(filepath.Join(root, "n0.md"), []byte(large), 0600); err != nil {
		t.Fatal(err)
	}
	if result := s.fetchBatchGroup(context.Background(), batchFetchGroup{ID: "n0", Anchors: []string{""}}); result.Status != "ok" {
		t.Fatal("fixture must be a successful read")
	}
	before, _ = s.store.Metric("fetch:n0")
	r = batchCall(t, s, context.Background(), []batchFetchItem{{"n0", ""}, {"n1", "other"}}, 256)
	after, _ = s.store.Metric("fetch:n0")
	if after != before || len(r.Omitted) == 0 || r.Omitted[0] != 0 {
		t.Fatal("budget-only omission was counted or not reported")
	}
}

func TestBatchFetchAuthorizationProvenanceAndFailures(t *testing.T) {
	s := batchFixture(t, 2)
	root := s.vaultRoot
	files := map[string]string{
		"secret.md": "---\nid: secret\ntype: note\nscope: [private]\n---\n# PRIVATE CONTENT\n",
		"import.md": "---\nid: imported\ntype: note\nscope: [public]\nsource: import:test\ndont: Do not deploy.\n---\n## Detail\nanswer\n",
	}
	for path, body := range files {
		if err := os.WriteFile(filepath.Join(root, path), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := index.ReindexFull(s.store, root); err != nil {
		t.Fatal(err)
	}
	ctx := WithScopeFilter(context.Background(), &ScopeFilter{AllowedRead: map[string]bool{"public": true}})
	r := batchCall(t, s, ctx, []batchFetchItem{{"secret", ""}, {"missing", ""}, {"imported", "detail"}, {"n0", "missing"}}, 8000)
	if r.Results[0].Status != r.Results[1].Status || r.Results[0].Text != "" || r.Results[3].Status == "ok" {
		t.Fatal("denied/missing note or anchor did not fail closed")
	}
	text := r.Results[2].Text
	if !strings.HasPrefix(text, untrustedOpenPrefix) || !strings.HasSuffix(text, untrustedClose) || !strings.Contains(text, sectionContextNotice) {
		t.Fatal("imported batch context lost trust wrapper")
	}
	// Replace indexed public file with an in-vault private symlink, without reindex.
	if err := os.Rename(filepath.Join(root, "n0.md"), filepath.Join(root, "n0.saved")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("secret.md", filepath.Join(root, "n0.md")); err != nil {
		t.Fatal(err)
	}
	r = batchCall(t, s, ctx, []batchFetchItem{{"n0", ""}}, 8000)
	if r.Results[0].Status == "ok" || strings.Contains(r.Results[0].Text, "PRIVATE") {
		t.Fatal("symlink bypassed indexed scope")
	}
	if err := os.WriteFile(filepath.Join(root, "n1.md"), []byte("---\nid: n1\ntype: note\nscope: [private]\n---\n# PRIVATE CHANGED\n"), 0600); err != nil {
		t.Fatal(err)
	}
	r = batchCall(t, s, ctx, []batchFetchItem{{"n1", ""}}, 8000)
	if r.Results[0].Status == "ok" || r.Results[0].Text != "" {
		t.Fatal("stale index exposed newly private file")
	}
	for _, raw := range []string{`{}`, `{"items":[]}`, `{"items":[{"id":"n1"}],"budget":1}`, `{"items":[{"id":"n1"}],"budget":32001}`, `{"items":[{"id":3}]}`} {
		if out, err := s.toolFetchMany(ctx, json.RawMessage(raw)); out != nil || err == nil {
			t.Fatalf("invalid request accepted: %s", raw)
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if out, err := s.toolFetchMany(canceled, json.RawMessage(`{"items":[{"id":"n1"}]}`)); out != nil || err == nil {
		t.Fatal("canceled batch returned data")
	}
}

func TestBatchFetchWorkersJoinOnSuccessAndErrors(t *testing.T) {
	groups := make([]batchFetchGroup, 16)
	for i := range groups {
		groups[i] = batchFetchGroup{ID: fmt.Sprint(i)}
	}
	for _, workers := range []int{1, 2, 4, 8} {
		for pass := 0; pass < 25; pass++ {
			var active, peak, calls atomic.Int64
			results, err := runBatchFetch(context.Background(), groups, workers, func(ctx context.Context, g batchFetchGroup) batchFetchResult {
				n := active.Add(1)
				defer active.Add(-1)
				for {
					old := peak.Load()
					if n <= old || peak.CompareAndSwap(old, n) {
						break
					}
				}
				calls.Add(1)
				return batchFetchResult{ID: g.ID, Status: "unavailable"}
			})
			if err != nil || active.Load() != 0 || calls.Load() != 16 || peak.Load() > int64(workers) {
				t.Fatal("worker cap/join violated")
			}
			for i, r := range results {
				if r.ID != groups[i].ID {
					t.Fatal("completion reordered results")
				}
			}
		}
	}
}

func TestBatchFetchCancellationWaitsForInFlightWorkers(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(fmt.Sprint(deadline), func(t *testing.T) {
			base, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx := base
			if deadline {
				var stop context.CancelFunc
				ctx, stop = context.WithTimeout(base, 100*time.Millisecond)
				defer stop()
			}
			entered := make(chan struct{}, 4)
			release := make(chan struct{})
			released := false
			defer func() {
				if !released {
					close(release)
				}
			}()
			var active, calls atomic.Int64
			done := make(chan error, 1)
			go func() {
				_, err := runBatchFetch(ctx, make([]batchFetchGroup, 16), 4, func(ctx context.Context, g batchFetchGroup) batchFetchResult {
					active.Add(1)
					defer active.Add(-1)
					calls.Add(1)
					entered <- struct{}{}
					<-release // models a filesystem operation that cannot be canceled mid-call
					return batchFetchResult{Status: "ok"}
				})
				done <- err
			}()
			for i := 0; i < 4; i++ {
				select {
				case <-entered:
				case <-time.After(2 * time.Second):
					t.Fatal("workers did not start")
				}
			}
			if deadline {
				<-ctx.Done()
			} else {
				cancel()
			}
			select {
			case <-done:
				t.Fatal("returned with in-flight workers")
			default:
			}
			close(release)
			released = true
			select {
			case err := <-done:
				if err == nil || active.Load() != 0 || calls.Load() != 4 {
					t.Fatal("cancellation did not join or started queued work")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("workers failed to exit")
			}
		})
	}
}

func TestBatchFetchReadBoundsAndDirectories(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "notes"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "notes", "ok.md"), []byte("ok"), 0600); err != nil {
		t.Fatal(err)
	}
	if body, err := readBatchFile(context.Background(), root, "notes/ok.md"); err != nil || string(body) != "ok" {
		t.Fatal(err)
	}
	if err := os.Symlink("notes", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"link/ok.md", "../escape", "notes"} {
		if _, err := readBatchFile(context.Background(), root, path); err == nil {
			t.Fatalf("unsafe path accepted: %s", path)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "big.md"), make([]byte, batchFetchFileBytes+1), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readBatchFile(context.Background(), root, "big.md"); err != errBatchFileTooLarge {
		t.Fatal("size cap failed")
	}
}

func TestBatchFetchSpanUnionDoesNotDropQuotedSections(t *testing.T) {
	s := batchFixture(t, 1)
	body := "---\nid: n0\ntype: note\n---\n## Example\n```\n## Real\nanswer\n```\n## Real\nanswer\n## Åtgärder\nlast answer\n"
	if err := os.WriteFile(filepath.Join(s.vaultRoot, "n0.md"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	r := batchCall(t, s, context.Background(), []batchFetchItem{{"n0", "example"}, {"n0", "real"}, {"n0", "atgarder"}, {"n0", "tg-rder"}}, 8000)
	if len(r.Results) != 1 || r.Results[0].Status != "ok" {
		t.Fatalf("bad result: %+v", r)
	}
	if strings.Count(r.Results[0].Text, "answer") != 3 {
		t.Fatal("lost real section or duplicated aliased span")
	}
}

func BenchmarkBatchFetchWarmReads(b *testing.B) {
	s := batchFixture(b, 16)
	groups := make([]batchFetchGroup, 16)
	for i := range groups {
		groups[i] = batchFetchGroup{ID: fmt.Sprintf("n%d", i), Anchors: []string{"parent"}}
	}
	for _, workers := range []int{1, 2, 4, 8} {
		b.Run(fmt.Sprint(workers), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				results, err := runBatchFetch(context.Background(), groups, workers, s.fetchBatchGroup)
				if err != nil {
					b.Fatal(err)
				}
				for _, r := range results {
					if r.Status != "ok" {
						b.Fatal(r.Status)
					}
				}
			}
		})
	}
}
