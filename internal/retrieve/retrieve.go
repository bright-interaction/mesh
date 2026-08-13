// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

// Package retrieve is the wedge: it fuses the FTS5 and graph-BM25 signals,
// expands one hop along the graph, boosts the institutional-memory tier, and
// packs the result to a token budget. The agent calls this, not raw search.
package retrieve

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/bright-interaction/mesh/internal/embed"
	"github.com/bright-interaction/mesh/internal/graph"
	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/meshcfg"
	"github.com/bright-interaction/mesh/internal/rerank"
	"github.com/bright-interaction/mesh/internal/vault"
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
	// godDegree: skip expansion into hub notes above this KNOWLEDGE degree (distinct
	// other notes linked in or out). Measured on raw fan-out it also skipped any note
	// with enough headings, so a 25-section runbook was passed over for a two-line stub.
	godDegree = 24

	rerankK = 30 // rerank at most this many top fused candidates

	// rerankBlendDefault weights the cross-encoder vs the fused score when reranking
	// the head: score = a*rerank + (1-a)*fused. 1.0 = pure rerank (the default).
	// On a large production vault an alpha sweep showed pure rerank dominates every blend
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
	// Limit is both the number of candidates pulled per signal AND the hard cap on
	// the cards returned (default 20). It has to be both: the vector arm and the
	// 1-hop expansion add candidates the per-signal fetch never saw, so a fetch-only
	// limit let a caller asking for 5 receive one card per vector-bearing note.
	Limit       int
	Budget      int     // token budget for packing; 0 = return all ranked (up to Limit)
	WeightFTS   float64 // fusion weight; 0 across all three => resolved defaults
	WeightGraph float64
	WeightVec   float64
	NoRerank    bool // skip the cross-encoder stage even when configured (for tuning the fusion itself)
	// AllowedScopes, when non-nil, restricts results to notes whose scope intersects
	// the set (access control). nil = unrestricted (solo / no-ACL fast path). This is
	// THE read boundary and it is enforced in three places, because one was not enough:
	// in candidate generation (SearchScoped / ScoreScoped, so the per-signal limit
	// counts only readable rows), at the expansion seeds (so an unreadable note cannot
	// stamp its title into a neighbour's Reason or donate score to it), and at the card
	// loop (the catch-all for the vector arm and expanded neighbours), which is still
	// before the reranker reads any doc.
	AllowedScopes map[string]bool
	// AllowPath, when non-nil, is the FOLDER read boundary: it reports whether the
	// caller may read the note at that vault-relative path. nil = unrestricted (no
	// folder ACL configured). It is a SEPARATE partition from AllowedScopes and a
	// caller must clear both: a team can fence folders with ACLs while never defining
	// a scope, in which case AllowedScopes is nil and filters nothing at all.
	AllowPath func(path string) bool
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

	freshHalfLife int                       // freshness decay half-life in days; 0 = off
	freshDates    map[string]index.NoteDate // note id -> lifecycle dates, lazy-loaded
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
	endpoint, fromEnv := envOrFile("MESH_EMBED_ENDPOINT", emb.Endpoint)
	model := envOr("MESH_EMBED_MODEL", emb.Model)
	if endpoint == "" || model == "" {
		return
	}
	vm, dim, vecs, err := r.store.LoadVectors()
	if err != nil || len(vecs) == 0 {
		return
	}
	r.queryPrefix = envOr("MESH_EMBED_QUERY_PREFIX", emb.QueryPrefix)
	// key_env is a POINTER to a process secret, so it is resolved through the closed
	// allow-list rather than read verbatim: a hand-edited config.toml that never passed
	// through the web config API must not be able to aim this at MESH_UI_TOKEN and have
	// the embedding endpoint receive it.
	keyEnv := meshcfg.ResolveKeyEnv("embedding.key_env", emb.KeyEnv, "MESH_EMBED_KEY")
	// Optional ANN: build an HNSW index past the threshold (0/unset = brute force,
	// the default; sub-5ms well past v1 scale). Env wins, then the config file.
	if v, err := strconv.Atoi(os.Getenv("MESH_HNSW_THRESHOLD")); err == nil && v > 0 {
		r.hnswGate = v
	} else if rv.HNSWThreshold > 0 {
		r.hnswGate = rv.HNSWThreshold
	}
	// The endpoint's SOURCE picks the HTTP client. From the process environment it is
	// operator input and may point at a local model server; from config.toml it is
	// member-writable (the web config API rewrites that file) and stays SSRF-guarded.
	newEmbedder := embed.NewHTTP
	if fromEnv {
		newEmbedder = embed.NewOperatorHTTP
	}
	r.EnableVectors(newEmbedder(endpoint, model, os.Getenv(keyEnv)), vm, dim, vecs)
}

