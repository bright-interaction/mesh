// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package retrieve

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"strconv"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/embed"
	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/meshcfg"
	"github.com/bright-interaction/mesh/internal/rerank"
)

// benchRandVec makes a deterministic random vector for the brute-force benchmark.
func benchRandVec(rng *rand.Rand, d int) []float32 {
	v := make([]float32, d)
	for i := range v {
		v[i] = float32(rng.NormFloat64())
	}
	return v
}

// BenchmarkBruteForceSearch measures one query's vector scan (the max-pool cosine
// over every note's chunks) at 768-dim for 1k and 5k notes - the operation the
// SPEC brute-force cap is about. It isolates the vector arm from FTS/graph so the
// number maps directly to the documented "~5k notes at 768-dim" ceiling.
func BenchmarkBruteForceSearch(b *testing.B) {
	const dim = 768
	for _, n := range []int{1000, 5000} {
		rng := rand.New(rand.NewSource(1))
		vecs := make(map[string][][]float32, n)
		for i := 0; i < n; i++ {
			vecs[strconv.Itoa(i)] = [][]float32{benchRandVec(rng, dim)}
		}
		qv := benchRandVec(rng, dim)
		b.Run("n"+strconv.Itoa(n), func(b *testing.B) {
			b.ReportAllocs()
			var sink float64
			for i := 0; i < b.N; i++ {
				for _, chunks := range vecs {
					best := -1.0
					for _, v := range chunks {
						if s := embed.Cosine(qv, v); s > best {
							best = s
						}
					}
					sink += best
				}
			}
			_ = sink
		})
	}
}

// fakeReranker scores a document 10 when it contains needle, else 0, so a test
// can force a specific candidate to the top and assert the head reordered.
type fakeReranker struct{ needle string }

func (f fakeReranker) Model() string { return "fake" }
func (f fakeReranker) Rerank(_ context.Context, _ string, docs []string) ([]rerank.Result, error) {
	out := make([]rerank.Result, len(docs))
	for i, d := range docs {
		if strings.Contains(strings.ToLower(d), f.needle) {
			out[i] = rerank.Result{Index: i, Score: 10}
		} else {
			out[i] = rerank.Result{Index: i, Score: 0}
		}
	}
	return out, nil
}

type errReranker struct{}

func (errReranker) Model() string { return "err" }
func (errReranker) Rerank(context.Context, string, []string) ([]rerank.Result, error) {
	return nil, fmt.Errorf("boom")
}

// constReranker returns the same score for every doc (an uninformative response).
type constReranker struct{}

func (constReranker) Model() string { return "const" }
func (constReranker) Rerank(_ context.Context, _ string, docs []string) ([]rerank.Result, error) {
	out := make([]rerank.Result, len(docs))
	for i := range docs {
		out[i] = rerank.Result{Index: i, Score: 5}
	}
	return out, nil
}

