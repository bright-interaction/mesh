// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Knowledge-lifecycle health. A note's value rots silently: it cites a file that
// was deleted, or it asked to be re-checked by a date that has passed. ComputeHealth
// finds these and records them so mesh_health + the dashboard can surface a vault
// that needs tending. Contradiction findings are written by the curator (C3) into
// the same table via RecordHealth.

// HealthFinding is one lifecycle issue against a note.
type HealthFinding struct {
	NoteID string `json:"note_id"`
	Path   string `json:"path"`
	Issue  string `json:"issue"`  // dead_ref | overdue | contradiction
	Detail string `json:"detail"` // the missing ref / overdue date / partner note
}

// codePathRe matches a source-file path token (high precision: a real extension).
var codePathRe = regexp.MustCompile(`[A-Za-z0-9_./-]+\.(?:go|ts|tsx|js|jsx|svelte|astro|py)\b`)

// isChangelogNote reports whether a note is an append-only history log (the vault's
// `*-log` entities and the root `log`). Such notes deliberately record file paths as
// they were at the time, so their references going dead is expected history, not rot;
// dead_ref detection skips them so the finding stays actionable.
func isChangelogNote(id string) bool {
	id = strings.ToLower(strings.TrimSpace(id))
	return id == "log" || strings.HasSuffix(id, "-log")
}

// ComputeHealth runs the dead-ref + overdue passes over the vault and replaces the
// note_health rows for those two issue types (it leaves contradiction rows, which
// the curator owns). Returns the findings it wrote. vaultRoot is the notes vault.
func (s *Store) ComputeHealth(vaultRoot string, now time.Time) ([]HealthFinding, error) {
	codeFiles, err := s.codeFilePaths()
	if err != nil {
		return nil, err
	}
	notes, err := s.noteList()
	if err != nil {
		return nil, err
	}
	today := now.Format("2006-01-02")
	// Directories we actually index. We only call a path dead when we index its
	// directory but not the file (it moved/was deleted). A reference into a folder we
	// do not index can't be judged and must NOT be flagged, or every cross-repo or
	// illustrative filename ("Next.js", "components/X.svelte") cries wolf.
	indexedDirs := indexedDirSet(codeFiles)
	var findings []HealthFinding
	for _, n := range notes {
		body, err := os.ReadFile(filepath.Join(vaultRoot, n.path))
		if err != nil {
			continue
		}
		// Dead source-file references (only meaningful once the code index exists).
		// Changelogs are exempt: an append-only history log records file paths as they
		// were at the time, so a reference that later points at a moved/deleted file is
		// expected history, not rot. Flagging every changelog line buries the genuine
		// cases (a live note claiming a current file that is gone), so skip them here,
		// the same high-precision spirit as the "don't judge dirs we don't index" guard.
		if len(codeFiles) > 0 && !isChangelogNote(n.id) {
			seen := map[string]bool{}
			for _, m := range codePathRe.FindAllString(string(body), -1) {
				ref := strings.TrimLeft(m, "./")
				slash := strings.LastIndexByte(ref, '/')
				if slash <= 0 { // bare filename / domain -> not a checkable path
					continue
				}
				if !dirIndexed(indexedDirs, ref[:slash]) { // a folder we don't index -> can't judge
					continue
				}
				if seen[ref] || ref == n.path {
					continue
				}
				seen[ref] = true
				if !codeFileKnown(codeFiles, ref) {
					findings = append(findings, HealthFinding{NoteID: n.id, Path: n.path, Issue: "dead_ref", Detail: ref})
				}
			}
		}
		// Overdue review_by.
		if n.reviewBy != "" && n.reviewBy < today {
			findings = append(findings, HealthFinding{NoteID: n.id, Path: n.path, Issue: "overdue", Detail: "review_by " + n.reviewBy})
		}
	}
	// Replace dead_ref + overdue rows atomically (keep contradiction rows).
	at := now.Unix()
	err = s.Write(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM note_health WHERE issue IN ('dead_ref','overdue')`); err != nil {
			return err
		}
		ins, err := tx.Prepare(`INSERT INTO note_health(note_id,path,issue,detail,detected_at) VALUES(?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer ins.Close()
		for _, f := range findings {
			if _, err := ins.Exec(f.NoteID, f.Path, f.Issue, f.Detail, at); err != nil {
				return err
			}
		}
		return nil
	})
	return findings, err
}

// tier0Health are the institutional types whose do/dont guidance is worth checking
// for contradictions.
var tier0Health = map[string]bool{"decision": true, "gotcha": true, "post-mortem": true}

// ComputeContradictions flags pairs of tier-0 notes that share a tag where one
// note's `do` strongly overlaps the other's `dont` (one recommends what the other
// forbids). Dependency-free heuristic (token Jaccard, high threshold to stay
// high-precision); the curator can later confirm with an LLM. Writes contradiction
// rows. Returns the findings.
func (s *Store) ComputeContradictions(now time.Time) ([]HealthFinding, error) {
	notes, err := s.tier0Guidance()
	if err != nil {
		return nil, err
	}
	const threshold = 0.6
	var findings []HealthFinding
	seen := map[string]bool{}
	for i := range notes {
		for j := range notes {
			if i == j || !shareTag(notes[i].tags, notes[j].tags) {
				continue
			}
			if jaccard(tokenSet(notes[i].do), tokenSet(notes[j].dont)) < threshold {
				continue
			}
			// One unordered finding per pair.
			key := pairKey(notes[i].id, notes[j].id)
			if seen[key] {
				continue
			}
			seen[key] = true
			findings = append(findings, HealthFinding{
				NoteID: notes[i].id, Path: notes[i].path, Issue: "contradiction",
				Detail: "guidance may conflict with [[" + notes[j].id + "]]",
			})
		}
	}
	return findings, s.RecordHealth("contradiction", findings, now)
}

