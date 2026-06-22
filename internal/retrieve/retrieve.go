// Package retrieve is the wedge: it fuses the FTS5 and graph-BM25 signals,
// expands one hop along the graph, boosts the institutional-memory tier, and
// packs the result to a token budget. The agent calls this, not raw search.
package retrieve

import (
	"context"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bright-interaction/mesh/internal/embed"
	"github.com/bright-interaction/mesh/internal/graph"
	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/meshcfg"
	"github.com/bright-interaction/mesh/internal/rerank"
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

	rerankK = 30 // rerank at most this many top fused candidates

	// rerankBlendDefault weights the cross-encoder vs the fused score when reranking
	// the head: score = a*rerank + (1-a)*fused. 1.0 = pure rerank (the default).
	// On the Hive vault an alpha sweep showed pure rerank dominates every blend
	// (lowering it monotonically hurt paraphrase answer@1 and never recovered the
	// one keyword case), so 1.0 ships; the MESH_RERANK_BLEND knob stays for corpora
	// where the lexical/graph signal is strong enough to deserve a vote.
	rerankBlendDefault = 1.0
)

var tier0Types = map[string]bool{"decision": true, "gotcha": true, "post-mortem": true}

// Card is one retrieval result: a note, why it surfaced, and its fused score.
type Card struct {
	NodeID  string
	NoteID  string
	Title   string
	Path    string
	Type    string
	Scope   string // access-control scope(s), comma-joined (for the scope read filter)
	Snippet string
	Score   float64
	Tier0   bool
	Reason  string
}

// Options tunes a retrieval. Zero values get sensible defaults.
type Options struct {
	Limit       int     // candidates pulled per signal (default 20)
	Budget      int     // token budget for packing; 0 = return all ranked
	WeightFTS   float64 // fusion weight; 0 across all three => resolved defaults
	WeightGraph float64
	WeightVec   float64
	NoRerank    bool // skip the cross-encoder stage even when configured (for tuning the fusion itself)
	// AllowedScopes, when non-nil, restricts results to notes whose scope intersects
	// the set (access control). nil = unrestricted (solo / no-ACL fast path). This is
	// THE read boundary: it is applied at the card loop below so it covers FTS, graph,
	// vector, and 1-hop expansion in one place, and before the reranker reads any doc.
	AllowedScopes map[string]bool
}

type Retriever struct {
	store  *index.Store
	graph  *graph.Graph
	ranker *graph.Ranker

	emb         embed.Embedder
	vecModel    string
	vecDim      int                    // stored embedding width; pins the space (query embeddings of any other width are rejected, never silently cosined to a uniform 0)
	vecs        map[string][][]float32 // node id -> per-section chunk vectors
	ann         annSearcher            // optional ANN index over vecs; nil => brute-force cosine scan
	hnswGate    int                    // build hnsw only when chunk count >= this; 0 => never (brute force)
	queryPrefix string                 // e.g. "search_query: " for nomic-style asymmetric models

	rr          rerank.Reranker // optional cross-encoder; reorders the top-K head
	rerankName  string          // model id, for status/diagnostics
	rerankBlend float64         // cross-encoder vs fused weight (see rerankBlendDefault)

	// Learned/operator fusion-weight defaults (0 across all three => built-in
	// defaults). Set from MESH_WEIGHT_FTS/GRAPH/VEC or by `mesh tune`.
	defWFTS, defWGraph, defWVec float64

	qvec   map[string][]float32 // query-embedding cache (keyed by prefixed query)
	qvecMu sync.Mutex

	freshHalfLife int                        // freshness decay half-life in days; 0 = off
	freshDates    map[string]index.NoteDate  // note id -> lifecycle dates, lazy-loaded
	freshOnce     sync.Once
}

func New(store *index.Store, g *graph.Graph) *Retriever {
	return &Retriever{store: store, graph: g, ranker: g.NewRanker(), rerankBlend: rerankBlendDefault, qvec: map[string][]float32{}}
}

