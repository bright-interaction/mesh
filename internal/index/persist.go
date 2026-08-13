// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/bright-interaction/mesh/internal/graph"
	"github.com/bright-interaction/mesh/internal/vault"
)

// searchText is the body Mesh indexes into FTS5: the prose with comment noise
// stripped, plus the flywheel fields (do/dont/why) and tags, which carry the
// institutional memory but live in frontmatter, not the body.
//
// It strips through the same scanner as the parser. A `(?s)<!--.*?-->` regex looks
// equivalent and is not: it cannot match a comment that is never closed, so text the
// graph treated as hidden stayed fully searchable, and the two halves of the product
// disagreed about whether that text existed. Code, unlike comments, IS content here:
// people search for the command in a code span.
func searchText(pn *ParsedNote) string {
	body, _ := vault.StripComments(pn.Body)
	parts := []string{body}
	// vault.Unfilled, not v != "": a scaffolded note carries the literal "TODO" here, and
	// indexing it made 77 notes in one vault match a search for "TODO" with their own
	// placeholder as the excerpt. An unfilled field must contribute nothing.
	for _, v := range []string{pn.FM.Do, pn.FM.Dont, pn.FM.Why} {
		if !vault.Unfilled(v) {
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

		// INSERT OR REPLACE (not a plain INSERT) so a duplicate effectiveID cannot abort
		// the whole reindex and take the index offline with an opaque PRIMARY KEY error.
		//
		// It is a backstop, NOT the duplicate policy. The comment here used to claim that
		// last-wins "converges the full path with IndexVaultIncremental and BuildGraph",
		// and that claim was simply false: OR REPLACE gives notes.path to the LAST file
		// walked while BuildGraph's AddNode gives nodes.note_path to the FIRST, so one
		// search card carried one file's path and the other file's snippet, and the loser,
		// having no notes row at all, was reported as Added by every DriftReport forever.
		// Believing the comment is why the collision survived the 2026-08-07 sweep.
		//
		// Callers must hand IndexVault a de-duplicated set: ReindexFull and `mesh index`
		// run ClaimUniqueIDs first, which picks one owner per id (the incumbent, else walk
		// order, the same rule DriftDeltaReport uses) and reports the rest through
		// RecordDropped. BuildGraph still raises its duplicate-id Issue for lint.
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
// for the changed notes + their FTS rows and a full rewrite of the (globally rebuilt)
// nodes/edges tables from the in-memory graph, all in one writer-goroutine transaction
// so a concurrent reader sees an all-or-nothing WAL snapshot. upserts are Added+Changed
// notes (vault-relative Path); removedIDs are ids whose files are gone (and old ids
// retired on an id change). Returns the number of upserted notes.
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
//
// A note whose file is still on disk but temporarily fails to parse also loses its note
// row, so this prunes its vectors too, and embeddings are paid BYOAI work no reindex
// regenerates. That was raised in the 2026-07-25 audit and deliberately NOT special-cased
// here. Exempting those ids costs the "incremental produces a byte-identical DB to a full
// reindex" invariant (TestIncrementalMatchesFullReindex), and the protection cannot even
// hold: once the note row is gone the next full reindex has no path-to-id mapping left,
// so it prunes anyway. The real window is closed at its source instead, by writeFileAtomic
// on both the hub (internal/hub/repo.go) and the client (pkg/meshclient/vault.go), so a
// reader never observes a half-written note. If you are here because you lost embeddings,
// look for a NON-atomic writer, do not weaken this prune.
func pruneOrphanVectors(tx *sql.Tx) error {
	_, err := tx.Exec(`DELETE FROM vectors WHERE node_id NOT IN (SELECT 'note:' || id FROM notes)`)
	return err
}

func titleOf(pn *ParsedNote) string {
	if t := pn.FM.Title; t != "" && t != pn.FM.ID {
		return t
	}
	// Fall back to the note's own H1, then to a readable form of the key. Returning the
	// raw key meant 33 notes in one vault showed up in search as
	// "paas-resurrected-weekly-by-root-maintenance-cron", which is the identifier, not
	// a title: harder to scan, and it is the FIRST thing an agent or a human reads on a
	// result card. A title that merely repeats the id is treated as absent for the same
	// reason. Nothing is invented here, both fallbacks are the note's own words.
	for _, h := range pn.Headings {
		if h.Level == 1 && h.Text != "" && h.Text != pn.FM.ID {
			return h.Text
		}
	}
	return humanizeKey(pn.Key)
}

// humanizeKey turns a kebab-case note key into something readable: hyphens to spaces and
// a leading capital. Deliberately minimal, since anything cleverer (title casing, acronym
// detection) would start guessing at words it cannot know.
func humanizeKey(key string) string {
	if key == "" {
		return key
	}
	out := strings.ReplaceAll(key, "-", " ")
	r := []rune(out)
	if r[0] >= 'a' && r[0] <= 'z' {
		r[0] -= 32
	}
	return string(r)
}

// RetrievalHash is the exported retrieval hash for a parsed note: it is what the
// notes table stores in retrieval_hash, and the embedder stamps onto each vector
// (note_hash) so a later content change can be detected and the stale vector
// excluded from retrieval.
func RetrievalHash(pn *ParsedNote) string { return retrievalHash(pn) }

// retrievalHash is SHA256 over the node identity (effectiveID) plus the body and every
// field that affects retrieval: the retrieval-critical frontmatter (type, status,
// supersedes, related) AND the fields the embedder puts into a note's vector chunk
// (title, do, dont, why, tags; see ChunkText). It is stamped onto each vector as
// note_hash, so if it omitted the chunk fields, editing a gotcha's do/dont/why/title/
// tags would change the embedding input WITHOUT changing the staleness key, and the
// pre-correction vector would keep being served until a manual re-embed. Including them
// means such an edit invalidates the stale vector (and forces a cheap note reindex).
// The id is included because it is the node identity: an id-only edit must retire the
// old node and create the new one, so the drift check has to see it.
//
// The invariant, and why it is load-bearing: this hash is the ONLY drift signal. Both
// DriftReport and DriftDeltaReport compare hashes and Reconcile returns early when
// nothing differs, so any frontmatter field that reaches a persisted notes column or a
// graph node attr MUST be hashed here or an edit to it never reindexes. Scope is the
// sharp edge: it is the live access-control input (retrieve.go scopeAllowed reads the
// node's scope attr, Store.NoteScope reads notes.scope for direct fetch/neighbors), so
// while it was unhashed, tightening a note's scope on a running hub did not revoke read
// access until some unrelated event forced a reindex. updated/when/review_by feed
// freshness decay and lifecycle health, and source feeds provenance, with the same
// staleness. If you add a column to noteRowValues or an attr in BuildGraph, add it here.
func retrievalHash(pn *ParsedNote) string {
	h := sha256.New()
	h.Write([]byte(effectiveID(pn)))
	h.Write([]byte{0})
	h.Write([]byte(pn.Body))
	h.Write([]byte{0})
	h.Write([]byte(string(pn.FM.Type)))
	h.Write([]byte{0})
	h.Write([]byte(pn.FM.Status))
	// Embedding-chunk fields (ChunkText): a change here must invalidate the vector.
	h.Write([]byte{0})
	h.Write([]byte(titleOf(pn)))
	for _, s := range []string{pn.FM.Do, pn.FM.Dont, pn.FM.Why} {
		h.Write([]byte{0})
		h.Write([]byte(s))
	}
	for _, s := range pn.FM.Tags {
		h.Write([]byte{0})
		h.Write([]byte(s))
	}
	for _, s := range pn.FM.Supersedes {
		h.Write([]byte{0})
		h.Write([]byte(s))
	}
	for _, s := range pn.FM.Related {
		h.Write([]byte{0})
		h.Write([]byte(s))
	}
	// Persisted-column / graph-attr fields: scope (notes.scope + the node's scope attr,
	// the access-control input), updated + when (notes.updated, freshness decay),
	// review_by (notes.review_by, lifecycle health) and source (notes.source,
	// provenance). EffectiveScopes is hashed rather than FM.Scope so the fail-safe
	// default is what the hash describes, matching exactly what gets persisted.
	for _, s := range pn.FM.EffectiveScopes() {
		h.Write([]byte{0})
		h.Write([]byte(s))
	}
	for _, s := range []string{pn.FM.Updated, pn.FM.When, pn.FM.ReviewBy, pn.FM.Source} {
		h.Write([]byte{0})
		h.Write([]byte(s))
	}
	return hex.EncodeToString(h.Sum(nil))
}
