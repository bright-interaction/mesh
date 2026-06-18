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
	"sync"
	"time"

	"github.com/bright-interaction/mesh/internal/graph"
	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/retrieve"
	"github.com/bright-interaction/mesh/internal/watch"
)

const (
	protocolVersion = "2024-11-05"
	serverName      = "mesh"
	serverVersion   = "0.1.0"
)

// Server holds the live index, graph, and retriever for one vault.
//
// Concurrency: tool calls run on the ServeStdio goroutine while the optional
// background watcher (Watch) rebuilds on file changes. mu guards the graph +
// retriever pointers so a reader always sees a consistent pair; reloadMu makes a
// rebuild single-flight so the dispatch goroutine (a write-back) and the watcher
// never reindex at the same time.
type Server struct {
	vaultRoot string
	store     *index.Store

	mu        sync.RWMutex // guards graph + retriever
	graph     *graph.Graph
	retriever *retrieve.Retriever

	reloadMu sync.Mutex       // serializes rebuilds across dispatch + watcher
	cache    *index.NoteCache // parsed-note cache for incremental reconcile; guarded by reloadMu
}

// NewServer opens the vault's index and loads it into memory.
func NewServer(vaultRoot string) (*Server, error) {
	store, err := index.Open(vaultRoot)
	if err != nil {
		return nil, err
	}
	s := &Server{vaultRoot: vaultRoot, store: store, cache: index.NewNoteCache()}
	if err := s.reload(); err != nil {
		store.Close()
		return nil, err
	}
	return s, nil
}

func (s *Server) Close() error { return s.store.Close() }

// snapshot returns the current graph + retriever under a read lock, so a
// concurrent rebuild swapping them in never tears a reader's view.
func (s *Server) snapshot() (*graph.Graph, *retrieve.Retriever) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.graph, s.retriever
}

// swap atomically replaces the in-memory graph + retriever.
func (s *Server) swap(g *graph.Graph) {
	r := retrieve.NewFromEnv(s.store, g)
	s.mu.Lock()
	s.graph = g
	s.retriever = r
	s.mu.Unlock()
}

// reload fully re-indexes the vault and rebuilds the in-memory graph +
// retriever, seeding the parsed-note cache so later reconciles can be incremental.
// Run at startup and after a write-back so new notes are immediately retrievable.
func (s *Server) reload() error {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	g, notes, err := index.ReindexFull(s.store, s.vaultRoot)
	if err != nil {
		return err
	}
	s.cache.Seed(notes)
	s.swap(g)
	return nil
}

// reconcileOnce reindexes only when the vault has drifted, swapping in the fresh
// graph when it did. It is the watcher's reindex callback. Incremental: it parses
// only changed files and rebuilds the graph in memory from the cache.
func (s *Server) reconcileOnce(authoritative bool) (index.Reconciliation, error) {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	rec, err := index.ReconcileIncremental(s.store, s.vaultRoot, s.cache, !authoritative)
	if err != nil {
		return rec, err
	}
	if rec.Reindexed {
		s.swap(rec.Graph)
	}
	return rec, nil
}

// Watch live-reindexes the vault in the background until ctx is cancelled, so a
// long-running agent session sees notes a human edits in their editor without a
// restart. logf must write to stderr: stdout carries the JSON-RPC stream.
func (s *Server) Watch(ctx context.Context, debounce, reconcile time.Duration, logf func(string, ...any)) error {
	return watch.Run(ctx, watch.Options{
		Root:      s.vaultRoot,
		Debounce:  debounce,
		Reconcile: reconcile,
		Logf:      logf,
		OnReindex: func(authoritative bool) (watch.Result, error) {
			rec, err := s.reconcileOnce(authoritative)
			if err != nil {
				return watch.Result{}, err
			}
			return watch.Result{
				Added:     rec.Added,
				Changed:   rec.Changed,
				Removed:   rec.Removed,
				Reindexed: rec.Reindexed,
				Dur:       rec.Dur,
			}, nil
		},
	})
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
		return s.handleToolsCall(ctx, req.Params)
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
