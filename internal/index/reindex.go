package index

import (
	"path/filepath"

	"github.com/brightinteraction/mesh/internal/graph"
	"github.com/brightinteraction/mesh/internal/vault"
)

// Reindex walks the vault, parses it, builds the graph + communities, persists
// everything into the store, and returns the freshly loaded in-memory graph.
// Shared by the CLI index path and the MCP server's write-back reload.
func Reindex(s *Store, root string) (*graph.Graph, error) {
	files, err := vault.Walk(root)
	if err != nil {
		return nil, err
	}
	notes, _ := ParseFiles(files, 0)
	for _, pn := range notes {
		if rel, err := filepath.Rel(root, pn.Path); err == nil {
			pn.Path = rel
		}
	}
	g, _ := BuildGraph(notes)
	g.DetectCommunities(0)
	if _, err := s.IndexVault(notes, g); err != nil {
		return nil, err
	}
	return s.LoadGraph()
}

// NoteRef is a lightweight note descriptor for delta/listing queries.
type NoteRef struct {
	ID    string `json:"id"`
	Path  string `json:"path"`
	Type  string `json:"type"`
	Mtime int64  `json:"mtime"`
}

// ChangedSince returns notes whose file mtime is newer than the given unix
// timestamp, newest first. Lets an agent resuming a session pull only deltas.
func (s *Store) ChangedSince(since int64) ([]NoteRef, error) {
	rows, err := s.readDB.Query(`SELECT id, path, type, mtime FROM notes WHERE mtime > ? ORDER BY mtime DESC`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NoteRef
	for rows.Next() {
		var n NoteRef
		if err := rows.Scan(&n.ID, &n.Path, &n.Type, &n.Mtime); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// NotePath resolves a note id to its vault-relative path (read pool).
func (s *Store) NotePath(id string) (string, error) {
	var p string
	err := s.readDB.QueryRow(`SELECT path FROM notes WHERE id = ?`, id).Scan(&p)
	return p, err
}
