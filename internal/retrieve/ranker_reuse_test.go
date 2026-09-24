// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package retrieve

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/bright-interaction/mesh/internal/graph"
	"github.com/bright-interaction/mesh/internal/index"
)

func rankerReuseInputs() *ConfigInputs {
	// Explicit local inputs cannot inherit a developer's provider credentials or
	// subscription configuration. Tests never invoke a model or network endpoint.
	return &ConfigInputs{env: map[string]string{"MESH_RERANK_AGENT": "http"}, reusable: true}
}

func rankerReuseFixture(t *testing.T) (*index.Store, *graph.Graph) {
	t.Helper()
	clearBYOAIEnv(t)
	t.Setenv("MESH_RERANK_AGENT", "http")
	r := buildVaultFrom(t, []noteSrc{
		{"stable.md", "---\nid: stable\ntype: note\nscope: public\nwhen: 2026-01-01\n---\n# SQLite storage\nStable SQLite guidance.\n"},
		{"moved.md", "---\nid: moved\ntype: note\nscope: public\nwhen: 2026-01-01\n---\n# SQLite deployment\nDeployment guidance.\n"},
		{"changed.md", "---\nid: changed\ntype: note\nscope: secret\nwhen: 2026-01-01\n---\n# Older storage choice\nEarlier choice.\n"},
	})
	return r.store, r.graph
}

func TestRankerReuseRetrieverMatchesFullSearchAndCurrentScope(t *testing.T) {
	s, oldGraph := rankerReuseFixture(t)
	ctx := context.Background()
	in := rankerReuseInputs()
	previous, err := NewFromInputsContext(ctx, s, oldGraph, in)
	if err != nil {
		t.Fatal(err)
	}
	// A real indexing transaction changes text, membership, path and scope while
	// leaving one note's ranker inputs unchanged for the reuse path.
	var notes []*index.ParsedNote
	for _, src := range []noteSrc{
		{"stable.md", "---\nid: stable\ntype: note\nscope: public\nwhen: 2026-01-01\n---\n# SQLite storage\nStable SQLite guidance.\n"},
		{"restricted.md", "---\nid: moved\ntype: note\nscope: secret\nwhen: 2026-01-01\n---\n# SQLite deployment\nDeployment guidance.\n"},
		{"added.md", "---\nid: added\ntype: note\nscope: public\nwhen: 2026-01-01\n---\n# PostgreSQL storage\nAdditional storage guidance.\n"},
	} {
		pn, err := index.Parse(src.path, []byte(src.body))
		if err != nil {
			t.Fatal(err)
		}
		notes = append(notes, pn)
	}
	built, _ := index.BuildGraph(notes)
	if _, err := s.IndexVault(notes, built); err != nil {
		t.Fatal(err)
	}
	nextGraph, err := s.LoadGraph()
	if err != nil {
		t.Fatal(err)
	}
	reused, err := NewFromInputsReusingContext(ctx, s, nextGraph, in, previous)
	if err != nil {
		t.Fatal(err)
	}
	full, err := NewFromInputsContext(ctx, s, nextGraph, in)
	if err != nil {
		t.Fatal(err)
	}
	for _, scopes := range []map[string]bool{nil, {"public": true}, {"secret": true}} {
		for _, query := range []string{"sqlite storage", "postgresql", "older choice", "deployment"} {
			opt := Options{Limit: 20, NoRerank: true, AllowedScopes: scopes}
			got, err := reused.Retrieve(ctx, query, opt)
			if err != nil {
				t.Fatal(err)
			}
			want, err := full.Retrieve(ctx, query, opt)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("reuse differs from full retrieval for %q scopes=%v:\ngot %+v\nwant %+v", query, scopes, got, want)
			}
			for _, card := range got {
				if card.NoteID == "changed" || (scopes != nil && scopes["public"] && card.NoteID == "moved") {
					t.Fatalf("deleted or newly restricted note surfaced: %+v", card)
				}
				if card.NoteID == "moved" && card.Path != "restricted.md" {
					t.Fatalf("reused ranker retained old path: %+v", card)
				}
			}
		}
	}
	for _, hit := range reused.ranker.Score("sqlite", 0) {
		current, ok := nextGraph.Node(hit.Node.ID)
		if !ok || hit.Node != current {
			t.Fatal("reused ranker retained an old graph node pointer")
		}
	}
}

