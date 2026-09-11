// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/graph"
)

func upsertTestNote(t testing.TB, id, body string) *ParsedNote {
	t.Helper()
	pn, err := Parse(id+".md", []byte("---\nid: "+id+"\ntype: note\n---\n# Note\n"+body))
	if err != nil {
		t.Fatal(err)
	}
	return pn
}

// A view accepting INSERT but not DELETE proves the new-ID branch never even
// prepares a search-table delete. A row mutation trigger would miss the original
// bug: deleting a nonexistent key scanned the entire FTS table but changed no row.
func TestNoteUpsertsNewIDsNeverDeleteSearch(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`DROP TABLE search_index;
CREATE TABLE search_sink(node_id,kind,anchor,title,body);
CREATE VIEW search_index AS SELECT * FROM search_sink;
CREATE TRIGGER insert_search INSTEAD OF INSERT ON search_index BEGIN
 INSERT INTO search_sink VALUES(NEW.node_id,NEW.kind,NEW.anchor,NEW.title,NEW.body);
END;`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	pn := upsertTestNote(t, "new-note", "fresh search text")
	if _, err := s.IndexVaultIncremental([]*ParsedNote{pn}, nil, graph.New()); err != nil {
		t.Fatalf("new note attempted a search delete: %v", err)
	}
	assertCount(t, s, "notes", 1)
	assertCount(t, s, "search_sink", 1)
	// Existing IDs must still try to delete. Otherwise repeated writes duplicate
	// search rows and old text stays searchable. This fixture rejects that delete.
	if _, err := s.IndexVaultIncremental([]*ParsedNote{pn}, nil, graph.New()); err == nil {
		t.Fatal("existing note skipped its required search replacement")
	}
	assertCount(t, s, "search_sink", 1)
}

func TestNoteUpsertsRepeatedIDsAndRollback(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	old := upsertTestNote(t, "repeated", "obsoleteword")
	updated := upsertTestNote(t, "repeated", "replacementword")
	updated.FM.Scope = []string{"private"}
	if _, err := s.IndexVaultIncremental([]*ParsedNote{old, updated}, nil, graph.New()); err != nil {
		t.Fatal(err)
	}
	assertCount(t, s, "notes", 1)
	assertCount(t, s, "search_index", 1)
	assertSearch := func(term string, want int) {
		t.Helper()
		var got int
		if err := s.readDB.QueryRow(`SELECT count(*) FROM search_index WHERE search_index MATCH ?`, term).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("search %q: got %d want %d", term, got, want)
		}
	}
	assertSearch("obsoleteword", 0)
	assertSearch("replacementword", 1)
	var scope string
	if err := s.readDB.QueryRow(`SELECT scope FROM notes WHERE id='repeated'`).Scan(&scope); err != nil || scope != "private" {
		t.Fatalf("note metadata not replaced: scope=%q err=%v", scope, err)
	}
	// Failure AFTER note and FTS work must roll back deletions and new inserts too.
	badGraph := graph.New()
	badGraph.AddNode(&graph.Node{ID: "bad", Attrs: map[string]any{"unsupported": make(chan int)}})
	added := upsertTestNote(t, "added", "uncommittedword")
	if _, err := s.IndexVaultIncremental([]*ParsedNote{added}, []string{"repeated"}, badGraph); err == nil {
		t.Fatal("expected graph persistence failure")
	}
	assertCount(t, s, "notes", 1)
	assertCount(t, s, "search_index", 1)
	assertSearch("replacementword", 1)
	assertSearch("uncommittedword", 0)
	// Remove then re-add the SAME ID in one batch. The delete phase has removed
	// both rows, so the subsequent absence probe must take the insert-only path.
	if _, err := s.IndexVaultIncremental([]*ParsedNote{old}, []string{"repeated"}, graph.New()); err != nil {
		t.Fatal(err)
	}
	assertCount(t, s, "search_index", 1)
	assertSearch("replacementword", 0)
	assertSearch("obsoleteword", 1)
	if err := s.Write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO search_index(search_index) VALUES('integrity-check')`)
		return err
	}); err != nil {
		t.Fatalf("FTS integrity: %v", err)
	}
}

func TestNoteUpsertsCanceledContext(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.Write(func(tx *sql.Tx) error {
		return upsertNoteRowsContext(ctx, tx, []*ParsedNote{upsertTestNote(t, "canceled", "body")})
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled upsert: %v", err)
	}
	assertCount(t, s, "notes", 0)
	assertCount(t, s, "search_index", 0)
}

// Compare the same upsert with and without the legacy missing-key FTS scan.
// Each iteration rolls back, so the note is genuinely absent every time and
// neither arm grows the corpus. This measures note/search work, not graph or
// filesystem work, and the legacy arm also pays the new small primary-key probe.
func BenchmarkNewNoteUpsert(b *testing.B) {
	s, err := Open(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	corpus := make([]*ParsedNote, 3000)
	body := strings.Repeat("existing searchable knowledge context ", 256)
	for i := range corpus {
		corpus[i] = upsertTestNote(b, fmt.Sprintf("existing-%d", i), body)
	}
	if err := s.Write(func(tx *sql.Tx) error {
		return upsertNoteRowsContext(b.Context(), tx, corpus)
	}); err != nil {
		b.Fatal(err)
	}
	pn := upsertTestNote(b, "new-note", "new searchable knowledge")
	rollback := errors.New("benchmark rollback")
	for _, legacyScan := range []bool{true, false} {
		b.Run(fmt.Sprintf("legacy_scan=%t", legacyScan), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				err := s.Write(func(tx *sql.Tx) error {
					if legacyScan {
						if _, err := tx.Exec(`DELETE FROM search_index WHERE node_id=?`, "note:new-note"); err != nil {
							return err
						}
					}
					if err := upsertNoteRowsContext(b.Context(), tx, []*ParsedNote{pn}); err != nil {
						return err
					}
					return rollback
				})
				if !errors.Is(err, rollback) {
					b.Fatal(err)
				}
			}
		})
	}
}