// envOr returns the env var if set (non-empty), else the fallback.
func envOr(key, fallback string) string {
	v, _ := envOrFile(key, fallback)
	return v
}

// envOrFile is envOr plus the PROVENANCE of the value it returned: true when it came
// from the process environment, false when it came from the config file. That boolean is
// load-bearing for BYOAI endpoints. An env var is operator input (no HTTP surface can
// write the environment), so a localhost model server named there is allowed; the same
// URL arriving through .mesh/config.toml could have been written by any caller of
// PUT /api/config, so it stays behind the SSRF guard.
func envOrFile(key, fallback string) (string, bool) {
	if v := os.Getenv(key); v != "" {
		return v, true
	}
	return fallback, false
}

// enableRerank turns on the cross-encoder rerank stage when the endpoint + model
// are set (BYOAI, sovereign or cloud), env-first then the solo config file.
func (r *Retriever) enableRerank(rv meshcfg.Retrieval) {
	endpoint, fromEnv := envOrFile("MESH_RERANK_ENDPOINT", rv.RerankEndpoint)
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
	// Same allow-list as the embedding key: see enableVectors.
	keyEnv := meshcfg.ResolveKeyEnv("rerank.key_env", rv.RerankKeyEnv, "MESH_RERANK_KEY")
	// Same provenance split as enableVectors: MESH_RERANK_ENDPOINT is operator input and
	// may be the local tools/rerank-server on 127.0.0.1; rerank.endpoint in config.toml
	// is member-writable and stays guarded.
	newReranker := rerank.NewHTTP
	if fromEnv {
		newReranker = rerank.NewOperatorHTTP
	}
	r.EnableRerank(newReranker(endpoint, model, os.Getenv(keyEnv)))
}

// EnableRerank turns on the cross-encoder rerank stage. The reranker reorders the top-K
// fused candidates. Once enabled it is part of the contract: an endpoint that cannot be
// reached fails the query with ErrRerankUnavailable rather than quietly returning the
// fused order as if it had been reranked. Returns false for a nil reranker.
func (r *Retriever) EnableRerank(rr rerank.Reranker) bool {
	if rr == nil {
		return false
	}
	r.rr, r.rerankName = rr, rr.Model()
	return true
}

// ErrRerankUnavailable wraps every failure of a CONFIGURED reranker, so a caller that
// genuinely wants to keep serving (a long-lived server, say) can errors.Is it and choose
// to, while the default for a one-shot CLI or MCP call is to surface it. It never fires
// when no reranker is configured.
var ErrRerankUnavailable = errors.New("rerank endpoint unavailable")

// rerankProbeTimeout bounds the liveness probe below. A cross-encoder scoring one short
// sentinel document answers in well under a second; anything slower is not usable on a
// query path either.
const rerankProbeTimeout = 5 * time.Second

