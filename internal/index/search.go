package index

import (
	"strings"

	"github.com/bright-interaction/mesh/internal/graph"
)

// SearchHit is one FTS5 result over the note corpus.
type SearchHit struct {
	NodeID  string // note:<id>
	Title   string
	Path    string  // vault-relative path of the owning note
	Snippet string  // bracketed match excerpt from the body
	Score   float64 // normalized so higher is more relevant (negated bm25)
}

// Search runs an FTS5 MATCH over search_index and returns the most relevant
// notes. User input is sanitized into quoted literal tokens so FTS5's reserved
// grammar (NEAR/OR/NOT/AND/*/parens) can never break the parser or inject.
func (s *Store) Search(query string, limit int) ([]SearchHit, error) {
	match := buildFTS5Query(query)
	if match == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 10
	}
	const q = `
SELECT si.node_id, si.title,
       COALESCE(n.path, ''),
       snippet(search_index, 4, '[', ']', ' ... ', 12),
       bm25(search_index)
FROM search_index si
LEFT JOIN notes n ON n.id = substr(si.node_id, 6)
WHERE search_index MATCH ?
ORDER BY bm25(search_index)
LIMIT ?`
	rows, err := s.readDB.Query(q, match, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SearchHit
	for rows.Next() {
		var h SearchHit
		var rank float64
		if err := rows.Scan(&h.NodeID, &h.Title, &h.Path, &h.Snippet, &rank); err != nil {
			return nil, err
		}
		// FTS5 bm25 returns lower-is-better; negate so the fuser (M1 step 3)
		// can treat all signals as higher-is-better.
		h.Score = -rank
		out = append(out, h)
	}
	return out, rows.Err()
}

// buildFTS5Query turns raw user input into an FTS5 MATCH expression. It uses the
// shared graph tokenizer (lowercase, unicode boundaries, stopwords dropped) so
// FTS and graph-BM25 see the same terms, then joins them with OR: an agent's
// natural-language query ("how do we store data") should recall any note that
// matches a content word and let bm25 rank, not require that every word be
// present AND-style. Reserved FTS grammar can't leak because each token is a
// quoted alphanumeric literal. Empty input returns "" so the caller
// short-circuits.
func buildFTS5Query(q string) string {
	toks := graph.Tokenize(q)
	out := make([]string, 0, len(toks))
	for _, t := range toks {
		out = append(out, `"`+strings.ReplaceAll(t, `"`, `""`)+`"`)
	}
	return strings.Join(out, " OR ")
}
