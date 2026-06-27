package web

import (
	"embed"
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

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
	// member, when set (mesh ui --hub-db), puts the app in per-member auth mode: each
	// request authenticates as a hub client instead of the single shared token.
	member *memberAuth

	mu              sync.RWMutex
	graph           *graph.Graph
	cachedRetriever *retrieve.Retriever // built lazily over graph; nil = rebuild needed

	configMu sync.Mutex // serializes config.toml read-modify-write (PUT /api/config)
}

// retriever returns a fused retriever over the current graph, building it lazily and
// caching it. Previously every /api/search rebuilt the retriever (LoadVectors from
// disk + an ANN rebuild in pro) per request: a latency cliff and a DoS amplifier.
// It is invalidated on a graph swap (reindex) and a config change, so a Settings edit
// still takes effect on the next search.
func (s *Server) retriever() *retrieve.Retriever {
	s.mu.RLock()
	rt := s.cachedRetriever
	s.mu.RUnlock()
	if rt != nil {
		return rt
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cachedRetriever == nil {
		s.cachedRetriever = retrieve.NewFromEnv(s.store, s.graph)
	}
	return s.cachedRetriever
}

// invalidateRetriever drops the cached retriever so the next search rebuilds it
// (after a config change). Caller must NOT already hold s.mu.
func (s *Server) invalidateRetriever() {
	s.mu.Lock()
	s.cachedRetriever = nil
	s.mu.Unlock()
}

// allowedScopes returns the caller's readable-scope set (nil = unrestricted).
func (s *Server) allowedScopes(r *http.Request) map[string]bool {
	if s.scopeResolver == nil {
		return nil
	}
	return s.scopeResolver(r)
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

// NewServer opens the vault index and builds the in-memory graph once.
func NewServer(vaultRoot string) (*Server, error) {
	store, err := index.Open(vaultRoot)
	if err != nil {
		return nil, err
	}
	g, err := index.Reindex(store, vaultRoot)
	if err != nil {
		store.Close()
		return nil, err
	}
	return &Server{vaultRoot: vaultRoot, store: store, graph: g}, nil
}

func (s *Server) Close() error { return s.store.Close() }

// Handler wires the routes: the SPA shell, the graph payload, embedded assets, and
// the /api surface, all behind the auth guard (a no-op on a loopback bind).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.handleIndex)
	mux.HandleFunc("GET /graph.json", s.handleGraph)
	mux.HandleFunc("GET /assets/", s.handleAsset)
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.handleLogout)
	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("GET /api/config", s.handleGetConfig)
	mux.HandleFunc("PUT /api/config", s.handlePutConfig)
	mux.HandleFunc("POST /api/reindex", s.handleReindex)
	mux.HandleFunc("GET /api/search", s.handleSearch)
	mux.HandleFunc("GET /api/note/{id}", s.handleNote)
	mux.HandleFunc("GET /api/docs", s.handleDocsList)
	mux.HandleFunc("GET /api/docs/{slug}", s.handleDoc)
	mux.HandleFunc("GET /api/mcp-tools", s.handleMCPTools)
	mux.HandleFunc("GET /api/dashboard", s.handleDashboard)
	mux.HandleFunc("GET /openapi.json", s.handleOpenAPI)
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

// securityHeaders sets the standard hardening headers on every response. In
// particular Referrer-Policy: no-referrer keeps any token that ends up in a URL out
// of the Referer header on outbound navigations; responses carry private vault data
// so they are also marked no-store and non-framable.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
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
	html = strings.ReplaceAll(html, "__MESH_AGPL__", buildinfo.FooterInline())
	body = []byte(html)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(body)
}

func (s *Server) handleGraph(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	g := s.graph
	s.mu.RUnlock()
	exp := BuildExport(g, s.vaultRoot, s.allowedScopes(r))
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

// handleStatus reports index counts and which retrieval signals are active, the
// browser equivalent of `mesh status`.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	notes, _ := s.store.Count("notes")
	nodes, _ := s.store.Count("nodes")
	edges, _ := s.store.Count("edges")
	vectors, _ := s.store.Count("vectors")
	writeJSON(w, map[string]any{
		"vault":  s.vaultRoot,
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
func Serve(vaultRoot, addr, token, basePath string, verify func(string) (int64, string, bool), scopesFor func(int64) map[string]bool) error {
	memberMode := verify != nil
	var auth authConfig
	if !memberMode {
		a, err := newAuthConfig(addr, token)
		if err != nil {
			return err
		}
		auth = a
	}
	s, err := NewServer(vaultRoot)
	if err != nil {
		return err
	}
	defer s.Close()
	s.auth = auth
	s.basePath = normalizeBasePath(basePath)
	if memberMode {
		s.SetMemberAuth(verify, scopesFor)
	}
	exp := BuildExport(s.graph, vaultRoot, nil)
	fmt.Printf("mesh ui: %d notes, %d links across %d communities\n", exp.Meta.NodeCount, exp.Meta.EdgeCount, len(exp.Communities))
	if memberMode {
		fmt.Printf("auth: per-member (hub client token; views scoped per member)\n")
	} else if auth.authRequired() {
		fmt.Printf("auth: token required (Authorization: Bearer ...)\n")
	}
	fmt.Printf("serving at http://%s%s  (Ctrl-C to stop)\n", addr, s.baseHref())
	return http.ListenAndServe(addr, s.Handler())
}