// SetWeights sets the fusion-weight defaults used when a retrieval does not pass
// explicit Options weights (e.g. learned weights from `mesh tune`). Any value
// may be 0; if all three are 0 the built-in defaults apply.
func (r *Retriever) SetWeights(fts, graph, vec float64) {
	r.defWFTS, r.defWGraph, r.defWVec = fts, graph, vec
}

// Weights reports the active fusion-weight defaults (0,0,0 => built-in defaults).
func (r *Retriever) Weights() (fts, graph, vec float64) {
	return r.defWFTS, r.defWGraph, r.defWVec
}

// NewFromEnv builds a retriever and enables the optional BYOAI stages from the
// environment. The semantic (vector) and rerank stages are independent: either,
// both, or neither can be on. Falls back silently to lexical-only when nothing
// is configured.
func NewFromEnv(store *index.Store, g *graph.Graph) *Retriever {
	r := New(store, g)
	cfg, _ := meshcfg.LoadConfig(store.MeshDir())
	r.enableVectors(cfg.Embedding, cfg.Retrieval)
	r.enableRerank(cfg.Retrieval)
	r.loadWeights(cfg.Retrieval)
	return r
}

// loadWeights applies fusion weights, env-first then the solo config file (0 means
// "use the built-in default"). Env MESH_WEIGHT_* overrides the file, matching every
// other knob's precedence.
func (r *Retriever) loadWeights(rv meshcfg.Retrieval) {
	pick := func(env string, file float64) float64 {
		if v, err := strconv.ParseFloat(os.Getenv(env), 64); err == nil && v >= 0 {
			return v
		}
		if file >= 0 {
			return file
		}
		return 0
	}
	r.SetWeights(
		pick("MESH_WEIGHT_FTS", rv.WeightFTS),
		pick("MESH_WEIGHT_GRAPH", rv.WeightGraph),
		pick("MESH_WEIGHT_VEC", rv.WeightVec),
	)
	r.freshHalfLife = rv.FreshnessHalfLifeDays
	if v, err := strconv.Atoi(os.Getenv("MESH_FRESHNESS_HALFLIFE_DAYS")); err == nil && v >= 0 {
		r.freshHalfLife = v
	}
}

// enableVectorsFromEnv turns on the semantic signal when the vault has stored
// vectors and the embedding endpoint + model are configured. Resolution is
// env-first, then the solo .mesh/config.toml (written by `mesh embed`), so a solo
// dev does not re-export env vars every session. Env always wins.
func (r *Retriever) enableVectors(emb meshcfg.Embedding, rv meshcfg.Retrieval) {
	endpoint := envOr("MESH_EMBED_ENDPOINT", emb.Endpoint)
	model := envOr("MESH_EMBED_MODEL", emb.Model)
	if endpoint == "" || model == "" {
		return
	}
	vm, dim, vecs, err := r.store.LoadVectors()
	if err != nil || len(vecs) == 0 {
		return
	}
	r.queryPrefix = envOr("MESH_EMBED_QUERY_PREFIX", emb.QueryPrefix)
	keyEnv := emb.KeyEnv
	if keyEnv == "" {
		keyEnv = "MESH_EMBED_KEY"
	}
	// Optional ANN: build an HNSW index past the threshold (0/unset = brute force,
	// the default; sub-5ms well past v1 scale). Env wins, then the config file.
	if v, err := strconv.Atoi(os.Getenv("MESH_HNSW_THRESHOLD")); err == nil && v > 0 {
		r.hnswGate = v
	} else if rv.HNSWThreshold > 0 {
		r.hnswGate = rv.HNSWThreshold
	}
	r.EnableVectors(embed.NewHTTP(endpoint, model, os.Getenv(keyEnv)), vm, dim, vecs)
}

