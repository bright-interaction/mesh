// Package mcp serves Mesh's retrieval + write-back surface to a coding agent
// over JSON-RPC 2.0 on stdio. A local agent (Claude Code / Codex) spawns
// `mesh mcp` and talks to it directly; no port or auth surface. The JSON-RPC
// envelope matches the hephaestus MCP house pattern.
package mcp

import (
	"context"
	"encoding/json"
	"io"
	"os"

	"github.com/brightinteraction/mesh/internal/graph"
	"github.com/brightinteraction/mesh/internal/index"
	"github.com/brightinteraction/mesh/internal/retrieve"
)

const (
	protocolVersion = "2024-11-05"
	serverName      = "mesh"
	serverVersion   = "0.1.0"
)

// Server holds the live index, graph, and retriever for one vault.
type Server struct {
	vaultRoot string
	store     *index.Store
	graph     *graph.Graph
	retriever *retrieve.Retriever
}

// NewServer opens the vault's index and loads it into memory.
func NewServer(vaultRoot string) (*Server, error) {
	store, err := index.Open(vaultRoot)
	if err != nil {
		return nil, err
	}
	s := &Server{vaultRoot: vaultRoot, store: store}
	if err := s.reload(); err != nil {
		store.Close()
		return nil, err
	}
	return s, nil
}

func (s *Server) Close() error { return s.store.Close() }

// reload re-indexes the vault and rebuilds the in-memory graph + retriever. Run
// after a write-back so new notes are immediately retrievable.
func (s *Server) reload() error {
	g, err := index.Reindex(s.store, s.vaultRoot)
	if err != nil {
		return err
	}
	s.graph = g
	s.retriever = retrieve.NewFromEnv(s.store, g)
	return nil
}

// ServeStdio reads newline-delimited JSON-RPC requests from stdin and writes
// responses to stdout until EOF.
func (s *Server) ServeStdio() error {
	dec := json.NewDecoder(os.Stdin)
	enc := json.NewEncoder(os.Stdout)
	for {
		var req request
		if err := dec.Decode(&req); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		// Notifications (no id) expect no response.
		if len(req.ID) == 0 || string(req.ID) == "null" {
			continue
		}
		result, rerr := s.dispatch(context.Background(), req)
		resp := response{JSONRPC: "2.0", ID: req.ID}
		if rerr != nil {
			resp.Error = rerr
		} else {
			resp.Result = result
		}
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
}

func (s *Server) dispatch(ctx context.Context, req request) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		return s.handleInitialize(), nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return s.handleToolsList(), nil
	case "tools/call":
		return s.handleToolsCall(req.Params)
	case "resources/list":
		return s.handleResourcesList(), nil
	case "resources/read":
		return s.handleResourcesRead(req.Params)
	default:
		return nil, &rpcError{Code: codeMethodNotFound, Message: "method not found", Data: req.Method}
	}
}

func (s *Server) handleInitialize() any {
	return map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities": map[string]any{
			"tools":     map[string]any{"listChanged": false},
			"resources": map[string]any{"listChanged": false, "subscribe": false},
		},
		"serverInfo":   map[string]any{"name": serverName, "version": serverVersion},
		"instructions": contractText,
	}
}

// ---- JSON-RPC envelope (matches the hephaestus house shape) ----

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

const (
	codeInvalidParams  = -32602
	codeInternalError  = -32603
	codeMethodNotFound = -32601
)

func internalErr(err error) *rpcError {
	return &rpcError{Code: codeInternalError, Message: err.Error()}
}

// textResult wraps a value as an MCP text content block, JSON-encoding it so the
// agent gets terse structured data, not chatty prose.
func textResult(v any) any {
	b, _ := json.Marshal(v)
	return map[string]any{"content": []map[string]any{{"type": "text", "text": string(b)}}}
}

func rawText(s string) any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": s}}}
}
