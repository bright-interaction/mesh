// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package graph

import (
	"context"
	"math"
	"sort"
	"strings"
	"unicode"

	"github.com/bright-interaction/mesh/internal/vault"
	"golang.org/x/text/unicode/norm"
)

// BM25 parameters. labelWeight repeats
// a node's label tokens so a title match outranks a body match.
const (
	bm25K1      = 1.5
	bm25B       = 0.75
	labelWeight = 2
)

// MatchesFullTitle recognizes explicit navigation without query tokenization:
// preserve punctuation, stopwords, version components and the complete suffix.
// Only case and outer whitespace are ignored. Shared by candidate selection and
// final retrieval so SQLite's ASCII-only NOCASE cannot disagree with Go.
func MatchesFullTitle(query, title string) bool {
	query = strings.TrimSpace(query)
	return query != "" && strings.EqualFold(query, strings.TrimSpace(title))
}

// ScoredNode pairs a node with its BM25 relevance (higher is better).
type ScoredNode struct {
	Node  *Node
	Score float64
}

// Ranker precomputes corpus statistics over the scorable (note) nodes once, so
// per-query scoring is O(queryTerms x candidates) instead of O(corpus) per call. Rebuild it whenever the graph changes.
type Ranker struct {
	node   map[string]*Node
	docs   map[string]*rankerDocument
	df     map[string]int
	avgLen float64
	n      float64
}

// A document owns immutable searchable inputs and term counts. In particular,
// it does not retain a Node or its mutable Attrs map: comparing an old Node to
// itself after an in-place edit could otherwise bless stale term counts.
type rankerDocument struct {
	label  string
	attrs  map[string]string
	tf     map[string]int
	length int
}

func (d *rankerDocument) matches(n *Node) bool {
	if d.label != n.Label {
		return false
	}
	count := 0
	for key, value := range n.Attrs {
		if key == "superseded_by" {
			continue
		}
		if text, ok := value.(string); ok {
			previous, found := d.attrs[key]
			if !found || previous != text {
				return false
			}
			count++
		}
	}
	return count == len(d.attrs)
}

func newRankerDocument(n *Node) *rankerDocument {
	tokens := nodeText(n)
	d := &rankerDocument{label: n.Label, tf: termFreq(tokens), length: len(tokens)}
	for key, value := range n.Attrs {
		if text, ok := value.(string); ok && key != "superseded_by" {
			if d.attrs == nil {
				d.attrs = make(map[string]string)
			}
			d.attrs[key] = text
		}
	}
	return d
}

// NewRanker builds the inverted statistics over every note node's label+attrs.
func (g *Graph) NewRanker() *Ranker {
	r, _ := g.NewRankerContext(context.Background())
	return r
}

// NewRankerContext is NewRanker with cooperative cancellation. The ranker's
// corpus maps remain private until the whole pass succeeds, so a caller can
// never observe partially built statistics.
func (g *Graph) NewRankerContext(ctx context.Context) (*Ranker, error) {
	return g.NewRankerReusingContext(ctx, nil)
}

// NewRankerReusingContext skips tokenization only when exact searchable inputs
// match a previous ranker's private immutable document. Every node pointer and
// corpus statistic is still rebuilt from this graph, including scope, document
// frequency and average length after edits/deletions. A nil previous ranker is
// a full build. Neither the previous ranker nor graph is mutated or retained.
func (g *Graph) NewRankerReusingContext(ctx context.Context, previous *Ranker) (*Ranker, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := contextCause(ctx); err != nil {
		return nil, err
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	r := &Ranker{
		node: map[string]*Node{},
		docs: map[string]*rankerDocument{},
		df:   map[string]int{},
	}
	total := 0
	for id, nd := range g.nodes {
		if err := contextCause(ctx); err != nil {
			return nil, err
		}
		if nd.Kind != "note" {
			continue
		}
		var doc *rankerDocument
		if previous != nil {
			if old := previous.docs[id]; old != nil && old.matches(nd) {
				doc = old
			}
		}
		if doc == nil {
			doc = newRankerDocument(nd)
		}
		r.node[id] = nd
		r.docs[id] = doc
		total += doc.length
		for term := range doc.tf {
			if err := contextCause(ctx); err != nil {
				return nil, err
			}
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
	if err := contextCause(ctx); err != nil {
		return nil, err
	}
	return r, nil
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
	for id, doc := range r.docs {
		if !nodeScopeAllowed(r.node[id], allowed) {
			continue
		}
		dl := float64(doc.length)
		score := 0.0
		for _, term := range qterms {
			f, ok := doc.tf[term]
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
	for key, v := range n.Attrs {
		// superseded_by is a relationship/receipt field, not searchable prose. If a
		// secret-scoped correction retires a public note, indexing its id into the
		// public target lets a scoped caller search that id and observe a graph hit even
		// though it cannot read the correction. Retrieval exposes and scores this field
		// only after checking the superseding note's current read boundaries.
		if key == "superseded_by" {
			continue
		}
		if s, ok := v.(string); ok {
			toks = append(toks, Tokenize(s)...)
		}
	}
	return toks
}

// Tokenize lowercases and splits on non-alphanumeric runs, dropping tokens
// shorter than 2 runes and stopwords. Bounded, complete dotted versions are also
// retained as literal terms: otherwise v0.41.1 and v0.41.2 both become [v0, 41].
// FTS5 consumes those additional terms as quoted token phrases, not operators.
func Tokenize(s string) []string {
	s = strings.ToLower(norm.NFC.String(s))
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
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			cur.WriteRune(r)
			continue
		}
		flush()
	}
	flush()
	return appendVersionLiterals(out, s)
}

// MaxQueryTerms bounds query work: one unit per ordinary distinct term and one
// per component of an additional version phrase. Both keyword signals consume
// these bounded terms, so an uncapped query
// is an uncapped amount of work on every surface that takes one. 64 is generous:
// no real question, in English or Swedish, carries more than a couple of dozen
// content words once stopwords are dropped.
const MaxQueryTerms = 64

// TokenizeQuery is Tokenize for the QUERY side of retrieval. It additionally drops
// repeated terms and truncates at MaxQueryTerms work units.
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
	work := 0
	for _, t := range toks {
		if seen[t] {
			continue
		}
		// A quoted version is one OR phrase but several FTS tokens. Charge each
		// component against the same work cap; compound input cannot evade it.
		cost := literalTokenCount(t)
		if work+cost > MaxQueryTerms {
			break
		}
		work += cost
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