// RerankProbe sends ONE sentinel scoring request to the configured reranker and reports
// whether it answered. `mesh status` prints "rerank active" off configuration alone,
// which told a user with a dead endpoint that everything was fine; a probe is the only
// honest answer to "is it on". Returns nil when nothing is configured (there is nothing
// to be wrong) and wraps failures in ErrRerankUnavailable.
func (r *Retriever) RerankProbe(ctx context.Context) error {
	if r.rr == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, rerankProbeTimeout)
	defer cancel()
	if _, err := r.rr.Rerank(ctx, "mesh rerank probe", []string{"mesh rerank probe"}); err != nil {
		return fmt.Errorf("%w: %w", ErrRerankUnavailable, err)
	}
	return nil
}

// SignalReport is the honest answer to "which retrieval signals will actually fire":
// each optional stage as CONFIGURED plus, separately, whether its endpoint answered.
// Reporting configuration alone is what let a dead reranker read as "active" on every
// surface at once, so the two are never collapsed into one flag here.
type SignalReport struct {
	VectorsConfigured bool
	VectorsReachable  bool
	VectorsError      string // empty when reachable or not configured
	VectorModel       string
	ANN               bool

	RerankConfigured bool
	RerankReachable  bool
	RerankError      string // empty when reachable or not configured
	RerankModel      string
}

// Signals probes every configured BYOAI stage once and returns what is really on. It is
// the single renderer behind `mesh status` and the MCP mesh://retrieval resource, which
// used to answer the same question from their own copies of the logic and could not
// disagree only by accident. A stage that is not configured is reported unreachable with
// no error: there is nothing to be wrong with it.
func (r *Retriever) Signals(ctx context.Context) SignalReport {
	rep := SignalReport{
		VectorsConfigured: r.VectorsActive(),
		VectorModel:       r.VectorModel(),
		ANN:               r.HNSWActive(),
		RerankConfigured:  r.RerankActive(),
		RerankModel:       r.RerankModel(),
	}
	if rep.VectorsConfigured {
		if err := r.EmbedderProbe(); err != nil {
			rep.VectorsError = err.Error()
		} else {
			rep.VectorsReachable = true
		}
	}
	if rep.RerankConfigured {
		if err := r.RerankProbe(ctx); err != nil {
			rep.RerankError = err.Error()
		} else {
			rep.RerankReachable = true
		}
	}
	return rep
}

// EmbedderProbe reports whether the configured query embedder answers. Like RerankProbe
// this exists because `mesh status` was reporting configuration, not reachability. Dim()
// performs the round trip (and caches the result), so a width of 0 means the endpoint did
// not answer. Returns nil when no embedder is configured.
func (r *Retriever) EmbedderProbe() error {
	if r.emb == nil {
		return nil
	}
	if r.emb.Dim() == 0 {
		return fmt.Errorf("embedding endpoint unavailable: model %s returned no vector width", r.emb.Model())
	}
	return nil
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
	// Bound the cache: a long-lived shared Retriever (the SSH viewer builds one and
	// never swaps it) under a high-cardinality query stream would otherwise grow this
	// map forever. A query-embedding cache tolerates a coarse reset on overflow.
	if len(r.qvec) >= maxQvecEntries {
		r.qvec = make(map[string][]float32, maxQvecEntries)
	}
	r.qvec[key] = qv[0]
	return qv[0]
}

