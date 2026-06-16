package index

import (
	"database/sql"
	"encoding/binary"
	"math"
)

// VectorRow is one stored chunk embedding (a note can have several: one per
// heading section, so a query matches the relevant section, not a blurry
// whole-note average).
type VectorRow struct {
	NodeID  string
	ChunkIx int
	Vec     []float32
}

func encodeVec(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(f))
	}
	return b
}

func decodeVec(b []byte) []float32 {
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return v
}

// ReplaceVectors wipes the vectors table and stores the given chunk embeddings
// under one canonical model (homogeneity: a vault holds exactly one embedding
// model so cosine stays meaningful). Runs in the writer goroutine.
func (s *Store) ReplaceVectors(model string, rows []VectorRow) error {
	return s.Write(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM vectors`); err != nil {
			return err
		}
		ins, err := tx.Prepare(`INSERT INTO vectors(node_id, chunk_ix, model, dim, embedding) VALUES(?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer ins.Close()
		for _, r := range rows {
			if _, err := ins.Exec(r.NodeID, r.ChunkIx, model, len(r.Vec), encodeVec(r.Vec)); err != nil {
				return err
			}
		}
		_, err = tx.Exec(`INSERT INTO meta(key,value) VALUES('vector_model',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, model)
		return err
	})
}

// LoadVectors returns the canonical model and every stored chunk vector grouped
// by node id. A note's relevance is later scored as the max cosine over its
// chunks (best-matching section).
func (s *Store) LoadVectors() (model string, byNode map[string][][]float32, err error) {
	_ = s.readDB.QueryRow(`SELECT value FROM meta WHERE key='vector_model'`).Scan(&model)
	byNode = map[string][][]float32{}
	rows, err := s.readDB.Query(`SELECT node_id, embedding FROM vectors ORDER BY node_id, chunk_ix`)
	if err != nil {
		return model, byNode, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var blob []byte
		if err := rows.Scan(&id, &blob); err != nil {
			return model, byNode, err
		}
		byNode[id] = append(byNode[id], decodeVec(blob))
	}
	return model, byNode, rows.Err()
}

// NoteFile pairs a note's graph node id with its vault-relative path.
type NoteFile struct {
	NodeID string
	Path   string
}

// NoteFiles lists every note's node id + path so the embedder can read and
// chunk the source file.
func (s *Store) NoteFiles() ([]NoteFile, error) {
	rows, err := s.readDB.Query(`SELECT 'note:' || id, path FROM notes`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NoteFile
	for rows.Next() {
		var nf NoteFile
		if err := rows.Scan(&nf.NodeID, &nf.Path); err != nil {
			return nil, err
		}
		out = append(out, nf)
	}
	return out, rows.Err()
}
