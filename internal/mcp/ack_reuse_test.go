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
	"time"

	"github.com/bright-interaction/mesh/internal/index"
)

func TestAcknowledgementReusesVerifiedSnapshot(t *testing.T) {
	srv, _, path := refreshFixture(t)
	g, r := srv.snapshot()
	if !srv.viewReusable {
		t.Fatal("fixture did not establish a verified reader snapshot")
	}
	builds := 0
	srv.beforeReaderInstall = func() { builds++ }
	if err := srv.awaitOwnerIndexed(context.Background(), "note", path); err != nil {
		t.Fatal(err)
	}
	g2, r2 := srv.snapshot()
	if builds != 0 || g != g2 || r != r2 {
		t.Fatalf("already-published note rebuilt graph/retriever %d times", builds)
	}
}

func TestAcknowledgementReusesFallbackAndBookkeepingSnapshots(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		t.Run(fmt.Sprint("fallback=", fallback), func(t *testing.T) {
			srv, owner, path := refreshFixture(t)
			if fallback {
				if err := owner.Write(func(tx *sql.Tx) error {
					_, err := tx.Exec(`DROP TRIGGER _mesh_rr_v1_nodes_update`)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if _, err := srv.refresh(); err != nil {
					t.Fatal(err)
				}
				if srv.viewVersion.Tracked {
					t.Fatal("incomplete tracker did not select conservative fallback")
				}
			} else {
				refreshBookkeeping(t, owner)
			}
			g, r := srv.snapshot()
			if err := srv.awaitOwnerIndexed(context.Background(), "note", path); err != nil {
				t.Fatal(err)
			}
			g2, r2 := srv.snapshot()
			if g != g2 || r != r2 {
				t.Fatal("unchanged snapshot rebuilt")
			}
		})
	}
}

func TestAcknowledgementCoalescesQueuedBuilds(t *testing.T) {
	srv, owner, path := refreshFixture(t)
	setRefreshLabel(t, owner, "Published graph change")
	var builds atomic.Int32
	srv.beforeReaderInstall = func() { builds.Add(1) }
	var workers sync.WaitGroup
	errs := make(chan error, 12)
	for i := 0; i < cap(errs); i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			errs <- srv.awaitOwnerIndexed(context.Background(), "note", path)
		}()
	}
	workers.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if builds.Load() != 1 {
		t.Fatalf("queued acknowledgements performed %d builds, want 1", builds.Load())
	}
	g, _ := srv.snapshot()
	if node, _ := g.Node("note:note"); node.Label != "Published graph change" {
		t.Fatal("coalescing hid a real graph change")
	}
}

func TestAcknowledgementReuseStillRequiresExactNoteVersion(t *testing.T) {
	srv, _, _ := refreshFixture(t)
	hash := srv.viewHashes["note.md"]
	if hash == "" {
		t.Fatal("fixture has no note hash")
	}
	g, r := srv.snapshot()
	for _, args := range [][3]string{{"absent", "note.md", hash}, {"note", "other.md", hash}, {"note", "note.md", "old-hash"}} {
		matched, err := srv.refreshAtNoteVersion(context.Background(), args[0], args[1], args[2])
		if err != nil || matched {
			t.Fatalf("mismatched version was acknowledged: %v %v", matched, err)
		}
	}
	g2, r2 := srv.snapshot()
	if g != g2 || r != r2 {
		t.Fatal("unindexed target rebuilt an unchanged snapshot")
	}
}

