// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bright-interaction/mesh/internal/buildinfo"
	"github.com/bright-interaction/mesh/internal/graph"
	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/retrieve"
)

//go:embed assets
var assetsFS embed.FS

// Server serves the localhost graph viewer + app shell for one vault.
type Server struct {
	vaultRoot string
	store     *index.Store
	auth      authConfig
	basePath  string // "" for root, or "/app" when served under a path

	// scopeResolver, when set, maps a request to the caller's allowed-scope set so the
	// graph/search/note surfaces are filtered per member. nil (standalone `mesh ui`) =
	// unrestricted, so the loopback single-user viewer is unchanged.
	scopeResolver func(*http.Request) map[string]bool
	// pathResolver is the folder-ACL twin of scopeResolver: it maps a request to the
	// caller's per-path read predicate (nil = unrestricted). The two are independent
	// partitions and BOTH gate every read surface, because a team can fence folders with
	// ACLs and never define a scope, and then scopeResolver returns nil and filters
	// nothing whatsoever.
	pathResolver func(*http.Request) func(string) bool
	// member, when set (mesh ui --hub-db), puts the app in per-member auth mode: each
	// request authenticates as a hub client instead of the single shared token.
	member *memberAuth

	mu    sync.RWMutex
	graph *graph.Graph
	// cachedRetriever is built lazily over graph; nil = rebuild needed. It is an atomic
	// pointer rather than a field of the graph lock so the (slow) build never has to
	// take s.mu exclusively, see retriever().
	cachedRetriever atomic.Pointer[retrieve.Retriever]
	buildMu         sync.Mutex // serializes retriever builds; NOT held while s.mu is held

	configMu sync.Mutex // serializes config.toml read-modify-write (PUT /api/config)

	// ownerWait bounds how long a read-only viewer waits for the owning writer to apply
	// what it queued. A field rather than a bare const so a test can shorten it without
	// mutating global state (which would race across parallel tests); production never
	// sets it and gets index.OwnerIndexBound.
	ownerWait time.Duration

	logins   *rateLimiter  // per-peer token bucket in front of POST /api/login
	asks     *rateLimiter  // per-caller token bucket in front of POST /api/ask
	askSlots chan struct{} // bounded in-flight LLM subprocesses/calls for POST /api/ask
}

// retriever returns a fused retriever over the current graph, building it lazily and
// caching it. Previously every /api/search rebuilt the retriever (LoadVectors from
// disk + an ANN rebuild in pro) per request: a latency cliff and a DoS amplifier.
// It is invalidated on a graph swap (reindex) and a config change, so a Settings edit
// still takes effect on the next search.
//
// The build runs OUTSIDE s.mu. retrieve.NewFromEnv reads every stored vector, builds
// the ANN index (pro) and probes the embedding endpoint over the network, so holding
// the exclusive graph lock across it froze /graph.json, every other search and the
// pending API for as long as that probe took (up to the HTTP client timeout) whenever
// the endpoint was unreachable. buildMu serialises the build itself so N concurrent
// searches cause one build, not N, and the result is only published if the graph it
// was built over is still the current one, so a reindex that lands mid-build wins.
// Same shape as mcp.Server.swap.
func (s *Server) retriever() *retrieve.Retriever {
	if rt := s.cachedRetriever.Load(); rt != nil {
		return rt
	}
	s.buildMu.Lock()
	defer s.buildMu.Unlock()
	if rt := s.cachedRetriever.Load(); rt != nil {
		return rt // another caller built it while we waited
	}
	s.mu.RLock()
	g := s.graph
	s.mu.RUnlock()
	built := retrieve.NewFromEnv(s.store, g)
	// Publish under the read lock: a graph swap (reindex, pending promote) takes the
	// write lock to set s.graph and clear the cache in one critical section, so this
	// compare-and-store cannot interleave with it and resurrect a stale retriever.
	s.mu.RLock()
	if s.graph == g {
		s.cachedRetriever.Store(built)
	}
	s.mu.RUnlock()
	return built
}