func buildVault(t *testing.T) *Retriever {
	t.Helper()
	dir := t.TempDir()
	mk := func(path, body string) *index.ParsedNote {
		pn, err := index.Parse(path, []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		return pn
	}
	notes := []*index.ParsedNote{
		mk("a.md", "---\nid: a\ntype: decision\nwhen: 2026-01-01\ndo: x\ndont: y\nwhy: use modernc sqlite for storage\nrelated: [b]\n---\n# Storage engine\n"),
		mk("b.md", "---\nid: b\ntype: gotcha\nwhen: 2026-01-01\ndo: x\ndont: y\nwhy: modernc cannot load sqlite vec extensions\n---\n# Modernc extensions\n"),
		mk("c.md", "---\nid: c\ntype: note\nwhen: 2026-01-01\n---\n# Unrelated\nsomething about marketing copy\n"),
	}
	g, _ := index.BuildGraph(notes)
	g.DetectCommunities(0)
	s, err := index.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := s.IndexVault(notes, g); err != nil {
		t.Fatal(err)
	}
	lg, err := s.LoadGraph()
	if err != nil {
		t.Fatal(err)
	}
	return New(s, lg)
}

// noteSrc is one (path, markdown) pair for buildVaultFrom.
type noteSrc struct{ path, body string }

// buildVaultFrom indexes an arbitrary set of notes and returns a Retriever over
// them, so a test can shape the corpus (scopes, link structure, corpus size)
// instead of reusing the fixed three-note vault.
func buildVaultFrom(t *testing.T, srcs []noteSrc) *Retriever {
	t.Helper()
	dir := t.TempDir()
	notes := make([]*index.ParsedNote, 0, len(srcs))
	for _, s := range srcs {
		pn, err := index.Parse(s.path, []byte(s.body))
		if err != nil {
			t.Fatal(err)
		}
		notes = append(notes, pn)
	}
	g, _ := index.BuildGraph(notes)
	g.DetectCommunities(0)
	s, err := index.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := s.IndexVault(notes, g); err != nil {
		t.Fatal(err)
	}
	lg, err := s.LoadGraph()
	if err != nil {
		t.Fatal(err)
	}
	return New(s, lg)
}

func cardIDs(cards []Card) []string {
	out := make([]string, len(cards))
	for i, c := range cards {
		out[i] = c.NodeID
	}
	return out
}

// TestScopedCandidateGenerationDoesNotStarve is the regression for the scope ACL
// being applied only after the per-signal limit. 60 unreadable notes match the
// query far better than the 3 readable ones, so any post-truncation filter (the
// old fixed 4x over-fetch) hands the caller an empty result while readable
// matches exist. The scope predicate now runs inside candidate generation, so the
// limit counts only readable rows.
func TestScopedCandidateGenerationDoesNotStarve(t *testing.T) {
	var srcs []noteSrc
	for i := 0; i < 60; i++ {
		n := strconv.Itoa(i)
		srcs = append(srcs, noteSrc{"dev" + n + ".md",
			"---\nid: dev" + n + "\ntype: note\nwhen: 2026-01-01\ntitle: Deploy pipeline hardening runbook " + n + "\nscope: [dev]\n---\n" +
				"# Deploy pipeline hardening runbook " + n + "\ndeploy pipeline hardening rollout deploy pipeline hardening\n"})
	}
	for i := 0; i < 3; i++ {
		n := strconv.Itoa(i)
		srcs = append(srcs, noteSrc{"team" + n + ".md",
			"---\nid: team" + n + "\ntype: note\nwhen: 2026-01-01\ntitle: Pipeline notes " + n + "\nscope: [team]\n---\n# Pipeline notes " + n + "\npipeline\n"})
	}
	// devops must never be readable by a caller allowed only dev: the scopes are
	// stored comma-joined, so a substring test would wrongly match the prefix.
	srcs = append(srcs, noteSrc{"devops.md",
		"---\nid: devops\ntype: note\nwhen: 2026-01-01\ntitle: Deploy pipeline hardening ops\nscope: [devops]\n---\n# Deploy pipeline hardening ops\ndeploy pipeline hardening\n"})
	r := buildVaultFrom(t, srcs)

	tests := []struct {
		name       string
		allowed    map[string]bool
		wantIDs    []string // every id that must be present
		forbidden  []string // ids that must never appear
		wantNoCard bool
	}{
		{
			name:      "scoped caller still gets its readable minority",
			allowed:   map[string]bool{"team": true},
			wantIDs:   []string{"note:team0", "note:team1", "note:team2"},
			forbidden: []string{"note:dev0", "note:devops"},
		},
		{
			name:      "dev must not read the devops scope by prefix",
			allowed:   map[string]bool{"dev": true},
			wantIDs:   []string{"note:dev0"},
			forbidden: []string{"note:devops", "note:team0"},
		},
		{
			name:       "an empty allowed set can read nothing",
			allowed:    map[string]bool{},
			wantNoCard: true,
		},
		{
			name:      "nil allowed set is unrestricted",
			allowed:   nil,
			wantIDs:   []string{"note:dev0"},
			forbidden: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cards, err := r.Retrieve(context.Background(), "deploy pipeline hardening",
				Options{Limit: 10, NoRerank: true, AllowedScopes: tc.allowed})
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]bool{}
			for _, c := range cards {
				got[c.NodeID] = true
			}
			if tc.wantNoCard && len(cards) != 0 {
				t.Fatalf("want no cards, got %v", cardIDs(cards))
			}
			for _, id := range tc.wantIDs {
				if !got[id] {
					t.Errorf("readable note %s missing from results %v", id, cardIDs(cards))
				}
			}
			for _, id := range tc.forbidden {
				if got[id] {
					t.Errorf("out-of-scope note %s leaked into results %v", id, cardIDs(cards))
				}
			}
		})
	}
}

