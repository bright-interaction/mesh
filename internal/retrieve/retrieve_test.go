package retrieve

import (
	"testing"

	"github.com/brightinteraction/mesh/internal/embed"
	"github.com/brightinteraction/mesh/internal/index"
)

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
	vecs := map[string][]float32{"note:a": {1, 0}, "note:b": {0, 1}}
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