// invalidateRetriever drops the cached retriever so the next search rebuilds it
// (after a config change).
func (s *Server) invalidateRetriever() {
	s.cachedRetriever.Store(nil)
}

// allowedScopes returns the caller's readable-scope set (nil = unrestricted).
func (s *Server) allowedScopes(r *http.Request) map[string]bool {
	if s.scopeResolver == nil {
		return nil
	}
	return s.scopeResolver(r)
}

// allowedPath returns the caller's per-path read predicate (nil = unrestricted).
func (s *Server) allowedPath(r *http.Request) func(string) bool {
	if s.pathResolver == nil {
		return nil
	}
	return s.pathResolver(r)
}

// canReadPath reports whether the caller may read a vault-relative note path.
func (s *Server) canReadPath(r *http.Request, path string) bool {
	allow := s.allowedPath(r)
	return allow == nil || allow(path)
}

// SetScopeResolver installs the per-request scope resolver (used by the hub to serve
// the app under per-member identity).
func (s *Server) SetScopeResolver(f func(*http.Request) map[string]bool) { s.scopeResolver = f }

// Store exposes the index store (for a host that needs NoteScope etc.).
func (s *Server) Store() *index.Store { return s.store }

// baseHref is the value injected into the SPA's <base> tag, so every relative
// asset and fetch resolves under the configured path. Always ends in "/".
func (s *Server) baseHref() string {
	if s.basePath == "" {
		return "/"
	}
	return s.basePath + "/"
}

// NewServer opens the vault's index READ-ONLY and loads the graph the owning writer has
// already persisted. This is the viewer you run beside an owner (`mesh watch` /
// `mesh sync --watch`), which is every laptop: the app stays open for hours, so a
// writable store here is a second long-lived writer against one mesh.db, and the whole
// point of the single-writer split is that there is exactly one.
//
// It does NOT reindex at startup. That used to be the first thing this constructor did,
// which meant opening the dashboard ran a full reindex of the vault before it drew a
// frame, against the same write lock the owner needs. Reading what the owner persisted
// is the same graph, without the fight.
//
// The write features still work; they route through the owner. See handleReindex and the
// pending queue in pending_api.go.
func NewServer(vaultRoot string) (*Server, error) {
	store, err := index.OpenReadOnly(vaultRoot)
	if err != nil {
		return nil, err
	}
	g, err := store.LoadGraph()
	if err != nil {
		store.Close()
		return nil, err
	}
	return newServer(vaultRoot, store, g), nil
}

// NewOwningServer opens the index WRITABLE and owns it: it reindexes at startup and
// applies its own writes. Use it only where this process is the sole writer of that
// index, which today is the mesh-ui container (`mesh ui --own-index`). There, no
// `mesh watch` runs against the served vault, so a read-only viewer would serve an index
// nobody ever updates. Same split as internal/mcp: NewServer is the per-window reader,
// NewServerAt is the hub that owns what it serves.
func NewOwningServer(vaultRoot string) (*Server, error) {
	store, err := index.Open(vaultRoot)
	if err != nil {
		return nil, err
	}
	g, err := index.Reindex(store, vaultRoot)
	if err != nil {
		store.Close()
		return nil, err
	}
	// Apply anything a reader queued for an owner before this process started. Normally
	// empty: it matters when a vault is served read-only for a while and then owned.
	if _, err := store.DrainOps(); err != nil {
		slog.Warn("mesh ui: could not drain the owner op queue at startup", "error", err)
	}
	// Seed the flywheel measurement from the existing agent-authored corpus once, so the
	// Dashboard shows a real reuse number immediately even if no mesh mcp ran (idempotent).
	_, _ = store.BackfillWritebacks()
	_, _ = store.LinkNotesToCode(vaultRoot) // build the note<->code bridge if a code index exists
	return newServer(vaultRoot, store, g), nil
}