// envOr returns the env var if set (non-empty), else the fallback.
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// enableRerank turns on the cross-encoder rerank stage when the endpoint + model
// are set (BYOAI, sovereign or cloud), env-first then the solo config file.
func (r *Retriever) enableRerank(rv meshcfg.Retrieval) {
	endpoint := envOr("MESH_RERANK_ENDPOINT", rv.RerankEndpoint)
	model := envOr("MESH_RERANK_MODEL", rv.RerankModel)
	if endpoint == "" || model == "" {
		return
	}
	if b := os.Getenv("MESH_RERANK_BLEND"); b != "" {
		if v, err := strconv.ParseFloat(b, 64); err == nil && v >= 0 && v <= 1 {
			r.rerankBlend = v
		}
	} else if rv.RerankBlend > 0 {
		r.rerankBlend = rv.RerankBlend
	}
	keyEnv := rv.RerankKeyEnv
	if keyEnv == "" {
		keyEnv = "MESH_RERANK_KEY"
	}
	r.EnableRerank(rerank.NewHTTP(endpoint, model, os.Getenv(keyEnv)))
}

// EnableRerank turns on the cross-encoder rerank stage. The reranker reorders
// the top-K fused candidates; it never gates retrieval, so a failing endpoint
// degrades silently to the fused order. Returns false for a nil reranker.
func (r *Retriever) EnableRerank(rr rerank.Reranker) bool {
	if rr == nil {
		return false
	}
	r.rr, r.rerankName = rr, rr.Model()
	return true
}

// RerankActive reports whether a cross-encoder rerank stage is configured.
func (r *Retriever) RerankActive() bool { return r.rr != nil }

// RerankModel returns the configured rerank model id (empty when inactive).
func (r *Retriever) RerankModel() string { return r.rerankName }

// VectorsActive reports whether the semantic signal will fire (an embedder is
// configured and the vault has stored vectors that match its model).
func (r *Retriever) VectorsActive() bool { return r.emb != nil && len(r.vecs) > 0 }

// VectorModel returns the active embedding model id (empty when inactive).
func (r *Retriever) VectorModel() string { return r.vecModel }

// HNSWActive reports whether the ANN index is built and serving the vector signal
// (vs the brute-force scan). Only true past the configured MESH_HNSW_THRESHOLD,
// and only in the pro build (the open core has no ANN implementation wired).
func (r *Retriever) HNSWActive() bool { return r.ann != nil }

// annResult is one ANN hit. It mirrors the (pro-only) hnsw.Result shape but lives
// here so the open core compiles without importing the hnsw package.
type annResult struct {
	NodeID  string
	ChunkIx int
	Score   float64
}

// annSearcher is the optional approximate-nearest-neighbour seam. The open core
// ships no implementation (brute-force cosine only); the pro build wires HNSW by
// setting buildANN (see retrieve_ann_pro.go, //go:build pro).
type annSearcher interface {
	Search(q []float32, k, ef int) []annResult
}

// buildANN constructs the ANN index from the per-node vectors. nil in the open
// core (brute-force always); set by the pro build. On any error the caller keeps
// the brute-force scan, so the ANN path can only speed up, never break, retrieval.
var buildANN func(byNode map[string][][]float32) (annSearcher, error)

// resolveWeights picks the fusion weights: explicit Options weights win; else the
// learned/operator defaults (SetWeights / env); else the built-in defaults. The
// vector weight is zeroed when no semantic signal is active.
func (r *Retriever) resolveWeights(opt Options, vectorsActive bool) (wFTS, wGraph, wVec float64) {
	switch {
	case opt.WeightFTS != 0 || opt.WeightGraph != 0 || opt.WeightVec != 0:
		wFTS, wGraph, wVec = opt.WeightFTS, opt.WeightGraph, opt.WeightVec
	case r.defWFTS != 0 || r.defWGraph != 0 || r.defWVec != 0:
		wFTS, wGraph, wVec = r.defWFTS, r.defWGraph, r.defWVec
	case vectorsActive:
		// FTS-top1 beat fused-top1 lexically, so FTS stays the largest share, the
		// semantic signal gets real weight, graph-BM25 the smallest.
		wFTS, wGraph, wVec = 0.5, 0.2, 0.3
	default:
		wFTS, wGraph = 0.7, 0.3
	}
	if !vectorsActive {
		wVec = 0
	}
	return
}

