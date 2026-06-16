package index

import (
	"time"

	"github.com/bright-interaction/mesh/internal/graph"
)

// Reconciliation reports what a Reconcile did: the drift it found and, when it
// rebuilt, the freshly loaded graph so a long-running server can swap it in.
type Reconciliation struct {
	Added     int
	Changed   int
	Removed   int
	Reindexed bool
	Graph     *graph.Graph // non-nil only when Reindexed
	Dur       time.Duration
}

// Any reports whether the vault had drifted from the index.
func (r Reconciliation) Any() bool { return r.Added+r.Changed+r.Removed > 0 }

// Reconcile brings the index up to date with the vault on disk, but only does
// the expensive rebuild when retrieval-relevant content actually changed. It
// runs the same content-hash DriftReport `mesh doctor` uses; if a file's mtime
// moved but its retrieval hash did not (a cosmetic edit, a touch), DriftReport
// reports no drift and Reconcile skips the reindex. This is the convergent,
// idempotent core the watcher calls on every file event and on its periodic
// safety tick: run it twice in a row with no edits in between and the second is
// a cheap no-op.
func Reconcile(s *Store, root string) (Reconciliation, error) {
	start := time.Now()
	d, err := s.DriftReport(root)
	if err != nil {
		return Reconciliation{}, err
	}
	r := Reconciliation{Added: len(d.Added), Changed: len(d.Changed), Removed: len(d.Removed)}
	if !d.Any() {
		r.Dur = time.Since(start)
		return r, nil
	}
	g, err := Reindex(s, root)
	if err != nil {
		return Reconciliation{}, err
	}
	r.Reindexed = true
	r.Graph = g
	r.Dur = time.Since(start)
	return r, nil
}
