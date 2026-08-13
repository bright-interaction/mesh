// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

// Package mcp serves Mesh's retrieval + write-back surface to a coding agent
// over JSON-RPC 2.0 on stdio. A local agent (Claude Code / Codex) spawns
// `mesh mcp` and talks to it directly; no port or auth surface. The JSON-RPC
// envelope follows the standard MCP shape.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
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
	// viewHashes fingerprints the index the in-memory graph was last loaded from
	// (path -> retrieval hash), so a read-only server can report what a refresh changed
	// in its view. Guarded by reloadMu; nil until the first refresh.
	viewHashes map[string]string

	// owner is the vault's owning-writer claim when THIS process elected itself (see
	// NewOwningServer). nil on a read-only server and on the hub, which owns an index
	// nobody else can reach. A declared owner (`mesh watch`, `mesh sync --watch`,
	// `mesh ui --own-index`) can take the claim back mid-session, so every write path
	// asks owns() rather than assuming the store's writability is the whole answer.
	owner *index.OwnerLock

	reindexMu      sync.Mutex // guards the mesh_reindex throttle state below
	lastReindexAt  time.Time
	lastReindex    index.Reconciliation
	lastReindexErr error // ErrOwnerNotIndexing when that pass left the index stale

	ready    chan struct{} // closed when retrieval is servable (initial reload done)
	readyErr error         // written once before ready closes
	bg       chan struct{} // closed when ALL background startup work is done (enrichment included)

	agent string // calling client's name from initialize (provenance default), guarded by mu

	// ownerIndexTimeout bounds the wait for the owning writer to index a just-written
	// note. A field rather than a bare const so a test can shorten it without mutating
	// global state (which would race across parallel tests); production never sets it.
	ownerIndexTimeout time.Duration
}

// NewServer opens the vault's index (at <vaultRoot>/.mesh) READ-ONLY and loads it into
// memory. This is the per-window server for a vault that already has an owning writer:
// there can be many of these at once against one vault, so none of them may hold the
// SQLite write lock. Write-back still works, because creating a note is a filesystem
// write the owner picks up; see toolWrite.
//
// Most callers want NewOwningServer, which uses this whenever the vault is already
// owned and otherwise elects itself. Use NewServer directly only where something else
// is guaranteed to be the writer.
//
// NewServerAt (the hub) deliberately does NOT go through here: the hub owns the index it
// serves, so it keeps a writable store.
func NewServer(vaultRoot string) (*Server, error) {
	store, err := index.OpenReadOnly(vaultRoot)
	if err != nil {
		return nil, err
	}
	return newServerWithStore(vaultRoot, store, nil)
}

// NewOwningServer elects this process the vault's OWNING WRITER when no other live
// writer holds it, and falls back to the read-only server (NewServer) when one does.
// role names this surface in the lock, for the error a losing peer prints.
//
// This exists because the shipped agent config is `mesh mcp --vault <path>` and nothing
// else. A read-only MCP server cannot index, so on a vault with no separate `mesh watch`
// running (which is every new user) mesh_append_note blocked the full owner bound,
// returned index_stale + owner_down, and left the note unqueryable, while a note edited
// in the user's editor never became searchable at all. Electing here makes the shipped
// configuration self-sufficient without reintroducing multi-writer contention: the lock
// is what keeps it to one.
//
// The claim is preemptible. A declared owner started later (`mesh watch`,
// `mesh sync --watch`, `mesh ui --own-index`) takes it, and this server drops back to
// reader behaviour on its own; see owns.
func NewOwningServer(vaultRoot, role string) (*Server, error) {
	lock, err := index.AcquireOwnerLock(filepath.Join(vaultRoot, ".mesh"), role, true)
	if err != nil {
		if errors.Is(err, index.ErrOwnerHeld) {
			return NewServer(vaultRoot) // someone else owns it: read beside them, as before
		}
		return nil, err
	}
	store, err := index.Open(vaultRoot)
	if err != nil {
		_ = lock.Release()
		return nil, err
	}
	s, err := newServerWithStore(vaultRoot, store, lock)
	if err != nil {
		_ = lock.Release()
		return nil, err
	}
	return s, nil
}

// NewServerAt is like NewServer but keeps the index in an explicit dir instead of
// <vaultRoot>/.mesh. The hub uses it to serve hosted MCP over its vault while
// indexing OUTSIDE the git repo (so the index never syncs to clients).
func NewServerAt(vaultRoot, indexDir string) (*Server, error) {
	store, err := index.OpenAt(vaultRoot, indexDir)
	if err != nil {
		return nil, err
	}
	return newServerWithStore(vaultRoot, store, nil)
}

