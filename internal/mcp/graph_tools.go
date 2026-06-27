package mcp

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/bright-interaction/mesh/internal/graph"
)

// notePrefix is the node-id namespace for notes (see internal/graph: a note's
// node id is "note:<frontmatter-id>").
const notePrefix = "note:"

// neighborRef is one note/tag/heading adjacent to the center note.
type neighborRef struct {
	ID        string `json:"id"` // frontmatter id for notes; node id for tags/headings
	Kind      string `json:"kind"`
	Title     string `json:"title"`
	Path      string `json:"path,omitempty"`
	Relation  string `json:"relation"`
	Direction string `json:"direction"` // out (this note -> neighbor) | in (neighbor -> this note)
	Depth     int    `json:"depth"`
	Community int    `json:"community"`
	Degree    int    `json:"degree"`
}

// toolNeighbors returns the typed neighborhood of a note out to a small depth, so
// an agent can walk the graph one hop at a time instead of fetching whole files.
func (s *Server) toolNeighbors(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var a struct {
		ID    string `json:"id"`
		Depth int    `json:"depth"`
		Limit int    `json:"limit"`
	}
	json.Unmarshal(raw, &a)
	if strings.TrimSpace(a.ID) == "" {
		return nil, &rpcError{Code: codeInvalidParams, Message: "id is required"}
	}
	depth := clampLimit(a.Depth, 1, 3)
	limit := clampLimit(a.Limit, 50, 200)

	g, _ := s.snapshot()
	sf := scopeFromCtx(ctx)
	centerID := notePrefix + a.ID
	center, ok := g.Node(centerID)
	if !ok || !sf.allowsNode(center) {
		return nil, &rpcError{Code: codeInvalidParams, Message: "unknown note id", Data: a.ID}
	}

	// Undirected BFS recording the shortest hop and the edge that first reached
	// each neighbor. We GATHER the full neighborhood (bounded by a hard ceiling so
	// memory is safe even on a mega-hub), then sort by importance and trim to limit
	// so the kept neighbors are the highest-degree ones, not just the first ones
	// the adjacency happened to list. The payload is always <= limit (token-safe);
	// the response reports the true total + a truncated flag so the agent never
	// mistakes a trimmed view for a complete one.
	const maxGather = 2000
	visited := map[string]bool{centerID: true}
	var gathered []neighborRef
	frontier := []string{centerID}
	reached := 0
	complete := true
	for d := 1; d <= depth && complete; d++ {
		var next []string
		add := func(other, relation, dir string) {
			if visited[other] {
				return
			}
			visited[other] = true
			// Scope: hide note neighbors the caller cannot read, and do not traverse
			// THROUGH them (so a forbidden note can't bridge to its other links).
			if nn, ok := g.Node(other); ok && nn.Kind == "note" && !sf.allowsNode(nn) {
				return
			}
			if ref, ok := refFor(g, other, relation, dir, d); ok {
				gathered = append(gathered, ref)
				next = append(next, other)
				if d > reached {
					reached = d
				}
			}
		}
		for _, cur := range frontier {
			for _, e := range g.Neighbors(cur) {
				add(e.Target, e.Relation, "out")
			}
			for _, e := range g.RefsTo(cur) {
				add(e.Source, e.Relation, "in")
			}
			if len(gathered) >= maxGather {
				complete = false // hit the safety ceiling: total is a lower bound
				break
			}
		}
		frontier = next
	}
	sort.SliceStable(gathered, func(i, j int) bool {
		if gathered[i].Depth != gathered[j].Depth {
			return gathered[i].Depth < gathered[j].Depth
		}
		if gathered[i].Degree != gathered[j].Degree {
			return gathered[i].Degree > gathered[j].Degree
		}
		return gathered[i].ID < gathered[j].ID
	})
	total := len(gathered)
	out := gathered
	truncated := !complete
	if len(out) > limit {
		out = out[:limit]
		truncated = true
	}
	return textResult(map[string]any{
		"center":        map[string]any{"id": center.NoteID, "title": center.Label, "path": center.NotePath, "community": center.Community, "degree": center.Degree},
		"depth":         depth,
		"depth_reached": reached,
		"neighbors":     out,
		"count":         len(out),
		"total":         total,
		"truncated":     truncated,
	}), nil
}

func refFor(g *graph.Graph, nodeID, relation, dir string, depth int) (neighborRef, bool) {
	n, ok := g.Node(nodeID)
	if !ok {
		return neighborRef{}, false
	}
	if n.Kind == "heading" {
		// A note's own headings are intra-note structure (the "contains" edges),
		// not graph neighbors, and they carry the owner's NoteID, so they would
		// otherwise show up as a self-referential neighbor. Skip them.
		return neighborRef{}, false
	}
	id := n.NoteID
	if id == "" {
		id = n.ID // tags have no frontmatter id; keep the node id
	}
	return neighborRef{
		ID: id, Kind: n.Kind, Title: n.Label, Path: n.NotePath,
		Relation: relation, Direction: dir, Depth: depth,
		Community: n.Community, Degree: n.Degree,
	}, true
}

