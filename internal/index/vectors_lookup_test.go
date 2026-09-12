// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// Keep the previous query as a benchmark baseline, using identical snapshot,
// scan, decode and grouping work for both lookup strategies.
type vectorLookupQueryer struct {
	vectorQueryer
	primary bool
}

func (q vectorLookupQueryer) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if !q.primary {
		query = strings.Replace(query, "n.id = CAST(substr(CAST(v.node_id AS BLOB), 6) AS TEXT)\n\t\t  AND ", "", 1)
	}
	return q.vectorQueryer.QueryContext(ctx, query, args...)
}

func TestVectorLookupPreservesExactIDsAndFiltering(t *testing.T) {
	s := openVecStore(t)
	ids := []string{"", "a", "A", "å猫🦊", "note:nested", "a#section", "a\x00tail", "\x00", "bad\xffutf8"}
	want := make(map[string][][]float32)
	if err := s.Write(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO meta(key,value) VALUES('vector_model','m'),('vector_dim','2')`); err != nil {
			return err
		}
		for i, id := range ids {
			if _, err := tx.Exec(`INSERT INTO notes(id,path,type,title,retrieval_hash,frontmatter,mtime) VALUES(?,?,'note','Test','same','{}',1)`, id, fmt.Sprintf("%d.md", i)); err != nil {
				return err
			}
			// Deliberately insert chunk indices out of order and with gaps.
			for _, chunk := range []int{9, -1, 2} {
				vec := []float32{float32(i), float32(chunk)}
				if _, err := tx.Exec(`INSERT INTO vectors(node_id,chunk_ix,model,dim,embedding,note_hash) VALUES(?,?,'m',2,?,'same')`, "note:"+id, chunk, encodeVec(vec)); err != nil {
					return err
				}
			}
			want["note:"+id] = [][]float32{{float32(i), -1}, {float32(i), 2}, {float32(i), 9}}
		}
		for _, row := range []struct{ id, model, hash string }{
			{"note:orphan", "m", "same"}, {"note:a", "wrong", "same"},
			{"note:A", "m", "stale"}, {"code:a", "m", "same"},
			{"NOTE:a", "m", "same"}, {"other", "m", "same"},
		} {
			if _, err := tx.Exec(`INSERT INTO vectors(node_id,chunk_ix,model,dim,embedding,note_hash) VALUES(?,99,?,2,?,?)`, row.id, row.model, encodeVec([]float32{99, 99}), row.hash); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	model, dim, got, err := s.LoadVectorsContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if model != "m" || dim != 2 || !reflect.DeepEqual(got, want) {
		t.Fatalf("load = %q, %d, %#v; want %#v", model, dim, got, want)
	}
	_, _, legacy, err := loadVectorsContext(context.Background(), vectorLookupQueryer{s.readDB, false}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, legacy) {
		t.Fatal("primary-key lookup differs from legacy query")
	}
}

func TestVectorLookupUsesExistingNotePrimaryKey(t *testing.T) {
	s := openVecStore(t)
	rows, err := s.readDB.Query("EXPLAIN QUERY PLAN " + liveVectorsQuery)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(plan, "\n")
	if !strings.Contains(joined, "SEARCH n USING INDEX sqlite_autoindex_notes_1 (id=?)") || strings.Contains(joined, "AUTOMATIC") || strings.Contains(joined, "TEMP B-TREE") {
		t.Fatalf("vector lookup must use existing note PK without temporary index/sort:\n%s", joined)
	}
}

func BenchmarkVectorLookup(b *testing.B) {
	for _, sharedHash := range []bool{false, true} {
		b.Run(fmt.Sprintf("shared_hash=%t", sharedHash), func(b *testing.B) {
			s, err := Open(b.TempDir())
			if err != nil {
				b.Fatal(err)
			}
			defer s.Close()
			if err := s.Write(func(tx *sql.Tx) error {
				if _, err := tx.Exec(`INSERT INTO meta(key,value) VALUES('vector_model','bench'),('vector_dim','768')`); err != nil {
					return err
				}
				notes, err := tx.Prepare(`INSERT INTO notes(id,path,type,title,retrieval_hash,frontmatter,mtime) VALUES(?,?,'note','Bench',?,'{}',1)`)
				if err != nil {
					return err
				}
				defer notes.Close()
				vectors, err := tx.Prepare(`INSERT INTO vectors(node_id,chunk_ix,model,dim,embedding,note_hash) VALUES(?,?,'bench',768,?,?)`)
				if err != nil {
					return err
				}
				defer vectors.Close()
				blob := encodeVec(make([]float32, 768))
				for i := 0; i < 3000; i++ {
					id := fmt.Sprintf("n%05d", i)
					hash := id
					if sharedHash {
						hash = "shared"
					}
					if _, err := notes.Exec(id, id+".md", hash); err != nil {
						return err
					}
					if i < 2350 {
						if _, err := vectors.Exec("note:"+id, 0, blob, hash); err != nil {
							return err
						}
					}
				}
				return nil
			}); err != nil {
				b.Fatal(err)
			}
			for _, primary := range []bool{false, true} {
				b.Run(fmt.Sprintf("primary=%t", primary), func(b *testing.B) {
					b.ReportAllocs()
					for i := 0; i < b.N; i++ {
						tx, err := s.readDB.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
						if err != nil {
							b.Fatal(err)
						}
						_, _, vecs, err := loadVectorsContext(context.Background(), vectorLookupQueryer{tx, primary}, nil)
						closeErr := tx.Rollback()
						if err != nil {
							b.Fatal(err)
						}
						if closeErr != nil {
							b.Fatal(closeErr)
						}
						if len(vecs) != 2350 {
							b.Fatalf("got %d vectors", len(vecs))
						}
					}
				})
			}
		})
	}
}
