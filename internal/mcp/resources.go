package mcp

import "encoding/json"

func (s *Server) handleResourcesList() any {
	return map[string]any{
		"resources": []map[string]any{
			{"uri": "mesh://capabilities", "name": "Mesh capabilities", "description": "Vault stats + tool surface", "mimeType": "application/json"},
			{"uri": "mesh://contract", "name": "Agent usage contract", "description": "How to retrieve from Mesh cheaply", "mimeType": "text/markdown"},
		},
	}
}

func (s *Server) handleResourcesRead(params json.RawMessage) (any, *rpcError) {
	var p struct {
		URI string `json:"uri"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: "bad params"}
	}
	switch p.URI {
	case "mesh://contract":
		return contents(p.URI, "text/markdown", contractText), nil
	case "mesh://capabilities":
		notes, _ := s.store.Count("notes")
		nodes, _ := s.store.Count("nodes")
		edges, _ := s.store.Count("edges")
		b, _ := json.Marshal(map[string]any{
			"server":  map[string]any{"name": serverName, "version": serverVersion},
			"vault":   s.vaultRoot,
			"counts":  map[string]any{"notes": notes, "nodes": nodes, "edges": edges},
			"tools":   []string{"mesh_search", "mesh_fetch", "mesh_god_nodes", "mesh_changed_since", "mesh_append_note", "mesh_write_entity"},
			"contract": "mesh://contract",
		})
		return contents(p.URI, "application/json", string(b)), nil
	default:
		return nil, &rpcError{Code: codeInvalidParams, Message: "unknown resource", Data: p.URI}
	}
}

func contents(uri, mime, text string) any {
	return map[string]any{
		"contents": []map[string]any{{"uri": uri, "mimeType": mime, "text": text}},
	}
}
