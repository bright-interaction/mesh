// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package web

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/mcp"
	"github.com/bright-interaction/mesh/internal/vault"
)

// The review queue: auto-extracted write-back candidates (the input side of the
// flywheel) that a human promotes into the vault with one click, or discards. Two
// gates keep it high-signal: on the way in, writeToPending drops the extractor's
// low-confidence self-ratings and lets a judge veto weak notes (so the queue is the
// judged set, not every raw extraction), and on the way out a human promotes or
// discards, so nothing lands unreviewed. GET lists; promote writes a real note +
// clears the item; discard clears.

func (s *Server) handlePendingList(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) { // extraction candidates are dev-scoped review content
		return
	}
	items, err := s.store.ListPending()
	if err != nil {
		http.Error(w, "list failed", http.StatusInternalServerError)
		return
	}
	if items == nil {
		items = []index.PendingNote{}
	}
	writeJSON(w, map[string]any{"pending": items})
}

func (s *Server) handlePendingPromote(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&req); err != nil || req.ID == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	p, err := s.store.GetPending(req.ID)
	if err != nil {
		http.Error(w, "unknown pending id", http.StatusNotFound)
		return
	}
	res, err := vault.CreateNote(s.vaultRoot, vault.NewNoteSpec{
		Type:       vault.NoteType(p.Type),
		Title:      noDash(p.Title),
		Do:         noDash(p.Do),
		Dont:       noDash(p.Dont),
		Why:        noDash(p.Why),
		Confidence: p.Confidence,
		Source:     "agent",
		Agent:      "mesh-extract",
		By:         "mesh-extract",
	})
	if err != nil {
		// Never echo the raw error. Everything vault.CreateNote raises about the
		// FILESYSTEM names the note's ABSOLUTE path, so a promote that hit an unwritable
		// note directory answered "create note failed: open <server vault root>/gotchas/
		// <slug>.md: permission denied" and handed a member the server's layout. Same
		// leak internal/mcp closed on its own write path; both surfaces now call the one
		// scrubber, so a fix to it cannot land on only one of them again. Detail stays in
		// the server log, where it is free.
		slog.Error("mesh ui: promoting a pending candidate failed", "id", req.ID, "type", p.Type, "error", err)
		http.Error(w, "create note failed: "+mcp.ScrubPathsUnder(err.Error(), s.vaultRoot), http.StatusInternalServerError)
		return
	}
	// Promoting a candidate IS a write-back, so stamp it in the flywheel now (source
	// "agent"), exactly like a direct mesh_append_note via the MCP does. Without this the
	// authored count only caught promoted notes at the next backfill, undercounting the
	// flywheel's input and diverging from the MCP write path.
	_ = s.store.RecordWriteback(res.ID, "agent")
	// The candidate is now a real note; clear it from the queue and refresh the graph
	// so it is immediately searchable.
	_ = s.store.DeletePending(req.ID)
	if g, e := index.Reindex(s.store, s.vaultRoot); e == nil {
		// One exclusive critical section for the swap + invalidation, so an in-flight
		// retriever build cannot publish over the old graph (see Server.retriever).
		s.mu.Lock()
		s.graph = g
		s.cachedRetriever.Store(nil)
		s.mu.Unlock()
	}
	// Return a vault-relative path, never the server's absolute filesystem path. This one
	// leaked on EVERY successful promote, not just on an error: in member mode the caller
	// is a remote teammate, and exposedVaultRoot already keeps the absolute root off
	// /api/status for exactly that reason. Same relativization the MCP write path does,
	// and the same shape /api/note already returns.
	notePath := res.Path
	if rel, err := filepath.Rel(s.vaultRoot, res.Path); err == nil && !strings.HasPrefix(rel, "..") {
		notePath = rel
	} else {
		notePath = filepath.Base(res.Path)
	}
	writeJSON(w, map[string]any{"promoted": true, "id": res.ID, "path": notePath})
}

func (s *Server) handlePendingDiscard(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&req); err != nil || req.ID == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.store.DeletePending(req.ID); err != nil {
		http.Error(w, "discard failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"discarded": true})
}

// noDash strips em/en dashes (house style: no em dashes ever) so a promoted note never
// trips the pre-commit em-dash guard when the vault is committed.
func noDash(s string) string {
	return strings.Map(func(r rune) rune {
		if r == 0x2014 || r == 0x2013 { // em dash, en dash -> hyphen
			return '-'
		}
		return r
	}, s)
}
