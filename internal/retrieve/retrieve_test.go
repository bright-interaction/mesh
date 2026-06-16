package retrieve

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/embed"
	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/rerank"
)

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

func TestRetrieveFusesAndBoostsTier0(t *testing.T) {
	r := buildVault(t)
	cards, err := r.Retrieve("sqlite storage", Options{Limit: 10})
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
	cards, err := r.Retrieve("storage", Options{Limit: 10})
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
	if r.EnableVectors(embed.Stub{D: 2}, "different-model", vecs) {
		t.Error("guard must reject when the query embedder's model != the vault's stored model")
	}
	if r.EnableVectors(embed.Stub{D: 2}, "stub-bow", vecs) != true {
		t.Error("should enable when the model matches")
	}
	if r.EnableVectors(embed.Stub{D: 2}, "stub-bow", nil) {
		t.Error("guard must reject when there are no stored vectors")
	}
	// With vectors enabled, retrieval still works end to end.
	cards, err := r.Retrieve("sqlite storage", Options{Limit: 10})
	if err != nil || len(cards) == 0 {
		t.Fatalf("retrieve with vectors enabled failed: err=%v cards=%d", err, len(cards))
	}
}

func TestRerankReordersHead(t *testing.T) {
	r := buildVault(t)
	base, err := r.Retrieve("sqlite storage", Options{Limit: 10})
	if err != nil || len(base) < 2 {
		t.Fatalf("precondition: need multiple fused cards, got %d (err %v)", len(base), err)
	}
	if base[0].NodeID != "note:a" {
		t.Fatalf("precondition: fused top should be note:a, got %s", base[0].NodeID)
	}
	// The reranker prefers the note whose text mentions "extensions" (note:b).
	r.EnableRerank(fakeReranker{needle: "extensions"})
	cards, err := r.Retrieve("sqlite storage", Options{Limit: 10})
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

func TestRerankDegradesOnError(t *testing.T) {
	r := buildVault(t)
	base, _ := r.Retrieve("sqlite storage", Options{Limit: 10})
	r.EnableRerank(errReranker{})
	cards, err := r.Retrieve("sqlite storage", Options{Limit: 10})
	if err != nil {
		t.Fatalf("a failing reranker must not fail retrieval: %v", err)
	}
	if len(cards) == 0 || cards[0].NodeID != base[0].NodeID {
		t.Errorf("failed rerank must leave the fused order intact")
	}
}

func TestRerankConstantScoresKeepFusedOrder(t *testing.T) {
	r := buildVault(t)
	base, _ := r.Retrieve("sqlite storage", Options{Limit: 10})
	r.EnableRerank(constReranker{})
	cards, err := r.Retrieve("sqlite storage", Options{Limit: 10})
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
	pure, _ := r.Retrieve("sqlite storage", Options{Limit: 10})
	if len(pure) == 0 || pure[0].NodeID != "note:b" {
		t.Errorf("alpha=1 (pure rerank) should put the reranked note first, got %v", topID(pure))
	}
	// alpha=0.0: the blend collapses to the fused score, restoring fused top note:a.
	r.rerankBlend = 0.0
	fused, _ := r.Retrieve("sqlite storage", Options{Limit: 10})
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
	all, _ := r.Retrieve("sqlite modernc storage", Options{Limit: 10})
	if len(all) < 2 {
		t.Skip("need multiple cards to test packing")
	}
	tiny := cardTokens(all[0]) + 1
	packed, err := r.Retrieve("sqlite modernc storage", Options{Limit: 10, Budget: tiny})
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
