// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

// Package relate derives the `related:` links a note should carry, using Mesh's own
// fused retrieval as the "what is this near" engine.
//
// It exists because `related` is optional on the write path, and a note written
// without it lands with no note-to-note edge at all: reachable by full-text search,
// invisible to graph proximity, and drawn as a disconnected dot in the web app. On
// 2026-08-06 that was 111 of 1140 notes in the reference vault, and they were not
// junk notes: 67 gotchas, 31 decisions and 13 post-mortems, which is exactly the
// tier-0 material retrieval is supposed to surface first.
package relate

import (
	"context"
	"strings"

	"github.com/bright-interaction/mesh/internal/graph"
	"github.com/bright-interaction/mesh/internal/retrieve"
)

// Derive returns up to max note ids for a note's `related:` frontmatter, best first.
//
// Policy: accept the top hit unconditionally, then accept a lower-ranked hit only if
// it shares a tag with the source note. Measured across the 111 orphans backfilled on
// 2026-08-06, a proposed link shared a tag with its note 78% of the time at rank 1,
// 68% at rank 2 and 56% at rank 3. An ungated top-3 therefore staples a coin-flip
// edge onto every note, and a wrong edge is not inert here: fused retrieval expands
// one hop along links, so it donates score to an unrelated note on every later query
// that touches either end. A missing link only costs the graph a line.
//
// selfID is skipped so a note never links to itself (the backfill path queries with
// the note's own title, which retrieves that note first every time). Pass "" when the
// note does not exist yet, as on the write path.
func Derive(ctx context.Context, rt *retrieve.Retriever, g *graph.Graph, query, selfID string, selfTags []string, max int) []string {
	if rt == nil || max <= 0 || strings.TrimSpace(query) == "" {
		return nil
	}
	// Over-fetch: the self hit, duplicates and tag-gate rejections all come out of
	// this pool, so asking for exactly max would routinely return fewer.
	cards, err := rt.Retrieve(ctx, query, retrieve.Options{Limit: max*4 + 4})
	if err != nil {
		return nil
	}
	return Select(cards, func(id string) []string { return TagsOf(g, id) }, selfID, selfTags, max)
}

// Select applies the ranking policy to already-retrieved cards. Split out from Derive
// so the policy is testable without standing up a store, an index and a retriever.
func Select(cards []retrieve.Card, tagsFor func(noteID string) []string, selfID string, selfTags []string, max int) []string {
	if max <= 0 {
		return nil
	}
	want := tagSet(selfTags)
	seen := map[string]bool{selfID: true}
	var out []string
	for _, c := range cards {
		id := strings.TrimSpace(c.NoteID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		// The first survivor is the top hit: take it on rank alone. Everything after
		// it has to earn the edge by sharing a tag.
		if len(out) > 0 && !sharesTag(want, tagSet(tagsFor(id))) {
			continue
		}
		out = append(out, id)
		if len(out) >= max {
			break
		}
	}
	return out
}

// TagsOf reads a note's tags off the built graph (the `tagged` edges), so the caller
// does not have to re-read and re-parse frontmatter it already indexed.
func TagsOf(g *graph.Graph, noteID string) []string {
	if g == nil || noteID == "" {
		return nil
	}
	var tags []string
	for _, e := range g.Neighbors("note:" + noteID) {
		if e.Relation == "tagged" {
			tags = append(tags, strings.TrimPrefix(e.Target, "tag:"))
		}
	}
	return tags
}

func tagSet(tags []string) map[string]bool {
	if len(tags) == 0 {
		return nil
	}
	m := make(map[string]bool, len(tags))
	for _, t := range tags {
		if t = strings.ToLower(strings.TrimSpace(t)); t != "" {
			m[t] = true
		}
	}
	return m
}

// sharesTag reports whether the two sets intersect. An untagged note on either side
// cannot intersect anything, so it gets the top hit and nothing else, which is the
// conservative outcome we want when there is no second signal to check.
func sharesTag(a, b map[string]bool) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	if len(b) < len(a) {
		a, b = b, a
	}
	for t := range a {
		if b[t] {
			return true
		}
	}
	return false
}
