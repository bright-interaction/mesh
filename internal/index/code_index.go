// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/bright-interaction/mesh/internal/index/code"
)

// This file is the source-code index: the persistence + retrieval half of the
// graphify replacement. It is deliberately separate from the note index (own
// tables, own FTS, own search) so locating a function never disturbs note
// retrieval, its ranking, or the tier-0 budget. The note graph and the code graph
// share only the SQLite file and the FTS5 tokenizer.

// codeEdgeFanoutCap drops a callee name that resolves to more than this many
// symbols: a call to a name like "Error", "String", or "New" matches dozens of
// unrelated definitions and would bury the real call graph in noise. This is the
// same heuristic graphify uses to keep its extracted edges meaningful.
const codeEdgeFanoutCap = 25

// CodeStats reports what a code reindex wrote, for the CLI and orient output.
type CodeStats struct {
	Files   int
	Symbols int
	Edges   int
}

// CodeHit is one FTS5 result over the symbol corpus: enough to render a card and a
// file:line deep link without a second lookup.
type CodeHit struct {
	ID        string
	Name      string
	Kind      string
	Lang      string
	Path      string
	Line      int
	Signature string
	Doc       string
	Snippet   string
	Score     float64 // negated bm25, higher is more relevant
}

// CodeRef is a neighbor in the call graph (a caller or callee).
type CodeRef struct {
	ID        string
	Name      string
	Kind      string
	Path      string
	Line      int
	Signature string
}

// IndexCodeFull writes the parsed source files as a full reindex in one
// transaction: wipe the code tables, insert files + symbols + FTS rows, then
// rebuild the call-graph edges from the freshly written symbols. Returns counts.
func (s *Store) IndexCodeFull(files []*code.CodeFile) (CodeStats, error) {
	var stats CodeStats
	err := s.Write(func(tx *sql.Tx) error {
		for _, t := range []string{"code_files", "code_symbols", "code_edges", "code_search"} {
			if _, err := tx.Exec("DELETE FROM " + t); err != nil {
				return err
			}
		}
		if err := insertCodeFiles(tx, files, &stats); err != nil {
			return err
		}
		return rebuildCodeEdges(tx)
	})
	if err != nil {
		return CodeStats{}, err
	}
	stats.Edges, _ = s.Count("code_edges")
	return stats, nil
}

