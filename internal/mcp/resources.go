// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"context"
	"encoding/json"
	"path/filepath"
)

func (s *Server) handleResourcesList() any {
	return map[string]any{
		"resources": []map[string]any{
			{"uri": "mesh://capabilities", "name": "Mesh capabilities", "description": "Vault stats + tool surface", "mimeType": "application/json"},
			{"uri": "mesh://contract", "name": "Agent usage contract", "description": "How to retrieve from Mesh cheaply", "mimeType": "text/markdown"},
			{"uri": "mesh://community", "name": "Community overview", "description": "The vault's clusters by size, each with an exemplar, for orientation", "mimeType": "application/json"},
			{"uri": "mesh://stats", "name": "Retrieval stats", "description": "Which signals are active AND reachable (each BYOAI endpoint is probed), plus how fresh the vectors are (live vs stale, re-embed coverage)", "mimeType": "application/json"},
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
		return contents(p.URI, "application/json", s.statsJSON(ctx)), nil
	case "mesh://capabilities":
		// resources/read is NOT routed through handleToolsCall, so the scope class gate
		// never runs here: the filtering has to be done inline. A scope-confined caller
		// must not learn global corpus volume from a resource when mesh_reindex is
		// careful to hide exactly that, so report the caller's readable view instead.
		notes, nodes, edges := s.counts(scopeFromCtx(ctx))
		b, _ := json.Marshal(map[string]any{
			"server": map[string]any{"name": serverName, "version": serverVersion},
			// The vault NAME, never the server's absolute path: on a hosted hub the
			// full value would leak the server's absolute vault path to every
			// authenticated client, which is the same disclosure toolWrite and
			// toolReindex already suppress.
			"vault":    filepath.Base(s.vaultRoot),
			"counts":   map[string]any{"notes": notes, "nodes": nodes, "edges": edges},
			"tools":    ToolNames(), // derived from ToolSpecs() so it never drifts from the real surface
			"contract": "mesh://contract",
			"stats":    "mesh://stats",
		})
		return contents(p.URI, "application/json", string(b)), nil
	default:
		return nil, &rpcError{Code: codeInvalidParams, Message: "unknown resource", Data: p.URI}
	}
}

// counts returns the note/node/edge totals VISIBLE to this caller. An unrestricted
// caller (nil filter or nil AllowedRead: a solo run, an admin) gets the stored totals;
// a scope-confined caller gets only their readable subgraph, so an aggregate cannot be
// used to learn the volume of another scope. Same helper mesh_reindex uses, so the two
// surfaces cannot drift apart again.
func (s *Server) counts(sf *ScopeFilter) (notes, nodes, edges int) {
	if sf == nil || sf.AllowedRead == nil {
		notes, _ = s.store.Count("notes")
		nodes, _ = s.store.Count("nodes")
		edges, _ = s.store.Count("edges")
		return notes, nodes, edges
	}
	g, _ := s.snapshot()
	nodes, edges = scopedGraphCounts(g, sf)
	for _, n := range g.Nodes() {
		if n.Kind == "note" && sf.allowsNode(n) {
			notes++
		}
	}
	return notes, nodes, edges
}

// statsJSON reports the live retrieval state: which signals will fire and how fresh the
// vectors are.
//
// It PROBES the configured BYOAI endpoints (retrieve.Signals, the same report
// `mesh status` renders) rather than answering from configuration. An agent asking this
// resource whether rerank is on was told "active: true" for an endpoint that had never
// answered a single request, so the agent's own reasoning about result quality was built
// on a flag that could not be false. reachable is the truthful field; active means only
// that the operator asked for the stage.
func (s *Server) statsJSON(ctx context.Context) string {
	_, r := s.snapshot()
	sig := r.Signals(ctx)
	model, dim := s.store.VectorMeta()
	total, live, stale, _ := s.store.VectorStats()
	freshPct := 0.0
	if total > 0 {
		freshPct = float64(live) / float64(total) * 100
	}
	b, _ := json.Marshal(map[string]any{
		// Vault NAME only: the absolute server path is an infrastructure detail no
		// client needs (see the capabilities branch above).
		"vault": filepath.Base(s.vaultRoot),
		"vectors": map[string]any{
			"active":          sig.VectorsConfigured, // embedder configured AND live vectors present
			"reachable":       sig.VectorsReachable,  // the endpoint answered a probe just now
			"error":           sig.VectorsError,      // empty unless the probe failed
			"model":           model,
			"dim":             dim,
			"total":           total,
			"live":            live,
			"stale_or_orphan": stale, // edited or deleted notes; cleared by mesh embed
			"fresh_pct":       freshPct,
			"ann":             sig.ANN, // HNSW index serving the scan (vs brute force)
		},
		"rerank": map[string]any{
			"active":    sig.RerankConfigured,
			"reachable": sig.RerankReachable,
			"error":     sig.RerankError,
			"model":     sig.RerankModel,
		},
	})
	return string(b)
}

func contents(uri, mime, text string) any {
	return map[string]any{
		"contents": []map[string]any{{"uri": uri, "mimeType": mime, "text": text}},
	}
}
