package index

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"os"
	"regexp"
	"strings"

	"github.com/bright-interaction/mesh/internal/graph"
)

var htmlCommentRe = regexp.MustCompile(`(?s)<!--.*?-->`)

// searchText is the body Mesh indexes into FTS5: the prose with comment noise
// stripped, plus the flywheel fields (do/dont/why) and tags, which carry the
// institutional memory but live in frontmatter, not the body.
func searchText(pn *ParsedNote) string {
	parts := []string{htmlCommentRe.ReplaceAllString(pn.Body, " ")}
	for _, v := range []string{pn.FM.Do, pn.FM.Dont, pn.FM.Why} {
		if v != "" {
			parts = append(parts, v)
		}
	}
	parts = append(parts, pn.FM.Tags...)
	// Collapse whitespace so snippets are not padded with skeleton blank lines,
	// which keeps budget-packed cards cheap.
	return strings.Join(strings.Fields(strings.Join(parts, " ")), " ")
}

// IndexVault writes the parsed notes and graph into the store as a full reindex
// in a single transaction (M0: wipe + insert; incremental upsert lands with the
// watcher). Returns the number of notes written.
func (s *Store) IndexVault(notes []*ParsedNote, g *graph.Graph) (int, error) {
	count := 0
	err := s.Write(func(tx *sql.Tx) error {
		for _, t := range []string{"notes", "nodes", "edges", "search_index"} {
			if _, err := tx.Exec("DELETE FROM " + t); err != nil {
				return err
			}
		}

		insNote, err := tx.Prepare(`INSERT INTO notes(id,path,type,title,retrieval_hash,frontmatter,mtime,updated) VALUES(?,?,?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer insNote.Close()
		insFTS, err := tx.Prepare(`INSERT INTO search_index(node_id,kind,anchor,title,body) VALUES(?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer insFTS.Close()

		for _, pn := range notes {
			id := effectiveID(pn)
			title := titleOf(pn)
			fmJSON, err := json.Marshal(pn.FM)
			if err != nil {
				return err
			}
			updated := pn.FM.When
			if pn.FM.Updated != "" {
				updated = pn.FM.Updated
			}
			if _, err := insNote.Exec(id, pn.Path, string(pn.FM.Type), title, retrievalHash(pn), string(fmJSON), fileMtime(pn.Path), updated); err != nil {
				return err
			}
			if _, err := insFTS.Exec("note:"+id, "note", "", title, searchText(pn)); err != nil {
				return err
			}
			count++
		}

		insNode, err := tx.Prepare(`INSERT OR REPLACE INTO nodes(id,kind,label,note_id,note_path,anchor,source_loc,community,attrs) VALUES(?,?,?,?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer insNode.Close()
		insEdge, err := tx.Prepare(`INSERT OR IGNORE INTO edges(source,target,relation,confidence,confidence_score,weight,source_loc) VALUES(?,?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer insEdge.Close()

		for _, nd := range g.Nodes() {
			attrs := "null"
			if nd.Attrs != nil {
				b, err := json.Marshal(nd.Attrs)
				if err != nil {
					return err
				}
				attrs = string(b)
			}
			if _, err := insNode.Exec(nd.ID, nd.Kind, nd.Label, nd.NoteID, nd.NotePath, nd.Anchor, nd.SourceLoc, nd.Community, attrs); err != nil {
				return err
			}
			for _, e := range g.Neighbors(nd.ID) {
				if _, err := insEdge.Exec(e.Source, e.Target, e.Relation, e.Confidence, e.ConfidenceScore, e.Weight, e.SourceLoc); err != nil {
					return err
				}
			}
		}
		return nil
	})
	return count, err
}

func titleOf(pn *ParsedNote) string {
	if pn.FM.Title != "" {
		return pn.FM.Title
	}
	return pn.Key
}

func fileMtime(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.ModTime().Unix()
}

// retrievalHash is SHA256 over the body plus the retrieval-critical frontmatter
// (type, status, supersedes, related), so a status change (accepted ->
// superseded) forces a reindex while a cosmetic frontmatter edit does not
// (spec section 3.2).
func retrievalHash(pn *ParsedNote) string {
	h := sha256.New()
	h.Write([]byte(pn.Body))
	h.Write([]byte{0})
	h.Write([]byte(string(pn.FM.Type)))
	h.Write([]byte{0})
	h.Write([]byte(pn.FM.Status))
	for _, s := range pn.FM.Supersedes {
		h.Write([]byte{0})
		h.Write([]byte(s))
	}
	for _, s := range pn.FM.Related {
		h.Write([]byte{0})
		h.Write([]byte(s))
	}
	return hex.EncodeToString(h.Sum(nil))
}
