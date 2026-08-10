// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"database/sql"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/bright-interaction/mesh/internal/vault"
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

// ComputeHealth runs ScanHealth and replaces the note_health rows for the two issue
// types it owns (it leaves contradiction rows, which the curator owns). Returns the
// findings it wrote. vaultRoot is the notes vault.
func (s *Store) ComputeHealth(vaultRoot string, now time.Time) ([]HealthFinding, error) {
	findings, err := s.ScanHealth(vaultRoot, now)
	if err != nil {
		return nil, err
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

// ScanHealth is the pass itself: the dead-ref + overdue analysis over the vault,
// returning the findings and writing NOTHING.
//
// It is separate from ComputeHealth because computing health and persisting it are two
// different rights. The analysis is a pure read (the vault's markdown plus this index's
// read side), so a read-only server serves mesh_health straight from here; recording the
// result for the dashboard stays with the single owning writer. Answering from the
// persisted rows instead would serve whatever the owner last wrote, and on a vault whose
// owner never runs the pass that is nothing at all.
func (s *Store) ScanHealth(vaultRoot string, now time.Time) ([]HealthFinding, error) {
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
	var candidates []HealthFinding
	for _, n := range notes {
		raw, err := os.ReadFile(filepath.Join(vaultRoot, n.path))
		if err != nil {
			continue
		}
		// Fences and comments only, NOT inline spans. A path inside a fenced block is a
		// command you could run ("npx vitest src/lib/utils.test.ts" in a testing guide), so
		// it is an example and not a claim about the tree. A path in backticks mid-sentence
		// is just how a filename is written in prose, and suppressing those loses the real
		// findings: doing so briefly hid a procedure step pointing at a deleted script.
		cleanBody, _ := vault.StripFencesAndComments(string(raw))
		body := []byte(cleanBody)
		// Dead source-file references (only meaningful once the code index exists).
		// Changelogs are exempt: an append-only history log records file paths as they
		// were at the time, so a reference that later points at a moved/deleted file is
		// expected history, not rot. Flagging every changelog line buries the genuine
		// cases (a live note claiming a current file that is gone), so skip them here,
		// the same high-precision spirit as the "don't judge dirs we don't index" guard.
		if len(codeFiles) > 0 && !isChangelogNote(n.id) && !n.expectDeadRefs {
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
				// A path the author declared synthetic. Per-ref rather than per-note, so
				// the rest of this note's references stay checked.
				if vault.ExemptsDeadRef(n.expectDeadRefPaths, ref) {
					continue
				}
				seen[ref] = true
				if !codeFileKnown(codeFiles, ref) {
					// Candidate only. The index may simply be behind the tree, so this is
					// confirmed against the filesystem below before it becomes a finding.
					candidates = append(candidates, HealthFinding{NoteID: n.id, Path: n.path, Issue: "dead_ref", Detail: ref})
				}
			}
		}
		// Overdue review_by.
		if n.reviewBy != "" && n.reviewBy < today {
			findings = append(findings, HealthFinding{NoteID: n.id, Path: n.path, Issue: "overdue", Detail: "review_by " + n.reviewBy})
		}
	}
	// Confirm the candidates against the filesystem. A note citing a file the code index
	// has not seen yet is not rot: the index is built from a shared working tree whose
	// branch is whatever the last session left checked out, so it is routinely behind. On
	// the live vault this turned 25 findings into 0, because every referenced file was
	// present in main and only the index was stale. One walk, and only when there is
	// something to confirm.
	if len(candidates) > 0 {
		refs := make(map[string]bool, len(candidates))
		for _, c := range candidates {
			refs[c.Detail] = true
		}
		onDisk := existsUnderRoots(s.codeRoots(), refs)
		for _, c := range candidates {
			if !onDisk[c.Detail] {
				findings = append(findings, c)
			}
		}
	}
	return findings, nil
}

// tier0Health are the institutional types whose do/dont guidance is worth checking
// for contradictions.
var tier0Health = map[string]bool{"decision": true, "gotcha": true, "post-mortem": true}

// ComputeContradictions runs ScanContradictions and replaces the contradiction rows.
func (s *Store) ComputeContradictions(now time.Time) ([]HealthFinding, error) {
	findings, err := s.ScanContradictions()
	if err != nil {
		return nil, err
	}
	return findings, s.RecordHealth("contradiction", findings, now)
}

// ScanContradictions flags pairs of tier-0 notes that share a tag where one
// note's `do` strongly overlaps the other's `dont` (one recommends what the other
// forbids). Dependency-free heuristic (token Jaccard, high threshold to stay
// high-precision); the curator can later confirm with an LLM. Pure computation over the
// index's read side, writing nothing, for the same reason as ScanHealth.
func (s *Store) ScanContradictions() ([]HealthFinding, error) {
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
	return findings, nil
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

type noteRow struct {
	id, path, reviewBy string
	expectDeadRefs     bool
	expectDeadRefPaths []string
}

func (s *Store) noteList() ([]noteRow, error) {
	rows, err := s.readDB.Query(`SELECT id, path, COALESCE(review_by,''), frontmatter FROM notes`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []noteRow
	for rows.Next() {
		var n noteRow
		var fmJSON string
		if err := rows.Scan(&n.id, &n.path, &n.reviewBy, &fmJSON); err != nil {
			return nil, err
		}
		// Frontmatter is stored as JSON, so read the one flag rather than unmarshalling
		// the whole struct for every note on every health run.
		var fmFlags struct {
			ExpectDeadRefs     bool     `json:"ExpectDeadRefs"`
			ExpectDeadRefPaths []string `json:"ExpectDeadRefPaths"`
		}
		if fmJSON != "" {
			_ = json.Unmarshal([]byte(fmJSON), &fmFlags)
		}
		n.expectDeadRefs = fmFlags.ExpectDeadRefs
		n.expectDeadRefPaths = fmFlags.ExpectDeadRefPaths
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

// setCodeRoots records the directories the code index was built from.
//
// The read-only check is not redundant with Write's: this is one of the few places that
// reaches s.writeDB DIRECTLY instead of going through Write, and on a read-only store
// writeDB is nil, so without this the call is a nil-pointer panic inside database/sql
// rather than an error. A panic here would take down a long-running `mesh mcp` server,
// and it would do it on a path (mesh_code_search) that looks purely read-only from the
// outside. Any future direct writeDB use needs the same guard.
func (s *Store) setCodeRoots(roots []string) error {
	if s.readOnly {
		return ErrReadOnly
	}
	_, err := s.writeDB.Exec(
		`INSERT OR REPLACE INTO meta(key, value) VALUES('code_roots', ?)`,
		strings.Join(roots, "\n"))
	return err
}

func (s *Store) codeRoots() []string {
	var v string
	if err := s.readDB.QueryRow(`SELECT value FROM meta WHERE key='code_roots'`).Scan(&v); err != nil {
		return nil
	}
	var out []string
	for _, r := range strings.Split(v, "\n") {
		if r = strings.TrimSpace(r); r != "" {
			out = append(out, r)
		}
	}
	return out
}

// existsUnderRoots reports which refs name a file that really exists, matched the same
// suffix way codeFileKnown matches indexed paths.
//
// This is the difference between "the file is gone" and "the index has not caught up".
// The code index is built from a shared working tree whose branch is whatever the last
// session checked out, so it is routinely behind: a note citing a file added an hour ago,
// or living on another branch, is not rot. Reporting it as rot cost 25 false findings in a
// 900-note vault, every one of which existed in main, and a health check that is wrong 25
// times out of 25 is one nobody reads.
//
// Walked lazily and once per health run, only when there is at least one candidate, so a
// clean vault pays nothing.
func existsUnderRoots(roots []string, refs map[string]bool) map[string]bool {
	found := make(map[string]bool, len(refs))
	if len(refs) == 0 {
		return found
	}
	// Ask git as well as the filesystem. A code root is typically a SHARED working tree
	// whose branch is whatever the last session checked out, so "not on disk right now"
	// routinely means "on another branch", not "deleted". Checking the mainline too is
	// what separates the two; without it, a file added an hour ago on main reads as rot.
	for _, root := range roots {
		markGitKnown(root, refs, found)
	}
	for _, root := range roots {
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil //nolint:nilerr // an unreadable dir must not fail the health run
			}
			if d.IsDir() {
				switch d.Name() {
				case ".git", "node_modules", "vendor", "dist", "build":
					return filepath.SkipDir
				}
				return nil
			}
			slashed := filepath.ToSlash(path)
			for ref := range refs {
				if found[ref] {
					continue
				}
				if slashed == ref || strings.HasSuffix(slashed, "/"+ref) {
					found[ref] = true
				}
			}
			return nil
		})
	}
	return found
}

// markGitKnown marks every ref that the repository at root knows about on its mainline.
//
// Consulted in addition to the working tree because a code root is usually shared between
// concurrent sessions: whichever branch was checked out last is what is on disk, and a
// file that lives on main but not on that branch is emphatically not rot. Falls back
// through the usual mainline names and does nothing at all outside a git repo, so a
// non-git code root keeps the plain filesystem behaviour.
func markGitKnown(root string, refs map[string]bool, found map[string]bool) {
	allFound := true
	for ref := range refs {
		if !found[ref] {
			allFound = false
			break
		}
	}
	if allFound {
		return
	}
	// Every plausible mainline, and the UNION of them, not the first that resolves. A repo
	// may have no remote (a fresh clone-less checkout), or a local main that is ahead of
	// origin, or be detached mid-rebase. Stopping at the first resolvable rev meant a repo
	// whose only mainline was a local `main` fell through to HEAD, which is exactly the
	// branch that may be missing the file.
	for _, rev := range []string{"origin/HEAD", "origin/main", "origin/master", "main", "master", "HEAD"} {
		cmd := exec.Command("git", "-C", root, "ls-tree", "-r", "--name-only", rev)
		out, err := cmd.Output()
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(out), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			for ref := range refs {
				if found[ref] {
					continue
				}
				if line == ref || strings.HasSuffix(line, "/"+ref) {
					found[ref] = true
				}
			}
		}
	}
}
