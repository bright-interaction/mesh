// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package eval

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/retrieve"
)

// conceptEmbedder is a deterministic, network-free embedder that maps text to a
// CONCEPT vector by keyword, not by token. Unlike the bag-of-words Stub it can
// model paraphrase: "persistence" and "storage" land on the same concept axis even
// though they share no tokens, so a vector query matches a lexically-disjoint note.
// It exists only to prove the vec arm adds paraphrase recall, never retrieval
// quality on real corpora.
type conceptEmbedder struct{}

var concepts = [][]string{
	{"sqlite", "storage", "engine", "persistence", "database", "durable", "disk"},
	{"marketing", "copy", "campaign", "brand", "promo"},
}

func (conceptEmbedder) Model() string { return "concept" }
func (conceptEmbedder) Dim() int      { return len(concepts) }
func (conceptEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		low := strings.ToLower(t)
		v := make([]float32, len(concepts))
		for c, kws := range concepts {
			for _, kw := range kws {
				if strings.Contains(low, kw) {
					v[c] = 1
					break
				}
			}
		}
		var n float64
		for _, x := range v {
			n += float64(x) * float64(x)
		}
		if n > 0 {
			inv := float32(1 / math.Sqrt(n))
			for j := range v {
				v[j] *= inv
			}
		}
		out[i] = v
	}
	return out, nil
}

// TestVectorArmAddsParaphraseRecall is the V.8 regression: a paraphrase query that
// shares NO tokens with the relevant note is missed by the lexical arm but found by
// the vector arm. If the vec signal ever stops being fused, this test fails.
func TestVectorArmAddsParaphraseRecall(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body string) *index.ParsedNote {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		pn, err := index.Parse(rel, []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		return pn
	}
	notes := []*index.ParsedNote{
		write("a.md", "---\nid: storage\ntype: note\nwhen: 2026-01-01\n---\n# Engine\nthe engine keeps widgets on sqlite\n"),
		write("b.md", "---\nid: promo\ntype: note\nwhen: 2026-01-01\n---\n# Promo\nmarketing copy for the campaign brand\n"),
		write("c.md", "---\nid: misc\ntype: note\nwhen: 2026-01-01\n---\n# Misc\nrandom gizmo notes about nothing\n"),
	}
	g, _ := index.BuildGraph(notes)
	s, err := index.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.IndexVault(notes, g); err != nil {
		t.Fatal(err)
	}
	lg, _ := s.LoadGraph()

	// A paraphrase: none of these tokens appear in the storage note.
	const query = "durable persistence database"

	// Lexical-only arm misses it.
	lexical := retrieve.New(s, lg)
	lc, err := lexical.Retrieve(context.Background(), query, retrieve.Options{Limit: 10, NoRerank: true})
	if err != nil {
		t.Fatal(err)
	}
	if topIs(lc, "note:storage") {
		t.Fatal("precondition: the lexical arm should NOT answer the paraphrase with note:storage")
	}

	// Vector arm finds it. Embed each note's chunk text with the concept embedder.
	emb := conceptEmbedder{}
	vecs := map[string][][]float32{}
	for _, nf := range mustNoteFiles(t, s) {
		pn, err := index.ParseFile(filepath.Join(dir, nf.Path))
		if err != nil {
			t.Fatal(err)
		}
		ev, err := emb.Embed(context.Background(), []string{strings.Join(index.ChunkText(pn), "\n")})
		if err != nil {
			t.Fatal(err)
		}
		vecs[nf.NodeID] = ev
	}
	vector := retrieve.New(s, lg)
	if !vector.EnableVectors(emb, "concept", emb.Dim(), vecs) {
		t.Fatal("EnableVectors should succeed for the concept embedder")
	}
	vc, err := vector.Retrieve(context.Background(), query, retrieve.Options{Limit: 10, WeightFTS: 0.2, WeightGraph: 0.1, WeightVec: 0.7, NoRerank: true})
	if err != nil {
		t.Fatal(err)
	}
	if !topIs(vc, "note:storage") {
		t.Fatalf("the vector arm should answer the paraphrase with note:storage, got top %v", topID(vc))
	}
}

func topIs(cards []retrieve.Card, id string) bool { return len(cards) > 0 && cards[0].NodeID == id }
func topID(cards []retrieve.Card) string {
	if len(cards) == 0 {
		return "(none)"
	}
	return cards[0].NodeID
}

func mustNoteFiles(t *testing.T, s *index.Store) []index.NoteFile {
	t.Helper()
	nf, err := s.NoteFiles()
	if err != nil {
		t.Fatal(err)
	}
	return nf
}

func TestRunGateBeatsBaselineOnLongNotes(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body string) *index.ParsedNote {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		pn, err := index.Parse(rel, []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		return pn
	}
	bigProse := strings.Repeat("modernc sqlite storage engine decision prose paragraph. ", 200)
	notes := []*index.ParsedNote{
		write("a.md", "---\nid: storage\ntype: decision\nwhen: 2026-01-01\ndo: x\ndont: y\nwhy: use modernc sqlite for storage\n---\n# Storage\n"+bigProse),
		write("b.md", "---\nid: other1\ntype: note\nwhen: 2026-01-01\n---\n# Other1\n"+bigProse),
		write("c.md", "---\nid: other2\ntype: note\nwhen: 2026-01-01\n---\n# Other2\n"+bigProse),
	}
	g, _ := index.BuildGraph(notes)
	s, err := index.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.IndexVault(notes, g); err != nil {
		t.Fatal(err)
	}
	lg, _ := s.LoadGraph()
	r := retrieve.New(s, lg)

	rep := RunGate(s, r, dir, []Case{{Query: "modernc sqlite storage", Relevant: []string{"storage"}}}, 400)
	if rep.MeshSurfaced != 1 {
		t.Errorf("mesh should surface the relevant note, got %d/%d", rep.MeshSurfaced, rep.N)
	}
	// Honest claim: on long notes, reading Mesh's cards + one body costs less than
	// naively reading the top-3 full bodies.
	if rep.MeshMedian >= rep.FTSTop3Median {
		t.Errorf("mesh median (%.0f) should beat naive read-top-3 (%.0f) on long notes", rep.MeshMedian, rep.FTSTop3Median)
	}
	// The matched single-body baseline must be tracked (the fix the review demanded).
	if rep.FTSTop1Median <= 0 {
		t.Errorf("matched fts-top1 baseline should be measured, got %.0f", rep.FTSTop1Median)
	}
}