// insertCodeFiles writes the file, symbol, and FTS rows. Symbol ids are
// "code:<path>#<qualified-name>"; a within-file name collision (e.g. two func
// init()) disambiguates by appending the start line, which is stable across runs
// because the parser yields symbols in source order.
func insertCodeFiles(tx *sql.Tx, files []*code.CodeFile, stats *CodeStats) error {
	insFile, err := tx.Prepare(`INSERT OR REPLACE INTO code_files(path,lang,package,mtime,retrieval_hash) VALUES(?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer insFile.Close()
	insSym, err := tx.Prepare(`INSERT OR REPLACE INTO code_symbols(id,path,lang,name,kind,start_line,end_line,signature,doc,calls) VALUES(?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer insSym.Close()
	insFTS, err := tx.Prepare(`INSERT INTO code_search(symbol_id,lang,kind,path,start_line,name,signature,doc) VALUES(?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer insFTS.Close()

	for _, cf := range files {
		if _, err := insFile.Exec(cf.Path, cf.Lang, cf.Package, cf.Mtime, retrievalHashCode(cf)); err != nil {
			return err
		}
		stats.Files++
		seen := map[string]bool{}
		for _, sym := range cf.Symbols {
			id := "code:" + cf.Path + "#" + sym.Name
			if seen[id] {
				id = fmt.Sprintf("%s~%d", id, sym.Start)
			}
			seen[id] = true
			callsJSON := ""
			if len(sym.Calls) > 0 {
				if b, err := json.Marshal(sym.Calls); err == nil {
					callsJSON = string(b)
				}
			}
			if _, err := insSym.Exec(id, cf.Path, cf.Lang, sym.Name, sym.Kind, sym.Start, sym.End, sym.Signature, sym.Doc, callsJSON); err != nil {
				return err
			}
			// The FTS name carries the identifier split into words (DeployHandler ->
			// "deployhandler deploy handler") so a natural-language query like
			// "deploy handler" matches the real camelCase symbol, not just the
			// underscore-tokenized test names. The symbols table keeps the canonical
			// qualified name for display.
			if _, err := insFTS.Exec(id, cf.Lang, sym.Kind, cf.Path, sym.Start, splitIdent(sym.Name), sym.Signature, sym.Doc); err != nil {
				return err
			}
			stats.Symbols++
		}
	}
	return nil
}

// rebuildCodeEdges recomputes the entire call graph from code_symbols. It is pure
// SQL + in-memory resolution (no file I/O), so it is cheap to run on every reindex
// and keeps edges globally consistent: a callee name resolves to every same-named
// symbol (graphify-style fuzziness), capped by codeEdgeFanoutCap. Storing each
// symbol's call list in the symbols table is what lets this run without re-parsing.
func rebuildCodeEdges(tx *sql.Tx) error {
	if _, err := tx.Exec(`DELETE FROM code_edges`); err != nil {
		return err
	}
	rows, err := tx.Query(`SELECT id, name, calls FROM code_symbols`)
	if err != nil {
		return err
	}
	type caller struct {
		id    string
		calls []string
	}
	nameToIDs := map[string][]string{}
	var callers []caller
	for rows.Next() {
		var id, name, callsJSON string
		if err := rows.Scan(&id, &name, &callsJSON); err != nil {
			rows.Close()
			return err
		}
		u := unqualify(name)
		nameToIDs[u] = append(nameToIDs[u], id)
		if callsJSON != "" && callsJSON != "[]" {
			var calls []string
			if json.Unmarshal([]byte(callsJSON), &calls) == nil && len(calls) > 0 {
				callers = append(callers, caller{id: id, calls: calls})
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	ins, err := tx.Prepare(`INSERT OR IGNORE INTO code_edges(src_id,dst_id,relation) VALUES(?,?,'calls')`)
	if err != nil {
		return err
	}
	defer ins.Close()
	for _, c := range callers {
		seen := map[string]bool{}
		for _, callee := range c.calls {
			ids := nameToIDs[callee]
			if len(ids) == 0 || len(ids) > codeEdgeFanoutCap {
				continue
			}
			for _, dst := range ids {
				if dst == c.id || seen[dst] {
					continue
				}
				seen[dst] = true
				if _, err := ins.Exec(c.id, dst); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// SearchCode runs an FTS5 MATCH over the symbol corpus, ranking name matches well
// above signature and doc (the bm25 weights). Display fields come from the
// canonical code_symbols (joined on the symbol id), not the FTS row, whose name
// column holds the split-identifier search text. It over-fetches and re-ranks with
// a test-code penalty so a real symbol (DeployHandler) outranks the many verbose
// test names that also mention the query terms. langs optionally restricts to a set
// of language tags. Shares buildFTS5Query with note search so both tokenize alike.
func (s *Store) SearchCode(query string, limit int, langs []string) ([]CodeHit, error) {
	match := buildFTS5Query(query)
	if match == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 12
	}
	const weights = `0,0,0,0,0,10.0,4.0,1.0`
	fetch := limit * 4 // headroom for the test-penalty re-rank to promote real symbols
	if fetch < 40 {
		fetch = 40
	}
	args := []any{match}
	langFilter := ""
	if len(langs) > 0 {
		ph := make([]string, len(langs))
		for i, l := range langs {
			ph[i] = "?"
			args = append(args, l)
		}
		langFilter = " AND cs.lang IN (" + strings.Join(ph, ",") + ")"
	}
	q := `
SELECT cs.id, cs.name, cs.kind, cs.lang, cs.path, cs.start_line, COALESCE(cs.signature,''), COALESCE(cs.doc,''),
       snippet(code_search, 6, '[', ']', ' ... ', 10),
       bm25(code_search, ` + weights + `)
FROM code_search
JOIN code_symbols cs ON cs.id = code_search.symbol_id
WHERE code_search MATCH ?` + langFilter + `
ORDER BY bm25(code_search, ` + weights + `)
LIMIT ?`
	args = append(args, fetch)
	rows, err := s.readDB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CodeHit
	for rows.Next() {
		var h CodeHit
		var rank float64
		if err := rows.Scan(&h.ID, &h.Name, &h.Kind, &h.Lang, &h.Path, &h.Line, &h.Signature, &h.Doc, &h.Snippet, &rank); err != nil {
			return nil, err
		}
		h.Score = -rank // bm25 is lower-is-better; negate so higher is better
		if looksLikeTest(h.Path, h.Name) {
			h.Score *= 0.4 // demote test symbols; "where is X" wants the production definition
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// looksLikeTest reports whether a symbol is test scaffolding rather than a
// production definition, by path (_test.go, .test/.spec.ts, testdata/__tests__) or
// by the Go test-function naming convention.
func looksLikeTest(path, name string) bool {
	lp := strings.ToLower(path)
	for _, m := range []string{"_test.", ".test.", ".spec.", "/testdata/", "/tests/", "/__tests__/", "/test/"} {
		if strings.Contains(lp, m) {
			return true
		}
	}
	for _, p := range []string{"Test", "Benchmark", "Example", "Fuzz"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// CodeNeighbors returns the call-graph neighbors of a symbol: callees (what it
// calls) and callers (what calls it). Empty for languages without a call graph.
func (s *Store) CodeNeighbors(id string) (callers, callees []CodeRef, err error) {
	callees, err = s.codeRefsByEdge(id, true)
	if err != nil {
		return nil, nil, err
	}
	callers, err = s.codeRefsByEdge(id, false)
	return callers, callees, err
}

func (s *Store) codeRefsByEdge(id string, outbound bool) ([]CodeRef, error) {
	join, where := "cs.id=e.dst_id", "e.src_id=?"
	if !outbound {
		join, where = "cs.id=e.src_id", "e.dst_id=?"
	}
	q := `SELECT cs.id, cs.name, cs.kind, cs.path, cs.start_line, COALESCE(cs.signature,'')
	      FROM code_edges e JOIN code_symbols cs ON ` + join + `
	      WHERE ` + where + ` AND e.relation='calls'
	      ORDER BY cs.path, cs.start_line LIMIT 200`
	rows, err := s.readDB.Query(q, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CodeRef
	for rows.Next() {
		var r CodeRef
		if err := rows.Scan(&r.ID, &r.Name, &r.Kind, &r.Path, &r.Line, &r.Signature); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CodeIndexed reports whether the source-code index holds any symbols, so orient
// and status can mention code search only when it is populated.
func (s *Store) CodeIndexed() bool {
	n, _ := s.Count("code_symbols")
	return n > 0
}

// retrievalHashCode is the drift fingerprint of a parsed file: a content change
// that alters any symbol's identity, kind, position, or signature flips the hash.
func retrievalHashCode(cf *code.CodeFile) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s", cf.Path, cf.Package)
	for _, s := range cf.Symbols {
		fmt.Fprintf(h, "\x00%s\x00%s\x00%d\x00%s", s.Name, s.Kind, s.Start, s.Signature)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// splitIdent expands an identifier into its searchable words: the whole name plus
// its camelCase and snake_case/dotted segments, lowercased and deduped. So
// "DeployHandler" -> "deployhandler deploy handler" and "Server.HTTPGet" ->
// "server.httpget server http get". This is what lets a natural-language
// "deploy handler" query match the real symbol instead of only underscore-split
// test names, matching graphify's identifier splitting.
func splitIdent(name string) string {
	words := []string{name}
	for _, seg := range strings.FieldsFunc(name, func(r rune) bool {
		return r == '.' || r == '_' || r == '-' || r == '/'
	}) {
		words = append(words, camelWords(seg)...)
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(words))
	for _, w := range words {
		lw := strings.ToLower(w)
		if lw == "" || seen[lw] {
			continue
		}
		seen[lw] = true
		out = append(out, lw)
	}
	return strings.Join(out, " ")
}

// camelWords splits a single segment on camelCase boundaries: "DeployHandler" ->
// [Deploy Handler], "HTTPServer" -> [HTTP Server], "fooBar" -> [foo Bar].
func camelWords(s string) []string {
	rs := []rune(s)
	var words []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			words = append(words, string(cur))
			cur = nil
		}
	}
	for i, r := range rs {
		if i > 0 && unicode.IsUpper(r) {
			prev := rs[i-1]
			var next rune
			if i+1 < len(rs) {
				next = rs[i+1]
			}
			// Boundary at lower->Upper (fooBar) or the end of an acronym run
			// (HTTPServer: the S before the lowercase e starts a new word).
			if unicode.IsLower(prev) || (unicode.IsUpper(prev) && unicode.IsLower(next)) {
				flush()
			}
		}
		cur = append(cur, r)
	}
	flush()
	return words
}

// unqualify drops a "Type." method prefix so a bare callee name ("Search") matches
// the qualified symbol name ("Server.Search") during edge resolution.
func unqualify(name string) string {
	if i := strings.LastIndex(name, "."); i >= 0 {
		return name[i+1:]
	}
	return name
}

// ReindexCode walks the configured code roots, parses every source file in
// parallel, and writes a full code index. Returned symbol paths are prefixed with
// the root's basename (e.g. "automations/dockyard/...") so several repos coexist
// and the path reads like graphify's src= locator.
func ReindexCode(s *Store, roots []string, langs map[string]bool) (CodeStats, error) {
	abs, err := code.WalkCode(roots, langs)
	if err != nil {
		return CodeStats{}, err
	}
	refs := make([]code.FileRef, 0, len(abs))
	for _, p := range abs {
		if rel, ok := relToRoots(roots, p); ok {
			refs = append(refs, code.FileRef{Abs: p, Rel: rel})
		}
	}
	files, _ := code.ParseCodeFiles(refs, 0)
	return s.IndexCodeFull(files)
}

func relToRoots(roots []string, p string) (string, bool) {
	for _, root := range roots {
		rel, err := filepath.Rel(root, p)
		if err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.ToSlash(filepath.Join(filepath.Base(root), rel)), true
		}
	}
	return "", false
}
