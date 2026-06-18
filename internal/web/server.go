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

	"github.com/bright-interaction/mesh/internal/graph"
	"github.com/bright-interaction/mesh/internal/index"
)

//go:embed assets
var assetsFS embed.FS

// Server serves the localhost graph viewer + app shell for one vault.
type Server struct {
	vaultRoot string
	store     *index.Store
	auth      authConfig

	mu    sync.RWMutex
	graph *graph.Graph
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
	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("GET /api/config", s.handleGetConfig)
	mux.HandleFunc("PUT /api/config", s.handlePutConfig)
	mux.HandleFunc("POST /api/reindex", s.handleReindex)
	mux.HandleFunc("GET /api/search", s.handleSearch)
	mux.HandleFunc("GET /api/note/{id}", s.handleNote)
	mux.HandleFunc("GET /api/docs", s.handleDocsList)
	mux.HandleFunc("GET /api/docs/{slug}", s.handleDoc)
	return s.auth.guard(mux)
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
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(body)
}

func (s *Server) handleGraph(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	g := s.graph
	s.mu.RUnlock()
	exp := BuildExport(g, s.vaultRoot)
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
		"authRequired": s.auth.authRequired(),
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

// Serve builds the server and listens on addr (e.g. 127.0.0.1:7474), printing the
// URL. A loopback bind needs no auth; binding beyond loopback is fail-closed and
// requires a token (see newAuthConfig).
func Serve(vaultRoot, addr, token string) error {
	auth, err := newAuthConfig(addr, token)
	if err != nil {
		return err
	}
	s, err := NewServer(vaultRoot)
	if err != nil {
		return err
	}
	defer s.Close()
	s.auth = auth
	exp := BuildExport(s.graph, vaultRoot)
	fmt.Printf("mesh ui: %d notes, %d links across %d communities\n", exp.Meta.NodeCount, exp.Meta.EdgeCount, len(exp.Communities))
	if auth.authRequired() {
		fmt.Printf("auth: token required (Authorization: Bearer ...)\n")
	}
	fmt.Printf("serving at http://%s  (Ctrl-C to stop)\n", addr)
	return http.ListenAndServe(addr, s.Handler())
}
