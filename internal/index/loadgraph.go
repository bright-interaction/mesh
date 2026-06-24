package index

import (
	"encoding/json"

	"github.com/bright-interaction/mesh/internal/graph"
)

// LoadGraph reconstructs the in-memory graph from the persisted nodes + edges
// tables. The CLI uses this for retrieval without re-parsing the vault; the
// long-running daemon (MCP) keeps the graph in memory instead.
func (s *Store) LoadGraph() (*graph.Graph, error) {
	g := graph.New()

	nrows, err := s.readDB.Query(`SELECT id, kind, label, COALESCE(note_id,''), COALESCE(note_path,''), COALESCE(anchor,''), COALESCE(source_loc,''), COALESCE(community,0), COALESCE(attrs,'') FROM nodes`)
	if err != nil {
		return nil, err
	}
	for nrows.Next() {
		n := &graph.Node{}
		var attrs string
		if err := nrows.Scan(&n.ID, &n.Kind, &n.Label, &n.NoteID, &n.NotePath, &n.Anchor, &n.SourceLoc, &n.Community, &attrs); err != nil {
			nrows.Close()
			return nil, err
		}
		if attrs != "" && attrs != "null" {
			_ = json.Unmarshal([]byte(attrs), &n.Attrs)
		}
		g.AddNode(n)
	}
	nrows.Close()

	erows, err := s.readDB.Query(`SELECT source, target, relation, confidence, confidence_score, weight, COALESCE(source_loc,'') FROM edges`)
	if err != nil {
		return nil, err
	}
	defer erows.Close()
	for erows.Next() {
		var e graph.Edge
		if err := erows.Scan(&e.Source, &e.Target, &e.Relation, &e.Confidence, &e.ConfidenceScore, &e.Weight, &e.SourceLoc); err != nil {
			return nil, err
		}
		g.AddEdge(e)
	}
	// Match BuildGraph: recompute degrees in a final pass so both paths agree exactly.
	g.RecomputeDegrees()
	return g, erows.Err()
}