func newServerWithStore(vaultRoot string, store *index.Store, owner *index.OwnerLock) (*Server, error) {
	s := &Server{vaultRoot: vaultRoot, store: store, owner: owner, cache: index.NewNoteCache(), ready: make(chan struct{}), bg: make(chan struct{}), ownerIndexTimeout: ownerIndexTimeout}
	// The initial load runs in the background so the MCP handshake answers
	// immediately: a full reload of a grown vault plus the note<->code bridge
	// exceeds a client's connect timeout (Claude Code kills the server at 30s
	// and never retries, orphaning the whole session), and a concurrent
	// `mesh code reindex` holding the db write lock makes it worse. ready
	// closes as soon as retrieval is servable (reload done) so a tool call
	// gating on awaitReady waits ~1s, not for the enrichment passes below it.
	go func() {
		defer close(s.bg)
		// A read-only store cannot reindex: ReindexFull rewrites notes / search_index /
		// nodes / edges, so on this path the owning writer has already done that work and
		// all this server has to do is read the result into memory. LoadGraph is pure SQL
		// over readDB and was written for exactly this split.
		if err := s.load(); err != nil {
			s.readyErr = fmt.Errorf("initial index load: %w", err)
			fmt.Fprintf(os.Stderr, "mesh mcp: %v\n", s.readyErr)
			close(s.ready)
			return
		}
		close(s.ready)
		if !s.owns() {
			// Both enrichment passes below are writes. They belong to the owning writer
			// now, not to every window that connects; doing them here would just be N
			// servers taking the write lock at startup, which is the contention this
			// whole split exists to remove.
			return
		}
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
	err := s.store.Close()
	// Give the vault up AFTER the store is closed, so the next owner never starts
	// indexing while this process still has a writable connection open.
	if rerr := s.owner.Release(); err == nil {
		err = rerr
	}
	return err
}

// owns reports whether this process is the vault's owning writer RIGHT NOW: the store is
// writable and, when this server elected itself, it still holds the claim.
//
// Every write path branches on this rather than on store.ReadOnly(), because an elected
// claim is preemptible: a declared owner started later takes the lock, and from that
// moment this server must behave exactly like the read-only one (publish the file, wait
// for the owner, re-read) or two processes reindex the same vault.
func (s *Server) owns() bool {
	if s.store.ReadOnly() {
		return false
	}
	if s.owner != nil {
		return s.owner.Held()
	}
	return true
}

// OwnsIndex reports whether this server is the vault's owning writer, so the command
// that started it can tell the operator which of the two it got.
func (s *Server) OwnsIndex() bool { return s.owns() }

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

// load puts the vault's graph in memory by whichever route this store allows: a writable
// store re-indexes (and seeds the parsed-note cache so later reconciles are incremental),
// a read-only one reads the owning writer's persisted result. Startup calls this.
func (s *Server) load() error {
	if !s.owns() {
		_, err := s.refresh() // the startup load has no previous view to report a delta against
		return err
	}
	return s.reload()
}

// refresh rebuilds the in-memory graph + retriever from the PERSISTED tables, without
// touching the vault or the write lock. It is how a read-only server picks up whatever
// the owning writer has indexed since. It takes reloadMu like reload does, so a refresh
// and a rebuild can never swap under each other.
//
// It reports what the swap changed in THIS server's view (notes it gained, lost, or whose
// content moved), which is the only honest set of counts available to a process that did
// not run the pass. That is a different question from the writable path's "what did the
// vault drift by", and it is the one a caller of mesh_reindex here is actually asking:
// the reindex may well have happened in the owner minutes ago, and what this call did was
// bring it into view.
//
// The parsed-note cache is deliberately left unseeded: it exists to make a REINDEX
// incremental, and this server never reindexes.
func (s *Server) refresh() (index.Reconciliation, error) {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	g, err := s.store.LoadGraph()
	if err != nil {
		return index.Reconciliation{}, err
	}
	// Fingerprint the index we just loaded. A read error here costs the NEXT refresh its
	// counts, never its correctness, so it must not fail the refresh: the graph is already
	// good and the caller's notes are already queryable.
	after, herr := s.store.NoteHashes()
	// Reindexed carries the same meaning it does on the writable path: something actually
	// changed. A refresh always swaps a graph in, but saying so every time would make the
	// watcher log a line on every idle tick (and mesh_reindex claim a pass it did not
	// need), which is noise that hides the ticks that did bring something in. Unknown
	// counts (the hash read failed) report true: we did swap, and under-reporting a real
	// change is the worse error.
	rec := index.Reconciliation{Reindexed: herr != nil}
	if herr == nil {
		first := s.viewHashes == nil
		for p, h := range after {
			switch prev, had := s.viewHashes[p]; {
			case !had && !first:
				rec.Added++
			case had && prev != h:
				rec.Changed++
			}
		}
		for p := range s.viewHashes {
			if _, still := after[p]; !still {
				rec.Removed++
			}
		}
		rec.Reindexed = first || rec.Any()
		s.viewHashes = after
	}
	s.swap(g)
	return rec, nil
}

// ErrOwnerNotIndexing means the single owning writer did not index a just-written note
// inside ownerIndexTimeout. The note IS durably on disk; only its indexing is missing,
// which is a liveness failure of the owner (`mesh watch` / `mesh sync --watch` stopped),
// not a durability failure of the write. Callers must say exactly that, because the one
// thing that must never happen here is reporting a failed write for a note that exists:
// the agent retries and Mesh mints a near-duplicate.
//
// It is the index package's sentinel, not a second one: the web viewer waits on the same
// owner for the same reason, and two sentinels would mean errors.Is answering differently
// on the two surfaces.
var ErrOwnerNotIndexing = index.ErrOwnerNotIndexing

// ownerIndexTimeout bounds the wait for the owning writer. The value, and why it is that
// value, live with the wait itself in internal/index.
const ownerIndexTimeout = index.OwnerIndexBound

// OwnerIndexBound is ownerIndexTimeout, re-exported so the owning writer's own periodic
// sweep can be pinned UNDER it (see cmd/mesh). A note that misses its fsnotify event is
// picked up by that sweep, so a sweep interval above this bound is precisely the case
// where a durable, perfectly fine note reads as owner_down.
const OwnerIndexBound = index.OwnerIndexBound

// awaitOwnerIndexed blocks until the single owning writer has indexed noteID, then
// refreshes this server's in-memory graph from what the owner persisted. This is the
// whole of "write-back through the owner": the wait is index.AwaitNoteIndexed, and what
// this adds is the refresh of THIS server's view once the owner has landed it.
func (s *Server) awaitOwnerIndexed(ctx context.Context, noteID string) error {
	if err := s.store.AwaitNoteIndexed(ctx, noteID, s.ownerIndexTimeout); err != nil {
		return err
	}
	_, rerr := s.refresh() // the caller wants the note queryable, not the counts
	return rerr
}

// pendingDrift is the part of the vault the owning writer has not absorbed yet and still
// can. See index.PendingDrift for what is excluded and why.
func (s *Server) pendingDrift() (index.Drift, error) {
	return s.store.PendingDrift(s.vaultRoot)
}

// awaitOwnerCaughtUp is mesh_reindex's half of the contract awaitOwnerIndexed gives
// write-back: it waits for the single owning writer to absorb whatever the vault has
// drifted by, then refreshes this server's in-memory graph from what the owner
// persisted. The counts it returns are what became queryable BECAUSE of this call.
//
// Without the wait, mesh_reindex on a read-only server is a lie in the one situation the
// tool exists for. The contract an agent is handed says "if you edited note files
// directly, call mesh_reindex to make those edits queryable now"; a bare snapshot swap
// returns instantly with the owner still inside its debounce, so the tool reports success
// and the edit is not there. The bound and the loud failure are the same as write-back's,
// because it is the same failure: the owner is not running.
func (s *Server) awaitOwnerCaughtUp(ctx context.Context) (index.Reconciliation, error) {
	start := time.Now()
	werr := s.store.AwaitOwnerCaughtUp(ctx, s.vaultRoot, s.ownerIndexTimeout)
	stale := errors.Is(werr, index.ErrOwnerNotIndexing)
	if werr != nil && !stale {
		return index.Reconciliation{}, werr
	}
	// The counts come from the refresh, not from the drift we waited on: what a caller
	// gets out of this call is what entered THIS server's view, and on the stale path
	// that is whatever the owner did manage to index, never what is still missing.
	rec, err := s.refresh()
	if err != nil {
		return index.Reconciliation{}, err
	}
	rec.Dur = time.Since(start)
	if stale {
		return rec, ErrOwnerNotIndexing
	}
	return rec, nil
}

// publishWriteBack makes a just-created note queryable, by whichever route this server
// is allowed to take: the owner of its index reindexes directly, a read-only server
// waits for the owning writer to do it.
func (s *Server) publishWriteBack(ctx context.Context, noteID string) error {
	if !s.owns() {
		return s.awaitOwnerIndexed(ctx, noteID)
	}
	_, err := s.reconcileOnce(true)
	return err
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
	// A read-only server has no reindex to run: the owning writer does that. The
	// equivalent act here is picking up what the owner has already persisted, so the
	// watcher, mesh_reindex and write-back all still refresh, none of them write, and
	// none of them need to know which kind of store they are on. Branch BEFORE taking
	// reloadMu: refresh takes it too, and it is not reentrant.
	//
	// The counts describe this server's VIEW rather than the vault: a snapshot swap
	// classifies nothing on disk, but it can say exactly which notes it gained, lost or
	// saw change, which is what the watcher's log line and mesh_reindex both want.
	if !s.owns() {
		start := time.Now()
		rec, err := s.refresh()
		if err != nil {
			return index.Reconciliation{}, err
		}
		rec.Dur = time.Since(start)
		return rec, nil
	}
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

// reindexThrottle is how long a REMOTE mesh_reindex serves the previous pass's
// result instead of forcing another authoritative reconcile. Each pass content-hashes
// every note and then re-indexes the code roots, and it takes reloadMu, the same lock
// the hub's post-sync reconcile worker needs, so an unthrottled remote caller can burn
// a core on hashing and delay synced notes becoming visible for other members.
const reindexThrottle = 5 * time.Second

// reindexPass is one authoritative mesh_reindex pass by whichever route this server is
// allowed to take: the owner of its index reindexes, a read-only server waits for the
// owning writer to catch up and then re-reads the result. Note this is NOT what the
// watcher calls (that is reconcileOnce): a watcher tick must never sit waiting on the
// owner, it just picks up whatever has landed.
func (s *Server) reindexPass(ctx context.Context) (index.Reconciliation, error) {
	if !s.owns() {
		return s.awaitOwnerCaughtUp(ctx)
	}
	return s.reconcileOnce(true)
}

// reconcileThrottled is reindexPass with a per-server cooldown for remote callers.
// remote=false (the local stdio operator, who owns the box and expects "reindex now" to
// mean now) always runs the real pass. It returns the pass result, whether the cooldown
// replayed the previous one, and the pass's staleness verdict.
//
// ErrOwnerNotIndexing is a RESULT, not a failure: the pass ran, and what it found is that
// the owner has not caught up. It is cached and replayed with the rest of the pass, so a
// throttled caller is told the index is stale instead of being handed the last healthy
// pass as if nothing were wrong.
func (s *Server) reconcileThrottled(ctx context.Context, remote bool) (index.Reconciliation, bool, error) {
	s.reindexMu.Lock()
	defer s.reindexMu.Unlock()
	if remote && !s.lastReindexAt.IsZero() && time.Since(s.lastReindexAt) < reindexThrottle {
		return s.lastReindex, true, s.lastReindexErr
	}
	rec, err := s.reindexPass(ctx)
	if err != nil && !errors.Is(err, ErrOwnerNotIndexing) {
		return rec, false, err
	}
	s.lastReindexAt = time.Now()
	s.lastReindex = rec
	s.lastReindexErr = err
	return rec, false, err
}

// Watch live-reindexes the vault in the background until ctx is cancelled, so a
// long-running agent session sees notes a human edits in their editor without a
// restart. logf must write to stderr: stdout carries the JSON-RPC stream.
func (s *Server) Watch(ctx context.Context, debounce, reconcile, fullReconcile time.Duration, logf func(string, ...any)) error {
	return watch.Run(ctx, watch.Options{
		Root:          s.vaultRoot,
		Debounce:      debounce,
		Reconcile:     reconcile,
		FullReconcile: fullReconcile,
		Logf:          logf,
		OnReindex: func(p watch.Pass) (watch.Result, error) {
			rec, err := s.reconcileOnce(p.Authoritative)
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
	// Stdio is the trusted local transport: the operator spawned this process, so its
	// requests carry the local-operator marker that gates the local-only tools. No
	// other transport sets it, so they all fail closed (see WithLocalOperator).
	base := WithLocalOperator(context.Background())
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
		result, rerr := s.dispatch(base, req)
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

// ---- JSON-RPC envelope ----

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
