// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"strings"
	"unicode/utf8"
)

// maxDocChars caps the text sent per candidate to a reranker. A cross-encoder
// truncates internally (ms-marco-MiniLM ~512 tokens), so this only keeps HTTP
// payloads small; the leading title + first prose carry the signal.
const maxDocChars = 2000

// NoteDocs returns the rerankable text (title + indexed body) for the given
// note node ids, keyed by node id. Missing ids are simply absent from the map.
// The text is the same searchText Mesh indexes into FTS, so the reranker scores
// the query against exactly what made the note a candidate.
func (s *Store) NoteDocs(ids []string) (map[string]string, error) {
	out := make(map[string]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := s.readDB.Query(`SELECT node_id, title, body FROM search_index WHERE node_id IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, title, body string
		if err := rows.Scan(&id, &title, &body); err != nil {
			return nil, err
		}
		doc := strings.TrimSpace(title + "\n" + body)
		if len(doc) > maxDocChars {
			// Cut on a rune boundary so Swedish (and any multibyte) text is not
			// sliced mid-rune into a garbage byte.
			cut := maxDocChars
			for cut > 0 && !utf8.RuneStart(doc[cut]) {
				cut--
			}
			doc = doc[:cut]
		}
		out[id] = doc
	}
	return out, rows.Err()
}