// TestScopedSearchLimitsCountOnlyReadableRows pins the same invariant one layer
// down, at the SQL: the store's LIMIT must be spent on readable rows.
func TestScopedSearchLimitsCountOnlyReadableRows(t *testing.T) {
	var srcs []noteSrc
	for i := 0; i < 30; i++ {
		n := strconv.Itoa(i)
		srcs = append(srcs, noteSrc{"dev" + n + ".md",
			"---\nid: dev" + n + "\ntype: note\nwhen: 2026-01-01\ntitle: Widget " + n + "\nscope: [dev]\n---\n# Widget " + n + "\nwidget widget widget\n"})
	}
	srcs = append(srcs, noteSrc{"team.md",
		"---\nid: team\ntype: note\nwhen: 2026-01-01\ntitle: Widget team\nscope: [team]\n---\n# Widget team\nwidget\n"})
	r := buildVaultFrom(t, srcs)

	hits, err := r.store.SearchScoped("widget", 5, map[string]bool{"team": true})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].NodeID != "note:team" {
		t.Errorf("scoped search should spend its limit on readable rows, got %+v", hits)
	}
	scored := r.ranker.ScoreScoped("widget", 5, map[string]bool{"team": true})
	if len(scored) != 1 || scored[0].Node.ID != "note:team" {
		t.Errorf("scoped graph ranking should spend its limit on readable nodes, got %d hits", len(scored))
	}
	// The unrestricted forms must be unchanged.
	all, err := r.store.Search("widget", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 5 {
		t.Errorf("unrestricted search should still fill its limit, got %d", len(all))
	}
}

// TestLimitBoundsReturnedCards is the regression for Limit acting only as a fetch
// knob: the vector arm and the 1-hop expansion added cards nobody counted, so a
// caller asking for 5 got one card per note in the vault.
func TestLimitBoundsReturnedCards(t *testing.T) {
	const n = 30
	var srcs []noteSrc
	for i := 0; i < n; i++ {
		s := strconv.Itoa(i)
		srcs = append(srcs, noteSrc{"n" + s + ".md",
			"---\nid: n" + s + "\ntype: note\nwhen: 2026-01-01\n---\n# Storage engine " + s + "\nsqlite storage engine notes " + s + "\n"})
	}
	tests := []struct {
		name    string
		limit   int
		vectors bool
	}{
		{name: "lexical only", limit: 3},
		{name: "with the semantic signal on", limit: 5, vectors: true},
		{name: "limit above the corpus returns everything it found", limit: 500, vectors: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := buildVaultFrom(t, srcs)
			if tc.vectors {
				stub := embed.Stub{D: 64}
				vecs := map[string][][]float32{}
				for i := 0; i < n; i++ {
					s := strconv.Itoa(i)
					ev, _ := stub.Embed(context.Background(), []string{"sqlite storage engine notes " + s})
					vecs["note:n"+s] = ev
				}
				if !r.EnableVectors(stub, "stub-bow", 64, vecs) {
					t.Fatal("EnableVectors failed")
				}
			}
			cards, err := r.Retrieve(context.Background(), "sqlite storage", Options{Limit: tc.limit, NoRerank: true})
			if err != nil {
				t.Fatal(err)
			}
			if len(cards) == 0 {
				t.Fatal("expected results")
			}
			if len(cards) > tc.limit {
				t.Errorf("Limit %d must bound the returned cards, got %d", tc.limit, len(cards))
			}
		})
	}
}

