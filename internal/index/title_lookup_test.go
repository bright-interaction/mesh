// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

type titleLookupFixtureNote struct {
	id, path, title, scope, body string
}

func titleLookupFixture(t *testing.T, src []titleLookupFixtureNote) *Store {
	t.Helper()
	notes := make([]*ParsedNote, 0, len(src))
	for _, n := range src {
		if n.path == "" {
			n.path = n.id + ".md"
		}
		if n.scope == "" {
			n.scope = "dev"
		}
		body := fmt.Sprintf("---\nid: %s\ntype: note\ntitle: %q\nscope: %s\n---\n# %s\n%s\n", n.id, n.title, n.scope, n.title, n.body)
		pn, err := Parse(n.path, []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		notes = append(notes, pn)
	}
	g, _ := BuildGraph(notes)
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, err := s.IndexVault(notes, g); err != nil {
		t.Fatal(err)
	}
	return s
}

// Keep the original BM25 ordering as an independent control. This intentionally
// does not call the title-matching helper or the production search query.
func titleLookupRawOrder(t *testing.T, s *Store, query string) []SearchHit {
	t.Helper()
	rows, err := s.readDB.QueryContext(context.Background(), `
SELECT node_id, -bm25(search_index)
FROM search_index WHERE search_index MATCH ?
ORDER BY bm25(search_index), node_id`, buildFTS5Query(query))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var hits []SearchHit
	for rows.Next() {
		var h SearchHit
		if err := rows.Scan(&h.NodeID, &h.Score); err != nil {
			t.Fatal(err)
		}
		hits = append(hits, h)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return hits
}

func titleLookupCompetition(title string) []titleLookupFixtureNote {
	return []titleLookupFixtureNote{
		{id: "a-warning", title: "Reader activation warning", body: strings.Repeat(title+". ", 3)},
		{id: "b-warning", title: "Reader activation caveat", body: strings.Repeat(title+". ", 3)},
		{id: "z-receipt", title: title, body: strings.Repeat("Verified executable checksum and preserved configuration. ", 30)},
	}
}

func TestSearchFullTitleSurvivesCandidateCutoff(t *testing.T) {
	const query = "Orbit v1.2.3 activated for local reader only"
	s := titleLookupFixture(t, titleLookupCompetition(query))
	raw := titleLookupRawOrder(t, s, query)
	if len(raw) != 3 || raw[2].NodeID != "note:z-receipt" {
		t.Fatalf("fixture must put exact title below both repeated-body distractors: %+v", raw)
	}
	// Exercise a separately opened read-only pool too: a SQL matching function
	// registered only on the writer or its first connection is not sufficient.
	reader, err := OpenReadOnly(filepath.Dir(s.MeshDir()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	for _, store := range []*Store{s, reader} {
		for _, limit := range []int{1, 2} {
			hits, err := store.Search(context.Background(), query, limit)
			if err != nil {
				t.Fatal(err)
			}
			if len(hits) != limit || hits[0].NodeID != "note:z-receipt" {
				t.Fatalf("readOnly=%t limit=%d: exact title lost before truncation: %+v", store.readOnly, limit, hits)
			}
			if hits[0].Score != raw[2].Score {
				t.Fatalf("navigation priority rewrote raw relevance: got %g want %g", hits[0].Score, raw[2].Score)
			}
			if limit == 2 && (hits[1].NodeID != raw[0].NodeID || hits[1].Score != raw[0].Score) {
				t.Fatalf("strongest warning must remain with its original relevance: %+v", hits)
			}
		}
	}
}

func TestSearchFullTitleUsesUnicodeCaseAndOuterWhitespace(t *testing.T) {
	for _, tc := range []struct{ title, query string }{
		{"Ångström v1.2.3: reader activated", "ÅNGSTRÖM v1.2.3: READER ACTIVATED"},
		{"Ångström v1.2.3: reader activated", " \tångström v1.2.3: reader activated\n"},
		{"Orbit v1.2.3-rc.1+build.2: reader's activation", "Orbit v1.2.3-rc.1+build.2: reader's activation"},
	} {
		t.Run(tc.query, func(t *testing.T) {
			s := titleLookupFixture(t, titleLookupCompetition(tc.title))
			hits, err := s.Search(context.Background(), tc.query, 1)
			if err != nil {
				t.Fatal(err)
			}
			if len(hits) != 1 || hits[0].NodeID != "note:z-receipt" {
				t.Fatalf("full Unicode title was not selected: %+v", hits)
			}
		})
	}
}

func TestSearchFullTitleDoesNotCollapseVersionOrPunctuation(t *testing.T) {
	const title = "Orbit v1.2.3: reader activated"
	s := titleLookupFixture(t, titleLookupCompetition(title))
	for _, query := range []string{
		"Orbit v1.2.30: reader activated",
		"Orbit v1.2.3-rc.1: reader activated",
		"Orbit v1.2.3+build.1: reader activated",
		"Orbit v1.2.3 reader activated",
		"Orbit v1.2.3: reader  activated",
		"please show " + title,
		title + `" OR "secret`,
	} {
		t.Run(query, func(t *testing.T) {
			raw := titleLookupRawOrder(t, s, query)
			if len(raw) != 3 || raw[0].NodeID == "note:z-receipt" {
				t.Fatalf("near-match control must prefer a distractor: %+v", raw)
			}
			for _, limit := range []int{1, 2} {
				hits, err := s.Search(context.Background(), query, limit)
				if err != nil {
					t.Fatal(err)
				}
				if len(hits) != limit {
					t.Fatalf("limit=%d returned %+v", limit, hits)
				}
				for i := range hits {
					if hits[i].NodeID != raw[i].NodeID || hits[i].Score != raw[i].Score {
						t.Fatalf("nonidentical full title changed BM25 order: got %+v want %+v", hits, raw[:limit])
					}
				}
			}
		})
	}
}

func TestSearchFullTitleRespectsScopeAndPathBeforeLimit(t *testing.T) {
	const query = "Orbit v1.2.3 activated for local reader only"
	s := titleLookupFixture(t, []titleLookupFixtureNote{
		{id: "a-private", path: "allowed/private-exact.md", title: query, scope: "private"},
		{id: "b-fenced", path: "fenced/exact.md", title: query},
		{id: "z-allowed", path: "allowed/exact.md", title: query, body: strings.Repeat("Verified executable checksum and preserved configuration. ", 30)},
		{id: "warning", path: "allowed/warning.md", title: "Reader activation warning", body: strings.Repeat(query+". ", 3)},
	})
	for _, limit := range []int{1, 2} {
		scoped, err := s.SearchScoped(context.Background(), query, limit, map[string]bool{"dev": true})
		if err != nil {
			t.Fatal(err)
		}
		if len(scoped) != limit || scoped[0].NodeID != "note:b-fenced" {
			t.Fatalf("private exact title consumed SQL limit=%d: %+v", limit, scoped)
		}
		for _, hit := range scoped {
			if hit.NodeID == "note:a-private" {
				t.Fatalf("private exact title crossed scope boundary: %+v", hit)
			}
		}
		hits, err := s.SearchScopedPaths(context.Background(), query, limit, map[string]bool{"dev": true}, func(path string) bool {
			return strings.HasPrefix(path, "allowed/")
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != limit || hits[0].NodeID != "note:z-allowed" {
			t.Fatalf("forbidden exact titles consumed limit=%d: %+v", limit, hits)
		}
		for _, hit := range hits {
			if !strings.HasPrefix(hit.Path, "allowed/") || (hit.NodeID != "note:z-allowed" && hit.NodeID != "note:warning") {
				t.Fatalf("scope/path boundary leaked an exact-title candidate: %+v", hit)
			}
		}
	}
}

func TestSearchFullTitleUsesCurrentNotesTitle(t *testing.T) {
	const query = "Orbit v1.2.3 activated for local reader only"
	s := titleLookupFixture(t, []titleLookupFixtureNote{
		{id: "a-stale", title: query, body: strings.Repeat(query+". ", 3)},
		{id: "z-current", title: "Different indexed title", body: query + " " + strings.Repeat("Verified executable checksum and preserved configuration. ", 30)},
	})
	if err := s.Write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE notes SET title = CASE id WHEN 'a-stale' THEN 'Renamed current title' ELSE ? END WHERE id IN ('a-stale', 'z-current')`, query)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	hits, err := s.Search(context.Background(), query, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].NodeID != "note:z-current" {
		t.Fatalf("stale search_index title controlled navigation priority: %+v", hits)
	}
}

func TestSearchFullTitleKeepsOrdinaryQueryOrder(t *testing.T) {
	s := titleLookupFixture(t, titleLookupCompetition("Orbit v1.2.3 activated for local reader only"))
	for _, query := range []string{"reader activation", "verified configuration", "reader warning"} {
		raw := titleLookupRawOrder(t, s, query)
		hits, err := s.Search(context.Background(), query, 20)
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != len(raw) {
			t.Fatalf("ordinary query %q changed recall: %+v vs %+v", query, hits, raw)
		}
		for i := range hits {
			if hits[i].NodeID != raw[i].NodeID || hits[i].Score != raw[i].Score {
				t.Fatalf("ordinary query %q changed BM25 order or scores: %+v vs %+v", query, hits, raw)
			}
		}
	}
}
