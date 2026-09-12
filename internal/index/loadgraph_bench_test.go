// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"database/sql"
	"fmt"
	"testing"
)

// Exercise SQLite scanning, attribute decoding, adjacency construction and degree
// calculation together. Synthetic data, not an end-to-end acknowledgement SLO.
func BenchmarkLoadGraphSnapshot(b *testing.B) {
	for _, size := range []int{30, 24000} {
		b.Run(fmt.Sprintf("nodes=%d", size), func(b *testing.B) {
			s, err := Open(b.TempDir())
			if err != nil {
				b.Fatal(err)
			}
			defer s.Close()
			if err := s.Write(func(tx *sql.Tx) error {
				nodes, err := tx.Prepare(`INSERT INTO nodes(id,kind,label,note_id,note_path,attrs) VALUES(?, 'note', ?, ?, ?, ?)`)
				if err != nil {
					return err
				}
				defer nodes.Close()
				edges, err := tx.Prepare(`INSERT INTO edges(source,target,relation,confidence) VALUES(?,?,'references','EXTRACTED')`)
				if err != nil {
					return err
				}
				defer edges.Close()
				for i := 0; i < size; i++ {
					id := fmt.Sprintf("note:%d", i)
					if _, err := nodes.Exec(id, "Shared development knowledge", fmt.Sprint(i), fmt.Sprintf("notes/%d.md", i), `{"do":"Verify before publishing","dont":"Skip freshness checks","why":"Readers share this knowledge"}`); err != nil {
						return err
					}
					for j := 1; j <= 2; j++ {
						if _, err := edges.Exec(id, fmt.Sprintf("note:%d", (i+j)%size)); err != nil {
							return err
						}
					}
				}
				return nil
			}); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := s.LoadGraph(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
