package index

import (
	"fmt"
	"path/filepath"
	"sort"

	"github.com/bright-interaction/mesh/internal/vault"
)

// Drift describes how the vault on disk has diverged from the persisted index.
type Drift struct {
	Added   []string // files present on disk but not in the index
	Changed []string // files whose retrieval-critical content changed since indexing
	Removed []string // files in the index but gone from disk
}

// Any reports whether the index is stale in any way.
func (d Drift) Any() bool { return len(d.Added)+len(d.Changed)+len(d.Removed) > 0 }

// DriftReport compares the current vault files against the persisted notes using
// the same retrieval_hash the indexer stores, so `mesh doctor` can tell the
// operator exactly which files need a reindex instead of guessing.
func (s *Store) DriftReport(root string) (Drift, error) {
	rows, err := s.readDB.Query(`SELECT path, retrieval_hash FROM notes`)
	if err != nil {
		return Drift{}, err
	}
	dbHash := map[string]string{}
	for rows.Next() {
		var p, h string
		if err := rows.Scan(&p, &h); err != nil {
			rows.Close()
			return Drift{}, err
		}
		dbHash[p] = h
	}
	rows.Close()

	files, err := vault.Walk(root)
	if err != nil {
		return Drift{}, err
	}
	var d Drift
	seen := map[string]bool{}
	for _, f := range files {
		rel, err := filepath.Rel(root, f)
		if err != nil {
			rel = f
		}
		seen[rel] = true
		pn, err := ParseFile(f)
		if err != nil {
			// A file that no longer parses but is in the index must be reported as
			// drift (a full reindex would drop it), not silently skipped.
			if _, ok := dbHash[rel]; ok {
				d.Removed = append(d.Removed, rel)
			}
			continue
		}
		h, ok := dbHash[rel]
		if !ok {
			d.Added = append(d.Added, rel)
		} else if retrievalHash(pn) != h {
			d.Changed = append(d.Changed, rel)
		}
	}
	for p := range dbHash {
		if !seen[p] {
			d.Removed = append(d.Removed, p)
		}
	}
	sort.Strings(d.Added)
	sort.Strings(d.Changed)
	sort.Strings(d.Removed)
	return d, nil
}

// DriftDelta is DriftReport plus the parsed notes for Added+Changed (so an
// incremental reconcile does not re-parse them) and the resolved ids of removed
// notes (so it can target the DELETEs). RemovedIDs also carries the OLD id of a
// note whose frontmatter id changed under the same path.
type DriftDelta struct {
	Upserts    []*ParsedNote // Added + Changed, parsed, vault-relative Path
	RemovedIDs []string      // ids whose files are gone, plus retired old ids
	Drift      Drift         // the path lists, for logging/reporting
}

// DriftDeltaReport is DriftReport that retains the parse work it must do anyway:
// it returns the parsed Added+Changed notes and the ids of removed notes, so the
// incremental reconcile path never parses a file twice. It uses the same
// authoritative retrieval_hash comparison, so a cosmetic edit (mtime moved, hash
// unchanged) yields no drift.
func (s *Store) DriftDeltaReport(root string) (DriftDelta, error) {
	rows, err := s.readDB.Query(`SELECT path, id, retrieval_hash FROM notes`)
	if err != nil {
		return DriftDelta{}, err
	}
	type rec struct{ id, hash string }
	dbByPath := map[string]rec{}
	for rows.Next() {
		var p, id, h string
		if err := rows.Scan(&p, &id, &h); err != nil {
			rows.Close()
			return DriftDelta{}, err
		}
		dbByPath[p] = rec{id, h}
	}
	rows.Close()

	files, err := vault.Walk(root)
	if err != nil {
		return DriftDelta{}, err
	}
	var dd DriftDelta
	upsertIDs := map[string]bool{}
	removed := map[string]bool{}
	seen := map[string]bool{}
	// finalOwner maps each id to the single path that will own it after this
	// reconcile. Two live files resolving to the same effectiveID is a data error
	// the schema cannot represent (notes.id is the PK); the full reindex aborts on
	// it, so we refuse here too with a clear message rather than let INSERT OR
	// REPLACE clobber a live note and flip-flop forever (never converging).
	finalOwner := map[string]string{}
	for _, f := range files {
		rel, err := filepath.Rel(root, f)
		if err != nil {
			rel = f
		}
		seen[rel] = true
		pn, err := ParseFile(f)
		if err != nil {
			// A file that no longer parses but is in the index must be dropped (a full
			// reindex would), not silently kept stale by the incremental cache path.
			if r, ok := dbByPath[rel]; ok {
				dd.Drift.Removed = append(dd.Drift.Removed, rel)
				removed[r.id] = true
			}
			continue
		}
		pn.Path = rel
		id := effectiveID(pn)
		if other, dup := finalOwner[id]; dup {
			return DriftDelta{}, fmt.Errorf("duplicate note id %q at %q and %q; ids must be unique before either can be indexed", id, other, rel)
		}
		finalOwner[id] = rel
		r, ok := dbByPath[rel]
		switch {
		case !ok:
			dd.Drift.Added = append(dd.Drift.Added, rel)
			dd.Upserts = append(dd.Upserts, pn)
			upsertIDs[id] = true
		case retrievalHash(pn) != r.hash:
			dd.Drift.Changed = append(dd.Drift.Changed, rel)
			dd.Upserts = append(dd.Upserts, pn)
			upsertIDs[id] = true
			if r.id != id { // frontmatter id changed under the same path: retire the old id
				removed[r.id] = true
			}
		}
	}
	for p, r := range dbByPath {
		if !seen[p] {
			dd.Drift.Removed = append(dd.Drift.Removed, p)
			removed[r.id] = true
		}
	}
	// A rename surfaces the old path as Removed and the new path as Added with the
	// SAME id; never delete an id we are upserting.
	for id := range removed {
		if !upsertIDs[id] {
			dd.RemovedIDs = append(dd.RemovedIDs, id)
		}
	}
	sort.Strings(dd.Drift.Added)
	sort.Strings(dd.Drift.Changed)
	sort.Strings(dd.Drift.Removed)
	sort.Strings(dd.RemovedIDs)
	return dd, nil
}