// maxQvecEntries caps the per-Retriever query-embedding cache.
const maxQvecEntries = 4096

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

	// Candidate generation is scope-aware: both keyword signals apply the read
	// boundary BEFORE their own truncation, so the fetch limit counts only rows this
	// caller may read. The old shape (fetch globally, over-fetch 4x, filter at the
	// card loop) starved a scoped caller to zero results as soon as ~4*Limit
	// higher-ranked unreadable notes matched the query.
	fetchLimit := opt.Limit
	// ctx, not context.Background(): this call used to drop the caller's context, so
	// cancelling the agent tool call or the HTTP request left the FTS read running to
	// completion. With the deadline the store now applies, a pathological query ends in
	// a named error instead of a process pinned at 100% CPU with nothing to cancel.
	ftsHits, err := r.store.SearchScoped(ctx, query, fetchLimit, opt.AllowedScopes)
	if err != nil {
		return nil, err
	}
	graphHits := r.ranker.ScoreScoped(query, fetchLimit, opt.AllowedScopes)

	fused := map[string]float64{}
	snippet := map[string]string{}
	reason := map[string]string{}

	// FTS signal, min-max normalized.
	fScores := make([]float64, len(ftsHits))
	for i, h := range ftsHits {
		fScores[i] = h.Score
	}
	fNorm := minMaxFloored(fScores)
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
	gNorm := minMaxFloored(gScores)
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
			// Both arms produce the same shape: the top-K chunk vectors folded to a
			// per-note max. Keeping the ANN and brute-force candidate sets identical is
			// what makes the two paths rank alike (see vectorCandidates).
			ids, sims := r.vectorCandidates(qv, vecCandidateK(opt.Limit))
			for i, id := range ids {
				// Path-independent scaling: map the cosine onto [0,1] against its own
				// fixed range instead of min-maxing the per-request candidate set. The
				// old min-max rescaled every score to whatever happened to be fetched,
				// so switching on the ANN index (a much smaller candidate set) silently
				// reordered results instead of only making them faster.
				fused[id] += wVec * cosineTo01(sims[i])
				if reason[id] == "" {
					reason[id] = "vector"
				}
			}
		}
	}

	// Capped 1-hop expansion from the strongest seeds the caller is allowed to read.
	// The seed filter is load-bearing, not defence in depth: a seed the caller cannot
	// read used to stamp its own frontmatter title into the neighbour's Reason
	// ("linked from <secret title>") and donate seed.score to the neighbour's rank,
	// so an unreadable note leaked its title and steered the scoped ranking. Scanning
	// past forbidden seeds (rather than dropping them from the top-5 slate) keeps
	// expansion recall intact for scoped callers.
	seeded := 0
	for _, seed := range topN(fused, 0) {
		if seeded >= expandSeeds {
			break
		}
		seedCard, seedOK := r.card(seed.id)
		if !seedOK || !scopeAllowed(seedCard.Scope, opt.AllowedScopes) || !pathAllowed(seedCard.Path, opt.AllowPath) {
			continue
		}
		seeded++
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
		c, ok := r.card(id)
		// The FTS index and the in-memory graph are refreshed independently, so a
		// long-running `mesh mcp --watch` can match a note its graph has not loaded yet.
		// Returning the shell card was worse than returning nothing: the caller got a
		// result with no title, no path and no id, so it could not fetch or even name
		// what matched, and it still cost budget. Observed on a live vault against a
		// daemon that had been up 10 hours.
		if !ok {
			continue
		}
		// Read boundary: drop notes the caller may not read BEFORE they reach the head,
		// the reranker, or the budget packer. Covers every signal at once, and both
		// partitions, because a folder ACL can fence a note whose scope is allowed.
		if !scopeAllowed(c.Scope, opt.AllowedScopes) || !pathAllowed(c.Path, opt.AllowPath) {
			continue
		}
		c.Snippet = snippet[id]
		c.Reason = reason[id]
		c.Score = score * r.boostMult(c)
		cards = append(cards, c)
	}
	sortCards(cards)

	// Cross-encoder rerank of the top-K head: a model that reads the query and
	// each candidate jointly reorders the strongest fused results, which is the
	// lever for top-1 precision. It refines the head only. A CONFIGURED endpoint that
	// cannot be reached now fails the query (ErrRerankUnavailable) instead of returning
	// the fused order dressed up as reranked. Skipped when tuning the fusion itself
	// (NoRerank), so the fused order is what gets measured.
	if !opt.NoRerank {
		if err := r.rerankHead(ctx, query, cards, fused); err != nil {
			return nil, err
		}
	}

	// Limit bounds the RETURNED set, after the reranker has had its say (so it can
	// still pull a card up from the tail) and before packing. Without this the vector
	// arm and the 1-hop expansion pushed cards the per-signal fetch never counted
	// straight to the caller: a Limit of 5 over a vector-enabled vault returned one
	// card per note in the vault.
	if opt.Limit > 0 && len(cards) > opt.Limit {
		cards = cards[:opt.Limit]
	}

	if opt.Budget > 0 {
		cards = packToBudget(cards, opt.Budget)
	}
	return cards, nil
}