// TestExpansionSeedRespectsScope is the regression for the 1-hop expansion seeding
// from notes the caller cannot read: the seed's frontmatter title rode out inside
// the neighbour's Reason string ("linked from <secret title>") and the seed's score
// was donated to that neighbour's rank. The vector case matters on its own because
// the vector arm has no scope information of its own, so it can still put a
// forbidden note into the fused map after the keyword signals were filtered.
func TestExpansionSeedRespectsScope(t *testing.T) {
	const secretTitle = "ACME root credential rotation runbook"
	srcs := []noteSrc{
		{"secret.md", "---\nid: secret\ntype: note\nwhen: 2026-01-01\ntitle: " + secretTitle + "\nscope: [dev]\nrelated: [pub]\n---\n# " + secretTitle + "\ncredential rotation runbook for the root account\n"},
		{"pub.md", "---\nid: pub\ntype: note\nwhen: 2026-01-01\ntitle: Sales onboarding\nscope: [sales]\n---\n# Sales onboarding\nonboarding checklist for new reps\n"},
	}
	tests := []struct {
		name    string
		vectors bool
	}{
		{name: "forbidden seed from the keyword signals"},
		{name: "forbidden seed from the vector arm", vectors: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := buildVaultFrom(t, srcs)
			if tc.vectors {
				// Only the forbidden note carries a vector, so the vector arm is the one
				// signal that can seed expansion here and pub can only arrive by expansion.
				stub := embed.Stub{D: 64}
				ev, _ := stub.Embed(context.Background(), []string{"credential rotation runbook for the root account"})
				if !r.EnableVectors(stub, "stub-bow", 64, map[string][][]float32{"note:secret": ev}) {
					t.Fatal("EnableVectors failed")
				}
			}
			cards, err := r.Retrieve(context.Background(), "credential rotation runbook",
				Options{Limit: 10, NoRerank: true, AllowedScopes: map[string]bool{"sales": true}})
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range cards {
				if c.NodeID == "note:secret" {
					t.Fatalf("out-of-scope note returned: %+v", c)
				}
				if strings.Contains(c.Reason, secretTitle) {
					t.Errorf("out-of-scope title leaked in Reason of %s: %q", c.NodeID, c.Reason)
				}
				if c.NodeID == "note:pub" && strings.HasPrefix(c.Reason, "linked from") {
					t.Errorf("readable note inherited score from a forbidden seed: %+v", c)
				}
			}
		})
	}
}

// TestExpansionStillSeedsFromReadableNotes guards the other side of the seed
// filter: a readable seed must still expand, and it may name itself in the Reason.
func TestExpansionStillSeedsFromReadableNotes(t *testing.T) {
	srcs := []noteSrc{
		{"seed.md", "---\nid: seed\ntype: note\nwhen: 2026-01-01\ntitle: Pricing rollout\nscope: [sales]\nrelated: [leaf]\n---\n# Pricing rollout\nquarterly pricing rollout plan\n"},
		{"leaf.md", "---\nid: leaf\ntype: note\nwhen: 2026-01-01\ntitle: Discount ladder\nscope: [sales]\n---\n# Discount ladder\nunrelated wording entirely\n"},
	}
	r := buildVaultFrom(t, srcs)
	cards, err := r.Retrieve(context.Background(), "pricing rollout",
		Options{Limit: 10, NoRerank: true, AllowedScopes: map[string]bool{"sales": true}})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, c := range cards {
		if c.NodeID == "note:leaf" {
			found = true
			if !strings.Contains(c.Reason, "Pricing rollout") {
				t.Errorf("expanded card should name its readable seed, got %q", c.Reason)
			}
		}
	}
	if !found {
		t.Errorf("expected note:leaf via 1-hop expansion from a readable seed, got %v", cardIDs(cards))
	}
}

