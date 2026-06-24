// Package graph holds Mesh's in-memory knowledge graph. The node/edge shape is
// lifted from dockyard's internal/knowledge graph and adapted for markdown:
// identity is the frontmatter id (node id "note:<id>"), never the file path, so
// a rename never rots an edge or an agent citation (spec section 3.6).
package graph

import "sync"

// Edge confidence levels (from the dockyard/graphify extraction model).
const (
	ConfExtracted = "EXTRACTED"
	ConfInferred  = "INFERRED"
	ConfAmbiguous = "AMBIGUOUS"
)

type Node struct {
	ID        string // note:<frontmatter-id> | tag:<name> | note:<id>#<anchor>
	Kind      string // note|heading|tag|external|...
	Label     string
	NoteID    string // owning note's frontmatter id
	NotePath  string // denormalized for fast file open; refreshed on rename
	Anchor    string // heading slug for fetch-by-anchor
	SourceLoc string // "L<line>" for the editor deep link
	Community int
	Attrs     map[string]any
	Degree    int
}

type Edge struct {
	Source          string
	Target          string
	Relation        string // contains|references|tagged|...
	Confidence      string
	ConfidenceScore float64
	Weight          float64
	SourceLoc       string
}

// Graph is a concurrent-safe adjacency list. Reads take the RLock; the index
// builder is the only writer in M0.
type Graph struct {
	mu      sync.RWMutex
	nodes   map[string]*Node
	adj     map[string][]Edge
	rev     map[string][]Edge
	edgeSet map[string]bool
}

func New() *Graph { return NewSized(0) }

// NewSized preallocates the maps for a graph expected to hold about n notes. A
// note contributes roughly one note node plus a handful of heading/tag nodes and
// edges, so the hints below avoid the repeated rehashing that dominates a
// from-scratch rebuild of a large vault. Hints are advisory; the graph still grows
// past them. n <= 0 builds with no hint (the small-vault default).
func NewSized(n int) *Graph {
	if n <= 0 {
		return &Graph{
			nodes:   make(map[string]*Node),
			adj:     make(map[string][]Edge),
			rev:     make(map[string][]Edge),
			edgeSet: make(map[string]bool),
		}
	}
	return &Graph{
		nodes:   make(map[string]*Node, n*3),
		adj:     make(map[string][]Edge, n*2),
		rev:     make(map[string][]Edge, n*2),
		edgeSet: make(map[string]bool, n*3),
	}
}

// AddNode inserts a node, or enriches a previously bare reference node with real
// path/label data once its owning file is parsed.
func (g *Graph) AddNode(n *Node) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if existing, ok := g.nodes[n.ID]; ok {
		if existing.NotePath == "" && n.NotePath != "" {
			existing.NotePath = n.NotePath
			existing.NoteID = n.NoteID
			existing.Label = n.Label
		}
		return
	}
	g.nodes[n.ID] = n
}

// AddEdge inserts a unique (source, target, relation) edge and bumps the degree
// of whichever endpoints already exist.
func (g *Graph) AddEdge(e Edge) {
	g.mu.Lock()
	defer g.mu.Unlock()
	key := e.Source + "\x00" + e.Target + "\x00" + e.Relation
	if g.edgeSet[key] {
		return
	}
	g.edgeSet[key] = true
	g.adj[e.Source] = append(g.adj[e.Source], e)
	g.rev[e.Target] = append(g.rev[e.Target], e)
	if n, ok := g.nodes[e.Source]; ok {
		n.Degree++
	}
	if n, ok := g.nodes[e.Target]; ok {
		n.Degree++
	}
}

// RecomputeDegrees sets every node's Degree from its adjacency lists, making the
// value independent of node/edge insertion order. AddEdge only bumps endpoints that
// already exist at insertion time, so a graph assembled by interleaving AddNode and
// AddEdge (BuildGraph: a references edge to a not-yet-added later note never counts
// that note's inbound degree) would otherwise disagree with one built nodes-first
// (LoadGraph). Call this once after the graph is fully assembled so both paths agree.
func (g *Graph) RecomputeDegrees() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for id, n := range g.nodes {
		n.Degree = len(g.adj[id]) + len(g.rev[id])
	}
}

func (g *Graph) Node(id string) (*Node, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	n, ok := g.nodes[id]
	return n, ok
}

func (g *Graph) NodeCount() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return len(g.nodes)
}

func (g *Graph) EdgeCount() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return len(g.edgeSet)
}

// Neighbors returns a copy of the outbound edges for id.
func (g *Graph) Neighbors(id string) []Edge {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return append([]Edge(nil), g.adj[id]...)
}

// RefsTo returns a copy of the inbound edges pointing at id.
func (g *Graph) RefsTo(id string) []Edge {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return append([]Edge(nil), g.rev[id]...)
}

func (g *Graph) Nodes() []*Node {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make([]*Node, 0, len(g.nodes))
	for _, n := range g.nodes {
		out = append(out, n)
	}
	return out
}

func (g *Graph) CountByKind() map[string]int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	m := map[string]int{}
	for _, n := range g.nodes {
		m[n.Kind]++
	}
	return m
}
