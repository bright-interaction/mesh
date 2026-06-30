// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"context"
	"encoding/json"
)

func (s *Server) handleResourcesList() any {
	return map[string]any{
		"resources": []map[string]any{
			{"uri": "mesh://capabilities", "name": "Mesh capabilities", "description": "Vault stats + tool surface", "mimeType": "application/json"},
			{"uri": "mesh://contract", "name": "Agent usage contract", "description": "How to retrieve from Mesh cheaply", "mimeType": "text/markdown"},
			{"uri": "mesh://community", "name": "Community overview", "description": "The vault's clusters by size, each with an exemplar, for orientation", "mimeType": "application/json"},
			{"uri": "mesh://stats", "name": "Retrieval stats", "description": "Which signals are active and how fresh the vectors are (live vs stale, re-embed coverage)", "mimeType": "application/json"},
		},
	}
}

func (s *Server) handleResourcesRead(ctx context.Context, params json.RawMessage) (any, *rpcError) {
	var p struct {
		URI string `json:"uri"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: "bad params"}
	}
	switch p.URI {
	case "mesh://contract":
		return contents(p.URI, "text/markdown", contractText), nil
	case "mesh://community":
		g, _ := s.snapshot()
		b, _ := json.Marshal(map[string]any{"communities": communityOverview(g, 50, scopeFromCtx(ctx))})
		return contents(p.URI, "application/json", string(b)), nil
	case "mesh://stats":
		return contents(p.URI, "application/json", s.statsJSON()), nil
	case "mesh://capabilities":
		notes, _ := s.store.Count("notes")
		nodes, _ := s.store.Count("nodes")
		edges, _ := s.store.Count("edges")
		b, _ := json.Marshal(map[string]any{
			"server":   map[string]any{"name": serverName, "version": serverVersion},
			"vault":    s.vaultRoot,
			"counts":   map[string]any{"notes": notes, "nodes": nodes, "edges": edges},
			"tools":    []string{"mesh_search", "mesh_fetch", "mesh_god_nodes", "mesh_changed_since", "mesh_neighbors", "mesh_community", "mesh_append_note", "mesh_write_entity", "mesh_reindex"},
			"contract": "mesh://contract",
			"stats":    "mesh://stats",
		})
		return contents(p.URI, "application/json", string(b)), nil
	default:
		return nil, &rpcError{Code: codeInvalidParams, Message: "unknown resource", Data: p.URI}
	}
}

// statsJSON reports the live retrieval state: which signals will fire and how
// fresh the vectors are. Lightweight - it reads vector counts + meta and the
// already-built retriever from the snapshot; it does NOT load vector blobs or
// probe the embedding endpoint.
func (s *Server) statsJSON() string {
	_, r := s.snapshot()
	model, dim := s.store.VectorMeta()
	total, live, stale, _ := s.store.VectorStats()
	freshPct := 0.0
	if total > 0 {
		freshPct = float64(live) / float64(total) * 100
	}
	b, _ := json.Marshal(map[string]any{
		"vault": s.vaultRoot,
		"vectors": map[string]any{
			"active":          r.VectorsActive(), // embedder configured AND live vectors present
			"model":           model,
			"dim":             dim,
			"total":           total,
			"live":            live,
			"stale_or_orphan": stale, // edited or deleted notes; cleared by mesh embed
			"fresh_pct":       freshPct,
			"ann":             r.HNSWActive(), // HNSW index serving the scan (vs brute force)
		},
		"rerank": map[string]any{
			"active": r.RerankActive(),
			"model":  r.RerankModel(),
		},
	})
	return string(b)
}

func contents(uri, mime, text string) any {
	return map[string]any{
		"contents": []map[string]any{{"uri": uri, "mimeType": mime, "text": text}},
	}
}
