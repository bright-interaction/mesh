// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"sort"
	"strings"

	"github.com/bright-interaction/mesh/internal/graph"
	"github.com/bright-interaction/mesh/internal/vault"
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
// Unrestricted: see SearchScoped for the access-controlled form.
func (s *Store) Search(query string, limit int) ([]SearchHit, error) {
	return s.SearchScoped(query, limit, nil)
}

// SearchScoped is Search restricted to notes whose scope intersects allowed
// (nil = unrestricted, the solo / no-ACL fast path).
//
// The scope predicate lives IN the SQL rather than in a post-filter on purpose:
// LIMIT is applied by SQLite after WHERE, so filtering afterwards let a run of
// higher-ranked unreadable notes consume the whole limit and starve a scoped
// caller down to zero hits even though readable matches existed. Filtering here
// means the limit counts only rows the caller may actually read.
func (s *Store) SearchScoped(query string, limit int, allowed map[string]bool) ([]SearchHit, error) {
	match := buildFTS5Query(query)
	if match == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 10
	}
	scopeSQL, scopeArgs, readable := scopePredicate(allowed)
	if !readable {
		// Scoping is on but the caller may read nothing: no row can qualify.
		return nil, nil
	}
	q := `
SELECT si.node_id, si.title,
       COALESCE(n.path, ''),
       snippet(search_index, 4, '[', ']', ' ... ', 12),
       bm25(search_index)
FROM search_index si
LEFT JOIN notes n ON n.id = substr(si.node_id, 6)
WHERE search_index MATCH ?` + scopeSQL + `
ORDER BY bm25(search_index)
LIMIT ?`
	args := make([]any, 0, len(scopeArgs)+2)
	args = append(args, match)
	args = append(args, scopeArgs...)
	args = append(args, limit)
	rows, err := s.readDB.Query(q, args...)
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

// scopePredicate renders the notes.scope read filter as a parameterized SQL
// fragment plus its bind values. It returns readable=false when scoping is on but
// the allowed set is empty, i.e. no row can qualify and the caller should skip the
// query entirely.
//
// notes.scope holds the effective scopes comma-joined, so membership is tested by
// wrapping both haystack and needle in commas and using instr(), NOT LIKE: instr
// is a literal substring test, so a scope name containing % or _ cannot widen the
// match the way a LIKE pattern would. A NULL (outer-joined) or empty scope falls
// back to vault.DefaultScope, the same fail-safe vault.ScopeAllows applies to an
// unlabeled note.
func scopePredicate(allowed map[string]bool) (sql string, args []any, readable bool) {
	if allowed == nil {
		return "", nil, true // unrestricted: scoping not configured
	}
	names := make([]string, 0, len(allowed))
	for s, ok := range allowed {
		if t := strings.TrimSpace(s); ok && t != "" {
			names = append(names, t)
		}
	}
	if len(names) == 0 {
		return "", nil, false
	}
	sort.Strings(names) // deterministic SQL text, so the prepared-statement cache hits
	conds := make([]string, 0, len(names))
	for _, n := range names {
		conds = append(conds, "instr(',' || COALESCE(NULLIF(n.scope, ''), ?) || ',', ?) > 0")
		args = append(args, vault.DefaultScope, ","+n+",")
	}
	return " AND (" + strings.Join(conds, " OR ") + ")", args, true
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
