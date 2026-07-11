// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

// Package mcp serves Mesh's retrieval + write-back surface to a coding agent
// over JSON-RPC 2.0 on stdio. A local agent (Claude Code / Codex) spawns
// `mesh mcp` and talks to it directly; no port or auth surface. The JSON-RPC
// envelope matches the hephaestus MCP house pattern.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
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

	ready    chan struct{} // closed when retrieval is servable (initial reload done)
	readyErr error         // written once before ready closes
	bg       chan struct{} // closed when ALL background startup work is done (enrichment included)

	agent string // calling client's name from initialize (provenance default), guarded by mu
}

// NewServer opens the vault's index (at <vaultRoot>/.mesh) and loads it into memory.
func NewServer(vaultRoot string) (*Server, error) {
	store, err := index.Open(vaultRoot)
	if err != nil {
		return nil, err
	}
	return newServerWithStore(vaultRoot, store)
}

// NewServerAt is like NewServer but keeps the index in an explicit dir instead of
// <vaultRoot>/.mesh. The hub uses it to serve hosted MCP over its vault while
// indexing OUTSIDE the git repo (so the index never syncs to clients).
func NewServerAt(vaultRoot, indexDir string) (*Server, error) {
	store, err := index.OpenAt(vaultRoot, indexDir)
	if err != nil {
		return nil, err
	}
	return newServerWithStore(vaultRoot, store)
}

func newServerWithStore(vaultRoot string, store *index.Store) (*Server, error) {
	s := &Server{vaultRoot: vaultRoot, store: store, cache: index.NewNoteCache(), ready: make(chan struct{}), bg: make(chan struct{})}
	// The initial load runs in the background so the MCP handshake answers
	// immediately: a full reload of a grown vault plus the note<->code bridge
	// exceeds a client's connect timeout (Claude Code kills the server at 30s
	// and never retries, orphaning the whole session), and a concurrent
	// `mesh code reindex` holding the db write lock makes it worse. ready
	// closes as soon as retrieval is servable (reload done) so a tool call
	// gating on awaitReady waits ~1s, not for the enrichment passes below it.
	go func() {
		defer close(s.bg)
		if err := s.reload(); err != nil {
			s.readyErr = fmt.Errorf("initial index load: %w", err)
			fmt.Fprintf(os.Stderr, "mesh mcp: %v\n", s.readyErr)
			close(s.ready)
			return
		}
		close(s.ready)
		// Seed the flywheel measurement from the existing agent-authored corpus once, so
		// the reuse number reflects accumulated knowledge from day one (idempotent).
		_, _ = store.BackfillWritebacks()
		_, _ = store.LinkNotesToCode(vaultRoot) // build the note<->code bridge if a code index exists
	}()
	return s, nil
}

// WaitReady blocks until ALL background startup work (initial reload plus the
// writeback backfill and note<->code bridge) has finished and reports the load
// error, for callers that need fully deterministic startup (tests, hub boot).
func (s *Server) WaitReady() error {
	<-s.bg
	return s.readyErr
}

// awaitReady blocks until the initial background load finishes. Early tool
// calls (a client may fire one right after the handshake) wait for the index
// rather than racing a nil graph.
func (s *Server) awaitReady(ctx context.Context) *rpcError {
	select {
	case <-s.ready:
		if s.readyErr != nil {
			return &rpcError{Code: codeInternalError, Message: s.readyErr.Error()}
		}
		return nil
	case <-ctx.Done():
		return &rpcError{Code: codeInternalError, Message: "index still loading: " + ctx.Err().Error()}
	}
}

// Reconcile re-reads the vault and rebuilds the in-memory index (authoritative).
// The hub calls this after a sync lands so hosted MCP serves fresh results.
func (s *Server) Reconcile() error {
	_, err := s.reconcileOnce(true)
	return err
}

// NotePath resolves a note id to its vault-relative path (for the hub's ACL gate).
func (s *Server) NotePath(id string) (string, error) { return s.store.NotePath(id) }

// FlywheelStats exposes the write-back reuse metrics (authored notes, reuse rate,
// median time-to-reuse, writes-per-100-reads) of this server's index. The hub reads
// it for the team-level /team metrics, so the flywheel shows up team-wide and not only
// on the per-vault web dashboard. Reuse events counted here are those served by THIS
// index (the hosted MCP), so a team on `mesh mcp --http` over synced vaults contributes
// authored counts but its reuse lands in each member's local index, not here.
func (s *Server) FlywheelStats() (index.FlywheelStats, error) { return s.store.FlywheelStats() }

func (s *Server) Close() error {
	<-s.bg // never close the store under the initial background load/enrichment
	return s.store.Close()
}

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

// HandleHTTP serves ONE JSON-RPC request over HTTP (MCP Streamable HTTP, the
// request/response shape; no SSE stream). Same dispatch as stdio, so a remote agent
// gets identical results. Stateless per call; auth + transport live in the caller
// (cmd/mesh `mesh mcp --http` or the hub's /mcp route). The body cap mirrors the
// largest sane tool call.
func (s *Server) HandleHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
		return
	}
	var req request
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&req); err != nil {
		writeRPC(w, response{JSONRPC: "2.0", Error: &rpcError{Code: codeInvalidParams, Message: "bad request"}})
		return
	}
	// Notifications (no id) get a bare 202 with no body.
	if len(req.ID) == 0 || string(req.ID) == "null" {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	result, rerr := s.dispatch(r.Context(), req)
	resp := response{JSONRPC: "2.0", ID: req.ID}
	if rerr != nil {
		resp.Error = rerr
	} else {
		resp.Result = result
	}
	writeRPC(w, resp)
}

func writeRPC(w http.ResponseWriter, resp response) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) dispatch(ctx context.Context, req request) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		return s.handleInitialize(req.Params), nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return s.handleToolsList(), nil
	case "tools/call":
		if rerr := s.awaitReady(ctx); rerr != nil {
			return nil, rerr
		}
		return s.handleToolsCall(ctx, req.Params)
	case "resources/list":
		return s.handleResourcesList(), nil
	case "resources/read":
		if rerr := s.awaitReady(ctx); rerr != nil {
			return nil, rerr
		}
		return s.handleResourcesRead(ctx, req.Params)
	default:
		return nil, &rpcError{Code: codeMethodNotFound, Message: "method not found", Data: req.Method}
	}
}

func (s *Server) handleInitialize(params json.RawMessage) any {
	// Capture the calling agent's name (provenance default for write-back).
	var p struct {
		ClientInfo struct {
			Name string `json:"name"`
		} `json:"clientInfo"`
	}
	if len(params) > 0 {
		_ = json.Unmarshal(params, &p)
	}
	if name := strings.TrimSpace(p.ClientInfo.Name); name != "" {
		s.mu.Lock()
		s.agent = name
		s.mu.Unlock()
	}
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

// internalErr wraps an internal failure (index reload, vault I/O, driver error)
// as a JSON-RPC error WITHOUT leaking the raw message: sqlite driver text and
// absolute filesystem paths would otherwise reach the agent verbatim. The real
// error is logged server-side; the agent gets a generic message. Validation
// errors use codeInvalidParams with explicit operator-authored messages instead.
func internalErr(err error) *rpcError {
	slog.Error("mesh mcp internal error", "err", err)
	return &rpcError{Code: codeInternalError, Message: "internal error"}
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
