// Package tui is `mesh tui`: a keyboard-driven terminal view of the vault over
// the same index + graph + retriever the agent uses. Three panes: a notes list,
// ranked search results, and a markdown preview with the note's neighbors. Built
// on bubbletea/lipgloss; the Backend interface keeps the UI testable and leaves
// room for a future remote backend (deferred, like the web viewer's).
package tui

import (
	"context"
	"os"
	"path/filepath"
	"sort"

	"github.com/bright-interaction/mesh/internal/graph"
	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/retrieve"
)

const notePrefix = "note:"

// NoteRef is one row in the notes list.
type NoteRef struct {
	ID, Title, Path, Type string
	Degree, Community     int
}

// NeighborRef is one linked note shown under the preview.
type NeighborRef struct {
	ID, Title, Rel, Dir string // Dir: out (this -> it) | in (it -> this)
}

// NoteDetail is the preview payload: the note body plus its neighbors.
type NoteDetail struct {
	ID, Title, Path, Type string
	Body                  string
	Neighbors             []NeighborRef
}

// Backend is the data surface the TUI renders. LocalBackend reads the on-disk
// index + graph; a RemoteBackend (over the hub) is deferred.
type Backend interface {
	Notes() []NoteRef
	Search(query string) ([]retrieve.Card, error)
	Note(id string) (NoteDetail, error)
}

// LocalBackend serves the TUI from the local SQLite index + in-memory graph.
type LocalBackend struct {
	vaultRoot string
	store     *index.Store
	graph     *graph.Graph
	rtr       *retrieve.Retriever
}

// NewLocalBackend opens the vault index, builds the graph once, and returns the
// backend plus a close func the caller defers.
func NewLocalBackend(vaultRoot string) (*LocalBackend, func() error, error) {
	store, err := index.Open(vaultRoot)
	if err != nil {
		return nil, nil, err
	}
	g, err := index.Reindex(store, vaultRoot)
	if err != nil {
		store.Close()
		return nil, nil, err
	}
	b := &LocalBackend{vaultRoot: vaultRoot, store: store, graph: g, rtr: retrieve.NewFromEnv(store, g)}
	return b, store.Close, nil
}

// Notes lists every note, hubs first (highest degree), ties by title.
func (b *LocalBackend) Notes() []NoteRef {
	var out []NoteRef
	for _, n := range b.graph.Nodes() {
		if n.Kind != "note" {
			continue
		}
		out = append(out, NoteRef{ID: n.NoteID, Title: n.Label, Path: n.NotePath, Type: typeOf(n), Degree: n.Degree, Community: n.Community})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Degree != out[j].Degree {
			return out[i].Degree > out[j].Degree
		}
		return out[i].Title < out[j].Title
	})
	return out
}

// Search runs the same fused retrieval the agent's mesh_search uses.
func (b *LocalBackend) Search(query string) ([]retrieve.Card, error) {
	return b.rtr.Retrieve(context.Background(), query, retrieve.Options{Limit: 30})
}

// Note reads a note's markdown and its typed neighbors (in + out, headings and
// the note itself excluded).
func (b *LocalBackend) Note(id string) (NoteDetail, error) {
	rel, err := b.store.NotePath(id)
	if err != nil {
		return NoteDetail{}, err
	}
	data, err := os.ReadFile(filepath.Join(b.vaultRoot, filepath.FromSlash(rel)))
	if err != nil {
		return NoteDetail{}, err
	}
	d := NoteDetail{ID: id, Path: rel, Body: string(data)}
	if n, ok := b.graph.Node(notePrefix + id); ok {
		d.Title = n.Label
		d.Type = typeOf(n)
	}
	seen := map[string]bool{notePrefix + id: true}
	add := func(nodeID, relation, dir string) {
		if seen[nodeID] {
			return
		}
		n, ok := b.graph.Node(nodeID)
		if !ok || n.Kind == "heading" {
			return
		}
		seen[nodeID] = true
		nid := n.NoteID
		if nid == "" {
			nid = n.ID
		}
		d.Neighbors = append(d.Neighbors, NeighborRef{ID: nid, Title: n.Label, Rel: relation, Dir: dir})
	}
	for _, e := range b.graph.Neighbors(notePrefix + id) {
		add(e.Target, e.Relation, "out")
	}
	for _, e := range b.graph.RefsTo(notePrefix + id) {
		add(e.Source, e.Relation, "in")
	}
	sort.SliceStable(d.Neighbors, func(i, j int) bool { return d.Neighbors[i].Title < d.Neighbors[j].Title })
	return d, nil
}

func typeOf(n *graph.Node) string {
	if n.Attrs != nil {
		if t, ok := n.Attrs["type"].(string); ok {
			return t
		}
	}
	return ""
}