func newServer(vaultRoot string, store *index.Store, g *graph.Graph) *Server {
	return &Server{
		vaultRoot: vaultRoot,
		store:     store,
		graph:     g,
		// POST /api/login is the one unauthenticated path that validates a secret, so it
		// is a guessing oracle for the shared admin token and every member client token.
		// 5 attempts per minute per peer, burst 5.
		logins: newRateLimiter(5.0/60.0, 5),
		// POST /api/ask forks an LLM subprocess (or bills a BYOAI key) per call, so it is
		// both rate limited (12/min, burst 4) and capped in flight.
		asks:      newRateLimiter(12.0/60.0, 4),
		askSlots:  make(chan struct{}, askMaxInFlight),
		ownerWait: index.OwnerIndexBound,
	}
}

func (s *Server) Close() error { return s.store.Close() }

// refresh rebuilds the in-memory graph from the PERSISTED tables, without touching the
// vault or the write lock. It is how a read-only viewer picks up whatever the owning
// writer has indexed since, and it is the read-only counterpart of the graph swap the
// writable paths do after their own reindex.
func (s *Server) refresh() error {
	g, err := s.store.LoadGraph()
	if err != nil {
		return err
	}
	// One exclusive critical section for the swap + invalidation, so an in-flight
	// retriever build cannot publish over the old graph (see Server.retriever).
	s.mu.Lock()
	s.graph = g
	s.cachedRetriever.Store(nil)
	s.mu.Unlock()
	return nil
}

// ownerDownNote is what every surface here says when the owning writer did not apply a
// change in time. The action itself succeeded and is durable; what is missing is the
// indexing, and the remedy is always the same, so the wording is too.
const ownerDownNote = "This took effect on disk but the index has NOT caught up: the single owning writer " +
	"did not apply it inside the wait, and this viewer only reads what that writer persists. " +
	"The usual cause is that `mesh watch` / `mesh sync --watch` is not running, so check that " +
	"first; the change is picked up as soon as one runs."

// resolveIndexWrites performs index mutations by whichever route this server is allowed
// to take. When it owns the index it applies them directly. When it is a reader it
// queues them for the owning writer and waits for the LAST one to land: ops are applied
// oldest first, so the last one disappearing means all of them did, and one wait means
// one bound rather than one per op.
//
// ownerDown=true means the change is queued and durable but not yet applied. It is never
// an error: reporting a failure for something that WILL take effect is how a caller ends
// up retrying and doing it twice.
func (s *Server) resolveIndexWrites(ctx context.Context, direct func() error, ops ...index.Op) (ownerDown bool, err error) {
	if !s.store.ReadOnly() {
		return false, direct()
	}
	var last string
	for _, op := range ops {
		name, err := s.store.EnqueueOp(op)
		if err != nil {
			return false, err
		}
		last = name
	}
	if err := s.store.AwaitOpApplied(ctx, last, s.ownerWait); err != nil {
		if errors.Is(err, index.ErrOwnerNotIndexing) {
			return true, nil
		}
		return false, err
	}
	return false, nil
}

// route is one served pattern. The routes live in a table rather than in a run of
// mux.HandleFunc calls so a test can enumerate them: every one of them has to keep
// working on a read-only viewer, and the only way to be sure a NEW one was considered is
// to make the guard fail when it has no entry. See TestEveryRouteSurvivesReadOnly.
type route struct {
	pattern string
	h       http.HandlerFunc
}

func (s *Server) routes() []route {
	return []route{
		{"GET /", s.handleIndex},
		{"GET /graph.json", s.handleGraph},
		{"GET /assets/", s.handleAsset},
		{"POST /api/login", s.handleLogin},
		{"POST /api/logout", s.handleLogout},
		{"GET /api/status", s.handleStatus},
		{"GET /api/config", s.handleGetConfig},
		{"PUT /api/config", s.handlePutConfig},
		{"POST /api/reindex", s.handleReindex},
		{"GET /api/search", s.handleSearch},
		{"GET /api/note/{id}", s.handleNote},
		{"GET /api/docs", s.handleDocsList},
		{"GET /api/docs/{slug}", s.handleDoc},
		{"GET /api/mcp-tools", s.handleMCPTools},
		{"GET /api/dashboard", s.handleDashboard},
		{"POST /api/ask", s.handleAsk},
		{"GET /api/pending", s.handlePendingList},
		{"POST /api/pending/promote", s.handlePendingPromote},
		{"POST /api/pending/discard", s.handlePendingDiscard},
		{"GET /openapi.json", s.handleOpenAPI},
	}
}

