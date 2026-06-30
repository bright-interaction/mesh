// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Bright Interaction AB

package index

import "testing"

// openVecStore opens a temp store for the vector tests.
func openVecStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestReplaceVectorsRoundTripDim(t *testing.T) {
	// LoadVectors JOINs to notes and filters by note_hash, so the notes must exist
	// and the stored note_hash must match for the vectors to come back.
	s, hashA, hashB := indexTwoNotes(t)
	rows := []VectorRow{
		{NodeID: "note:a", ChunkIx: 0, Vec: []float32{1, 0, 0, 0}, NoteHash: hashA},
		{NodeID: "note:b", ChunkIx: 0, Vec: []float32{0, 1, 0, 0}, NoteHash: hashB},
		{NodeID: "note:b", ChunkIx: 1, Vec: []float32{0, 0, 1, 0}, NoteHash: hashB},
	}
	if err := s.ReplaceVectors("test-model", rows); err != nil {
		t.Fatalf("ReplaceVectors: %v", err)
	}
	model, dim, byNode, err := s.LoadVectors()
	if err != nil {
		t.Fatalf("LoadVectors: %v", err)
	}
	if model != "test-model" {
		t.Errorf("model = %q, want test-model", model)
	}
	if dim != 4 {
		t.Errorf("dim = %d, want 4 (must round-trip through meta)", dim)
	}
	if len(byNode["note:a"]) != 1 || len(byNode["note:b"]) != 2 {
		t.Errorf("chunk grouping wrong: a=%d b=%d", len(byNode["note:a"]), len(byNode["note:b"]))
	}
}

// TestReplaceVectorsRaggedDimErrors is the V.3 write-side guard: a batch with mixed
// widths must be refused before it reaches storage, where it would later cosine
// across incompatible dimensions.
func TestReplaceVectorsRaggedDimErrors(t *testing.T) {
	s := openVecStore(t)
	rows := []VectorRow{
		{NodeID: "note:a", ChunkIx: 0, Vec: []float32{1, 0, 0}},
		{NodeID: "note:b", ChunkIx: 0, Vec: []float32{0, 1}}, // width 2 != 3
	}
	if err := s.ReplaceVectors("test-model", rows); err == nil {
		t.Fatal("ReplaceVectors must error on a ragged-dim batch")
	}
	// Nothing should have been written (the dim check runs before the write tx).
	_, _, byNode, err := s.LoadVectors()
	if err != nil {
		t.Fatalf("LoadVectors after rejected write: %v", err)
	}
	if len(byNode) != 0 {
		t.Errorf("a rejected ragged batch must not persist any vectors, got %d nodes", len(byNode))
	}
}

func TestCachedVectorsHashHit(t *testing.T) {
	s := openVecStore(t)
	h := ContentHash("search_document: ", "the storage engine note")
	rows := []VectorRow{
		{NodeID: "note:a", ChunkIx: 0, Vec: []float32{1, 0, 0, 0}, ContentHash: h},
		{NodeID: "note:b", ChunkIx: 0, Vec: []float32{0, 1, 0, 0}, ContentHash: "other"},
	}
	if err := s.ReplaceVectors("m1", rows); err != nil {
		t.Fatal(err)
	}
	// Same model -> cache returns the rows keyed by VecKey, with their hashes.
	cache, err := s.CachedVectors("m1")
	if err != nil {
		t.Fatal(err)
	}
	c, ok := cache[VecKey("note:a", 0)]
	if !ok {
		t.Fatal("note:a/0 should be cached")
	}
	if c.Hash != h {
		t.Errorf("cached hash = %q, want %q", c.Hash, h)
	}
	if len(c.Vec) != 4 || c.Vec[0] != 1 {
		t.Errorf("cached vec wrong: %v", c.Vec)
	}
	// A different model invalidates the whole cache (vectors are not reusable).
	other, err := s.CachedVectors("m2")
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 0 {
		t.Errorf("a different model must yield an empty cache, got %d entries", len(other))
	}
}

// TestCachedVectorsSkipsBlankHash: a row with no recorded hash (e.g. from before
// the cache existed) is not reusable, so it must not appear in the cache.
func TestCachedVectorsSkipsBlankHash(t *testing.T) {
	s := openVecStore(t)
	if err := s.ReplaceVectors("m1", []VectorRow{
		{NodeID: "note:a", ChunkIx: 0, Vec: []float32{1, 0}, ContentHash: ""},
	}); err != nil {
		t.Fatal(err)
	}
	cache, err := s.CachedVectors("m1")
	if err != nil {
		t.Fatal(err)
	}
	if len(cache) != 0 {
		t.Errorf("a blank-hash row must not be cached, got %d", len(cache))
	}
}

