// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"log/slog"
	"sync"
	"time"

	"github.com/bright-interaction/mesh/internal/graph"
)

// NoteCache holds the last-parsed notes for a long-running indexer (the watcher /
// MCP server) so an incremental reconcile can rebuild the in-memory graph without
// re-parsing the whole vault from disk. Keyed by effectiveID; byPath maps a
// vault-relative path back to its id so a removed file (which can no longer be
// parsed) resolves to the id it was indexed under.
type NoteCache struct {
	mu     sync.Mutex
	byID   map[string]*ParsedNote
	byPath map[string]string
}

// NewNoteCache returns an empty cache.
func NewNoteCache() *NoteCache {
	return &NoteCache{byID: map[string]*ParsedNote{}, byPath: map[string]string{}}
}

// Seed replaces the cache contents with the given notes (a full-reindex result).
func (c *NoteCache) Seed(notes []*ParsedNote) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.byID = make(map[string]*ParsedNote, len(notes))
	c.byPath = make(map[string]string, len(notes))
	for _, pn := range notes {
		id := effectiveID(pn)
		c.byID[id] = pn
		c.byPath[pn.Path] = id
	}
}

// Snapshot returns the cached notes. Order is unspecified: BuildGraph resolves by
// id and DetectCommunities sorts ids internally, so the resulting graph is
// order-invariant.
func (c *NoteCache) Snapshot() []*ParsedNote {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*ParsedNote, 0, len(c.byID))
	for _, pn := range c.byID {
		out = append(out, pn)
	}
	return out
}

// Apply mutates the cache for a delta: removedIDs drop their entries; upserts
// replace/insert by id and refresh the path index, retiring a stale path (rename)
// or a stale id (frontmatter id changed under the same path).
func (c *NoteCache) Apply(upserts []*ParsedNote, removedIDs []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, id := range removedIDs {
		if pn, ok := c.byID[id]; ok {
			delete(c.byPath, pn.Path)
			delete(c.byID, id)
		}
	}
	for _, pn := range upserts {
		id := effectiveID(pn)
		if old, ok := c.byID[id]; ok && old.Path != pn.Path {
			delete(c.byPath, old.Path) // same id moved to a new path (rename)
		}
		if oldID, ok := c.byPath[pn.Path]; ok && oldID != id {
			delete(c.byID, oldID) // same path now holds a new id (id changed)
		}
		c.byID[id] = pn
		c.byPath[pn.Path] = id
	}
}

// LiveIndexer bundles a NoteCache with the seed-then-incremental policy a single
// long-running watcher needs. The first Reconcile (or any Full) does a complete
// reindex and seeds the cache; subsequent Reconciles are incremental. It is safe
// for one watcher goroutine plus occasional Full() calls (its own mutex serializes
// them); the MCP server uses its own cache under reloadMu instead.
type LiveIndexer struct {
	store *Store
	root  string

	mu         sync.Mutex
	cache      *NoteCache
	seeded     bool
	lastHealth time.Time // when this owner last refreshed the persisted health findings
}

// healthRefreshInterval is how often the owning writer recomputes the lifecycle health
// findings (dead refs, overdue reviews, contradictions) into note_health.
//
// Someone has to, and it can only be a writer. The pass is a write, so a read-only
// `mesh mcp` window computes its answer in memory and persists nothing; before the
// single-writer split those windows wrote the rows as a side effect of an agent calling
// mesh_health, and the web dashboard read them. Nothing else refreshes them, so the
// dashboard would sit on whatever the last `mesh health` run left behind. Derived state
// belongs to the process that owns the index, on a cadence slow enough that a whole-vault
// walk is nowhere near a per-tick cost.
const healthRefreshInterval = 5 * time.Minute

// NewLiveIndexer returns a LiveIndexer for the given store + vault root.
func NewLiveIndexer(store *Store, root string) *LiveIndexer {
	return &LiveIndexer{store: store, root: root, cache: NewNoteCache()}
}

// Full forces a complete reindex and (re)seeds the cache. Used after a write-back
// that bypassed the watcher.
func (li *LiveIndexer) Full() (*graph.Graph, error) {
	li.mu.Lock()
	defer li.mu.Unlock()
	g, notes, err := ReindexFull(li.store, li.root)
	if err != nil {
		return nil, err
	}
	li.cache.Seed(notes)
	li.seeded = true
	return g, nil
}

// Reconcile brings the index up to date. The first call does a full reindex to
// seed the cache; later calls are incremental (parse only changed files, targeted
// note/FTS writes, in-memory graph rebuild). authoritative=false enables the mtime
// fast path (skip parsing mtime-unchanged files); pass true for the periodic
// safety tick so a mtime-preserving edit is still caught.
func (li *LiveIndexer) Reconcile(authoritative bool) (Reconciliation, error) {
	li.mu.Lock()
	defer li.mu.Unlock()
	li.drainOps()
	if !li.seeded {
		start := time.Now()
		g, notes, err := ReindexFull(li.store, li.root)
		if err != nil {
			return Reconciliation{}, err
		}
		li.cache.Seed(notes)
		li.seeded = true
		li.refreshHealthIfDue()
		return Reconciliation{
			Added:     len(notes),
			Reindexed: true,
			Graph:     g,
			Dropped:   len(li.store.DroppedNotes()), // recorded by ReindexFull
			Dur:       time.Since(start),
		}, nil
	}
	rec, err := ReconcileIncremental(li.store, li.root, li.cache, !authoritative)
	if err == nil {
		li.refreshHealthIfDue()
	}
	return rec, err
}

// drainOps applies whatever a read-only surface queued for this owner, before the
// reindex rather than after: a promote enqueues its bookkeeping AND writes the note
// file, and the caller is waiting on both, so making it wait an extra reconcile for
// the half that costs nothing would be gratuitous. Called with li.mu held.
//
// Best effort in the same sense as the health pass: an op that cannot be applied must
// not fail the reindex retrieval depends on. The op file survives a failure, so the
// next pass retries it and the caller's own wait is what reports the delay.
func (li *LiveIndexer) drainOps() {
	n, err := li.store.DrainOps()
	if err != nil {
		slog.Warn("mesh: could not drain the owner op queue", "err", err)
	}
	if n > 0 {
		slog.Debug("mesh: applied queued ops", "count", n)
	}
}

// refreshHealthIfDue recomputes the persisted health findings when they are older than
// healthRefreshInterval. Called with li.mu held, from the one goroutine that reconciles,
// so the passes can never overlap. Best effort: a health pass that fails must never fail
// the reindex it rode in on, because the index is what retrieval depends on and the
// findings are a lifecycle report about it.
func (li *LiveIndexer) refreshHealthIfDue() {
	if !li.lastHealth.IsZero() && time.Since(li.lastHealth) < healthRefreshInterval {
		return
	}
	now := time.Now()
	li.lastHealth = now
	if _, err := li.store.ComputeHealth(li.root, now); err != nil {
		slog.Warn("mesh: could not refresh the health findings", "err", err)
		return
	}
	if _, err := li.store.ComputeContradictions(now); err != nil {
		slog.Warn("mesh: could not refresh the contradiction findings", "err", err)
	}
}
