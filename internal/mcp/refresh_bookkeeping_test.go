// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/bright-interaction/mesh/internal/index"
)

func TestReaderRefreshFTSOnlyWritesRemainVisibleWithoutRebuild(t *testing.T) {
	srv, owner, _ := refreshFixture(t)
	g, r := srv.snapshot()
	if err := owner.Write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE search_index SET body='distinctiveftskeyword' WHERE node_id='note:note'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.refresh(); err != nil {
		t.Fatal(err)
	}
	g2, r2 := srv.snapshot()
	if g2 != g || r2 != r {
		t.Fatal("live FTS data unnecessarily rebuilt cached state")
	}
	hits, err := srv.store.Search(context.Background(), "distinctiveftskeyword", 5)
	if err != nil || len(hits) == 0 {
		t.Fatalf("FTS update invisible with cached graph: %v %v", hits, err)
	}
}

func refreshBookkeeping(t testing.TB, owner *index.Store) {
	t.Helper()
	if err := owner.Write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO metrics(key,value) VALUES('refresh-test',1) ON CONFLICT(key) DO UPDATE SET value=value+1`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestReaderRefreshReusesAcrossBookkeepingCommits(t *testing.T) {
	srv, owner, _ := refreshFixture(t)
	if !srv.viewVersion.Tracked {
		t.Fatal("fixture has no validated tracker")
	}
	g, r := srv.snapshot()
	for i := 0; i < 3; i++ {
		refreshBookkeeping(t, owner)
		if _, err := srv.refresh(); err != nil {
			t.Fatal(err)
		}
		g2, r2 := srv.snapshot()
		if g2 != g || r2 != r {
			t.Fatal("bookkeeping rebuilt graph/retriever")
		}
	}
	if count, err := owner.Metric("refresh-test"); err != nil || count != 3 {
		t.Fatalf("reused view hid telemetry: %d %v", count, err)
	}
	setRefreshLabel(t, owner, "changed")
	srv.beforeReaderInstall = func() { refreshBookkeeping(t, owner) }
	if _, err := srv.refresh(); err != nil {
		t.Fatal(err)
	}
	if !srv.viewReusable {
		t.Fatal("bookkeeping during graph construction defeated reuse")
	}
	g2, r2 := srv.snapshot()
	if g2 == g || r2 == r {
		t.Fatal("real retrieval change did not rebuild")
	}
	srv.beforeReaderInstall = nil
	if _, err := srv.refresh(); err != nil {
		t.Fatal(err)
	}
	g3, r3 := srv.snapshot()
	if g3 != g2 || r3 != r2 {
		t.Fatal("bookkeeping-during-build triggered another rebuild")
	}
}

func TestReaderRefreshFallsBackWhenTrackingIncomplete(t *testing.T) {
	srv, owner, _ := refreshFixture(t)
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
		t.Fatal("incomplete tracker trusted")
	}
	g, r := srv.snapshot()
	refreshBookkeeping(t, owner)
	if _, err := srv.refresh(); err != nil {
		t.Fatal(err)
	}
	g2, r2 := srv.snapshot()
	if g2 == g || r2 == r {
		t.Fatal("fallback ignored a database commit")
	}
	setRefreshLabel(t, owner, "untracked update")
	if _, err := srv.refresh(); err != nil {
		t.Fatal(err)
	}
	g3, _ := srv.snapshot()
	if n, _ := g3.Node("note:note"); n.Label != "untracked update" {
		t.Fatal("fallback missed change with no trigger")
	}
}

// Compare refreshes after identical real telemetry commits, including commit
// cost. Missing tracker models old writers/databases, not a forced test-only miss.
func BenchmarkBookkeepingRefresh(b *testing.B) {
	for _, tracked := range []bool{false, true} {
		b.Run(fmt.Sprintf("tracked=%t", tracked), func(b *testing.B) {
			b.Setenv("MESH_RERANK_AGENT", "http")
			root := b.TempDir()
			owner, err := index.Open(root)
			if err != nil {
				b.Fatal(err)
			}
			defer owner.Close()
			if err := owner.Write(func(tx *sql.Tx) error {
				if !tracked {
					if _, err := tx.Exec(`DROP TRIGGER _mesh_rr_v1_nodes_update`); err != nil {
						return err
					}
				}
				stmt, err := tx.Prepare(`INSERT INTO nodes(id,kind,label,note_id,attrs) VALUES(?,'note','Benchmark',?,?)`)
				if err != nil {
					return err
				}
				defer stmt.Close()
				for i := 0; i < 3000; i++ {
					id := fmt.Sprint(i)
					if _, err := stmt.Exec("note:"+id, id, `{"body":"Reader refresh benchmark"}`); err != nil {
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
			if srv.viewVersion.Tracked != tracked {
				b.Fatal("wrong benchmark tracking mode")
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				refreshBookkeeping(b, owner)
				if _, err := srv.refresh(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