// memberRef is one note inside a community.
type memberRef struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Path   string `json:"path"`
	Degree int    `json:"degree"`
}

// communitySummary describes one label-propagation community.
type communitySummary struct {
	Community  int         `json:"community"`
	Size       int         `json:"size"`
	Exemplar   string      `json:"exemplar"`    // title of the highest-degree member
	ExemplarID string      `json:"exemplar_id"` // its frontmatter id
	Top        []memberRef `json:"top,omitempty"`
}

// toolCommunity returns one note's community + its members (when id is given), or
// the community overview (when it is not), for orienting before a search.
func (s *Server) toolCommunity(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var a struct {
		ID    string `json:"id"`
		Limit int    `json:"limit"`
	}
	json.Unmarshal(raw, &a)
	g, _ := s.snapshot()
	sf := scopeFromCtx(ctx)

	if strings.TrimSpace(a.ID) != "" {
		center, ok := g.Node(notePrefix + a.ID)
		if !ok || !sf.allowsNode(center) {
			return nil, &rpcError{Code: codeInvalidParams, Message: "unknown note id", Data: a.ID}
		}
		members, total := communityMembers(g, center.Community, clampLimit(a.Limit, 50, 500), sf)
		return textResult(map[string]any{
			"community": center.Community,
			"size":      total,
			"members":   members,
		}), nil
	}
	overview := communityOverview(g, clampLimit(a.Limit, 24, 200), sf)
	return textResult(map[string]any{"communities": overview, "count": len(overview)}), nil
}

// communityMembers returns the readable notes in a community (highest-degree first,
// capped) plus the true total readable count.
// scopedGraphCounts returns the node count and edge count VISIBLE to a scoped caller:
// only readable nodes, and only edges whose endpoints are both readable (so an edge
// never reveals an out-of-scope node exists). Used by mesh_reindex to report the
// caller's own view instead of leaking global volume across scopes.
func scopedGraphCounts(g *graph.Graph, sf *ScopeFilter) (nodes, edges int) {
	readable := map[string]bool{}
	for _, n := range g.Nodes() {
		if sf.allowsNode(n) {
			readable[n.ID] = true
		}
	}
	for id := range readable {
		for _, e := range g.Neighbors(id) {
			if readable[e.Target] {
				edges++
			}
		}
	}
	return len(readable), edges
}

func communityMembers(g *graph.Graph, comm, limit int, sf *ScopeFilter) (members []memberRef, total int) {
	for _, n := range g.Nodes() {
		if n.Kind != "note" || n.Community != comm || !sf.allowsNode(n) {
			continue
		}
		total++
		members = append(members, memberRef{n.NoteID, n.Label, n.NotePath, n.Degree})
	}
	sort.Slice(members, func(i, j int) bool {
		if members[i].Degree != members[j].Degree {
			return members[i].Degree > members[j].Degree
		}
		return members[i].ID < members[j].ID
	})
	if len(members) > limit {
		members = members[:limit]
	}
	return members, total
}

// communityOverview groups notes by community, largest first, with each
// community's top-degree exemplar and a few top members.
func communityOverview(g *graph.Graph, limit int, sf *ScopeFilter) []communitySummary {
	byComm := map[int][]*graph.Node{}
	for _, n := range g.Nodes() {
		if n.Kind != "note" || !sf.allowsNode(n) {
			continue
		}
		byComm[n.Community] = append(byComm[n.Community], n)
	}
	out := make([]communitySummary, 0, len(byComm))
	for comm, ns := range byComm {
		sort.Slice(ns, func(i, j int) bool {
			if ns[i].Degree != ns[j].Degree {
				return ns[i].Degree > ns[j].Degree
			}
			return ns[i].NoteID < ns[j].NoteID
		})
		cs := communitySummary{Community: comm, Size: len(ns), Exemplar: ns[0].Label, ExemplarID: ns[0].NoteID}
		for i := 0; i < len(ns) && i < 3; i++ {
			cs.Top = append(cs.Top, memberRef{ns[i].NoteID, ns[i].Label, ns[i].NotePath, ns[i].Degree})
		}
		out = append(out, cs)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Size != out[j].Size {
			return out[i].Size > out[j].Size
		}
		return out[i].Community < out[j].Community
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func clampLimit(v, def, max int) int {
	if v <= 0 {
		return def
	}
	if v > max {
		return max
	}
	return v
}
