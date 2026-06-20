package retrieve

import (
	"context"
	"fmt"
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

func TestRerankDegradesOnError(t *testing.T) {
	r := buildVault(t)
	base, _ := r.Retrieve(context.Background(), "sqlite storage", Options{Limit: 10})
	r.EnableRerank(errReranker{})
	cards, err := r.Retrieve(context.Background(), "sqlite storage", Options{Limit: 10})
	if err != nil {
		t.Fatalf("a failing reranker must not fail retrieval: %v", err)
	}
	if len(cards) == 0 || cards[0].NodeID != base[0].NodeID {
		t.Errorf("failed rerank must leave the fused order intact")
	}
}

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
