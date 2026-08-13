// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import "fmt"

// Two files that resolve to the same effective note id are a data error the schema
// cannot represent: notes.id is the PRIMARY KEY, and the graph keys a note node on
// "note:"+id. Exactly one of the two can be indexed, so Mesh picks one deterministically
// and QUARANTINES the rest: they are dropped from BOTH the graph and the notes table and
// reported through DroppedNotes (mesh health, mesh_health, mesh doctor, the reindex log).
//
// The rule lives here, in one place, because it has to be the SAME rule everywhere. It
// used to exist only on the incremental path, so the full path (ReindexFull, `mesh index`)
// collapsed a duplicate differently: INSERT OR REPLACE made the LAST file win the notes
// row while BuildGraph's AddNode made the FIRST one win the graph node. A search card then
// showed one file's path next to the other file's snippet, and the losing file, having no
// notes row, was reported as Added by every DriftReport forever, so `mesh doctor` printed
// STALE and exited 1 while `mesh index` produced a byte-identical index every time.

// duplicateIDErr is the single wording for a quarantined duplicate. It is wrapped, not
// merely formatted, so consumers tell a duplicate from unparseable frontmatter with
// errors.Is instead of matching message text, and it names the remedy because a stranger
// hitting this (two README.md files, a copied template that kept its id: line) has no way
// to guess it. It names only vault-relative paths, so a remote surface can echo it.
func duplicateIDErr(id, owner string) error {
	return fmt.Errorf("%w %q: already claimed by %q; give one of the two notes a different id "+
		"(edit its `id:` frontmatter, or rename the file if it has none), then reindex", ErrDuplicateNoteID, id, owner)
}

// idClaim is one file's claim on a note id in a single index pass. Path is
// vault-relative, so a claim compares equal to the path stored in notes.path.
type idClaim struct {
	ID   string
	Path string
}

// resolveIDOwners picks the one file that owns each id. incumbent maps id -> the path
// that currently holds it in the index (nil for a cold index); when the incumbent is
// among the claimants it keeps the id, otherwise the first claimant in walk order wins.
//
// Preferring the incumbent is what makes the full and the incremental path agree. Walk
// order alone would be enough for a cold index, but the incremental reconcile can only
// ever see the incumbent (it does not re-parse unchanged files), so with a walk-order
// rule a full reindex and a reconcile would hand the id to different files and the winner
// would FLIP depending on which ran last. Preferring the incumbent is also stable: it
// never demotes a note that is already indexed and searchable.
func resolveIDOwners(claims []idClaim, incumbent map[string]string) map[string]string {
	owner := make(map[string]string, len(claims))
	for _, c := range claims {
		if c.ID == "" {
			continue
		}
		cur, seen := owner[c.ID]
		if !seen {
			owner[c.ID] = c.Path
			continue
		}
		if cur == c.Path {
			continue
		}
		if inc, ok := incumbent[c.ID]; ok && inc == c.Path {
			owner[c.ID] = c.Path
		}
	}
	return owner
}

// ClaimUniqueIDs enforces the one-file-per-id invariant on a whole-vault parse before the
// graph is built and before anything is written. It returns the notes to index and a
// FileError wrapping ErrDuplicateNoteID for every file it quarantined, which the caller
// passes to RecordDropped so the loss is visible instead of silent.
//
// notes must already carry vault-relative Paths, so the incumbent comparison (and the
// reported path) matches notes.path. incumbent may be nil.
func ClaimUniqueIDs(notes []*ParsedNote, incumbent map[string]string) (kept []*ParsedNote, dropped []FileError) {
	claims := make([]idClaim, 0, len(notes))
	for _, pn := range notes {
		claims = append(claims, idClaim{ID: effectiveID(pn), Path: pn.Path})
	}
	owner := resolveIDOwners(claims, incumbent)
	kept = make([]*ParsedNote, 0, len(notes))
	for _, pn := range notes {
		id := effectiveID(pn)
		if o, ok := owner[id]; ok && o != pn.Path {
			dropped = append(dropped, FileError{Path: pn.Path, Err: duplicateIDErr(id, o)})
			continue
		}
		kept = append(kept, pn)
	}
	return kept, dropped
}

// IDOwners maps every indexed note id to the vault-relative path that currently owns it.
// It is the incumbent set ClaimUniqueIDs and the drift reports resolve ties against.
func (s *Store) IDOwners() (map[string]string, error) {
	rows, err := s.readDB.Query(`SELECT id, path FROM notes`)
	if err != nil {
		return nil, err
	}
	// A leaked *sql.Rows pins a WAL read snapshot for the life of the process, which
	// stops every checkpoint from reclaiming past it.
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, path string
		if err := rows.Scan(&id, &path); err != nil {
			return nil, err
		}
		out[id] = path
	}
	return out, rows.Err()
}
