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
	items, err := s.store.ListPendingContext(r.Context())
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
	// A syntactically valid promotion request is accepted here. From this point on a
	// browser disconnect must not abandon it, while process shutdown must still cancel
	// pre-publication work and let the durable compensation path take over afterward.
	durableCtx := s.lifetimeContext()
	p, err := s.store.GetPendingContext(durableCtx, req.ID)
	if err != nil {
		http.Error(w, "unknown pending id", http.StatusNotFound)
		return
	}
	// Process shutdown must be able to stop the pre-publication whole-vault id scan.
	// CreateNoteContext deliberately finishes or removes its O_EXCL claim after the
	// publication boundary, so using the server lifetime here is safe on both sides.
	res, err := vault.CreateNoteContext(durableCtx, s.vaultRoot, vault.NewNoteSpec{
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
	if s.afterPendingFilePublished != nil {
		s.afterPendingFilePublished()
	}
	// The note file is now on disk, which is the durable part and the part that matters:
	// everything below is bookkeeping and indexing, and none of it can un-create it. So
	// from here on a failure downgrades the RESPONSE, never the outcome.
	//
	// Two index mutations follow. Promoting a candidate IS a write-back, so it is stamped
	// in the flywheel (source "agent") exactly like a direct mesh_append_note via the MCP;
	// without it the authored count only caught promoted notes at the next backfill. And
	// the candidate is now a real note, so it leaves the review queue. A read-only viewer
	// owns neither table, so both go to the owning writer as ops and this waits for them.
	// This work follows an already-durable file creation. A browser disconnect must not
	// strand that file outside the long-lived server's graph, so use the server lifetime
	// (canceled by SIGTERM), not the request lifetime, for the required follow-through.
	ownerDown, err := s.resolveIndexWrites(durableCtx,
		func() error {
			_ = s.store.RecordWritebackContext(durableCtx, res.ID, "agent")
			return s.store.DeletePendingContext(durableCtx, req.ID)
		},
		index.Op{Kind: index.OpDeletePending, ID: req.ID},
		index.Op{Kind: index.OpRecordWriteback, NoteID: res.ID, Source: "agent"},
	)
	if err != nil {
		slog.Error("mesh ui: promoted a note but could not settle its bookkeeping", "id", req.ID, "note", res.ID, "error", err)
		ownerDown = true // the note exists; say what is missing rather than failing the promote
	}
	// Make it searchable. The owner of the index reindexes; a reader waits for the owning
	// writer to have indexed the new note and then re-reads.
	if s.store.ReadOnly() {
		if werr := s.store.AwaitNoteIndexed(durableCtx, res.ID, s.ownerWait); werr != nil {
			ownerDown = true
		}
		if rerr := s.refreshContext(durableCtx); rerr != nil {
			slog.Error("mesh ui: reloading the graph after a promote failed", "error", rerr)
		}
	} else {
		if reindexErr := s.reindexAndPublish(durableCtx, false); reindexErr != nil {
			ownerDown = true
			slog.Error("mesh ui: promoted a note but could not reindex it", "note", res.ID, "error", reindexErr)
		}
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
	out := map[string]any{"promoted": true, "id": res.ID, "path": notePath}
	if ownerDown {
		// promoted stays true: the note exists at the path above. What is missing is the
		// indexing and the queue clearing, both of which land as soon as an owner runs.
		out["owner_down"] = true
		out["index_stale"] = true
		out["warning"] = ownerDownNote
	}
	writeJSON(w, out)
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
	// Same split as promote: the owner of the index deletes the row, a reader queues the
	// deletion for the owning writer and waits for it. A discard that reports success
	// while the item is still in the queue would have the reviewer discard it twice.
	ownerDown, err := s.resolveIndexWrites(r.Context(),
		func() error { return s.store.DeletePendingContext(r.Context(), req.ID) },
		index.Op{Kind: index.OpDeletePending, ID: req.ID},
	)
	if err != nil {
		slog.Error("mesh ui: discarding a pending candidate failed", "id", req.ID, "error", err)
		http.Error(w, "discard failed", http.StatusInternalServerError)
		return
	}
	out := map[string]any{"discarded": true}
	if ownerDown {
		// discarded stays true: the deletion is durably queued and will be applied. The
		// item may still show in the list until then, which is what the warning is for.
		out["owner_down"] = true
		out["warning"] = ownerDownNote
	}
	writeJSON(w, out)
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
