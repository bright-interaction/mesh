// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"context"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/bright-interaction/mesh/internal/vault"
)

// maxDocChars caps the text sent per candidate to a reranker. A cross-encoder
// truncates internally (ms-marco-MiniLM ~512 tokens), so this only keeps HTTP
// payloads small; the leading title + first prose carry the signal.
const maxDocChars = 2000

// NoteMetadata is the current, persisted identity and read-boundary metadata for
// one note. Retrieval keeps an in-memory graph for ranking, but that graph can be
// one watcher generation behind the notes/search tables. Anything used for access
// control or returned to a caller must therefore come from this current snapshot.
type NoteMetadata struct {
	NodeID          string
	NoteID          string
	Path            string
	Type            string
	Title           string
	Scope           string
	SupersededBy    string
	SupersederPath  string
	SupersederScope string
	MissingGuidance []string
}

// Only read the three guidance fields, not the complete frontmatter or note body.
// These values share the identity/ACL snapshot, including on the reranker path.
const guidanceFieldsSQL = `CASE WHEN json_valid(n.frontmatter)
  THEN json_extract(n.frontmatter, '$.Do', '$.Dont', '$.Why') ELSE '[]' END`

func missingGuidance(kind, fieldsJSON string) []string {
	if !vault.NoteType(kind).RequiresFlywheel() {
		return nil
	}
	var fields [3]string
	if err := json.Unmarshal([]byte(fieldsJSON), &fields); err != nil {
		// Invalid stored guidance is not evidence of a complete note.
		fields = [3]string{}
	}
	var missing []string
	for i, name := range []string{"do", "dont", "why"} {
		text, _ := vault.StripComments(fields[i])
		if vault.Unfilled(text) {
			missing = append(missing, name)
		}
	}
	return missing
}

// NoteDocument pairs rerankable text with the note metadata read in the SAME SQL
// statement. A reindex cannot therefore pair a new secret body with an old public
// path/scope between separate reads.
type NoteDocument struct {
	NoteMetadata
	Text string
}

// NoteMetadataFor returns current persisted metadata for the requested note node
// ids, keyed by node id. Missing/deleted ids are absent. Supersession fields come
// from the target graph row joined to the CURRENT superseding note in the same SQL
// statement, so callers never combine a stale relation with old scope/path metadata.
func (s *Store) NoteMetadataFor(ctx context.Context, ids []string) (map[string]NoteMetadata, error) {
	out := make(map[string]NoteMetadata, len(ids))
	// Keep comfortably below SQLite's host-parameter ceiling. Graph/vector candidate
	// validation can cover an entire large vault, unlike the <=30 reranker document read.
	const batchSize = 500
	for start := 0; start < len(ids); start += batchSize {
		end := start + batchSize
		if end > len(ids) {
			end = len(ids)
		}
		if err := s.noteMetadataBatch(ctx, ids[start:end], out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Store) noteMetadataBatch(ctx context.Context, ids []string, out map[string]NoteMetadata) error {
	placeholders, args := noteIDArgs(ids)
	rows, err := s.readDB.QueryContext(ctx, `
SELECT 'note:' || n.id, n.id, n.path, n.type, n.title, n.scope,
       COALESCE(sup.id, ''), COALESCE(sup.path, ''), COALESCE(sup.scope, ''),
       `+guidanceFieldsSQL+`
FROM notes n
LEFT JOIN nodes gn ON gn.id = 'note:' || n.id
LEFT JOIN notes sup ON sup.id = CASE
  WHEN json_valid(gn.attrs) THEN json_extract(gn.attrs, '$.superseded_by')
  ELSE NULL
END
WHERE 'note:' || n.id IN (`+placeholders+`)`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var m NoteMetadata
		var guidance string
		if err := rows.Scan(
			&m.NodeID, &m.NoteID, &m.Path, &m.Type, &m.Title, &m.Scope,
			&m.SupersededBy, &m.SupersederPath, &m.SupersederScope,
			&guidance,
		); err != nil {
			return err
		}
		m.MissingGuidance = missingGuidance(m.Type, guidance)
		out[m.NodeID] = m
	}
	return rows.Err()
}

// NoteDocuments returns current rerankable text and its access-control metadata
// from one snapshot, keyed by node id. Missing/deleted ids are absent.
func (s *Store) NoteDocuments(ctx context.Context, ids []string) (map[string]NoteDocument, error) {
	out := make(map[string]NoteDocument, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	placeholders, args := noteIDArgs(ids)
	rows, err := s.readDB.QueryContext(ctx, `
SELECT si.node_id, n.id, n.path, n.type, n.title, n.scope,
       COALESCE(sup.id, ''), COALESCE(sup.path, ''), COALESCE(sup.scope, ''),
       si.body, `+guidanceFieldsSQL+`
FROM search_index si
JOIN notes n ON si.node_id = 'note:' || n.id
LEFT JOIN nodes gn ON gn.id = si.node_id
LEFT JOIN notes sup ON sup.id = CASE
  WHEN json_valid(gn.attrs) THEN json_extract(gn.attrs, '$.superseded_by')
  ELSE NULL
END
WHERE si.node_id IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var d NoteDocument
		var body, guidance string
		if err := rows.Scan(
			&d.NodeID, &d.NoteID, &d.Path, &d.Type, &d.Title, &d.Scope,
			&d.SupersededBy, &d.SupersederPath, &d.SupersederScope,
			&body, &guidance,
		); err != nil {
			return nil, err
		}
		d.MissingGuidance = missingGuidance(d.Type, guidance)
		d.Text = boundedDocument(d.Title, body)
		out[d.NodeID] = d
	}
	return out, rows.Err()
}

// NoteDocs returns the rerankable text (title + indexed body) for the given
// note node ids, keyed by node id. Missing ids are simply absent from the map.
// The text is the same searchText Mesh indexes into FTS, so the reranker scores
// the query against exactly what made the note a candidate.
func (s *Store) NoteDocs(ids []string) (map[string]string, error) {
	docs, err := s.NoteDocuments(context.Background(), ids)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(docs))
	for id, doc := range docs {
		out[id] = doc.Text
	}
	return out, nil
}

func noteIDArgs(ids []string) (string, []any) {
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return placeholders, args
}

func boundedDocument(title, body string) string {
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
	return doc
}
