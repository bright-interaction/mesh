// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package retrieve

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/rerank"
)

type recordingReranker struct {
	called bool
	docs   []string
}

// TestPathACLFilteringDoesNotConsumeSignalLimits proves the folder boundary is
// applied while each ranked candidate stream is consumed. Post-limit filtering
// lets a run of stronger fenced notes starve readable matches to zero.
func TestPathACLFilteringDoesNotConsumeSignalLimits(t *testing.T) {
	var srcs []noteSrc
	for i := 0; i < 40; i++ {
		n := strconv.Itoa(i)
		srcs = append(srcs, noteSrc{"secret/strong-" + n + ".md",
			"---\nid: secret-" + n + "\ntype: note\nwhen: 2026-01-01\ntitle: Deploy pipeline hardening secret " + n + "\n---\n" +
				"# Deploy pipeline hardening secret\ndeploy pipeline hardening deploy pipeline hardening\n"})
	}
	for i := 0; i < 3; i++ {
		n := strconv.Itoa(i)
		srcs = append(srcs, noteSrc{"public/readable-" + n + ".md",
			"---\nid: public-" + n + "\ntype: note\nwhen: 2026-01-01\ntitle: Pipeline note " + n + "\n---\n" +
				"# Pipeline note\npipeline\n"})
	}
	r := buildVaultFrom(t, srcs)
	cards, err := r.Retrieve(context.Background(), "deploy pipeline hardening", Options{
		Limit:     3,
		NoRerank:  true,
		AllowPath: func(path string) bool { return strings.HasPrefix(path, "public/") },
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := cardIDs(cards); len(got) != 3 {
		t.Fatalf("path ACL starved readable matches: got %v", got)
	}
	for _, c := range cards {
		if !strings.HasPrefix(c.Path, "public/") {
			t.Fatalf("forbidden path returned: %+v", c)
		}
	}
}

func (r *recordingReranker) Model() string { return "recording" }

func (r *recordingReranker) Rerank(_ context.Context, _ string, docs []string) ([]rerank.Result, error) {
	r.called = true
	r.docs = append([]string(nil), docs...)
	out := make([]rerank.Result, len(docs))
	for i := range docs {
		out[i] = rerank.Result{Index: i, Score: float64(len(docs) - i)}
	}
	return out, nil
}

// TestRetrieveUsesCurrentStoreACLForFreshContent is the mixed-generation ACL
// regression. A watcher may commit the new notes/search rows before a long-lived
// reader swaps its in-memory graph. The fresh body must never be paired with the
// stale graph's old public path and sent to either the caller or the reranker.
func TestRetrieveUsesCurrentStoreACLForFreshContent(t *testing.T) {
	parse := func(path, body string) *index.ParsedNote {
		t.Helper()
		pn, err := index.Parse(path, []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		return pn
	}

	oldNotes := []*index.ParsedNote{
		parse("public/a.md", "---\nid: a\ntype: note\nwhen: 2026-01-01\ntitle: Old public A\n---\n# Old public A\nharmless old text\n"),
		parse("public/b.md", "---\nid: b\ntype: note\nwhen: 2026-01-01\ntitle: Public B\n---\n# Public B\nharmless old text\n"),
	}
	oldGraph, issues := index.BuildGraph(oldNotes)
	if len(issues) != 0 {
		t.Fatalf("old graph issues: %+v", issues)
	}

	currentNotes := []*index.ParsedNote{
		parse("secret/a.md", "---\nid: a\ntype: note\nwhen: 2026-01-01\ntitle: Newly secret A\n---\n# Newly secret A\nsecretneedle classified payload\n"),
		parse("public/b.md", "---\nid: b\ntype: note\nwhen: 2026-01-01\ntitle: Public B\n---\n# Public B\nsecretneedle public payload\n"),
	}
	currentGraph, issues := index.BuildGraph(currentNotes)
	if len(issues) != 0 {
		t.Fatalf("current graph issues: %+v", issues)
	}
	store, err := index.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.IndexVault(currentNotes, currentGraph); err != nil {
		t.Fatal(err)
	}

	r := New(store, oldGraph)
	recorder := &recordingReranker{}
	r.EnableRerank(recorder)
	cards, err := r.Retrieve(context.Background(), "secretneedle", Options{
		Limit: 10,
		AllowPath: func(path string) bool {
			return strings.HasPrefix(path, "public/")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := cardIDs(cards); len(got) != 1 || got[0] != "note:b" {
		t.Fatalf("current ACL must return only public note:b, got %v (%+v)", got, cards)
	}
	if cards[0].Path != "public/b.md" || cards[0].Title != "Public B" {
		t.Fatalf("card metadata must come from the current index generation: %+v", cards[0])
	}
	if recorder.called {
		t.Fatalf("reranker must not receive a secret candidate; docs=%q", recorder.docs)
	}
}

func TestStaleGraphNodeDoesNotConsumeGraphLimit(t *testing.T) {
	parse := func(path, body string) *index.ParsedNote {
		t.Helper()
		pn, err := index.Parse(path, []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		return pn
	}
	deleted := parse("deleted.md", "---\nid: deleted\ntype: note\nwhen: 2026-01-01\ntitle: Deploy pipeline hardening\ndo: deploy pipeline hardening deploy\n---\n# Deleted\n")
	liveOld := parse("live.md", "---\nid: live\ntype: note\nwhen: 2026-01-01\ntitle: Pipeline note\n---\n# Live\n")
	oldGraph, _ := index.BuildGraph([]*index.ParsedNote{deleted, liveOld})

	liveCurrent := parse("live.md", "---\nid: live\ntype: note\nwhen: 2026-01-01\ntitle: Pipeline note\n---\n# Live\n")
	currentGraph, _ := index.BuildGraph([]*index.ParsedNote{liveCurrent})
	store, err := index.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.IndexVault([]*index.ParsedNote{liveCurrent}, currentGraph); err != nil {
		t.Fatal(err)
	}

	r := New(store, oldGraph)
	cards, err := r.Retrieve(context.Background(), "deploy pipeline hardening", Options{
		Limit: 1, WeightGraph: 1, NoRerank: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := cardIDs(cards); len(got) != 1 || got[0] != "note:live" {
		t.Fatalf("stale graph node consumed Limit=1: got %v", got)
	}
}

func TestStaleVectorsDoNotConsumeVectorCandidatePool(t *testing.T) {
	r := buildVaultFrom(t, []noteSrc{{"live.md", "---\nid: live\ntype: note\nwhen: 2026-01-01\ntitle: Live\n---\n# Live\ncurrent note\n"}})
	vecs := map[string][][]float32{"note:live": {{0.8, 0.2}}}
	for i := 0; i < vecCandidateFloor; i++ {
		vecs["note:deleted-"+strconv.Itoa(i)] = [][]float32{{1, 0}}
	}
	if !r.EnableVectors(staticEmbedder{}, "static-two", 2, vecs) {
		t.Fatal("EnableVectors failed")
	}
	cards, err := r.Retrieve(context.Background(), "anything", Options{
		Limit: 1, WeightVec: 1, NoRerank: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := cardIDs(cards); len(got) != 1 || got[0] != "note:live" {
		t.Fatalf("stale vectors consumed the vector pool: got %v", got)
	}
}

func TestStaleNeighborsDoNotConsumeExpansionLimit(t *testing.T) {
	parse := func(path, body string) *index.ParsedNote {
		t.Helper()
		pn, err := index.Parse(path, []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		return pn
	}
	seedBody := "---\nid: seed\ntype: note\nwhen: 2026-01-01\ntitle: Seed\nrelated: [a, b, c, z]\n---\n# Seed\nseedneedle\n"
	seedOld := parse("seed.md", seedBody)
	oldNotes := []*index.ParsedNote{seedOld}
	for _, id := range []string{"a", "b", "c", "z"} {
		oldNotes = append(oldNotes, parse(id+".md", "---\nid: "+id+"\ntype: note\nwhen: 2026-01-01\ntitle: "+strings.ToUpper(id)+"\n---\n# "+id+"\n"))
	}
	oldGraph, _ := index.BuildGraph(oldNotes)

	seedCurrent := parse("seed.md", seedBody)
	live := parse("z.md", "---\nid: z\ntype: note\nwhen: 2026-01-01\ntitle: Z\n---\n# Z\n")
	currentNotes := []*index.ParsedNote{seedCurrent, live}
	currentGraph, _ := index.BuildGraph(currentNotes)
	store, err := index.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.IndexVault(currentNotes, currentGraph); err != nil {
		t.Fatal(err)
	}

	r := New(store, oldGraph)
	cards, err := r.Retrieve(context.Background(), "seedneedle", Options{
		Limit: 10, WeightFTS: 1, NoRerank: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := cardIDs(cards); !containsString(got, "note:z") {
		t.Fatalf("stale neighbors consumed expansion K=%d: got %v", expandK, got)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
