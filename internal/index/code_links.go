// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"database/sql"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// The note<->code bridge: link a note to the code symbols it names in backticks, so an
// agent can ask "what do we know about this function" (symbol -> notes) and see a
// note's code alongside it (note -> symbols). Resolution is conservative: a backtick
// token only links when it is distinctive AND exactly matches an indexed symbol name
// (qualified or by last segment), so generic words never create noise.

// backtickRe pulls `code spans` out of a note body.
var backtickRe = regexp.MustCompile("`([^`\n]{2,80})`")

// symbolTokenRe matches a single code identifier path (Foo, Foo.Bar, pkg_thing).
var symbolTokenRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)*$`)

// distinctive reports whether a token is specific enough to link safely: it must look
// like code (qualified, snake_case, or mixedCase) and not be a short common word, so
// "Open"/"New"/"git" never link but "RecordReuse"/"Server.toolSearch" do.
func distinctive(tok string) bool {
	if len(tok) < 5 || !symbolTokenRe.MatchString(tok) {
		return false
	}
	if strings.ContainsAny(tok, "._") {
		return true
	}
	hasUpper, hasLower := false, false
	for _, r := range tok {
		if r >= 'A' && r <= 'Z' {
			hasUpper = true
		}
		if r >= 'a' && r <= 'z' {
			hasLower = true
		}
	}
	return hasUpper && hasLower // mixedCase identifier
}

// identRe pulls identifier-like tokens (Foo, Foo.Bar) out of a note TITLE, which is
// short and high-signal, so a symbol it names links even without backticks.
var identRe = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)*`)

// LinkNotesToCode rebuilds note_code_links from note titles + bodies + the code index.
// A distinctive token links to a symbol when it equals the symbol name or its last
// segment (so a bare `RecordReuse` matches `Store.RecordReuse`). Titles are scanned in
// full (high signal); bodies only inside backticks (prose would be too noisy). Capped
// per token so a common name does not fan out. No-op when the code index is empty.
func (s *Store) LinkNotesToCode(vaultRoot string) (int, error) {
	if n, _ := s.Count("code_symbols"); n == 0 {
		_ = s.Write(func(tx *sql.Tx) error { _, e := tx.Exec(`DELETE FROM note_code_links`); return e })
		return 0, nil
	}
	rows, err := s.readDB.Query(`SELECT id, path, title FROM notes`)
	if err != nil {
		return 0, err
	}
	type noteMeta struct{ id, path, title string }
	var notes []noteMeta
	for rows.Next() {
		var nm noteMeta
		if err := rows.Scan(&nm.id, &nm.path, &nm.title); err != nil {
			rows.Close()
			return 0, err
		}
		notes = append(notes, nm)
	}
	rows.Close()

	type link struct{ noteID, symID, name string }
	var links []link
	for _, n := range notes {
		seen := map[string]bool{}
		consider := func(tok string) {
			tok = strings.TrimSpace(tok)
			if seen[tok] || !distinctive(tok) {
				return
			}
			seen[tok] = true
			for _, sym := range s.symbolsByName(tok, 5) {
				links = append(links, link{n.id, sym.id, sym.name})
			}
		}
		// Title: scan every identifier-like token (the note's subject).
		for _, tok := range identRe.FindAllString(n.title, -1) {
			consider(tok)
		}
		// Body: only backtick code spans (prose is too noisy for a full scan).
		if body, err := os.ReadFile(filepath.Join(vaultRoot, n.path)); err == nil {
			for _, m := range backtickRe.FindAllStringSubmatch(string(body), -1) {
				consider(m[1])
			}
		}
	}
	err = s.Write(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM note_code_links`); err != nil {
			return err
		}
		ins, err := tx.Prepare(`INSERT OR IGNORE INTO note_code_links(note_id,symbol_id,name) VALUES(?,?,?)`)
		if err != nil {
			return err
		}
		defer ins.Close()
		for _, l := range links {
			if _, err := ins.Exec(l.noteID, l.symID, l.name); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	n, _ := s.Count("note_code_links")
	return n, nil
}

type symRow struct{ id, name string }

// symbolsByName resolves a token to symbols by exact name or last-segment match,
// capped (an ambiguous common method name returning many is skipped by the caller's
// cap so it does not link to everything).
func (s *Store) symbolsByName(tok string, limit int) []symRow {
	rows, err := s.readDB.Query(
		`SELECT id, name FROM code_symbols WHERE name = ? OR name LIKE ? LIMIT ?`,
		tok, "%."+tok, limit+1)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []symRow
	for rows.Next() {
		var r symRow
		if rows.Scan(&r.id, &r.name) == nil {
			out = append(out, r)
		}
	}
	if len(out) > limit { // too ambiguous to link confidently
		return nil
	}
	return out
}

// NoteCodeRef is a note linked to a symbol (either direction).
type NoteCodeRef struct {
	NoteID   string `json:"note_id"`
	Title    string `json:"title"`
	Path     string `json:"path"`
	Type     string `json:"type"`
	SymbolID string `json:"symbol_id,omitempty"`
	Symbol   string `json:"symbol,omitempty"`
}

// NotesForSymbol returns the notes that reference a given code symbol id.
func (s *Store) NotesForSymbol(symbolID string) ([]NoteCodeRef, error) {
	rows, err := s.readDB.Query(
		`SELECT n.id, n.title, n.path, n.type
		   FROM note_code_links l JOIN notes n ON n.id = l.note_id
		  WHERE l.symbol_id = ? ORDER BY n.title`, symbolID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NoteCodeRef
	for rows.Next() {
		var r NoteCodeRef
		if err := rows.Scan(&r.NoteID, &r.Title, &r.Path, &r.Type); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SymbolsForNote returns the code symbols a note references.
func (s *Store) SymbolsForNote(noteID string) ([]NoteCodeRef, error) {
	rows, err := s.readDB.Query(
		`SELECT symbol_id, name FROM note_code_links WHERE note_id = ? ORDER BY name`, noteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NoteCodeRef
	for rows.Next() {
		var r NoteCodeRef
		if err := rows.Scan(&r.SymbolID, &r.Symbol); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// NotesForSymbolName returns notes linked to any symbol matching a name (exact, or as
// the type/receiver or last segment), so "Verifier" surfaces notes about Verifier,
// Verifier.Configured, or x.Verifier, regardless of which symbol id FTS ranked first.
func (s *Store) NotesForSymbolName(name string) ([]NoteCodeRef, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, nil
	}
	rows, err := s.readDB.Query(
		`SELECT DISTINCT n.id, n.title, n.path, n.type
		   FROM note_code_links l JOIN notes n ON n.id = l.note_id
		  WHERE l.name = ? OR l.name LIKE ? OR l.name LIKE ?
		  ORDER BY n.title`,
		name, name+".%", "%."+name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NoteCodeRef
	for rows.Next() {
		var r NoteCodeRef
		if err := rows.Scan(&r.NoteID, &r.Title, &r.Path, &r.Type); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// NoteCountForSymbol returns how many notes reference a symbol (for result badges).
func (s *Store) NoteCountForSymbol(symbolID string) int {
	var n int
	_ = s.readDB.QueryRow(`SELECT count(*) FROM note_code_links WHERE symbol_id = ?`, symbolID).Scan(&n)
	return n
}