// TestVectorScoreIsPathIndependent pins the fix for the min-max over a
// path-dependent candidate set: a note's semantic contribution must depend only on
// its own cosine, never on what else was fetched. Two retrievals over corpora that
// differ only in the OTHER notes must give the shared note the same score.
func TestVectorScoreIsPathIndependent(t *testing.T) {
	stub := embed.Stub{D: 64}
	embedOf := func(s string) [][]float32 {
		v, _ := stub.Embed(context.Background(), []string{s})
		return v
	}
	build := func(extra int) []noteSrc {
		srcs := []noteSrc{{"target.md", "---\nid: target\ntype: note\nwhen: 2026-01-01\n---\n# Sqlite storage\nsqlite storage engine\n"}}
		for i := 0; i < extra; i++ {
			s := strconv.Itoa(i)
			srcs = append(srcs, noteSrc{"f" + s + ".md",
				"---\nid: f" + s + "\ntype: note\nwhen: 2026-01-01\n---\n# Filler " + s + "\nsqlite unrelated filler " + s + "\n"})
		}
		return srcs
	}
	score := func(extra int) float64 {
		srcs := build(extra)
		r := buildVaultFrom(t, srcs)
		vecs := map[string][][]float32{}
		for _, s := range srcs {
			id := strings.TrimSuffix(s.path, ".md")
			body := s.body[strings.LastIndex(s.body, "\n---\n")+5:]
			vecs["note:"+id] = embedOf(body)
		}
		if !r.EnableVectors(stub, "stub-bow", 64, vecs) {
			t.Fatal("EnableVectors failed")
		}
		cards, err := r.Retrieve(context.Background(), "sqlite storage",
			Options{Limit: 10, WeightFTS: 0, WeightGraph: 0, WeightVec: 1, NoRerank: true})
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range cards {
			if c.NodeID == "note:target" {
				return c.Score
			}
		}
		t.Fatal("target note missing from results")
		return 0
	}
	// With a relative (min-max) normalizer the target scored 1.0 in both runs only
	// because it was the max; the tell is the FILLER set moving the target's score.
	// Assert the absolute value instead: (cos+1)/2 is fixed, so it cannot move.
	a, b := score(1), score(20)
	if math.Abs(a-b) > 1e-9 {
		t.Errorf("vector contribution moved with the candidate set: %v vs %v", a, b)
	}
	if a >= 1.0 {
		t.Errorf("a normalized cosine of 1.0 means the score is still relative to the candidate set, got %v", a)
	}
}

