package index

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/bright-interaction/mesh/internal/graph"
)

var htmlCommentRe = regexp.MustCompile(`(?s)<!--.*?-->`)

// searchText is the body Mesh indexes into FTS5: the prose with comment noise
// stripped, plus the flywheel fields (do/dont/why) and tags, which carry the
// institutional memory but live in frontmatter, not the body.
func searchText(pn *ParsedNote) string {
	parts := []string{htmlCommentRe.ReplaceAllString(pn.Body, " ")}
	for _, v := range []string{pn.FM.Do, pn.FM.Dont, pn.FM.Why} {
		if v != "" {
			parts = append(parts, v)
		}
	}
	parts = append(parts, pn.FM.Tags...)
	// Collapse whitespace so snippets are not padded with skeleton blank lines,
	// which keeps budget-packed cards cheap.
	return strings.Join(strings.Fields(strings.Join(parts, " ")), " ")
}

// IndexVault writes the parsed notes and graph into the store as a full reindex
// in a single transaction (M0: wipe + insert; incremental upsert lands with the
// watcher). Returns the number of notes written.
func (s *Store) IndexVault(notes []*ParsedNote, g *graph.Graph) (int, error) {
	count := 0
	err := s.Write(func(tx *sql.Tx) error {
		// Note + FTS get a full wipe here; nodes/edges are wiped+rewritten by
		// writeGraphTables below.
		for _, t := range []string{"notes", "search_index"} {
			if _, err := tx.Exec("DELETE FROM " + t); err != nil {
				return err
			}
		}

		// INSERT OR REPLACE (not a plain INSERT) so a duplicate effectiveID does not
		// abort the whole reindex. Two files that share a basename and carry no
		// frontmatter id resolve to the same effectiveID; a plain INSERT hit the
		// notes.id PRIMARY KEY and rolled back the entire transaction, taking the whole
		// index offline. This converges the full path with IndexVaultIncremental (which
		// already uses OR REPLACE) and BuildGraph (which collapses the node): last-wins,
		// while BuildGraph still raises a duplicate-id Issue surfaced by index/doctor/lint.
		insNote, err := tx.Prepare(`INSERT OR REPLACE INTO notes(id,path,type,title,retrieval_hash,frontmatter,mtime,updated,review_by,source,scope) VALUES(?,?,?,?,?,?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer insNote.Close()
		insFTS, err := tx.Prepare(`INSERT INTO search_index(node_id,kind,anchor,title,body) VALUES(?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer insFTS.Close()

		seen := make(map[string]bool, len(notes))
		for _, pn := range notes {
			id, title, fmJSON, updated, reviewBy, source, scope, mtime, err := noteRowValues(pn)
			if err != nil {
				return err
			}
			// On a duplicate effectiveID within this reindex, drop the FTS row already
			// written for this node (FTS5 has no PK upsert) so we never leave two FTS
			// rows for one node; only count distinct notes.
			if seen[id] {
				if _, err := tx.Exec(`DELETE FROM search_index WHERE node_id=?`, "note:"+id); err != nil {
					return err
				}
			} else {
				seen[id] = true
				count++
			}
			if _, err := insNote.Exec(id, pn.Path, string(pn.FM.Type), title, retrievalHash(pn), fmJSON, mtime, updated, reviewBy, source, scope); err != nil {
				return err
			}
			if _, err := insFTS.Exec("note:"+id, "note", "", title, searchText(pn)); err != nil {
				return err
			}
		}

		if err := writeGraphTables(tx, g); err != nil {
			return err
		}
		return pruneOrphanVectors(tx)
	})
	return count, err
}

// IndexVaultIncremental applies a drift delta: targeted INSERT OR REPLACE / DELETE
// for the changed notes + their FTS rows, a full rewrite of the (globally rebuilt)
// nodes/edges tables from the in-memory graph, and the orphan-vector prune, all in
// one writer-goroutine transaction so a concurrent reader sees an all-or-nothing
// WAL snapshot. upserts are Added+Changed notes (vault-relative Path); removedIDs
// are ids whose files are gone (and old ids retired on an id change). Returns the
// number of upserted notes.
func (s *Store) IndexVaultIncremental(upserts []*ParsedNote, removedIDs []string, g *graph.Graph) (int, error) {
	err := s.Write(func(tx *sql.Tx) error {
		// Deletes first: a rename frees a path another note now claims, and an id
		// change retires the old id; deleting before inserting avoids the notes.path
		// UNIQUE and notes.id PK collisions.
		for _, id := range removedIDs {
			if _, err := tx.Exec(`DELETE FROM notes WHERE id=?`, id); err != nil {
				return err
			}
			if _, err := tx.Exec(`DELETE FROM search_index WHERE node_id=?`, "note:"+id); err != nil {
				return err
			}
		}

		insNote, err := tx.Prepare(`INSERT OR REPLACE INTO notes(id,path,type,title,retrieval_hash,frontmatter,mtime,updated,review_by,source,scope) VALUES(?,?,?,?,?,?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer insNote.Close()
		insFTS, err := tx.Prepare(`INSERT INTO search_index(node_id,kind,anchor,title,body) VALUES(?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer insFTS.Close()

		for _, pn := range upserts {
			id, title, fmJSON, updated, reviewBy, source, scope, mtime, err := noteRowValues(pn)
			if err != nil {
				return err
			}
			// FTS5 has no PK upsert; delete the existing row (if any) then insert, so
			// the body can never lag the note.
			if _, err := tx.Exec(`DELETE FROM search_index WHERE node_id=?`, "note:"+id); err != nil {
				return err
			}
			if _, err := insNote.Exec(id, pn.Path, string(pn.FM.Type), title, retrievalHash(pn), fmJSON, mtime, updated, reviewBy, source, scope); err != nil {
				return err
			}
			if _, err := insFTS.Exec("note:"+id, "note", "", title, searchText(pn)); err != nil {
				return err
			}
		}

		if err := writeGraphTables(tx, g); err != nil {
			return err
		}
		return pruneOrphanVectors(tx)
	})
	return len(upserts), err
}

// noteRowValues derives the notes-table column values for a parsed note. Shared by
// the full and incremental index paths so they can never drift.
func noteRowValues(pn *ParsedNote) (id, title, fmJSON, updated, reviewBy, source, scope string, mtime int64, err error) {
	id = effectiveID(pn)
	title = titleOf(pn)
	b, err := json.Marshal(pn.FM)
	if err != nil {
		return "", "", "", "", "", "", "", 0, err
	}
	fmJSON = string(b)
	updated = pn.FM.When
	if pn.FM.Updated != "" {
		updated = pn.FM.Updated
	}
	reviewBy = pn.FM.ReviewBy                          // lifecycle re-check date (Phase C)
	source = pn.FM.Source                              // provenance origin (Phase A/D)
	scope = strings.Join(pn.FM.EffectiveScopes(), ",") // access-control scope(s); absent = dev
	mtime = pn.Mtime                                   // captured by ParseFile from the on-disk file (CWD-independent)
	return id, title, fmJSON, updated, reviewBy, source, scope, mtime, nil
}

// writeGraphTables wipes and rewrites the nodes + edges tables from the in-memory
// graph. Shared by the full and incremental paths: communities are label-prop
// (global), so the graph is rebuilt whole in memory either way, and dumping it to
// two small tables is cheap relative to parsing the vault.
func writeGraphTables(tx *sql.Tx, g *graph.Graph) error {
	if _, err := tx.Exec(`DELETE FROM nodes`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM edges`); err != nil {
		return err
	}
	insNode, err := tx.Prepare(`INSERT OR REPLACE INTO nodes(id,kind,label,note_id,note_path,anchor,source_loc,community,attrs) VALUES(?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer insNode.Close()
	insEdge, err := tx.Prepare(`INSERT OR IGNORE INTO edges(source,target,relation,confidence,confidence_score,weight,source_loc) VALUES(?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer insEdge.Close()

	for _, nd := range g.Nodes() {
		attrs := "null"
		if nd.Attrs != nil {
			b, err := json.Marshal(nd.Attrs)
			if err != nil {
				return err
			}
			attrs = string(b)
		}
		if _, err := insNode.Exec(nd.ID, nd.Kind, nd.Label, nd.NoteID, nd.NotePath, nd.Anchor, nd.SourceLoc, nd.Community, attrs); err != nil {
			return err
		}
		for _, e := range g.Neighbors(nd.ID) {
			if _, err := insEdge.Exec(e.Source, e.Target, e.Relation, e.Confidence, e.ConfidenceScore, e.Weight, e.SourceLoc); err != nil {
				return err
			}
		}
	}
	return nil
}

// pruneOrphanVectors removes vectors whose note was deleted (a note left in the
// vectors table with no live note row). Stale vectors for still-existing-but-edited
// notes are kept on disk and excluded from retrieval by LoadVectors' note_hash
// JOIN; they are refreshed in place on the next `mesh embed`. Orphans have no note
// to refresh them, so they are removed here to bound table growth across deletes.
func pruneOrphanVectors(tx *sql.Tx) error {
	_, err := tx.Exec(`DELETE FROM vectors WHERE node_id NOT IN (SELECT 'note:' || id FROM notes)`)
	return err
}

func titleOf(pn *ParsedNote) string {
	if pn.FM.Title != "" {
		return pn.FM.Title
	}
	return pn.Key
}

// RetrievalHash is the exported retrieval hash for a parsed note: it is what the
// notes table stores in retrieval_hash, and the embedder stamps onto each vector
// (note_hash) so a later content change can be detected and the stale vector
// excluded from retrieval.
func RetrievalHash(pn *ParsedNote) string { return retrievalHash(pn) }

// retrievalHash is SHA256 over the node identity (effectiveID) plus the body and
// the retrieval-critical frontmatter (type, status, supersedes, related), so a
// status change (accepted -> superseded) or an id change forces a reindex while a
// cosmetic frontmatter edit (title, unrelated fields) does not (spec section 3.2).
// The id is included because it is the node identity: an id-only edit must retire
// the old node and create the new one, so the drift check has to see it.
func retrievalHash(pn *ParsedNote) string {
	h := sha256.New()
	h.Write([]byte(effectiveID(pn)))
	h.Write([]byte{0})
	h.Write([]byte(pn.Body))
	h.Write([]byte{0})
	h.Write([]byte(string(pn.FM.Type)))
	h.Write([]byte{0})
	h.Write([]byte(pn.FM.Status))
	for _, s := range pn.FM.Supersedes {
		h.Write([]byte{0})
		h.Write([]byte(s))
	}
	for _, s := range pn.FM.Related {
		h.Write([]byte{0})
		h.Write([]byte(s))
	}
	return hex.EncodeToString(h.Sum(nil))
}
