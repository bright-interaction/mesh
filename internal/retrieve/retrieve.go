// Package retrieve is the wedge: it fuses the FTS5 and graph-BM25 signals,
// expands one hop along the graph, boosts the institutional-memory tier, and
// packs the result to a token budget. The agent calls this, not raw search.
package retrieve

import (
	"context"
	"os"
	"sort"

	"github.com/brightinteraction/mesh/internal/embed"
	"github.com/brightinteraction/mesh/internal/graph"
	"github.com/brightinteraction/mesh/internal/index"
)

const (
	// tier0Mult nudges decision/gotcha/post-mortem notes UP among similarly-scored
	// results so institutional memory surfaces, but as a small multiplier (not the
	// old +0.5 additive, which could override a much stronger content match and
	// flip the top-1 pick to a wrong tier-0 note - the Gate-1 answer@1 regression).
	tier0Mult   = 1.1
	expandSeeds = 5   // expand from the top-N fused notes
	expandK     = 3   // pull at most K strong note-neighbors per seed
	expandDecay = 0.4 // a neighbor inherits this fraction of the seed's score
	godDegree   = 24  // skip expansion into hub nodes above this degree
)

var tier0Types = map[string]bool{"decision": true, "gotcha": true, "post-mortem": true}

// Card is one retrieval result: a note, why it surfaced, and its fused score.
type Card struct {
	NodeID  string
	NoteID  string
	Title   string
	Path    string
	Type    string
	Snippet string
	Score   float64
	Tier0   bool
	Reason  string
}

// Options tunes a retrieval. Zero values get sensible defaults.
type Options struct {
	Limit       int     // candidates pulled per signal (default 20)
	Budget      int     // token budget for packing; 0 = return all ranked
	WeightFTS   float64 // default 0.6
	WeightGraph float64 // default 0.4
}

type Retriever struct {
	store  *index.Store
	graph  *graph.Graph
	ranker *graph.Ranker

	emb         embed.Embedder
	vecModel    string
	vecs        map[string][][]float32 // node id -> per-section chunk vectors
	queryPrefix string                 // e.g. "search_query: " for nomic-style asymmetric models
}

func New(store *index.Store, g *graph.Graph) *Retriever {
	return &Retriever{store: store, graph: g, ranker: g.NewRanker()}
}

// NewFromEnv builds a retriever and enables the semantic signal when the vault
// has stored vectors and MESH_EMBED_ENDPOINT + MESH_EMBED_MODEL are set (BYOAI).
// Falls back silently to lexical-only when no embedder is configured.
func NewFromEnv(store *index.Store, g *graph.Graph) *Retriever {
	r := New(store, g)
	endpoint, model := os.Getenv("MESH_EMBED_ENDPOINT"), os.Getenv("MESH_EMBED_MODEL")
	if endpoint == "" || model == "" {
		return r
	}
	vm, vecs, err := store.LoadVectors()
	if err != nil || len(vecs) == 0 {
		return r
	}
	r.queryPrefix = os.Getenv("MESH_EMBED_QUERY_PREFIX")
	r.EnableVectors(embed.NewHTTP(endpoint, model, os.Getenv("MESH_EMBED_KEY")), vm, vecs)
	return r
}

// EnableVectors turns on the semantic signal. It is a no-op unless the query
// embedder's model matches the vault's stored model (homogeneity guard: vectors
// from different models are not comparable, so we fail safe to lexical-only
// rather than mix them).
func (r *Retriever) EnableVectors(e embed.Embedder, model string, vecs map[string][][]float32) bool {
	if e == nil || model == "" || len(vecs) == 0 || e.Model() != model {
		return false
	}
	r.emb, r.vecModel, r.vecs = e, model, vecs
	return true
}