func TestFreshnessDecayReordersTie(t *testing.T) {
	dir := t.TempDir()
	mk := func(path, body string) *index.ParsedNote {
		pn, err := index.Parse(path, []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		return pn
	}
	// Two plain notes with identical searchable text (equal FTS score). Without
	// freshness, the tie-break is NodeID ascending so note:astale wins. The fresh
	// note has a later id, so if it ranks first, freshness decay did the reordering.
	notes := []*index.ParsedNote{
		mk("astale.md", "---\nid: astale\ntype: note\nwhen: 2015-01-01\nupdated: 2015-01-01\n---\n# Stale\nalpha beta gamma delta epsilon\n"),
		mk("zfresh.md", "---\nid: zfresh\ntype: note\nwhen: 2026-06-20\nupdated: 2026-06-20\n---\n# Fresh\nalpha beta gamma delta epsilon\n"),
	}
	g, _ := index.BuildGraph(notes)
	g.DetectCommunities(0)
	s, err := index.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := s.IndexVault(notes, g); err != nil {
		t.Fatal(err)
	}
	lg, _ := s.LoadGraph()

	// Freshness off: tie-break puts astale first.
	r := New(s, lg)
	cards, err := r.Retrieve(context.Background(), "alpha beta gamma delta epsilon", Options{Limit: 10, NoRerank: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) < 2 || cards[0].NodeID != "note:astale" {
		t.Fatalf("freshness off: want astale first, got %v", cards)
	}
	// Freshness on: the fresh note overtakes the stale one.
	r2 := New(s, lg)
	r2.freshHalfLife = 30
	cards2, err := r2.Retrieve(context.Background(), "alpha beta gamma delta epsilon", Options{Limit: 10, NoRerank: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(cards2) < 2 || cards2[0].NodeID != "note:zfresh" {
		t.Fatalf("freshness on: want zfresh first, got %v", cards2)
	}
}

func TestRetrieveFusesAndBoostsTier0(t *testing.T) {
	r := buildVault(t)
	cards, err := r.Retrieve(context.Background(), "sqlite storage", Options{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) == 0 {
		t.Fatal("expected results")
	}
	if cards[0].NodeID != "note:a" {
		t.Errorf("top card = %s, want note:a", cards[0].NodeID)
	}
	if !cards[0].Tier0 {
		t.Errorf("decision note should be flagged tier-0")
	}
	// The marketing note must not surface for a storage query.
	for _, c := range cards {
		if c.NodeID == "note:c" {
			t.Errorf("unrelated note should not surface")
		}
	}
}

func TestRetrieveExpandsAlongReferences(t *testing.T) {
	r := buildVault(t)
	// "storage" hits a; a references b. Expansion should surface b even though
	// "storage" is not in b's text.
	cards, err := r.Retrieve(context.Background(), "storage", Options{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	var foundB bool
	for _, c := range cards {
		if c.NodeID == "note:b" {
			foundB = true
			if c.Reason == "" {
				t.Errorf("expanded card should carry a reason")
			}
		}
	}
	if !foundB {
		t.Errorf("expected note:b via 1-hop expansion from note:a; got %+v", cards)
	}
}

func TestEnableVectorsHomogeneityGuard(t *testing.T) {
	r := buildVault(t)
	vecs := map[string][][]float32{"note:a": {{1, 0}}, "note:b": {{0, 1}}}
	if r.EnableVectors(embed.Stub{D: 2}, "different-model", 2, vecs) {
		t.Error("guard must reject when the query embedder's model != the vault's stored model")
	}
	if r.EnableVectors(embed.Stub{D: 2}, "stub-bow", 2, vecs) != true {
		t.Error("should enable when the model matches")
	}
	if r.EnableVectors(embed.Stub{D: 2}, "stub-bow", 2, nil) {
		t.Error("guard must reject when there are no stored vectors")
	}
	// With vectors enabled, retrieval still works end to end.
	cards, err := r.Retrieve(context.Background(), "sqlite storage", Options{Limit: 10})
	if err != nil || len(cards) == 0 {
		t.Fatalf("retrieve with vectors enabled failed: err=%v cards=%d", err, len(cards))
	}
}

// TestEnableVectorsDimMismatch is the V.3 guard: a same-named model at a different
// width must be refused, never activated to emit a uniform (garbage) vector signal.
func TestEnableVectorsDimMismatch(t *testing.T) {
	r := buildVault(t)
	// Stored vectors are width 2; the query embedder reports the SAME model name but
	// width 3. Cosine across mismatched widths returns 0, which min-max normalizes to
	// a uniform 1 that boosts every note equally. The guard must refuse.
	stored := map[string][][]float32{"note:a": {{1, 0}}, "note:b": {{0, 1}}}
	if r.EnableVectors(embed.Stub{D: 3}, "stub-bow", 2, stored) {
		t.Error("guard must reject when the embedder width != the vault's stored width")
	}
	if r.VectorsActive() {
		t.Error("a refused EnableVectors must leave the semantic signal off")
	}
	// Matching width activates.
	if !r.EnableVectors(embed.Stub{D: 2}, "stub-bow", 2, stored) {
		t.Error("should enable when both model and width match")
	}
}

// TestEnableVectorsRefusesIndeterminateDim: if the stored dim is unknown (0) and
// the vectors are all zero-length, EnableVectors must refuse rather than activate
// with vecDim==0 (which would disable the per-query length guard).
func TestEnableVectorsRefusesIndeterminateDim(t *testing.T) {
	r := buildVault(t)
	zero := map[string][][]float32{"note:a": {{}}, "note:b": {{}}}
	if r.EnableVectors(embed.Stub{D: 2}, "stub-bow", 0, zero) {
		t.Error("must refuse when the stored width cannot be determined")
	}
	if r.VectorsActive() {
		t.Error("a refused EnableVectors must leave the signal off")
	}
}

// TestEnableVectorsFromConfigToml proves the solo config.toml fallback: with no
// MESH_EMBED_* env vars set, a persisted .mesh/config.toml drives vector activation.
func TestEnableVectorsFromConfigToml(t *testing.T) {
	// Stub embedder model name; seed matching stored vectors and a config.toml.
	t.Setenv("MESH_EMBED_ENDPOINT", "")
	t.Setenv("MESH_EMBED_MODEL", "")
	r := buildVault(t)
	// note_hash must match the indexed notes' retrieval_hash or LoadVectors' staleness
	// JOIN would exclude these vectors.
	ha, _ := r.store.NoteRetrievalHash("note:a")
	hb, _ := r.store.NoteRetrievalHash("note:b")
	if err := r.store.ReplaceVectors("stub-bow", []index.VectorRow{
		{NodeID: "note:a", ChunkIx: 0, Vec: []float32{1, 0, 0, 0}, NoteHash: ha},
		{NodeID: "note:b", ChunkIx: 0, Vec: []float32{0, 1, 0, 0}, NoteHash: hb},
	}); err != nil {
		t.Fatal(err)
	}
	// The HTTP endpoint is unreachable (Dim() probe returns 0), so EnableVectors is
	// lenient on width; this test asserts config-driven activation, not network I/O.
	if err := meshcfg.Save(r.store.MeshDir(), meshcfg.Embedding{
		Endpoint: "http://127.0.0.1:1/v1",
		Model:    "stub-bow",
		Dim:      4,
		KeyEnv:   "MESH_EMBED_KEY",
	}); err != nil {
		t.Fatal(err)
	}
	cfg, _ := meshcfg.LoadConfig(r.store.MeshDir())
	r.enableVectors(cfg.Embedding, cfg.Retrieval)
	if !r.VectorsActive() {
		t.Fatal("config.toml should enable the semantic signal with no env vars set")
	}
	if r.VectorModel() != "stub-bow" {
		t.Errorf("VectorModel = %q, want stub-bow", r.VectorModel())
	}
}

// TestEnableVectorsDerivesDimFromVectors covers old vaults that stored vectors
// before vector_dim was recorded: storedDim==0 means derive from the loaded vectors.
func TestEnableVectorsDerivesDimFromVectors(t *testing.T) {
	r := buildVault(t)
	stored := map[string][][]float32{"note:a": {{1, 0, 0}}, "note:b": {{0, 1, 0}}}
	// storedDim 0 (unknown) but the embedder is width 2 while the vectors are width 3:
	// the derived dim (3) must still catch the mismatch.
	if r.EnableVectors(embed.Stub{D: 2}, "stub-bow", 0, stored) {
		t.Error("derived-dim guard must reject a width-2 embedder against width-3 vectors")
	}
	if !r.EnableVectors(embed.Stub{D: 3}, "stub-bow", 0, stored) {
		t.Error("derived-dim guard must accept a width-3 embedder against width-3 vectors")
	}
}

func TestRerankReordersHead(t *testing.T) {
	r := buildVault(t)
	base, err := r.Retrieve(context.Background(), "sqlite storage", Options{Limit: 10})
	if err != nil || len(base) < 2 {
		t.Fatalf("precondition: need multiple fused cards, got %d (err %v)", len(base), err)
	}
	if base[0].NodeID != "note:a" {
		t.Fatalf("precondition: fused top should be note:a, got %s", base[0].NodeID)
	}
	// The reranker prefers the note whose text mentions "extensions" (note:b).
	r.EnableRerank(fakeReranker{needle: "extensions"})
	cards, err := r.Retrieve(context.Background(), "sqlite storage", Options{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if cards[0].NodeID != "note:b" {
		t.Errorf("rerank should lift the 'extensions' note to #1, got %s", cards[0].NodeID)
	}
	if !strings.Contains(cards[0].Reason, "rerank") {
		t.Errorf("reranked card should note rerank in its reason, got %q", cards[0].Reason)
	}
}

// The old TestRerankDegradesOnError asserted the exact defect: it required a failing
// reranker to return the fused order with a nil error, which is how a configured
// endpoint that never answered looked identical to a working one. The replacement lives
// in byoai_local_endpoint_test.go as TestRerankSurfacesEndpointFailure. Silent-degrade
// cases that are NOT failures (no reranker, a flat response) keep their own tests.

func TestRerankConstantScoresKeepFusedOrder(t *testing.T) {
	r := buildVault(t)
	base, _ := r.Retrieve(context.Background(), "sqlite storage", Options{Limit: 10})
	r.EnableRerank(constReranker{})
	cards, err := r.Retrieve(context.Background(), "sqlite storage", Options{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	// A flat rerank response must be a no-op, not an alphabetical reshuffle.
	if len(cards) != len(base) || cards[0].NodeID != base[0].NodeID {
		t.Errorf("uninformative rerank must preserve fused order: base[0]=%s got[0]=%s", base[0].NodeID, cards[0].NodeID)
	}
}

func TestRerankBlendKnobSpansPureFusedToPureRerank(t *testing.T) {
	r := buildVault(t)
	r.EnableRerank(fakeReranker{needle: "extensions"}) // prefers note:b
	// alpha=1.0: cross-encoder owns the head, note:b wins.
	r.rerankBlend = 1.0
	pure, _ := r.Retrieve(context.Background(), "sqlite storage", Options{Limit: 10})
	if len(pure) == 0 || pure[0].NodeID != "note:b" {
		t.Errorf("alpha=1 (pure rerank) should put the reranked note first, got %v", topID(pure))
	}
	// alpha=0.0: the blend collapses to the fused score, restoring fused top note:a.
	r.rerankBlend = 0.0
	fused, _ := r.Retrieve(context.Background(), "sqlite storage", Options{Limit: 10})
	if len(fused) == 0 || fused[0].NodeID != "note:a" {
		t.Errorf("alpha=0 (pure fused) should restore the fused top note:a, got %v", topID(fused))
	}
}

func topID(cards []Card) string {
	if len(cards) == 0 {
		return "(none)"
	}
	return cards[0].NodeID
}

func TestRetrieveBudgetPacking(t *testing.T) {
	r := buildVault(t)
	all, _ := r.Retrieve(context.Background(), "sqlite modernc storage", Options{Limit: 10})
	if len(all) < 2 {
		t.Skip("need multiple cards to test packing")
	}
	tiny := cardTokens(all[0]) + 1
	packed, err := r.Retrieve(context.Background(), "sqlite modernc storage", Options{Limit: 10, Budget: tiny})
	if err != nil {
		t.Fatal(err)
	}
	if len(packed) >= len(all) {
		t.Errorf("budget %d should drop cards: packed %d vs all %d", tiny, len(packed), len(all))
	}
	if TotalTokens(packed) > tiny {
		t.Errorf("packed tokens %d exceed budget %d", TotalTokens(packed), tiny)
	}
}