// Handler wires the routes: the SPA shell, the graph payload, embedded assets, and
// the /api surface, all behind the auth guard (a no-op on a loopback bind).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	for _, rt := range s.routes() {
		mux.HandleFunc(rt.pattern, rt.h)
	}
	var h http.Handler
	if s.member != nil {
		h = s.memberGuard(mux) // per-member auth (mesh ui --hub-db)
	} else {
		h = s.auth.guard(mux) // single shared token (standalone)
	}
	if s.basePath != "" {
		// Serve the whole app under the path: strip it before the inner mux (so its
		// root-relative routes match) and let the subtree pattern redirect /app -> /app/.
		outer := http.NewServeMux()
		outer.Handle(s.basePath+"/", http.StripPrefix(s.basePath, h))
		h = outer
	}
	return securityHeaders(h)
}

// contentSecurityPolicy is the second line of defence behind renderMDSafe: the SPA
// assigns server-rendered note HTML to innerHTML, so if a sanitiser gap ever let a
// tag through, script-src 'self' still refuses to run an inline handler or a
// javascript: URL. Everything the app needs is same-origin (self-hosted fonts, no
// CDN, no third-party JS), so 'self' costs nothing. style-src keeps 'unsafe-inline'
// because the shell and a few panels use style="" attributes; that is a cosmetic
// surface, not a script one. base-uri stays 'self' rather than 'none' because the
// shell ships a <base href> the server rewrites for the configured base path.
const contentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'"

// securityHeaders sets the standard hardening headers on every response. In
// particular Referrer-Policy: no-referrer keeps any token that ends up in a URL out
// of the Referer header on outbound navigations; responses carry private vault data
// so they are also marked no-store and non-framable.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", contentSecurityPolicy)
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	body, err := assetsFS.ReadFile("assets/index.html")
	if err != nil {
		http.Error(w, "not found", http.StatusInternalServerError)
		return
	}
	html := strings.ReplaceAll(string(body), "__MESH_BASE__", s.baseHref())
	html = strings.ReplaceAll(html, "__MESH_SOURCE__", buildinfo.FooterInline())
	body = []byte(html)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(body)
}

func (s *Server) handleGraph(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	g := s.graph
	s.mu.RUnlock()
	exp := BuildExport(g, s.exposedVaultRoot(), s.allowedScopes(r), s.allowedPath(r))
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	_ = json.NewEncoder(w).Encode(exp)
}

func (s *Server) handleAsset(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/assets/")
	if name == "" || strings.Contains(name, "..") {
		http.NotFound(w, r)
		return
	}
	body, err := assetsFS.ReadFile("assets/" + name)
	// Dev-serve: when MESH_WEB_DEV points at an assets dir, read from disk so a
	// front-end edit is live on refresh with no binary rebuild. Off in prod (env
	// unset). name is already rejected if it contains "..", so this cannot escape.
	if dir := os.Getenv("MESH_WEB_DEV"); dir != "" {
		if b, e := os.ReadFile(filepath.Join(dir, name)); e == nil {
			body, err = b, nil
		}
	}
	if err != nil {
		http.NotFound(w, r)
		return
	}
	switch {
	case strings.HasSuffix(name, ".js"):
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache") // assets change on every binary rebuild; revalidate
	case strings.HasSuffix(name, ".css"):
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
	case strings.HasSuffix(name, ".woff2"):
		w.Header().Set("Content-Type", "font/woff2")
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable") // fonts never change
	default:
		if ct := mime.TypeByExtension(filepath.Ext(name)); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.Header().Set("Cache-Control", "no-cache")
	}
	_, _ = w.Write(body)
}

