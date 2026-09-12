// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/bright-interaction/mesh/internal/index"
)

func refreshFixture(t *testing.T) (*Server, *index.Store, string) {
	t.Helper()
	t.Setenv("MESH_RERANK_AGENT", "http") // never use the developer's private opt-in
	root := t.TempDir()
	path := filepath.Join(root, "note.md")
	if err := os.WriteFile(path, []byte("---\nid: note\ntype: note\ntitle: Original\n---\nOriginal bytes.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	owner, err := index.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	if _, err := index.Reindex(owner, root); err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	if err := srv.WaitReady(); err != nil {
		t.Fatal(err)
	}
	return srv, owner, path
}

func setRefreshLabel(t *testing.T, owner *index.Store, label string) {
	t.Helper()
	if err := owner.Write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE nodes SET label=? WHERE id='note:note'`, label)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestReaderRefreshReusesUnchangedAndCoalescesQueuedWork(t *testing.T) {
	srv, owner, _ := refreshFixture(t)
	g, r := srv.snapshot()
	for i := 0; i < 3; i++ {
		rec, err := srv.refresh()
		if err != nil || rec.Reindexed || rec.Any() {
			t.Fatalf("unchanged refresh: %+v %v", rec, err)
		}
		g2, r2 := srv.snapshot()
		if g2 != g || r2 != r {
			t.Fatal("unchanged refresh rebuilt reader state")
		}
	}
	// A node/code-only commit has no note-hash changes, but still invalidates.
	setRefreshLabel(t, owner, "Updated code metadata")
	var builds atomic.Int32
	srv.beforeReaderInstall = func() { builds.Add(1) }
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := srv.refresh(); errs <- err }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if builds.Load() != 1 {
		t.Fatalf("queued callers performed %d builds, want one", builds.Load())
	}
	g2, r2 := srv.snapshot()
	if g2 == g || r2 == r {
		t.Fatal("changed database reused stale reader state")
	}
	if n, ok := g2.Node("note:note"); !ok || n.Label != "Updated code metadata" {
		t.Fatalf("stale node: %+v", n)
	}
}

func TestReaderRefreshInvalidatesOnNotesVectorsAndSettings(t *testing.T) {
	srv, owner, path := refreshFixture(t)
	check := func(change func()) {
		t.Helper()
		g, r := srv.snapshot()
		change()
		if _, err := srv.refresh(); err != nil {
			t.Fatal(err)
		}
		g2, r2 := srv.snapshot()
		if g2 == g || r2 == r {
			t.Fatal("changed input did not rebuild reader")
		}
		if _, err := srv.refresh(); err != nil {
			t.Fatal(err)
		}
		g3, r3 := srv.snapshot()
		if g3 != g2 || r3 != r2 {
			t.Fatal("stable changed input was not cached")
		}
	}
	check(func() {
		if err := os.WriteFile(path, []byte("---\nid: note\ntype: note\ntitle: Revised\n---\nRevised bytes.\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := index.Reindex(owner, filepath.Dir(path)); err != nil {
			t.Fatal(err)
		}
	})
	check(func() {
		hash, err := owner.NoteRetrievalHash("note:note")
		if err != nil {
			t.Fatal(err)
		}
		if err := owner.ReplaceVectors("freshness-test", []index.VectorRow{{NodeID: "note:note", Vec: []float32{1, 0}, NoteHash: hash}}); err != nil {
			t.Fatal(err)
		}
	})
	configPath := filepath.Join(filepath.Dir(path), ".mesh", "config.toml")
	check(func() {
		if err := os.WriteFile(configPath, []byte("[retrieval]\nweight_fts = 0.7\n"), 0600); err != nil {
			t.Fatal(err)
		}
	})
	_, r := srv.snapshot()
	if fts, _, _ := r.Weights(); fts != 0.7 {
		t.Fatalf("new file weights not consumed: %v", fts)
	}
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	check(func() {
		if err := os.WriteFile(configPath, []byte("[retrieval]\nweight_fts = 0.8\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(configPath, info.ModTime(), info.ModTime()); err != nil {
			t.Fatal(err)
		}
	})
	check(func() { t.Setenv("MESH_WEIGHT_FTS", "0.9") })
	_, r = srv.snapshot()
	if fts, _, _ := r.Weights(); fts != 0.9 {
		t.Fatalf("new environment weights not consumed: %v", fts)
	}
}

func TestReaderRefreshDoesNotBlessCommitDuringConstruction(t *testing.T) {
	srv, owner, _ := refreshFixture(t)
	setRefreshLabel(t, owner, "First")
	srv.beforeReaderInstall = func() { setRefreshLabel(t, owner, "Second") }
	if _, err := srv.refresh(); err != nil {
		t.Fatal(err)
	}
	if srv.viewReusable {
		t.Fatal("post-build version blessed a graph loaded before a commit")
	}
	g, _ := srv.snapshot()
	if n, _ := g.Node("note:note"); n.Label != "First" {
		t.Fatal("test did not retain pre-commit graph")
	}
	srv.beforeReaderInstall = nil
	if _, err := srv.refresh(); err != nil {
		t.Fatal(err)
	}
	g, _ = srv.snapshot()
	if n, _ := g.Node("note:note"); n.Label != "Second" {
		t.Fatal("next refresh skipped concurrent commit")
	}
	if !srv.viewReusable {
		t.Fatal("stable subsequent build was not reusable")
	}
}

func TestReaderRefreshUsesConsumedConfigAcrossConcurrentEdit(t *testing.T) {
	srv, owner, path := refreshFixture(t)
	configPath := filepath.Join(filepath.Dir(path), ".mesh", "config.toml")
	write := func(value string) {
		if err := os.WriteFile(configPath, []byte("[retrieval]\nweight_fts = "+value+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("0.4")
	setRefreshLabel(t, owner, "Force graph load")
	srv.beforeReaderInstall = func() { write("0.8") }
	if _, err := srv.refresh(); err != nil {
		t.Fatal(err)
	}
	_, r := srv.snapshot()
	if fts, _, _ := r.Weights(); fts != 0.4 {
		t.Fatalf("constructor reread racing config: %v", fts)
	}
	srv.beforeReaderInstall = nil
	if _, err := srv.refresh(); err != nil {
		t.Fatal(err)
	}
	_, r = srv.snapshot()
	if fts, _, _ := r.Weights(); fts != 0.8 {
		t.Fatalf("new file did not invalidate consumed config: %v", fts)
	}
}

func TestReaderRefreshProbeFailureAndCancellationDoNotReuse(t *testing.T) {
	srv, _, _ := refreshFixture(t)
	g, _ := srv.snapshot()
	if err := srv.changeMonitor.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.refresh(); err != nil {
		t.Fatal(err)
	}
	g2, _ := srv.snapshot()
	if g2 == g || srv.viewReusable {
		t.Fatal("failed monitor was treated as proof of unchanged state")
	}
	if _, err := srv.refresh(); err != nil {
		t.Fatal(err)
	}
	if !srv.viewReusable {
		t.Fatal("new monitor never established its own baseline")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	g, r := srv.snapshot()
	if _, err := srv.refreshContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled refresh: %v", err)
	}
	g2, r2 := srv.snapshot()
	if g != g2 || r != r2 {
		t.Fatal("canceled refresh published state")
	}
}

func TestAcknowledgementRetainsVersionProofAndPrimesOrdinaryRefresh(t *testing.T) {
	srv, _, path := refreshFixture(t)
	g, _ := srv.snapshot()
	if err := srv.awaitOwnerIndexed(context.Background(), "note", path); err != nil {
		t.Fatal(err)
	}
	g2, r2 := srv.snapshot()
	if g == g2 {
		t.Fatal("acknowledgement skipped its exact-version graph load")
	}
	if _, err := srv.refresh(); err != nil {
		t.Fatal(err)
	}
	g3, r3 := srv.snapshot()
	if g3 != g2 || r3 != r2 {
		t.Fatal("ordinary refresh duplicated the acknowledged snapshot")
	}
}

func BenchmarkReaderRefresh(b *testing.B) {
	b.Setenv("MESH_RERANK_AGENT", "http")
	root := b.TempDir()
	owner, err := index.Open(root)
	if err != nil {
		b.Fatal(err)
	}
	defer owner.Close()
	if err := owner.Write(func(tx *sql.Tx) error {
		stmt, err := tx.Prepare(`INSERT INTO nodes(id,kind,label,note_id,attrs) VALUES(?, 'note', ?, ?, ?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for i := 0; i < 3000; i++ {
			id := fmt.Sprintf("benchmark-%d", i)
			if _, err := stmt.Exec("note:"+id, "Reader refresh benchmark", id, `{"body":"Repeated graph ranker construction allocates unnecessary state"}`); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		b.Fatal(err)
	}
	srv, err := NewServer(root)
	if err != nil {
		b.Fatal(err)
	}
	defer srv.Close()
	if err := srv.WaitReady(); err != nil {
		b.Fatal(err)
	}
	for _, reuse := range []bool{false, true} {
		b.Run(map[bool]string{false: "forced_rebuild", true: "unchanged_reuse"}[reuse], func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if !reuse {
					srv.viewReusable = false
				}
				if _, err := srv.refresh(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
