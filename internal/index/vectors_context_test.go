// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

func replaceVectorSnapshot(t *testing.T, s *Store, model string, vec []float32) {
	t.Helper()
	if err := s.Write(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO notes(id,path,type,title,retrieval_hash,frontmatter,mtime)
			VALUES('vector-note','vector-note.md','note','Vector note','vector-hash','{}',1)
			ON CONFLICT(id) DO UPDATE SET retrieval_hash=excluded.retrieval_hash`); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM vectors`); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO meta(key,value) VALUES('vector_model',?)
			ON CONFLICT(key) DO UPDATE SET value=excluded.value`, model); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO meta(key,value) VALUES('vector_dim',?)
			ON CONFLICT(key) DO UPDATE SET value=excluded.value`, len(vec)); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO vectors(node_id,chunk_ix,model,dim,embedding,content_hash,note_hash)
			VALUES('note:vector-note',0,?,?,?,'','vector-hash')`, model, len(vec), encodeVec(vec))
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestLoadVectorsContextUsesOneSnapshotAcrossMetadataAndRows(t *testing.T) {
	s := openVecStore(t)
	replaceVectorSnapshot(t, s, "old-model", []float32{1, 2})

	model, dim, byNode, err := s.loadVectorsSnapshotContext(context.Background(), func() {
		// The metadata reads establish the old snapshot. WAL allows this replacement to
		// commit before the vector query without blocking that reader.
		replaceVectorSnapshot(t, s, "new-model", []float32{3, 4, 5})
	})
	if err != nil {
		t.Fatal(err)
	}
	if model != "old-model" || dim != 2 {
		t.Fatalf("metadata crossed snapshots: model=%q dim=%d", model, dim)
	}
	rows := byNode["note:vector-note"]
	if len(rows) != 1 || len(rows[0]) != 2 || rows[0][0] != 1 || rows[0][1] != 2 {
		t.Fatalf("vector rows crossed snapshots: %+v", rows)
	}

	currentModel, currentDim, current, err := s.LoadVectors()
	if err != nil {
		t.Fatal(err)
	}
	if currentModel != "new-model" || currentDim != 3 || len(current["note:vector-note"][0]) != 3 {
		t.Fatalf("test turnover did not commit: model=%q dim=%d rows=%+v", currentModel, currentDim, current)
	}
}

func TestLoadVectorsContextRejectsCanceledLoad(t *testing.T) {
	s := openVecStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	model, dim, byNode, err := s.LoadVectorsContext(ctx)
	if !errors.Is(err, context.Canceled) || model != "" || dim != 0 || byNode != nil {
		t.Fatalf("LoadVectorsContext = (%q, %d, %v, %v), want zero values + context.Canceled", model, dim, byNode, err)
	}
}