// exposedVaultRoot is the vault path we are willing to hand a browser. In standalone
// mode the browser IS the operator on the same machine and the absolute path drives the
// editor:// bridge, so it stays. In member mode (mesh ui --hub-db) the caller is a
// remote teammate, possibly a read-only viewer, and the server's filesystem layout is
// pure reconnaissance for them, so only the vault's basename leaves the process.
func (s *Server) exposedVaultRoot() string {
	if s.member == nil {
		return s.vaultRoot
	}
	return filepath.Base(s.vaultRoot)
}

// handleStatus reports index counts and which retrieval signals are active, the
// browser equivalent of `mesh status`.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	// Count what THIS caller may read, not the whole corpus. The stored totals span
	// every scope, so a scope-confined member could read them off /api/status and learn
	// how many notes exist outside their partition, then poll to watch another scope
	// grow. The same aggregate is deliberately scoped in internal/mcp/tools.go ("A
	// scope-confined caller must not learn out-of-scope volume") and admin-gated on
	// /api/dashboard; this route was the one surface that reported it raw.
	// A folder ACL confines volume exactly as a scope does, and it is the only boundary
	// a team that fenced folders without defining a scope has, so it gates this branch too.
	var notes, nodes, edges, vectors int
	allowed, allowPath := s.allowedScopes(r), s.allowedPath(r)
	if allowed != nil || allowPath != nil {
		notes, nodes, edges = s.visibleCounts(func(n *graph.Node) bool {
			return scopeVisible(n, allowed) && pathVisible(n, allowPath)
		})
		vectors = 0 // per-partition vector counts are not tracked; do not leak the global
	} else {
		// An unrestricted caller (owner/admin, or the standalone single-token viewer)
		// still needs the note count: the web app reads counts.notes to decide whether
		// to show the "this vault is empty" overlay, so leaving it at zero here put a
		// full vault behind an empty-state card for exactly the people who can see all
		// of it.
		notes, _ = s.store.Count("notes")
		nodes, _ = s.store.Count("nodes")
		edges, _ = s.store.Count("edges")
		vectors, _ = s.store.Count("vectors")
	}
	writeJSON(w, map[string]any{
		"vault":  s.exposedVaultRoot(),
		"counts": map[string]int{"notes": notes, "nodes": nodes, "edges": edges, "vectors": vectors},
		"signals": map[string]bool{
			"fts":    true,
			"graph":  true,
			"vector": vectors > 0,
			"rerank": os.Getenv("MESH_RERANK_ENDPOINT") != "",
			"ann":    os.Getenv("MESH_HNSW_THRESHOLD") != "" && os.Getenv("MESH_HNSW_THRESHOLD") != "0",
		},
		"authRequired": s.auth.authRequired() || s.member != nil,
	})
}

// visibleCounts walks the graph under the read lock and counts what visible admits.
// The lock is the same one handleGraph takes for s.graph and the same one refresh, the
// reindex route and the pending promote take to swap it; this walk is the only reader
// that used to skip it, which made an ordinary /api/status poll race a promote.
func (s *Server) visibleCounts(visible func(*graph.Node) bool) (notes, nodes, edges int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	g := s.graph
	if g == nil {
		return 0, 0, 0
	}
	seen := map[string]bool{}
	for _, n := range g.Nodes() {
		if !visible(n) {
			continue
		}
		nodes++
		if n.Kind == "note" && !seen[n.NoteID] {
			seen[n.NoteID] = true
			notes++
		}
		for _, e := range g.Neighbors(n.ID) {
			if tn, ok := g.Node(e.Target); ok && visible(tn) {
				edges++
			}
		}
	}
	return notes, nodes, edges
}