// Retrieve runs the full pipeline and returns ranked (and optionally
// budget-packed) cards.
func (r *Retriever) Retrieve(query string, opt Options) ([]Card, error) {
	if opt.Limit <= 0 {
		opt.Limit = 20
	}
	vectorsActive := r.emb != nil && len(r.vecs) > 0
	wVec := 0.0
	if opt.WeightFTS == 0 && opt.WeightGraph == 0 {
		if vectorsActive {
			// With a semantic signal, give it real weight; FTS-top1 beat fused-top1
			// lexically, so FTS stays the largest share and graph-BM25 the smallest.
			opt.WeightFTS, opt.WeightGraph, wVec = 0.5, 0.2, 0.3
		} else {
			opt.WeightFTS, opt.WeightGraph = 0.7, 0.3
		}
	}

	ftsHits, err := r.store.Search(query, opt.Limit)
	if err != nil {
		return nil, err
	}
	graphHits := r.ranker.Score(query, opt.Limit)

	fused := map[string]float64{}
	snippet := map[string]string{}
	reason := map[string]string{}

	// FTS signal, min-max normalized.
	fScores := make([]float64, len(ftsHits))
	for i, h := range ftsHits {
		fScores[i] = h.Score
	}
	fNorm := minMax(fScores)
	for i, h := range ftsHits {
		fused[h.NodeID] += opt.WeightFTS * fNorm[i]
		snippet[h.NodeID] = h.Snippet
		reason[h.NodeID] = "fts"
	}

	// graph-BM25 signal, min-max normalized.
	gScores := make([]float64, len(graphHits))
	for i, h := range graphHits {
		gScores[i] = h.Score
	}
	gNorm := minMax(gScores)
	for i, h := range graphHits {
		fused[h.Node.ID] += opt.WeightGraph * gNorm[i]
		if reason[h.Node.ID] == "" {
			reason[h.Node.ID] = "graph"
		}
	}

	// Semantic signal: cosine of the query embedding against stored note vectors
	// (brute-force; the homogeneity guard already ensured comparable models). A
	// note is scored by its best-matching section (max over its chunk vectors),
	// so a long multi-topic note still surfaces on the one section that answers
	// the query instead of being diluted by a whole-note average.
	if vectorsActive && wVec > 0 {
		if qv, err := r.emb.Embed(context.Background(), []string{r.queryPrefix + query}); err == nil && len(qv) == 1 {
			ids := make([]string, 0, len(r.vecs))
			sims := make([]float64, 0, len(r.vecs))
			for id, chunks := range r.vecs {
				best := -1.0
				for _, v := range chunks {
					if s := embed.Cosine(qv[0], v); s > best {
						best = s
					}
				}
				ids = append(ids, id)
				sims = append(sims, best)
			}
			vNorm := minMax(sims)
			for i, id := range ids {
				fused[id] += wVec * vNorm[i]
				if reason[id] == "" {
					reason[id] = "vector"
				}
			}
		}
	}

	// Capped 1-hop expansion from the strongest seeds.
	for _, seed := range topN(fused, expandSeeds) {
		for _, nb := range r.strongNeighbors(seed.id, expandK) {
			if _, seen := fused[nb.id]; seen {
				continue
			}
			fused[nb.id] = seed.score * expandDecay * nb.weight
			reason[nb.id] = "linked from " + r.title(seed.id)
		}
	}

	// Enrich into cards, apply the tier-0 boost.
	cards := make([]Card, 0, len(fused))
	for id, score := range fused {
		c := r.card(id)
		c.Score = score
		c.Snippet = snippet[id]
		c.Reason = reason[id]
		if c.Tier0 {
			c.Score *= tier0Mult
		}
		cards = append(cards, c)
	}
	sort.Slice(cards, func(i, j int) bool {
		if cards[i].Score != cards[j].Score {
			return cards[i].Score > cards[j].Score
		}
		return cards[i].NodeID < cards[j].NodeID
	})

	if opt.Budget > 0 {
		cards = packToBudget(cards, opt.Budget)
	}
	return cards, nil
}

// card builds a Card from a node id, reading title/path/type/tier-0 from the
// in-memory graph node.
func (r *Retriever) card(id string) Card {
	c := Card{NodeID: id}
	n, ok := r.graph.Node(id)
	if !ok {
		return c
	}
	c.Title = n.Label
	c.Path = n.NotePath
	c.NoteID = n.NoteID
	if t, ok := n.Attrs["type"].(string); ok {
		c.Type = t
		c.Tier0 = tier0Types[t]
	}
	return c
}

func (r *Retriever) title(id string) string {
	if n, ok := r.graph.Node(id); ok {
		return n.Label
	}
	return id
}

type neighbor struct {
	id     string
	weight float64
}

// strongNeighbors returns the top-K note neighbors of id by edge weight,
// following reference edges in both directions and skipping hub (god) nodes.
func (r *Retriever) strongNeighbors(id string, k int) []neighbor {
	seen := map[string]float64{}
	consider := func(other string, w float64) {
		n, ok := r.graph.Node(other)
		if !ok || n.Kind != "note" || n.Degree > godDegree {
			return
		}
		if w > seen[other] {
			seen[other] = w
		}
	}
	for _, e := range r.graph.Neighbors(id) {
		if e.Relation == "references" {
			consider(e.Target, e.Weight)
		}
	}
	for _, e := range r.graph.RefsTo(id) {
		if e.Relation == "references" {
			consider(e.Source, e.Weight)
		}
	}
	out := make([]neighbor, 0, len(seen))
	for nid, w := range seen {
		out = append(out, neighbor{nid, w})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].weight != out[j].weight {
			return out[i].weight > out[j].weight
		}
		return out[i].id < out[j].id
	})
	if k > 0 && len(out) > k {
		out = out[:k]
	}
	return out
}

type scored struct {
	id    string
	score float64
}

func topN(m map[string]float64, n int) []scored {
	out := make([]scored, 0, len(m))
	for id, s := range m {
		out = append(out, scored{id, s})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score > out[j].score
		}
		return out[i].id < out[j].id
	})
	if n > 0 && len(out) > n {
		out = out[:n]
	}
	return out
}

// minMax scales scores to [0,1]. When all values are equal (or there is one),
// every value maps to 1 so the signal still contributes.
func minMax(xs []float64) []float64 {
	out := make([]float64, len(xs))
	if len(xs) == 0 {
		return out
	}
	lo, hi := xs[0], xs[0]
	for _, x := range xs {
		if x < lo {
			lo = x
		}
		if x > hi {
			hi = x
		}
	}
	if hi == lo {
		for i := range out {
			out[i] = 1
		}
		return out
	}
	for i, x := range xs {
		out[i] = (x - lo) / (hi - lo)
	}
	return out
}