// queryVec returns the (cached) embedding of the query, prefixed for asymmetric
// models. Returns nil if no embedder is set or the call fails. The cache makes
// repeated retrievals of the same query (e.g. a weight sweep) embed only once.
func (r *Retriever) queryVec(ctx context.Context, query string) []float32 {
	if r.emb == nil {
		return nil
	}
	key := r.queryPrefix + query
	r.qvecMu.Lock()
	defer r.qvecMu.Unlock()
	if v, ok := r.qvec[key]; ok {
		return v
	}
	qv, err := r.emb.Embed(ctx, []string{key})
	if err != nil || len(qv) != 1 {
		return nil
	}
	r.qvec[key] = qv[0]
	return qv[0]
}

// EnableVectors turns on the semantic signal. It is a no-op unless the query
// embedder's model matches the vault's stored model AND its vector width matches
// the stored width (homogeneity guard: vectors from a different model, or even the
// same model name at a different dimension, are not comparable. A length mismatch
// makes every cosine return 0, which min-max then normalizes to a uniform 1 - a
// silent garbage signal that boosts every note equally. We fail safe to
// lexical-only rather than emit it). storedDim is the vault's recorded width; if it
// is 0 (old vault, pre-vector_dim) we derive it from the loaded vectors.
func (r *Retriever) EnableVectors(e embed.Embedder, model string, storedDim int, vecs map[string][][]float32) bool {
	if e == nil || model == "" || len(vecs) == 0 || e.Model() != model {
		return false
	}
	dim := storedDim
	if dim == 0 {
		for _, chunks := range vecs {
			if len(chunks) > 0 && len(chunks[0]) > 0 {
				dim = len(chunks[0])
				break
			}
		}
	}
	// We must know the stored width to guard the query side; if we cannot determine
	// it (no stamped dim AND only zero-length vectors), refuse rather than activate
	// with vecDim==0, which would disable the per-query length guard and let a
	// uniform-garbage signal through.
	if dim == 0 {
		return false
	}
	// If the embedder reports a width and it disagrees with the stored width, refuse.
	// A 0 from Dim() means the probe failed (endpoint down); allow activation and let
	// the per-query length guard in Retrieve catch any mismatch at retrieval time.
	if ed := e.Dim(); ed != 0 && ed != dim {
		return false
	}
	r.emb, r.vecModel, r.vecDim, r.vecs = e, model, dim, vecs
	// Optional ANN index for large vaults (pro build only). Off unless hnswGate is
	// set AND a buildANN implementation is wired; on any build error the brute-force
	// scan stays (r.ann nil), so this can only speed up, never break, retrieval.
	// Built from the same vecs map, so the vectors are identical. In the open core
	// buildANN is nil, so retrieval is always brute-force cosine.
	if r.hnswGate > 0 && buildANN != nil {
		total := 0
		for _, chunks := range vecs {
			total += len(chunks)
		}
		if total >= r.hnswGate {
			if ix, err := buildANN(vecs); err == nil {
				r.ann = ix
			}
		}
	}
	return true
}