type guidanceRow struct {
	id, path, do, dont string
	tags               []string
}

func (s *Store) tier0Guidance() ([]guidanceRow, error) {
	rows, err := s.readDB.Query(`SELECT id, path, type, frontmatter FROM notes`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []guidanceRow
	for rows.Next() {
		var id, path, typ, fmJSON string
		if err := rows.Scan(&id, &path, &typ, &fmJSON); err != nil {
			return nil, err
		}
		if !tier0Health[typ] {
			continue
		}
		var fm struct {
			Do   string   `json:"Do"`
			Dont string   `json:"Dont"`
			Tags []string `json:"Tags"`
		}
		_ = json.Unmarshal([]byte(fmJSON), &fm)
		if fm.Do == "" && fm.Dont == "" {
			continue
		}
		out = append(out, guidanceRow{id: id, path: path, do: fm.Do, dont: fm.Dont, tags: fm.Tags})
	}
	return out, rows.Err()
}

func shareTag(a, b []string) bool {
	set := map[string]bool{}
	for _, t := range a {
		set[strings.ToLower(t)] = true
	}
	for _, t := range b {
		if set[strings.ToLower(t)] {
			return true
		}
	}
	return false
}

func tokenSet(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.Fields(strings.ToLower(s)) {
		w = strings.Trim(w, ".,;:!?\"'()[]")
		if len(w) > 2 && !stopWord[w] {
			out[w] = true
		}
	}
	return out
}

var stopWord = map[string]bool{"the": true, "and": true, "for": true, "you": true, "use": true, "not": true, "with": true, "this": true, "that": true, "are": true, "but": true}

func jaccard(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for k := range a {
		if b[k] {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

func pairKey(a, b string) string {
	if a > b {
		a, b = b, a
	}
	return a + "\x00" + b
}

// RecordHealth upserts contradiction (or any) findings without touching the
// dead_ref/overdue rows. Used by the curator's contradiction pass (C3).
func (s *Store) RecordHealth(issue string, findings []HealthFinding, now time.Time) error {
	at := now.Unix()
	return s.Write(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM note_health WHERE issue=?`, issue); err != nil {
			return err
		}
		ins, err := tx.Prepare(`INSERT INTO note_health(note_id,path,issue,detail,detected_at) VALUES(?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer ins.Close()
		for _, f := range findings {
			if _, err := ins.Exec(f.NoteID, f.Path, f.Issue, f.Detail, at); err != nil {
				return err
			}
		}
		return nil
	})
}

// ListHealth returns current findings (optionally filtered by issue, "" = all).
func (s *Store) ListHealth(issue string) ([]HealthFinding, error) {
	var rows *sql.Rows
	var err error
	if issue == "" {
		rows, err = s.readDB.Query(`SELECT note_id,path,issue,detail FROM note_health ORDER BY issue, note_id`)
	} else {
		rows, err = s.readDB.Query(`SELECT note_id,path,issue,detail FROM note_health WHERE issue=? ORDER BY note_id`, issue)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HealthFinding
	for rows.Next() {
		var f HealthFinding
		if err := rows.Scan(&f.NoteID, &f.Path, &f.Issue, &f.Detail); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// HealthCounts returns issue -> count for the dashboard.
func (s *Store) HealthCounts() (map[string]int, error) {
	rows, err := s.readDB.Query(`SELECT issue, count(*) FROM note_health GROUP BY issue`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var issue string
		var n int
		if err := rows.Scan(&issue, &n); err != nil {
			return nil, err
		}
		out[issue] = n
	}
	return out, rows.Err()
}

type noteRow struct{ id, path, reviewBy string }

func (s *Store) noteList() ([]noteRow, error) {
	rows, err := s.readDB.Query(`SELECT id, path, COALESCE(review_by,'') FROM notes`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []noteRow
	for rows.Next() {
		var n noteRow
		if err := rows.Scan(&n.id, &n.path, &n.reviewBy); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *Store) codeFilePaths() (map[string]bool, error) {
	rows, err := s.readDB.Query(`SELECT path FROM code_files`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out[p] = true
	}
	return out, rows.Err()
}

// indexedDirSet returns the directory of every indexed code file (code paths may
// carry a root prefix, e.g. "<repo>/internal/foo/bar.go").
func indexedDirSet(codeFiles map[string]bool) map[string]bool {
	out := map[string]bool{}
	for p := range codeFiles {
		if i := strings.LastIndexByte(p, '/'); i > 0 {
			out[p[:i]] = true
		}
	}
	return out
}

// dirIndexed reports whether refDir names a directory we index. Because indexed
// paths may carry a root prefix the note omits, an indexed dir whose tail is the
// ref's dir counts (e.g. indexed "<repo>/internal/foo" matches ref dir "internal/foo").
func dirIndexed(indexedDirs map[string]bool, refDir string) bool {
	if indexedDirs[refDir] {
		return true
	}
	for d := range indexedDirs {
		if strings.HasSuffix(d, "/"+refDir) {
			return true
		}
	}
	return false
}

// codeFileKnown reports whether ref names a known source file. Code paths are
// indexed root-relative while a note may cite a shorter suffix, so a suffix match
// (on a path boundary) counts as known.
func codeFileKnown(codeFiles map[string]bool, ref string) bool {
	if codeFiles[ref] {
		return true
	}
	for p := range codeFiles {
		if strings.HasSuffix(p, "/"+ref) || strings.HasSuffix(ref, "/"+p) {
			return true
		}
	}
	return false
}
