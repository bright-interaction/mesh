// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/retrieve"
)

func TestReaderRankerRefreshMatchesFullRetriever(t *testing.T) {
	t.Setenv("MESH_EMBED_ENDPOINT", "")
	t.Setenv("MESH_EMBED_MODEL", "")
	t.Setenv("MESH_RERANK_ENDPOINT", "")
	srv, owner, path := refreshFixture(t)
	oldGraph, oldRetriever := srv.snapshot()
	setRefreshLabel(t, owner, "newrankerneedle")
	if err := srv.awaitOwnerIndexed(context.Background(), "note", path); err != nil {
		t.Fatal(err)
	}
	g, got := srv.snapshot()
	if oldGraph == g || oldRetriever == got {
		t.Fatal("changed graph was not published")
	}
	oldNode, _ := oldGraph.Node("note:note")
	if oldNode.Label != "Original" {
		t.Fatal("published old graph was mutated")
	}
	inputs, err := retrieve.LoadConfigInputs(context.Background(), srv.store.MeshDir())
	if err != nil {
		t.Fatal(err)
	}
	want, err := retrieve.NewFromInputsContext(context.Background(), srv.store, g, inputs)
	if err != nil {
		t.Fatal(err)
	}
	for _, allowed := range []map[string]bool{nil, {"dev": true}, {"public": true}} {
		opt := retrieve.Options{WeightGraph: 1, Limit: 5, NoRerank: true, AllowedScopes: allowed}
		actual, err := got.Retrieve(context.Background(), "newrankerneedle", opt)
		if err != nil {
			t.Fatal(err)
		}
		expected, err := want.Retrieve(context.Background(), "newrankerneedle", opt)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(actual, expected) {
			t.Fatalf("reader differs from full retriever: %v vs %v", actual, expected)
		}
	}
}

