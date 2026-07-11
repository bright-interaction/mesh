// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package web

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/bright-interaction/mesh/internal/index"
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
		http.Error(w, "create note failed: "+err.Error(), http.StatusInternalServerError)
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
		s.mu.Lock()
		s.graph = g
		s.cachedRetriever = nil
		s.mu.Unlock()
	}
	writeJSON(w, map[string]any{"promoted": true, "id": res.ID, "path": res.Path})
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
