// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
)

// VectorRow is one stored chunk embedding (a note can have several: one per
// heading section, so a query matches the relevant section, not a blurry
// whole-note average).
type VectorRow struct {
	NodeID      string
	ChunkIx     int
	Vec         []float32
	ContentHash string // sha256 of the embedding input; lets a re-embed skip unchanged chunks
	NoteHash    string // the note's retrieval_hash when embedded; lets retrieval exclude this vector once the note changes
}

// CachedVec is a stored embedding plus the content hash it was computed from.
type CachedVec struct {
	Hash string
	Vec  []float32
}

// VecKey is the stable cache key for a chunk (node id + chunk index).
func VecKey(nodeID string, chunkIx int) string {
	return nodeID + "\x00" + strconv.Itoa(chunkIx)
}

// ContentHash is the sha256 (hex) of the embedding input parts, NUL-joined so
// distinct splits cannot collide. The cache keys reuse on this: an unchanged
// embedding input (same prefix + same chunk text) yields the same hash, so the
// chunk is not re-sent to the endpoint.
func ContentHash(parts ...string) string {
	h := sha256.New()
	for i, p := range parts {
		if i > 0 {
			h.Write([]byte{0})
		}
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))
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
	// Defensive no-op: never wipe stored vectors (and stamp a 0 dim) for an empty
	// batch. The embed command already returns early when there are no notes, so this
	// only guards against a future caller passing nil.
	if len(rows) == 0 {
		return nil
	}
	// One vault = one embedding space. Reject a ragged batch up front so a
	// mixed-dim set can never reach storage (where it would later cosine across
	// incompatible widths).
	dim := 0
	for _, r := range rows {
		if dim == 0 {
			dim = len(r.Vec)
		} else if len(r.Vec) != dim {
			return fmt.Errorf("ReplaceVectors: ragged embedding dims (%d vs %d); a vault holds exactly one model/dim", dim, len(r.Vec))
		}
	}
	return s.Write(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM vectors`); err != nil {
			return err
		}
		ins, err := tx.Prepare(`INSERT INTO vectors(node_id, chunk_ix, model, dim, embedding, content_hash, note_hash) VALUES(?,?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer ins.Close()
		for _, r := range rows {
			if _, err := ins.Exec(r.NodeID, r.ChunkIx, model, len(r.Vec), encodeVec(r.Vec), r.ContentHash, r.NoteHash); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`INSERT INTO meta(key,value) VALUES('vector_model',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, model); err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT INTO meta(key,value) VALUES('vector_dim',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, strconv.Itoa(dim))
		return err
	})
}

// LoadVectors returns the canonical model and every LIVE chunk vector grouped by
// node id. A note's relevance is later scored as the max cosine over its chunks
// (best-matching section). "Live" means the JOIN to notes excludes both orphans
// (a vector whose note was deleted) and stale vectors (a vector whose note_hash no
// longer matches the note's current retrieval_hash because the note was edited
// since the last embed). This is the fix for the reindex-drift bug: an edited or
// deleted note never contributes a stale semantic signal; it simply falls back to
// the lexical + graph signals until the next `mesh embed` refreshes its vector.
func (s *Store) LoadVectors() (model string, dim int, byNode map[string][][]float32, err error) {
	_ = s.readDB.QueryRow(`SELECT value FROM meta WHERE key='vector_model'`).Scan(&model)
	var dimStr string
	_ = s.readDB.QueryRow(`SELECT value FROM meta WHERE key='vector_dim'`).Scan(&dimStr)
	dim, _ = strconv.Atoi(dimStr)
	byNode = map[string][][]float32{}
	rows, err := s.readDB.Query(`
		SELECT v.node_id, v.embedding
		FROM vectors v
		JOIN notes n ON v.node_id = 'note:' || n.id
		WHERE v.note_hash = n.retrieval_hash
		  AND v.model = (SELECT value FROM meta WHERE key='vector_model')
		ORDER BY v.node_id, v.chunk_ix`)
	if err != nil {
		return model, dim, byNode, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var blob []byte
		if err := rows.Scan(&id, &blob); err != nil {
			return model, dim, byNode, err
		}
		byNode[id] = append(byNode[id], decodeVec(blob))
	}
	return model, dim, byNode, rows.Err()
}

// VectorMeta returns the stored canonical model and dim from meta. It is
// lightweight: it does not load the vector blobs. Zero values when none stored.
func (s *Store) VectorMeta() (model string, dim int) {
	_ = s.readDB.QueryRow(`SELECT value FROM meta WHERE key='vector_model'`).Scan(&model)
	var d string
	_ = s.readDB.QueryRow(`SELECT value FROM meta WHERE key='vector_dim'`).Scan(&d)
	dim, _ = strconv.Atoi(d)
	return model, dim
}

// VectorStats reports vector freshness for status / mesh://stats: total stored
// rows, how many are live (note exists and unchanged since embed), and how many
// are stale or orphaned (note edited or deleted since embed). A re-embed clears
// the stale count.
func (s *Store) VectorStats() (total, live, staleOrOrphan int, err error) {
	if err = s.readDB.QueryRow(`SELECT count(*) FROM vectors`).Scan(&total); err != nil {
		return 0, 0, 0, err
	}
	err = s.readDB.QueryRow(`
		SELECT count(*)
		FROM vectors v
		JOIN notes n ON v.node_id = 'note:' || n.id
		WHERE v.note_hash = n.retrieval_hash
		  AND v.model = (SELECT value FROM meta WHERE key='vector_model')`).Scan(&live)
	if err != nil {
		return 0, 0, 0, err
	}
	return total, live, total - live, nil
}

// CachedVectors returns the stored embeddings keyed by VecKey, but ONLY when the
// stored canonical model equals the requested model. A different model invalidates
// the whole cache (vectors from another model are not reusable), so callers get an
// empty map and re-embed everything. This is the content-hash cache's read side: a
// re-embed reuses any chunk whose hash matches, calling the endpoint only for the
// changed or new ones.
func (s *Store) CachedVectors(model string) (map[string]CachedVec, error) {
	out := map[string]CachedVec{}
	var stored string
	_ = s.readDB.QueryRow(`SELECT value FROM meta WHERE key='vector_model'`).Scan(&stored)
	if stored == "" || stored != model {
		return out, nil
	}
	rows, err := s.readDB.Query(`SELECT node_id, chunk_ix, embedding, content_hash FROM vectors`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, hash string
		var ix int
		var blob []byte
		if err := rows.Scan(&id, &ix, &blob, &hash); err != nil {
			return nil, err
		}
		if hash == "" {
			continue // no recorded hash -> cannot prove the chunk is unchanged
		}
		out[VecKey(id, ix)] = CachedVec{Hash: hash, Vec: decodeVec(blob)}
	}
	return out, rows.Err()
}

// NoteRetrievalHash returns the stored retrieval_hash for a note node id (e.g.
// "note:a"), or "" if the note is absent. It is the value the embedder stamps onto
// a vector's note_hash; retrieval compares the two to detect staleness.
func (s *Store) NoteRetrievalHash(nodeID string) (string, error) {
	var h string
	err := s.readDB.QueryRow(`SELECT retrieval_hash FROM notes WHERE 'note:' || id = ?`, nodeID).Scan(&h)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return h, err
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
