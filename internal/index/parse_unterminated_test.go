// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"errors"
	"testing"

	"github.com/bright-interaction/mesh/internal/vault"
)

// decisions/flare-coldtier-duckdb-attach-postgres-parquet-export-then-drop.md in the live
// Corpus vault opened its frontmatter and never closed it. Parse returned no error, so the
// note never reached FileError, never reached DroppedNotes and never reached mesh health,
// while the index recorded type=note with no tags, no related edges and an id-as-prose
// title - a decision that had silently stopped being one. Refusing is the point: a note
// that will not index is recoverable, a note that indexes as a lie is not noticed.
func TestParseRefusesUnterminatedFrontmatter(t *testing.T) {
	const unterminated = "---\n" +
		"id: flare-coldtier\n" +
		"type: decision\n" +
		"title: Flare cold tier is DuckDB attached to Postgres\n" +
		"related:\n    - flare\n" +
		"tags:\n    - flare\n    - duckdb\n"

	_, err := Parse("decisions/flare-coldtier.md", []byte(unterminated))
	if !errors.Is(err, vault.ErrUnterminatedFrontmatter) {
		t.Fatalf("Parse() err = %v, want ErrUnterminatedFrontmatter", err)
	}

	// Closing the block is the whole repair, and it must restore every field the
	// unterminated version silently dropped.
	pn, err := Parse("decisions/flare-coldtier.md", []byte(unterminated+"---\n\n# Title\n"))
	if err != nil {
		t.Fatalf("Parse() on the repaired note: %v", err)
	}
	if pn.FM.Type != vault.TypeDecision {
		t.Errorf("type = %q, want decision", pn.FM.Type)
	}
	if pn.FM.ID != "flare-coldtier" {
		t.Errorf("id = %q, want flare-coldtier", pn.FM.ID)
	}
	if len(pn.FM.Tags) != 2 {
		t.Errorf("tags = %v, want 2", pn.FM.Tags)
	}
	if len(pn.FM.Related) != 1 {
		t.Errorf("related = %v, want 1", pn.FM.Related)
	}
}

// A note with no frontmatter at all is legal (maps and index.md have none) and must keep
// parsing. The refusal above must key on "opened and never closed", not on "had == false".
func TestParseStillAcceptsNoFrontmatter(t *testing.T) {
	pn, err := Parse("maps/a-map.md", []byte("# A Map\n\nSome [[link]] here.\n"))
	if err != nil {
		t.Fatalf("Parse() on a frontmatter-less note: %v", err)
	}
	if pn.FM.Type != vault.TypeNote {
		t.Errorf("type = %q, want the note default", pn.FM.Type)
	}
}