// This measures the complete local reader acknowledgement AFTER a writer has
// committed. It includes file proofs, monitor/version gates, SQL graph/hash/vector
// loads, ranker/retriever construction and atomic publication. It does not time
// file publication, owner indexing/debounce, network/tool encoding or model use.
// The no-reuse control removes only the previous retriever at the existing install
// seam; all production deadline and proof code is identical in both arms.
func BenchmarkChangedGraphRankerAcknowledgement(b *testing.B) {
	b.Setenv("MESH_RERANK_AGENT", "http")
	b.Setenv("MESH_RERANK_ENDPOINT", "")
	b.Setenv("MESH_RERANK_MODEL", "")
	b.Setenv("MESH_EMBED_ENDPOINT", "http://127.0.0.1:1") // construction must never call it
	b.Setenv("MESH_EMBED_MODEL", "ack-ranker-fixture")
	b.Setenv("MESH_HNSW_THRESHOLD", "0")
	root := b.TempDir()
	path := filepath.Join(root, "target.md")
	if err := os.WriteFile(path, []byte("---\nid: target\ntype: note\n---\nInitial target.\n"), 0600); err != nil {
		b.Fatal(err)
	}
	owner, err := index.Open(root)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = owner.Close() })
	if _, err := index.Reindex(owner, root); err != nil {
		b.Fatal(err)
	}
	const notes = 4200
	attrs, err := json.Marshal(map[string]string{
		"do":  strings.Repeat("verify exact versions and preserve current scope knowledge ", 12),
		"why": "Shared retrieval saves model tokens through reuse", "scope": "dev",
	})
	if err != nil {
		b.Fatal(err)
	}
	if err := owner.Write(func(tx *sql.Tx) error {
		noteStmt, err := tx.Prepare(`INSERT INTO notes(id,path,type,title,retrieval_hash,frontmatter,mtime) VALUES(?,?,'note','Fixture','fixture','{}',1)`)
		if err != nil {
			return err
		}
		defer noteStmt.Close()
		nodeStmt, err := tx.Prepare(`INSERT INTO nodes(id,kind,label,note_id,attrs) VALUES(?,?,'Mesh release safety',?,?)`)
		if err != nil {
			return err
		}
		defer nodeStmt.Close()
		edgeStmt, err := tx.Prepare(`INSERT INTO edges(source,target,relation,confidence,confidence_score,weight) VALUES(?,?,?,'EXTRACTED',1,1)`)
		if err != nil {
			return err
		}
		defer edgeStmt.Close()
		for i := 0; i < notes; i++ {
			id := fmt.Sprintf("fixture-%04d", i)
			if _, err := noteStmt.Exec(id, id+".md"); err != nil {
				return err
			}
			if _, err := nodeStmt.Exec("note:"+id, "note", id, string(attrs)); err != nil {
				return err
			}
			for h := 0; h < 6; h++ {
				heading := fmt.Sprintf("note:%s#h%d", id, h)
				if _, err := nodeStmt.Exec(heading, "heading", id, "{}"); err != nil {
					return err
				}
				if _, err := edgeStmt.Exec("note:"+id, heading, "contains"); err != nil {
					return err
				}
				if _, err := edgeStmt.Exec(heading, fmt.Sprintf("note:%s#h%d", id, (h+1)%6), "next"); err != nil {
					return err
				}
			}
			for j := 1; j <= 2; j++ {
				if _, err := edgeStmt.Exec("note:"+id, fmt.Sprintf("note:fixture-%04d", (i+j)%notes), "references"); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		b.Fatal(err)
	}
	var vectors []index.VectorRow
	for i := 0; i < 2350; i++ {
		vec := make([]float32, 768)
		vec[i%len(vec)] = 1
		vectors = append(vectors, index.VectorRow{NodeID: fmt.Sprintf("note:fixture-%04d", i), Vec: vec, NoteHash: "fixture"})
	}
	if err := owner.ReplaceVectors("ack-ranker-fixture", vectors); err != nil {
		b.Fatal(err)
	}
	srv, err := NewServer(root)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = srv.Close() })
	if err := srv.WaitReady(); err != nil {
		b.Fatal(err)
	}
	g, r := srv.snapshot()
	if !r.VectorsActive() {
		b.Fatal("vector fixture not active")
	}
	if g.NodeCount() != notes*7+1 || g.EdgeCount() != notes*14 {
		b.Fatalf("fixture graph changed: %d nodes, %d edges", g.NodeCount(), g.EdgeCount())
	}
	if total, live, stale, err := owner.VectorStats(); err != nil || total != 2350 || live != 2350 || stale != 0 {
		b.Fatalf("fixture vector counts: total=%d live=%d stale=%d err=%v", total, live, stale, err)
	}
	if model, dim := owner.VectorMeta(); model != "ack-ranker-fixture" || dim != 768 {
		b.Fatalf("fixture vector space: %q/%d", model, dim)
	}
	version := 0
	for _, reuseFirst := range []bool{false, true} {
		for _, reuse := range []bool{reuseFirst, !reuseFirst} {
			b.Run(fmt.Sprintf("reuse_first=%t/reuse=%t", reuseFirst, reuse), func(b *testing.B) {
				srv.beforeReaderInstall = nil
				if !reuse {
					srv.beforeReaderInstall = func() { srv.mu.Lock(); srv.retriever = nil; srv.mu.Unlock() }
				}
				defer func() { srv.beforeReaderInstall = nil }()
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					b.StopTimer()
					version++
					label := fmt.Sprintf("Changed target %d", version)
					if err := os.WriteFile(path, []byte(fmt.Sprintf("---\nid: target\ntype: note\ntitle: %s\n---\nChanged body %d.\n", label, version)), 0600); err != nil {
						b.Fatal(err)
					}
					parsed, err := index.ParseFile(path)
					if err != nil {
						b.Fatal(err)
					}
					parsed.Path = "target.md"
					if err := owner.Write(func(tx *sql.Tx) error {
						if _, err := tx.Exec(`UPDATE notes SET retrieval_hash=?,title=? WHERE id='target'`, index.RetrievalHash(parsed), label); err != nil {
							return err
						}
						_, err := tx.Exec(`UPDATE nodes SET label=? WHERE id='note:target'`, label)
						return err
					}); err != nil {
						b.Fatal(err)
					}
					_, prior := srv.snapshot()
					b.StartTimer()
					if err := srv.awaitOwnerIndexed(context.Background(), "target", path); err != nil {
						b.Fatal(err)
					}
					b.StopTimer()
					// Keep the previous graph/retriever alive through construction in
					// both arms; clearing the control pointer must not gain earlier GC.
					runtime.KeepAlive(prior)
					g, r := srv.snapshot()
					n, ok := g.Node("note:target")
					if !ok || n.Label != label || !r.VectorsActive() {
						b.Fatal("acknowledgement published stale or incomplete view")
					}
				}
			})
		}
	}
}