// normalizeBasePath returns "" for root, or a clean "/seg" with a leading slash and
// no trailing slash, so it composes with the route prefixes and the <base> href.
func normalizeBasePath(p string) string {
	p = strings.Trim(strings.TrimSpace(p), "/")
	if p == "" {
		return ""
	}
	return "/" + p
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

// Serve builds the server and listens on addr (e.g. 127.0.0.1:7474), printing the
// URL. A loopback bind needs no auth; binding beyond loopback is fail-closed and
// requires a token (see newAuthConfig).
// Serve builds the server and listens. When verify != nil the app runs in per-member
// mode (each request authenticates as a hub client and is scoped to them); member auth
// is then the fail-closed gate, so the single-token requirement is skipped. Otherwise
// it is the standalone single-token viewer (loopback needs no token).
// ownIndex=true makes this process the OWNING WRITER of the vault's index instead of a
// reader of it. Only correct where nothing else writes that index (the mesh-ui
// container); beside a `mesh watch` / `mesh sync --watch` it reintroduces the second
// long-lived writer this whole split removed.
func Serve(vaultRoot, addr, token, basePath string, ownIndex bool, verify func(string) (int64, string, bool), scopesFor func(int64) map[string]bool, pathsFor func(int64) func(string) bool, roleFor func(int64) (string, int64, bool)) error {
	memberMode := verify != nil
	var auth authConfig
	if !memberMode {
		a, err := newAuthConfig(addr, token)
		if err != nil {
			return err
		}
		auth = a
	}
	newSrv := NewServer
	if ownIndex {
		// Claim the vault first. This process is a DECLARED owner, so it takes the claim
		// from an opportunistic `mesh mcp` (which drops back to reading) and refuses to
		// start beside another declared owner, rather than silently becoming the second
		// long-lived writer this whole split exists to prevent.
		lock, lerr := index.AcquireOwnerLock(filepath.Join(vaultRoot, ".mesh"), "mesh ui --own-index", false)
		if lerr != nil {
			return lerr
		}
		defer lock.Release()
		newSrv = NewOwningServer
	}
	s, err := newSrv(vaultRoot)
	if err != nil {
		// The read-only open refuses a vault with no index, which is right (an empty graph
		// served as if it were the vault is worse), but its advice is written for the MCP.
		// Say the part that is specific to being the viewer.
		if !ownIndex && errors.Is(err, index.ErrNoIndexYet) {
			return fmt.Errorf("%w\n\nmesh ui reads the index, it does not build it. Either start the owning "+
				"writer (`mesh watch %s`), index once (`mesh index %s`), or pass --own-index if nothing "+
				"else writes this vault", err, vaultRoot, vaultRoot)
		}
		return err
	}
	defer s.Close()
	s.auth = auth
	s.basePath = normalizeBasePath(basePath)
	if memberMode {
		s.SetMemberAuth(verify, scopesFor, pathsFor, roleFor)
	}
	exp := BuildExport(s.graph, vaultRoot, nil, nil)
	fmt.Printf("mesh ui: %d notes, %d links across %d communities\n", exp.Meta.NodeCount, exp.Meta.EdgeCount, len(exp.Communities))
	if ownIndex {
		fmt.Printf("index: OWNED by this process (it reindexes and writes)\n")
	} else {
		fmt.Printf("index: read-only; the owning writer (mesh watch / mesh sync --watch) indexes\n")
	}
	if memberMode {
		fmt.Printf("auth: per-member (hub client token; views scoped per member)\n")
	} else if auth.authRequired() {
		fmt.Printf("auth: token required (Authorization: Bearer ...)\n")
	}
	fmt.Printf("serving at http://%s%s  (Ctrl-C to stop)\n", addr, s.baseHref())
	// Explicit timeouts, matching the hub's. The zero-value http.Server has none, so a
	// client that opens a connection and never finishes its headers (or never reads its
	// response) pins a goroutine, an fd and a connection forever. In prod this port sits
	// behind a buffering proxy, but anything else on the docker network can reach it
	// directly, and `mesh ui --token` on a public bind has no proxy at all.
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      askMaxDuration + 30*time.Second, // /api/ask is the long pole
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 16,
	}
	return srv.ListenAndServe()
}
