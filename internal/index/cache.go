// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
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

	mu     sync.Mutex
	cache  *NoteCache
	seeded bool
}

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
	if !li.seeded {
		start := time.Now()
		g, notes, err := ReindexFull(li.store, li.root)
		if err != nil {
			return Reconciliation{}, err
		}
		li.cache.Seed(notes)
		li.seeded = true
		return Reconciliation{Added: len(notes), Reindexed: true, Graph: g, Dur: time.Since(start)}, nil
	}
	return ReconcileIncremental(li.store, li.root, li.cache, !authoritative)
}
