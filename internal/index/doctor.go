package index

import (
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