func TestContentHashStableAndSeparated(t *testing.T) {
	if ContentHash("a", "b") != ContentHash("a", "b") {
		t.Error("ContentHash must be deterministic")
	}
	// NUL-join: "a","b" must not collide with "ab","".
	if ContentHash("a", "b") == ContentHash("ab", "") {
		t.Error("ContentHash must separate parts so distinct splits do not collide")
	}
}

// indexTwoNotes indexes a.md + b.md and returns the store plus each note's current
// retrieval hash, so a vector test can store matching (live) or mismatched (stale)
// note_hash values.
func indexTwoNotes(t *testing.T) (s *Store, hashA, hashB string) {
	t.Helper()
	a, err := Parse("a.md", []byte("---\nid: a\ntype: note\nwhen: 2026-01-01\n---\n# A\nstorage body\n"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Parse("b.md", []byte("---\nid: b\ntype: note\nwhen: 2026-01-01\n---\n# B\nother body\n"))
	if err != nil {
		t.Fatal(err)
	}
	notes := []*ParsedNote{a, b}
	g, _ := BuildGraph(notes)
	s = openVecStore(t)
	if _, err := s.IndexVault(notes, g); err != nil {
		t.Fatal(err)
	}
	return s, RetrievalHash(a), RetrievalHash(b)
}

// TestLoadVectorsExcludesStaleAndOrphan is the V.6 drift fix: LoadVectors returns
// only vectors whose note exists and is unchanged since embed.
func TestLoadVectorsExcludesStaleAndOrphan(t *testing.T) {
	s, hashA, _ := indexTwoNotes(t)
	if err := s.ReplaceVectors("m1", []VectorRow{
		{NodeID: "note:a", ChunkIx: 0, Vec: []float32{1, 0}, NoteHash: hashA},        // live
		{NodeID: "note:b", ChunkIx: 0, Vec: []float32{0, 1}, NoteHash: "STALE"},      // note exists but edited since embed
		{NodeID: "note:ghost", ChunkIx: 0, Vec: []float32{1, 1}, NoteHash: "ORPHAN"}, // note deleted since embed
	}); err != nil {
		t.Fatal(err)
	}
	_, _, byNode, err := s.LoadVectors()
	if err != nil {
		t.Fatal(err)
	}
	if len(byNode) != 1 {
		t.Fatalf("LoadVectors should return only the live vector, got %d nodes: %v", len(byNode), keys(byNode))
	}
	if _, ok := byNode["note:a"]; !ok {
		t.Error("the live note:a vector must be present")
	}

	total, live, staleOrOrphan, err := s.VectorStats()
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 || live != 1 || staleOrOrphan != 2 {
		t.Errorf("VectorStats = total %d live %d staleOrOrphan %d; want 3/1/2", total, live, staleOrOrphan)
	}
}

// TestIndexVaultPrunesOrphanVectors: a reindex physically removes vectors whose
// note no longer exists (but keeps stale-but-existing ones for in-place refresh).
func TestIndexVaultPrunesOrphanVectors(t *testing.T) {
	s, hashA, _ := indexTwoNotes(t)
	if err := s.ReplaceVectors("m1", []VectorRow{
		{NodeID: "note:a", ChunkIx: 0, Vec: []float32{1, 0}, NoteHash: hashA},
		{NodeID: "note:b", ChunkIx: 0, Vec: []float32{0, 1}, NoteHash: "STALE"},
		{NodeID: "note:ghost", ChunkIx: 0, Vec: []float32{1, 1}, NoteHash: "ORPHAN"},
	}); err != nil {
		t.Fatal(err)
	}
	// Reindex with only a + b: note:ghost is orphaned and must be pruned; note:b is
	// stale-but-existing and must survive (it is refreshed in place on the next embed).
	a, _ := Parse("a.md", []byte("---\nid: a\ntype: note\nwhen: 2026-01-01\n---\n# A\nstorage body\n"))
	b, _ := Parse("b.md", []byte("---\nid: b\ntype: note\nwhen: 2026-01-01\n---\n# B\nother body\n"))
	g, _ := BuildGraph([]*ParsedNote{a, b})
	if _, err := s.IndexVault([]*ParsedNote{a, b}, g); err != nil {
		t.Fatal(err)
	}
	total, _, _, err := s.VectorStats()
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 {
		t.Errorf("after reindex, orphan must be pruned and stale kept: total = %d, want 2", total)
	}
}

func keys(m map[string][][]float32) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestLoadVectorsEmpty(t *testing.T) {
	s := openVecStore(t)
	model, dim, byNode, err := s.LoadVectors()
	if err != nil {
		t.Fatalf("LoadVectors on empty store: %v", err)
	}
	if model != "" || dim != 0 || len(byNode) != 0 {
		t.Errorf("empty store should yield zero values, got model=%q dim=%d nodes=%d", model, dim, len(byNode))
	}
}
