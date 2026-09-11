// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func seedBridgeSymbols(t testing.TB, s *Store, count int) {
	t.Helper()
	if err := s.Write(func(tx *sql.Tx) error {
		stmt, err := tx.Prepare(`INSERT INTO code_symbols(id,path,lang,name,kind,start_line,end_line) VALUES(?,?,'go',?,'func',1,2)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for i := 0; i < count; i++ {
			if _, err := stmt.Exec(fmt.Sprintf("symbol-%d", i), "test.go", fmt.Sprintf("DistinctiveSymbol%d", i)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func bridgeRows(t testing.TB, s *Store) []noteCodeLink {
	t.Helper()
	rows, err := s.readDB.Query(`SELECT note_id,symbol_id,name FROM note_code_links ORDER BY note_id,symbol_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var result []noteCodeLink
	for rows.Next() {
		var l noteCodeLink
		if err := rows.Scan(&l.noteID, &l.symID, &l.name); err != nil {
			t.Fatal(err)
		}
		result = append(result, l)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestCodeLinkDeltaReconciliationMatchesFull(t *testing.T) {
	for _, targeted := range []bool{false, true} {
		t.Run(fmt.Sprintf("targeted=%t", targeted), func(t *testing.T) {
			root := t.TempDir()
			write := func(path, id, text string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(root, path), []byte("---\nid: "+id+"\ntype: note\n---\n# Note\n"+text), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			write("a.md", "a", "`DistinctiveSymbol0`")
			write("b.md", "b", "`DistinctiveSymbol1`")
			s, err := Open(root)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			seedBridgeSymbols(t, s, 3)
			_, notes, err := ReindexFull(s, root)
			if err != nil {
				t.Fatal(err)
			}
			cache := NewNoteCache()
			cache.Seed(notes)
			reconcile := func(paths ...string) {
				t.Helper()
				var rec Reconciliation
				var err error
				if targeted {
					rec, err = ReconcilePaths(s, root, cache, paths)
				} else {
					rec, err = ReconcileIncremental(s, root, cache, false)
				}
				if err != nil || !rec.Reindexed {
					t.Fatalf("reconcile: %+v, %v", rec, err)
				}
				before := bridgeRows(t, s)
				if _, err := s.LinkNotesToCode(root); err != nil {
					t.Fatal(err)
				}
				if after := bridgeRows(t, s); !reflect.DeepEqual(before, after) {
					t.Fatalf("delta/full mismatch: delta=%+v full=%+v", before, after)
				}
			}
			write("a.md", "a", "`DistinctiveSymbol2` `DistinctiveSymbol2` `Open`")
			reconcile("a.md")
			write("c.md", "c", "`DistinctiveSymbol0`")
			reconcile("c.md")
			write("a.md", "new-id", "`DistinctiveSymbol1`")
			reconcile("a.md")
			if err := os.Rename(filepath.Join(root, "a.md"), filepath.Join(root, "renamed.md")); err != nil {
				t.Fatal(err)
			}
			reconcile("a.md", "renamed.md")
			if err := os.Remove(filepath.Join(root, "c.md")); err != nil {
				t.Fatal(err)
			}
			reconcile("c.md")
			// A malformed note is removed from the index and its bridge links too.
			if err := os.WriteFile(filepath.Join(root, "renamed.md"), []byte("---\nid: [broken\n---\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			reconcile("renamed.md")
			if got := bridgeRows(t, s); len(got) != 1 || got[0].noteID != "b" {
				t.Fatalf("unexpected surviving links: %+v", got)
			}
		})
	}
}

func TestCodeLinkDeltaIsolationAndRollback(t *testing.T) {
	root := t.TempDir()
	for _, id := range []string{"a", "b"} {
		if err := os.WriteFile(filepath.Join(root, id+".md"), []byte("---\nid: "+id+"\ntype: note\n---\n# Note\n`DistinctiveSymbol0`"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	seedBridgeSymbols(t, s, 2)
	if _, err := Reindex(s, root); err != nil {
		t.Fatal(err)
	}
	// Make an unrelated raw file unavailable. A full rebuild would lose its body
	// link; an incremental rebuild must neither read it nor touch its stored row.
	if err := os.Remove(filepath.Join(root, "b.md")); err != nil {
		t.Fatal(err)
	}
	if err := s.Write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`CREATE TRIGGER protect_unrelated BEFORE DELETE ON note_code_links WHEN OLD.note_id='b' BEGIN SELECT RAISE(ABORT,'unrelated link touched'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	before := bridgeRows(t, s)
	if _, err := s.linkNotesToCodeContext(t.Context(), root, []string{"a"}); err != nil {
		t.Fatal(err)
	}
	if after := bridgeRows(t, s); !reflect.DeepEqual(before, after) {
		t.Fatalf("unrelated link changed: %+v", after)
	}
	if err := os.WriteFile(filepath.Join(root, "a.md"), []byte("# Note\n`DistinctiveSymbol1`"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`CREATE TRIGGER fail_link BEFORE INSERT ON note_code_links BEGIN SELECT RAISE(ABORT,'injected failure'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.linkNotesToCodeContext(t.Context(), root, []string{"a"}); err == nil {
		t.Fatal("expected injected insert failure")
	}
	if after := bridgeRows(t, s); !reflect.DeepEqual(before, after) {
		t.Fatalf("failed replacement was not rolled back: %+v", after)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.linkNotesToCodeContext(ctx, root, []string{"a"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled delta: %v", err)
	}
	if _, err := s.linkNotesToCodeContext(t.Context(), root, []string{}); err != nil {
		t.Fatalf("empty delta touched links: %v", err)
	}
}

func TestCodeLinkDeltaMatchingAndCodeRefresh(t *testing.T) {
	root := t.TempDir()
	// Full matching deliberately reads raw markdown, including backticks in
	// frontmatter. Do not quietly replace that with parsed Body-only matching.
	if err := os.WriteFile(filepath.Join(root, "a.md"), []byte("---\nid: a\ntype: note\ntitle: DistinctiveSymbol0\ndo: 'Use `DistinctiveSymbol1`'\n---\n# Note\n`DistinctiveSymbol2` `pkg.DistinctiveSymbol2`"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	seedBridgeSymbols(t, s, 3)
	if err := s.Write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO code_symbols(id,path,lang,name,kind,start_line,end_line) VALUES('qualified','test.go','go','pkg.DistinctiveSymbol2','func',1,2)`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	_, notes, err := ReindexFull(s, root)
	if err != nil {
		t.Fatal(err)
	}
	want := bridgeRows(t, s)
	if len(want) != 3 { // title, frontmatter, qualified; ambiguous bare token rejected
		t.Fatalf("unexpected full matching: %+v", want)
	}
	if _, err := s.linkChangedNotesToCode(root, append(notes, notes...), []string{"a", "a"}); err != nil {
		t.Fatal(err)
	}
	if got := bridgeRows(t, s); !reflect.DeepEqual(got, want) {
		t.Fatalf("delta matching differs: got %+v want %+v", got, want)
	}
	// A code-index change still needs a FULL bridge refresh: unchanged note IDs
	// can lose a previously unique match, and removed symbols must not linger.
	if err := s.Write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM code_symbols WHERE id='qualified'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LinkNotesToCode(root); err != nil {
		t.Fatal(err)
	}
	got := bridgeRows(t, s)
	if len(got) != 3 || got[2].symID != "symbol-2" {
		t.Fatalf("full refresh did not resolve changed code: %+v", got)
	}
	if err := s.Write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM code_symbols`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if n, err := s.linkChangedNotesToCode(root, notes, nil); err != nil || n != 0 {
		t.Fatalf("empty-code cleanup: count=%d err=%v", n, err)
	}
	if got := bridgeRows(t, s); len(got) != 0 {
		t.Fatalf("empty-code stale links: %+v", got)
	}
}

func BenchmarkCodeLinkRefresh(b *testing.B) {
	root := b.TempDir()
	s, err := Open(root)
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	seedBridgeSymbols(b, s, 12000)
	if err := s.Write(func(tx *sql.Tx) error {
		stmt, err := tx.Prepare(`INSERT INTO notes(id,path,type,title,retrieval_hash,frontmatter,mtime) VALUES(?,?,'note','Note','','{}',0)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for i := 0; i < 3000; i++ {
			id := fmt.Sprintf("note-%d", i)
			if err := os.WriteFile(filepath.Join(root, id+".md"), []byte(fmt.Sprintf("# Note\nUse `DistinctiveSymbol%d`.\n", i)), 0o600); err != nil {
				return err
			}
			if _, err := stmt.Exec(id, id+".md"); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		b.Fatal(err)
	}
	if _, err := s.LinkNotesToCode(root); err != nil {
		b.Fatal(err)
	}
	for _, mode := range []string{"full", "one_note"} {
		b.Run(mode, func(b *testing.B) {
			var changed []string
			if mode == "one_note" {
				changed = []string{"note-0"}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := s.linkNotesToCodeContext(b.Context(), root, changed); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
