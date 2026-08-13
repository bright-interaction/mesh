// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package graph

import (
	"math"
	"sort"
	"strings"
	"unicode"

	"github.com/bright-interaction/mesh/internal/vault"
)

// BM25 parameters. labelWeight repeats
// a node's label tokens so a title match outranks a body match.
const (
	bm25K1      = 1.5
	bm25B       = 0.75
	labelWeight = 2
)

// ScoredNode pairs a node with its BM25 relevance (higher is better).
type ScoredNode struct {
	Node  *Node
	Score float64
}

// Ranker precomputes corpus statistics over the scorable (note) nodes once, so
// per-query scoring is O(queryTerms x candidates) instead of O(corpus) per call. Rebuild it whenever the graph changes.
type Ranker struct {
	node   map[string]*Node
	tf     map[string]map[string]int
	docLen map[string]int
	df     map[string]int
	avgLen float64
	n      float64
}

// NewRanker builds the inverted statistics over every note node's label+attrs.
func (g *Graph) NewRanker() *Ranker {
	g.mu.RLock()
	defer g.mu.RUnlock()
	r := &Ranker{
		node:   map[string]*Node{},
		tf:     map[string]map[string]int{},
		docLen: map[string]int{},
		df:     map[string]int{},
	}
	total := 0
	for id, nd := range g.nodes {
		if nd.Kind != "note" {
			continue
		}
		toks := nodeText(nd)
		tf := termFreq(toks)
		r.node[id] = nd
		r.tf[id] = tf
		r.docLen[id] = len(toks)
		total += len(toks)
		for term := range tf {
			r.df[term]++
		}
		r.n++
	}
	if r.n > 0 {
		r.avgLen = float64(total) / r.n
	}
	if r.avgLen == 0 {
		r.avgLen = 1
	}
	return r
}

// Score ranks note nodes against the query by BM25 over label+attrs. Returns
// nodes with a positive score, sorted by score desc with node id as the
// deterministic tiebreak. Unrestricted: see ScoreScoped for the access-controlled
// form.
func (r *Ranker) Score(query string, limit int) []ScoredNode {
	return r.ScoreScoped(query, limit, nil)
}

// ScoreScoped is Score restricted to nodes whose scope intersects allowed (nil =
// unrestricted).
//
// The filter runs BEFORE the limit truncation, not after: truncating the global
// ranking first and filtering later let a run of higher-ranked unreadable notes
// eat the whole limit, so a scoped caller could get nothing back while readable
// matches existed further down the ranking.
func (r *Ranker) ScoreScoped(query string, limit int, allowed map[string]bool) []ScoredNode {
	// TokenizeQuery, not Tokenize: this loop is O(corpus x queryTerms), so an
	// unbounded query text turned into unbounded CPU. A 1 MiB query admitted by the
	// hub's 1 MiB body limit produced ~100k terms and burned minutes of single-core
	// time on a 500-note vault. The cap is shared with the FTS side so both keyword
	// signals see the same terms.
	qterms := TokenizeQuery(query)
	if len(qterms) == 0 {
		return nil
	}
	var out []ScoredNode
	for id, tf := range r.tf {
		if !nodeScopeAllowed(r.node[id], allowed) {
			continue
		}
		dl := float64(r.docLen[id])
		score := 0.0
		for _, term := range qterms {
			f, ok := tf[term]
			if !ok {
				continue
			}
			n := float64(r.df[term])
			idf := math.Log(1 + (r.n-n+0.5)/(n+0.5))
			tff := float64(f)
			norm := tff * (bm25K1 + 1) / (tff + bm25K1*(1-bm25B+bm25B*dl/r.avgLen))
			score += idf * norm
		}
		if score > 0 {
			out = append(out, ScoredNode{Node: r.node[id], Score: score})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Node.ID < out[j].Node.ID
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// nodeScopeAllowed reports whether a node may be scored for a caller holding the
// allowed-read set. nil allowed = unrestricted. A missing node or a node with no
// scope attr falls through to vault.ScopeAllowsCSV, whose unlabeled fail-safe is
// dev-only, so this can never be more permissive than the shared predicate.
func nodeScopeAllowed(n *Node, allowed map[string]bool) bool {
	if allowed == nil {
		return true
	}
	var csv string
	if n != nil {
		if s, ok := n.Attrs["scope"].(string); ok {
			csv = s
		}
	}
	return vault.ScopeAllowsCSV(csv, allowed)
}

func nodeText(n *Node) []string {
	var toks []string
	lbl := Tokenize(n.Label)
	for i := 0; i < labelWeight; i++ {
		toks = append(toks, lbl...)
	}
	for _, v := range n.Attrs {
		if s, ok := v.(string); ok {
			toks = append(toks, Tokenize(s)...)
		}
	}
	return toks
}

// Tokenize lowercases and splits on non-alphanumeric runs, dropping tokens
// shorter than 2 runes and stopwords. The unicode boundaries match FTS5's
// unicode61 tokenizer so the two keyword signals share term shape.
func Tokenize(s string) []string {
	var out []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			tok := cur.String()
			if len([]rune(tok)) >= 2 && !stopwords[tok] {
				out = append(out, tok)
			}
			cur.Reset()
		}
	}
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			cur.WriteRune(r)
			continue
		}
		flush()
	}
	flush()
	return out
}

// MaxQueryTerms bounds how many distinct terms one query may contribute to a
// keyword signal. Both signals cost time linear in the term count (FTS5 evaluates
// one phrase per term, graph-BM25 walks the corpus per term), so an uncapped query
// is an uncapped amount of work on every surface that takes one. 64 is generous:
// no real question, in English or Swedish, carries more than a couple of dozen
// content words once stopwords are dropped.
const MaxQueryTerms = 64

// TokenizeQuery is Tokenize for the QUERY side of retrieval. It additionally drops
// repeated terms and truncates at MaxQueryTerms.
//
// Deduplicating first matters as much as the cap: a query that repeats one word ten
// thousand times collapses to a single term instead of filling the whole budget with
// copies of itself, and repeating a phrase in an OR expression never changed which
// notes matched, only how long the match took.
//
// Every caller that turns user text into search terms must use this rather than
// Tokenize: Tokenize is the CORPUS-side tokenizer, where the input is a note Mesh
// already read from disk, not something a stranger types.
func TokenizeQuery(s string) []string {
	toks := Tokenize(s)
	seen := make(map[string]bool, len(toks))
	out := make([]string, 0, MaxQueryTerms)
	for _, t := range toks {
		if seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
		if len(out) == MaxQueryTerms {
			break
		}
	}
	return out
}

func termFreq(tokens []string) map[string]int {
	m := make(map[string]int, len(tokens))
	for _, t := range tokens {
		m[t]++
	}
	return m
}

// stopwords: English plus common Swedish function words, since
// the vault is bilingual.
var stopwords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "that": true,
	"this": true, "from": true, "into": true, "are": true, "was": true,
	"but": true, "not": true, "you": true,
	"och": true, "att": true, "det": true, "som": true, "en": true,
	"ett": true, "för": true, "med": true, "den": true, "har": true,
	"inte": true, "om": true, "till": true, "av": true, "är": true,
	"på": true, "de": true, "vi": true, "kan": true, "ska": true,
}