// Retrieve runs the full pipeline and returns ranked (and optionally
// budget-packed) cards.
func (r *Retriever) Retrieve(ctx context.Context, query string, opt Options) ([]Card, error) {
	if opt.Limit <= 0 {
		opt.Limit = 20
	}
	vectorsActive := r.emb != nil && len(r.vecs) > 0
	wFTS, wGraph, wVec := r.resolveWeights(opt, vectorsActive)

	// When a scope filter is active, over-fetch candidates so the allowed head still
	// fills after forbidden notes are dropped at the card loop.
	fetchLimit := opt.Limit
	if opt.AllowedScopes != nil {
		fetchLimit *= 4
	}
	ftsHits, err := r.store.Search(query, fetchLimit)
	if err != nil {
		return nil, err
	}
	graphHits := r.ranker.Score(query, fetchLimit)

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
		fused[h.NodeID] += wFTS * fNorm[i]
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
		fused[h.Node.ID] += wGraph * gNorm[i]
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
		// Length guard: a query embedding whose width disagrees with the stored width
		// would make every cosine 0, which min-max turns into a uniform 1 boosting every
		// note equally. Skip the whole vector contribution rather than emit that garbage.
		// vecDim is always > 0 once EnableVectors succeeds, so a mismatch is a real skip.
		if qv := r.queryVec(ctx, query); qv != nil && r.vecDim > 0 && len(qv) == r.vecDim {
			var ids []string
			var sims []float64
			if r.ann != nil {
				// ANN path (large vaults): the index returns the top chunks; fold them to
				// a per-note max, exactly the brute-force semantics, over a candidate set
				// instead of every note. hnswK is generous so the fused/reranked head is
				// stable even though the deep tail is approximate.
				hnswK := opt.Limit * 4
				if hnswK < 50 {
					hnswK = 50
				}
				best := map[string]float64{}
				for _, h := range r.ann.Search(qv, hnswK, 0) {
					if cur, ok := best[h.NodeID]; !ok || h.Score > cur {
						best[h.NodeID] = h.Score
					}
				}
				ids = make([]string, 0, len(best))
				sims = make([]float64, 0, len(best))
				for id, s := range best {
					ids = append(ids, id)
					sims = append(sims, s)
				}
			} else {
				ids = make([]string, 0, len(r.vecs))
				sims = make([]float64, 0, len(r.vecs))
				for id, chunks := range r.vecs {
					bestc := -1.0
					for _, v := range chunks {
						if s := embed.Cosine(qv, v); s > bestc {
							bestc = s
						}
					}
					ids = append(ids, id)
					sims = append(sims, bestc)
				}
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
		// Scope read boundary: drop notes the caller may not read BEFORE they reach
		// the head, the reranker, or the budget packer. Covers every signal at once.
		if !scopeAllowed(c.Scope, opt.AllowedScopes) {
			continue
		}
		c.Score = score
		c.Snippet = snippet[id]
		c.Reason = reason[id]
		if c.Tier0 {
			c.Score *= tier0Mult
		}
		if r.freshHalfLife > 0 {
			c.Score *= r.freshnessMult(c)
		}
		cards = append(cards, c)
	}
	sortCards(cards)

	// Cross-encoder rerank of the top-K head: a model that reads the query and
	// each candidate jointly reorders the strongest fused results, which is the
	// lever for top-1 precision. It refines the head only and never gates: any
	// endpoint error leaves the fused order intact. Skipped when tuning the fusion
	// itself (NoRerank), so the fused order is what gets measured.
	if !opt.NoRerank {
		r.rerankHead(ctx, query, cards)
	}

	if opt.Budget > 0 {
		cards = packToBudget(cards, opt.Budget)
	}
	return cards, nil
}

func sortCards(cards []Card) {
	sort.Slice(cards, func(i, j int) bool {
		if cards[i].Score != cards[j].Score {
			return cards[i].Score > cards[j].Score
		}
		return cards[i].NodeID < cards[j].NodeID
	})
}

// rerankHead reorders the top-K cards in place using the configured
// cross-encoder. Reranked cards are rescored above any fused tail card so the
// head stays on top after the final sort, with the tier-0 nudge preserved.
func (r *Retriever) rerankHead(ctx context.Context, query string, cards []Card) {
	if r.rr == nil || len(cards) < 2 {
		return
	}
	k := rerankK
	if k > len(cards) {
		k = len(cards)
	}
	head := cards[:k]
	ids := make([]string, k)
	for i := range head {
		ids[i] = head[i].NodeID
	}
	docText, err := r.store.NoteDocs(ids)
	if err != nil {
		return
	}
	docs := make([]string, k)
	for i, id := range ids {
		if d := docText[id]; d != "" {
			docs[i] = d
		} else {
			docs[i] = head[i].Title
		}
	}
	res, err := r.rr.Rerank(ctx, query, docs)
	if err != nil || len(res) != k {
		return
	}
	scores := make([]float64, k)
	lo, hi := res[0].Score, res[0].Score
	for _, x := range res {
		scores[x.Index] = x.Score
		if x.Score < lo {
			lo = x.Score
		}
		if x.Score > hi {
			hi = x.Score
		}
	}
	// A flat (uninformative) rerank response carries no ranking signal; leave the
	// fused head order intact rather than collapsing it to alphabetical via the
	// constant-score branch of minMax.
	if hi == lo {
		return
	}
	norm := minMax(scores)
	// Capture the head's fused scores before we overwrite them, normalized over the
	// head, so the blend can give the lexical/graph/vector signal a real vote
	// instead of discarding it. Pure rerank (alpha=1) threw away a correct fused
	// top-1 on keyword queries; blending keeps a strong fused hit in contention.
	fused := make([]float64, k)
	for i := range head {
		fused[i] = head[i].Score
	}
	fusedNorm := minMax(fused)
	// Lift the reranked head above the untouched fused tail. Derive the base from
	// the actual max tail score (not a fixed constant) so the invariant holds
	// regardless of edge-weight magnitudes in graph expansion.
	base := 1.0
	for _, c := range cards[k:] {
		if c.Score+1.0 > base {
			base = c.Score + 1.0
		}
	}
	a := r.rerankBlend
	for i := range head {
		// Convex blend of cross-encoder relevance and fused score, both in [0,1].
		rel := a*norm[i] + (1-a)*fusedNorm[i]
		// The tier-0 nudge multiplies the relevance component only, never the
		// offset, so institutional-memory notes get a small (<=0.1) tiebreak among
		// near-equal scores without overriding a clearly better pick.
		if head[i].Tier0 {
			rel *= tier0Mult
		}
		head[i].Score = base + rel
		if head[i].Reason != "" {
			head[i].Reason += " +reranked"
		} else {
			head[i].Reason = "reranked"
		}
	}
	sortCards(cards)
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
	if sc, ok := n.Attrs["scope"].(string); ok {
		c.Scope = sc
	}
	return c
}

// scopeAllowed reports whether a card may be returned given an allowed-scope set.
// allowed==nil means unrestricted (the solo / no-ACL fast path). A card with no
// scope attr is treated as the fail-safe default (dev-only).
func scopeAllowed(cardScope string, allowed map[string]bool) bool {
	if allowed == nil {
		return true
	}
	cs := cardScope
	if strings.TrimSpace(cs) == "" {
		cs = "dev"
	}
	for _, s := range strings.Split(cs, ",") {
		if allowed[strings.TrimSpace(s)] {
			return true
		}
	}
	return false
}

// freshnessTypes are NON-institutional notes that decay with age. Decisions,
// gotchas, post-mortems (tier-0) and entities/concepts/maps are structural memory
// and never decay; only loose notes + status pages do.
var freshnessTypes = map[string]bool{"note": true, "status": true, "": true}

// freshnessMult returns a [floor,1] multiplier from a note's age. Institutional
// types return 1 (no decay). An overdue review_by applies a small extra penalty.
// 0.5^(age/halfLife), floored at 0.6 so an old note is demoted, never buried.
func (r *Retriever) freshnessMult(c Card) float64 {
	r.freshOnce.Do(func() {
		if d, err := r.store.NoteDates(); err == nil {
			r.freshDates = d
		}
	})
	d, ok := r.freshDates[c.NoteID]
	if !ok {
		return 1
	}
	now := time.Now()
	mult := 1.0
	if freshnessTypes[c.Type] {
		if t, err := time.Parse("2006-01-02", d.Updated); err == nil {
			ageDays := now.Sub(t).Hours() / 24
			if ageDays > 0 {
				mult = math.Pow(0.5, ageDays/float64(r.freshHalfLife))
				if mult < 0.6 {
					mult = 0.6
				}
			}
		}
	}
	// Overdue review: a small nudge down regardless of type (it asked to be rechecked).
	if d.ReviewBy != "" {
		if t, err := time.Parse("2006-01-02", d.ReviewBy); err == nil && now.After(t) {
			mult *= 0.85
		}
	}
	return mult
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