func TestRankerReuseRetrieverUsesFreshConfigAndIndependentCaches(t *testing.T) {
	s, g := rankerReuseFixture(t)
	ctx := context.Background()
	a := rankerReuseInputs()
	a.cfg.Retrieval.WeightFTS, a.cfg.Retrieval.WeightGraph = 0.1, 0.9
	a.cfg.Retrieval.FreshnessHalfLifeDays, a.cfg.Retrieval.RerankBlend = 30, 0.3
	previous, err := NewFromInputsContext(ctx, s, g, a)
	if err != nil {
		t.Fatal(err)
	}
	previous.qvec["old query"] = []float32{1, 0}
	previous.semanticCircuitUntil = time.Now().Add(time.Hour)
	previous.freshOnce.Do(func() {
		previous.freshDates = map[string]index.NoteDate{"stable": {Updated: "1900-01-01"}}
	})
	b := rankerReuseInputs()
	b.cfg.Retrieval.WeightFTS, b.cfg.Retrieval.WeightGraph = 0.8, 0.2
	b.cfg.Retrieval.FreshnessHalfLifeDays, b.cfg.Retrieval.RerankBlend = 7, 0.6
	b.env["MESH_WEIGHT_FTS"] = "0.6"
	t.Setenv("MESH_WEIGHT_FTS", "0.99") // captured inputs, not current environment, win
	next, err := NewFromInputsReusingContext(ctx, s, g, b, previous)
	if err != nil {
		t.Fatal(err)
	}
	if fts, graphWeight, vec := next.Weights(); fts != 0.6 || graphWeight != 0.2 || vec != 0 {
		t.Fatalf("old or reread weights survived: %v %v %v", fts, graphWeight, vec)
	}
	if next.freshHalfLife != 7 || next.rerankBlend != 0.6 || next.RerankActive() {
		t.Fatal("previous freshness/reranker configuration survived")
	}
	if len(next.qvec) != 0 || next.freshDates != nil || !next.semanticCircuitUntil.IsZero() {
		t.Fatal("mutable query/freshness/provider-failure cache was shared")
	}
	next.qvec["new query"] = []float32{0, 1}
	_ = next.freshnessMult(Card{NoteID: "stable", Type: "note"})
	if next.freshDates["stable"].Updated != "2026-01-01" {
		t.Fatalf("new retriever inherited the old sync.Once/cache: %+v", next.freshDates)
	}
	next.freshDates["stable"] = index.NoteDate{Updated: "2099-01-01"}
	if len(previous.qvec) != 1 || previous.qvec["old query"][0] != 1 || previous.freshDates["stable"].Updated != "1900-01-01" {
		t.Fatal("new cache mutation changed the previous retriever")
	}
	if fts, graphWeight, _ := previous.Weights(); fts != 0.1 || graphWeight != 0.9 || previous.freshHalfLife != 30 || previous.rerankBlend != 0.3 {
		t.Fatal("new configuration mutated the previous retriever")
	}
}

func TestRankerReuseRetrieverReloadsAndFiltersVectors(t *testing.T) {
	s, g := rankerReuseFixture(t)
	ctx := context.Background()
	const model = "ranker-reuse-fixture"
	var rows []index.VectorRow
	for _, id := range []string{"stable", "changed", "moved"} {
		hash, err := s.NoteRetrievalHash("note:" + id)
		if err != nil || hash == "" {
			t.Fatalf("fixture hash unavailable: %q %v", hash, err)
		}
		rows = append(rows, index.VectorRow{NodeID: "note:" + id, NoteHash: hash, Vec: []float32{1, 0}})
	}
	if err := s.ReplaceVectors(model, rows); err != nil {
		t.Fatal(err)
	}
	in := rankerReuseInputs()
	// Unsupported scheme cannot reach a provider. Construction reads stored
	// dimensions; this test never calls a vector query or health probe.
	in.env["MESH_EMBED_ENDPOINT"], in.env["MESH_EMBED_MODEL"] = "fixture://never-contact", model
	previous, err := NewFromInputsContext(ctx, s, g, in)
	if err != nil || !previous.VectorsActive() || len(previous.vecs) != 3 {
		t.Fatalf("vector fixture not active: %v", err)
	}
	if err := s.Write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE notes SET retrieval_hash='changed-version' WHERE id='changed';
			DELETE FROM notes WHERE id='moved'; DELETE FROM nodes WHERE id='note:moved';
			DELETE FROM edges WHERE source='note:moved' OR target='note:moved'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	nextGraph, err := s.LoadGraph()
	if err != nil {
		t.Fatal(err)
	}
	next, err := NewFromInputsReusingContext(ctx, s, nextGraph, in, previous)
	if err != nil {
		t.Fatal(err)
	}
	full, err := NewFromInputsContext(ctx, s, nextGraph, in)
	if err != nil {
		t.Fatal(err)
	}
	if !next.VectorsActive() || len(next.vecs) != 1 || len(next.vecs["note:stable"]) != 1 || !reflect.DeepEqual(next.vecs, full.vecs) {
		t.Fatalf("stale/deleted vectors reused: %+v", next.vecs)
	}
	next.vecs["note:stable"][0][0] = 7
	if len(previous.vecs) != 3 || previous.vecs["note:stable"][0][0] != 1 || full.vecs["note:stable"][0][0] != 1 {
		t.Fatal("new retriever shares mutable stored vectors with a previous build")
	}
	other := rankerReuseInputs()
	other.env["MESH_EMBED_ENDPOINT"], other.env["MESH_EMBED_MODEL"] = "fixture://never-contact", "different-model"
	mismatch, err := NewFromInputsReusingContext(ctx, s, nextGraph, other, previous)
	if err != nil || mismatch.VectorsActive() {
		t.Fatalf("ranker reuse carried vectors across model change: %v", err)
	}
}

func TestRankerReuseRetrieverCancellationPreservesPrevious(t *testing.T) {
	s, g := rankerReuseFixture(t)
	in := rankerReuseInputs()
	previous, err := NewFromInputsContext(context.Background(), s, g, in)
	if err != nil {
		t.Fatal(err)
	}
	before := previous.ranker.Score("sqlite", 0)
	previous.qvec["sentinel"] = []float32{1}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	next, err := NewFromInputsReusingContext(ctx, s, g, in, previous)
	if !errors.Is(err, context.Canceled) || next != nil {
		t.Fatalf("cancelled constructor published: %v %v", next, err)
	}
	if !reflect.DeepEqual(before, previous.ranker.Score("sqlite", 0)) || len(previous.qvec) != 1 || previous.qvec["sentinel"][0] != 1 {
		t.Fatal("cancelled constructor changed the previous retriever")
	}
	if next, err := NewFromInputsReusingContext(context.Background(), s, g, in, previous); err != nil || next == nil {
		t.Fatalf("cancelled attempt poisoned subsequent construction: %v", err)
	}
}