// vecCandidateFloor is the smallest chunk-candidate pool the vector arm considers,
// so a tiny Limit still leaves the fused head a stable semantic signal.
const vecCandidateFloor = 50

// vecCandidateK is the number of chunk vectors the semantic signal considers for a
// given result limit. It is generous, so the fused and reranked head is stable even
// though the deep tail is cut off (and, on the pro build, approximate).
func vecCandidateK(limit int) int {
	k := limit * 4
	if k < vecCandidateFloor {
		k = vecCandidateFloor
	}
	return k
}

// cosineTo01 maps a cosine similarity onto [0,1] against its own fixed range.
//
// This must NOT be a min-max over the request's candidates. The candidate set
// differs per path (every chunk in the vault on the brute-force arm, the ANN
// index's top-k on the pro arm), so a relative normalizer made a note's semantic
// contribution depend on what else happened to be fetched: enabling the HNSW index
// rescaled every surviving score and reordered the results rather than only
// speeding them up. A fixed reference keeps a given cosine worth the same thing on
// both arms. The clamp only absorbs float drift outside [-1,1].
func cosineTo01(c float64) float64 {
	v := (c + 1) / 2
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// vectorCandidates returns the top-k chunk vectors for the query, folded to one
// entry per note carrying that note's best chunk score, in descending score order.
// Both arms go through here so the ANN and brute-force candidate sets have the same
// shape and size: the ANN index is then an accelerator for the same computation,
// not a different ranking.
func (r *Retriever) vectorCandidates(qv []float32, k int) (ids []string, sims []float64) {
	if k <= 0 {
		return nil, nil
	}
	var hits []annResult
	if r.ann != nil {
		hits = r.ann.Search(qv, k, 0)
	} else {
		hits = r.bruteForceTopChunks(qv, k)
	}
	best := map[string]float64{}
	for _, h := range hits {
		cur, seen := best[h.NodeID]
		if !seen {
			best[h.NodeID] = h.Score
			ids = append(ids, h.NodeID)
			continue
		}
		if h.Score > cur {
			best[h.NodeID] = h.Score
		}
	}
	sims = make([]float64, len(ids))
	for i, id := range ids {
		sims[i] = best[id]
	}
	return ids, sims
}

// bruteForceTopChunks scans every stored chunk vector and returns the k closest,
// mirroring what the ANN index returns (chunks, not notes: the caller max-pools).
// Ties break on node id then chunk index so the order is deterministic.
func (r *Retriever) bruteForceTopChunks(qv []float32, k int) []annResult {
	all := make([]annResult, 0, len(r.vecs))
	for id, chunks := range r.vecs {
		for ci, v := range chunks {
			all = append(all, annResult{NodeID: id, ChunkIx: ci, Score: embed.Cosine(qv, v)})
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Score != all[j].Score {
			return all[i].Score > all[j].Score
		}
		if all[i].NodeID != all[j].NodeID {
			return all[i].NodeID < all[j].NodeID
		}
		return all[i].ChunkIx < all[j].ChunkIx
	})
	if len(all) > k {
		all = all[:k]
	}
	return all
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
//
// fusedRaw carries the PRE-BOOST fused score per node id (the map Retrieve built),
// because this function ASSIGNS head[i].Score rather than adjusting it. Reading the
// fused component back off head[i].Score instead had two consequences: the tier-0
// factor was folded in twice (once by the card loop, once here, an effective 1.21x
// on the fused half of the blend), and the freshness decay was applied to the tail
// only, so the head never decayed at all.
//
// It returns an error when the CONFIGURED reranker could not score the head. That is a
// deliberate reversal: this used to return silently on a connect error, so a user who
// pointed MESH_RERANK_ENDPOINT at a server that was down (or, before the client split
// above, at a loopback address the SSRF guard refused) got byte-identical unreranked
// results, exit 0, and `mesh status` still printing "rerank active". Zero requests ever
// reached their server and nothing said so. A reranker the operator asked for and that
// cannot be reached is an error, not a no-op. Conditions that are NOT errors (no
// reranker configured, a head too short to reorder, a flat uninformative response) still
// leave the fused order intact and return nil.
func (r *Retriever) rerankHead(ctx context.Context, query string, cards []Card, fusedRaw map[string]float64) error {
	if r.rr == nil || len(cards) < 2 {
		return nil
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
		return fmt.Errorf("rerank: reading note bodies for the head: %w", err)
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
	if err != nil {
		return fmt.Errorf("%w (cross-encoder %s): %w\n  start the rerank endpoint (see tools/rerank-server), or unset MESH_RERANK_ENDPOINT + MESH_RERANK_MODEL to search without it", ErrRerankUnavailable, r.rerankName, err)
	}
	if len(res) != k {
		return fmt.Errorf("%w (cross-encoder %s): endpoint returned %d scores for %d documents", ErrRerankUnavailable, r.rerankName, len(res), k)
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
		return nil
	}
	norm := minMaxFloored(scores)
	// The head's fused scores, normalized over the head, so the blend can give the
	// lexical/graph/vector signal a real vote instead of discarding it. Pure rerank
	// (alpha=1) threw away a correct fused top-1 on keyword queries; blending keeps a
	// strong fused hit in contention. Read from fusedRaw, NOT from head[i].Score:
	// head[i].Score already carries the tier-0 and freshness multipliers, and this
	// loop applies them again below.
	fused := make([]float64, k)
	for i := range head {
		fused[i] = fusedRaw[head[i].NodeID]
	}
	fusedNorm := minMaxFloored(fused)
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
		// The tier-0 nudge and the freshness decay multiply the relevance component
		// only, never the offset, so institutional-memory notes get a small (<=0.1)
		// tiebreak among near-equal scores without overriding a clearly better pick,
		// and the head decays with age exactly like the tail does.
		rel *= r.boostMult(head[i])
		head[i].Score = base + rel
		if head[i].Reason != "" {
			head[i].Reason += " +reranked"
		} else {
			head[i].Reason = "reranked"
		}
	}
	sortCards(cards)
	return nil
}

// card builds a Card from a node id, reading title/path/type/tier-0 from the
// in-memory graph node. The bool is false when the node is not in the graph, in which
// case the Card is a shell the caller must NOT return: see the drop in Retrieve.
func (r *Retriever) card(id string) (Card, bool) {
	c := Card{NodeID: id}
	n, ok := r.graph.Node(id)
	if !ok {
		return c, false
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
	return c, true
}

// scopeAllowed reports whether a card may be returned given an allowed-scope set.
// allowed==nil means unrestricted (the solo / no-ACL fast path). A card with no scope
// attr is treated as the fail-safe default (dev-only). Delegates to the one shared
// predicate so this surface cannot drift from the MCP/web scope checks.
func scopeAllowed(cardScope string, allowed map[string]bool) bool {
	return vault.ScopeAllowsCSV(cardScope, allowed)
}

// pathAllowed reports whether a card's note path clears the folder read boundary.
// allow==nil means unrestricted (no folder ACL configured). Unlike the scope filter this
// cannot be pushed into candidate generation (the store indexes scope, not ACL prefixes),
// so a folder-fenced caller trades some recall for the boundary: their fetch limit is
// spent partly on rows that are then dropped.
func pathAllowed(path string, allow func(string) bool) bool {
	return allow == nil || allow(path)
}

// freshnessTypes are NON-institutional notes that decay with age. Decisions,
// gotchas, post-mortems (tier-0) and entities/concepts/maps are structural memory
// and never decay; only loose notes + status pages do.
var freshnessTypes = map[string]bool{"note": true, "status": true, "": true}

// freshnessMult returns a (floor,1] multiplier from a note's age. Institutional
// types return 1 (no decay). An overdue review_by applies a small extra penalty.
//
// The curve is floor + (1-floor)*0.5^(age/halfLife): it DECAYS ASYMPTOTICALLY
// TOWARD the floor instead of being clipped at it.
//
// That distinction is the whole point. The old form was
// `mult = 0.5^(age/halfLife); if mult < 0.6 { mult = 0.6 }`, a hard clamp, and
// 0.5^(age/30) crosses 0.6 at just 22 days. So EVERY note older than ~22 days
// received the identical 0.6 multiplier and freshness stopped discriminating
// entirely: a 24-day-old note and an 11-year-old note ranked the same, ties
// never broke, and the alphabetical NodeID fallback decided the order. The
// signal was dead for essentially the whole corpus, which is exactly the corpus
// it exists to rank.
//
// Asymptotic decay keeps the same guarantee (an old note is demoted at most
// 40%, never buried) while staying strictly monotonic in age forever, so
// freshness always breaks a tie in favour of the fresher note.
const freshnessFloor = 0.6

// boostMult is everything that multiplies a card's fused signal: the tier-0 nudge
// and the freshness decay. It lives in one place because the head (rerankHead) and
// the tail (the card loop in Retrieve) must apply exactly the same factors.
//
// They did not. rerankHead ASSIGNS head[i].Score, so every multiplier the card loop
// folded in was discarded for the whole head, and rerankK (30) is larger than the
// default Limit (20), which makes the head the entire returned set. The tier-0 nudge
// was re-applied there by hand; the freshness decay was not, so
// MESH_FRESHNESS_HALFLIFE_DAYS was a silent no-op on any deployment with a rerank
// endpoint configured (freshness on and off produced identical scores).
func (r *Retriever) boostMult(c Card) float64 {
	m := 1.0
	if c.Tier0 {
		m *= tier0Mult
	}
	if r.freshHalfLife > 0 {
		m *= r.freshnessMult(c)
	}
	return m
}

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
				// Asymptotic toward freshnessFloor, never clipped at it: stays
				// strictly monotonic in age, so a fresher note always outranks a
				// staler one on a tie, at any age.
				decay := math.Pow(0.5, ageDays/float64(r.freshHalfLife))
				mult = freshnessFloor + (1-freshnessFloor)*decay
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
		if !ok || n.Kind != "note" || n.KnowledgeDegree > godDegree {
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

// normFloor is the share of a normalized signal that every candidate keeps, so the
// weakest one never normalizes to exactly 0.
//
// minMax maps the minimum to 0, and 0 * tier0Mult == 0, so a multiplicative boost
// had nothing to act on over part of its own domain: the weakest of three FTS
// matches scored exactly 0.000000, and when that was a decision note its
// institutional-memory nudge bought it nothing at all - it sorted dead last on the
// alphabetical NodeID tie-break. Same shape as the historic 0.6 freshness clamp
// documented above freshnessMult: a boost that cannot discriminate over part of the
// corpus it exists to rank. A candidate that never matched a signal is simply absent
// from that signal's slice, so the floor lifts weak MATCHES only, never non-matches.
const normFloor = 0.02

// minMaxFloored is minMax lifted off zero by normFloor. Every fused signal and both
// halves of the rerank blend go through it, so the multiplicative boosts always have
// a positive quantity to move.
func minMaxFloored(xs []float64) []float64 {
	out := minMax(xs)
	for i, v := range out {
		out[i] = normFloor + (1-normFloor)*v
	}
	return out
}
