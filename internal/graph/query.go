package graph

import (
	"math"
	"sort"
	"strings"
	"unicode"
)

// BM25 parameters (ported from arachne knowledge/search.go). labelWeight repeats
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
// per-query scoring is O(queryTerms x candidates) instead of arachne's
// O(corpus)-per-call. Rebuild it whenever the graph changes.
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
// deterministic tiebreak.
func (r *Ranker) Score(query string, limit int) []ScoredNode {
	qterms := Tokenize(query)
	if len(qterms) == 0 {
		return nil
	}
	var out []ScoredNode
	for id, tf := range r.tf {
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

func termFreq(tokens []string) map[string]int {
	m := make(map[string]int, len(tokens))
	for _, t := range tokens {
		m[t]++
	}
	return m
}

// stopwords: English (from arachne) plus common Swedish function words, since
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