func TestAcknowledgementReuseRejectsRacesBetweenProofSamples(t *testing.T) {
	for _, change := range []string{"graph", "fallback-graph", "vectors", "note-hash", "monitor", "cancel"} {
		t.Run(change, func(t *testing.T) {
			srv, owner, _ := refreshFixture(t)
			if change == "fallback-graph" {
				if err := owner.Write(func(tx *sql.Tx) error {
					_, err := tx.Exec(`DROP TRIGGER _mesh_rr_v1_nodes_update`)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if _, err := srv.refresh(); err != nil {
					t.Fatal(err)
				}
				if srv.viewVersion.Tracked || !srv.viewReusable {
					t.Fatal("fixture did not establish a reusable fallback snapshot")
				}
			}
			g, r := srv.snapshot()
			hash := srv.viewHashes["note.md"]
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var once sync.Once
			srv.afterCachedNoteVersion = func() {
				once.Do(func() {
					switch change {
					case "graph", "fallback-graph":
						setRefreshLabel(t, owner, "Commit between proof samples")
					case "vectors":
						if err := owner.ReplaceVectors("ack-test", []index.VectorRow{{NodeID: "note:note", Vec: []float32{1, 0}, NoteHash: hash}}); err != nil {
							t.Fatal(err)
						}
					case "note-hash":
						if err := owner.Write(func(tx *sql.Tx) error {
							_, err := tx.Exec(`UPDATE notes SET retrieval_hash='new-version' WHERE id='note'`)
							return err
						}); err != nil {
							t.Fatal(err)
						}
					case "monitor":
						if err := srv.changeMonitor.Close(); err != nil {
							t.Fatal(err)
						}
					case "cancel":
						cancel()
					}
				})
			}
			matched, err := srv.refreshAtNoteVersion(ctx, "note", "note.md", hash)
			g2, r2 := srv.snapshot()
			switch change {
			case "cancel":
				if matched || !errors.Is(err, context.Canceled) || g != g2 || r != r2 {
					t.Fatalf("cancelled proof published a snapshot: matched=%v err=%v", matched, err)
				}
			case "note-hash":
				if matched || err != nil || g != g2 || r != r2 {
					t.Fatalf("concurrently superseded version was acknowledged: %v %v", matched, err)
				}
			default:
				if !matched || err != nil || g == g2 || r == r2 {
					t.Fatalf("changed/invalid proof did not rebuild: matched=%v err=%v", matched, err)
				}
				if change == "graph" || change == "fallback-graph" {
					if node, _ := g2.Node("note:note"); node.Label != "Commit between proof samples" {
						t.Fatal("rebuilt graph missed concurrent commit")
					}
				}
			}
		})
	}
}

func TestAcknowledgementReuseRechecksCurrentFile(t *testing.T) {
	srv, _, path := refreshFixture(t)
	srv.ownerIndexTimeout = 150 * time.Millisecond
	g, r := srv.snapshot()
	changed := false
	srv.afterCachedNoteVersion = func() {
		if changed {
			return
		}
		changed = true
		if err := os.WriteFile(path, []byte("---\nid: note\ntype: note\n---\nNot yet indexed.\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	err := srv.awaitOwnerIndexed(context.Background(), "note", path)
	if !changed || !errors.Is(err, ErrOwnerNotIndexing) {
		t.Fatalf("cached acknowledgement missed a newer file: cached=%v err=%v", changed, err)
	}
	g2, r2 := srv.snapshot()
	if g != g2 || r != r2 {
		t.Fatal("file-only change unnecessarily rebuilt the unchanged database snapshot")
	}
}

func TestAcknowledgementReuseRejectsQueryError(t *testing.T) {
	srv, _, _ := refreshFixture(t)
	g, r := srv.snapshot()
	hash := srv.viewHashes["note.md"]
	if err := srv.store.Close(); err != nil {
		t.Fatal(err)
	}
	matched, err := srv.refreshAtNoteVersion(context.Background(), "note", "note.md", hash)
	g2, r2 := srv.snapshot()
	if matched || err == nil || g != g2 || r != r2 {
		t.Fatalf("failed note query was acknowledged: matched=%v err=%v", matched, err)
	}
}

func TestAcknowledgementReuseRejectsDatabaseReplacement(t *testing.T) {
	srv, _, _ := refreshFixture(t)
	srv.reloadMu.Lock()
	defer srv.reloadMu.Unlock()
	ctx := context.Background()
	before, valid := srv.readerVersion(ctx)
	if !valid || !srv.viewReusable {
		t.Fatal("fixture has no reusable snapshot")
	}
	monitor := srv.changeMonitor
	dbPath := filepath.Join(srv.store.MeshDir(), "mesh.db")
	retainedPath := dbPath + ".retained-test"
	var replaced bool
	defer func() {
		// Restore the disposable fixture before SQLite closes/checkpoints it.
		if replaced {
			if err := os.Rename(retainedPath, dbPath); err != nil {
				t.Error(err)
			}
		}
	}()
	srv.afterCachedNoteVersion = func() {
		if err := os.Rename(dbPath, retainedPath); err != nil {
			t.Fatal(err)
		}
		replaced = true
		if err := os.WriteFile(dbPath, []byte("replacement identity only"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	matched, reused, err := srv.reuseAcknowledgement(ctx, before, valid, "note", "note.md", srv.viewHashes["note.md"])
	if matched || reused || err != nil || srv.viewReusable || srv.changeMonitor != monitor {
		t.Fatalf("replacement reused/reopened old proof: matched=%v reused=%v err=%v", matched, reused, err)
	}
	if _, valid := srv.readerVersion(ctx); valid || srv.changeMonitor != monitor {
		t.Fatal("replacement invalidation did not remain sticky")
	}
}

func TestAcknowledgementReuseInvalidatesConfiguration(t *testing.T) {
	srv, _, path := refreshFixture(t)
	g, r := srv.snapshot()
	config := filepath.Join(filepath.Dir(path), ".mesh", "config.toml")
	if err := os.WriteFile(config, []byte("[retrieval]\nweight_fts = 0.7\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := srv.awaitOwnerIndexed(context.Background(), "note", path); err != nil {
		t.Fatal(err)
	}
	g2, r2 := srv.snapshot()
	fts, _, _ := r2.Weights()
	if g == g2 || r == r2 || fts != 0.7 {
		t.Fatal("acknowledgement reused stale retrieval settings")
	}
	t.Setenv("MESH_WEIGHT_FTS", "0.8")
	if err := srv.awaitOwnerIndexed(context.Background(), "note", path); err != nil {
		t.Fatal(err)
	}
	_, r3 := srv.snapshot()
	fts, _, _ = r3.Weights()
	if r3 == r2 || fts != 0.8 {
		t.Fatal("acknowledgement ignored changed environment inputs")
	}
}

func BenchmarkAcknowledgementSnapshot(b *testing.B) {
	b.Setenv("MESH_RERANK_AGENT", "http")
	root := b.TempDir()
	path := filepath.Join(root, "note.md")
	if err := os.WriteFile(path, []byte("---\nid: note\ntype: note\n---\nPublished benchmark note.\n"), 0600); err != nil {
		b.Fatal(err)
	}
	owner, err := index.Open(root)
	if err != nil {
		b.Fatal(err)
	}
	defer owner.Close()
	if _, err := index.Reindex(owner, root); err != nil {
		b.Fatal(err)
	}
	if err := owner.Write(func(tx *sql.Tx) error {
		stmt, err := tx.Prepare(`INSERT INTO nodes(id,kind,label,note_id,attrs) VALUES(?, 'note', ?, ?, ?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for i := 0; i < 3000; i++ {
			id := fmt.Sprintf("ack-benchmark-%d", i)
			if _, err := stmt.Exec("note:"+id, "Acknowledgement benchmark", id, `{"body":"Avoid repeated graph and ranker construction"}`); err != nil {
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
		b.Run(map[bool]string{false: "forced_rebuild", true: "verified_reuse"}[reuse], func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if !reuse {
					srv.viewReusable = false
				}
				if err := srv.awaitOwnerIndexed(context.Background(), "note", path); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
