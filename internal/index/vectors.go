package index

import (
	"database/sql"
	"encoding/binary"
	"math"
)

// VectorRow is one stored embedding.
type VectorRow struct {
	NodeID string
	Vec    []float32
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

// ReplaceVectors wipes the vectors table and stores the given embeddings under
// one canonical model (homogeneity: a vault holds exactly one embedding model so
// cosine stays meaningful). Runs in the writer goroutine.
func (s *Store) ReplaceVectors(model string, rows []VectorRow) error {
	return s.Write(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM vectors`); err != nil {
			return err
		}
		ins, err := tx.Prepare(`INSERT INTO vectors(node_id, chunk_ix, model, dim, embedding) VALUES(?,0,?,?,?)`)
		if err != nil {
			return err
		}
		defer ins.Close()
		for _, r := range rows {
			if _, err := ins.Exec(r.NodeID, model, len(r.Vec), encodeVec(r.Vec)); err != nil {
				return err
			}
		}
		_, err = tx.Exec(`INSERT INTO meta(key,value) VALUES('vector_model',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, model)
		return err
	})
}

// LoadVectors returns the canonical model and every stored vector by node id.
func (s *Store) LoadVectors() (model string, byNode map[string][]float32, err error) {
	_ = s.readDB.QueryRow(`SELECT value FROM meta WHERE key='vector_model'`).Scan(&model)
	byNode = map[string][]float32{}
	rows, err := s.readDB.Query(`SELECT node_id, embedding FROM vectors`)
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
		byNode[id] = decodeVec(blob)
	}
	return model, byNode, rows.Err()
}

// NoteText pairs a note node id with the text to embed for it.
type NoteText struct {
	NodeID string
	Text   string
}

// NoteTexts returns the embeddable text (title + indexed body) for every note,
// read from the FTS rows so it matches what search sees.
func (s *Store) NoteTexts() ([]NoteText, error) {
	rows, err := s.readDB.Query(`SELECT node_id, title, body FROM search_index`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NoteText
	for rows.Next() {
		var id, title, body string
		if err := rows.Scan(&id, &title, &body); err != nil {
			return nil, err
		}
		out = append(out, NoteText{NodeID: id, Text: title + "\n" + body})
	}
	return out, rows.Err()
}
