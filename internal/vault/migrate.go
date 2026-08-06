// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// MigrateResult records what a migration pass did to one file.
type MigrateResult struct {
	Path    string
	Changed bool
	Actions []string // changes applied (or that would be, in dry-run)
	Issues  []string // flywheel fields still needing human authoring (never fabricated)
}

var wikilinkTarget = regexp.MustCompile(`\[\[([^\]|#]+)`)

// MigrateFile brings one file up to the Mesh schema, idempotently:
//   - synthesize a stable id from the filename when missing,
//   - add type: note when absent (concept/map are valid, so they are kept),
//   - map updated -> when (falling back to the file mtime),
//   - lift a "## Related" section's [[links]] into a related: array.
//
// It never fabricates do/dont/why; missing flywheel fields are reported in
// Issues. With dryRun it reports without writing. Re-running is a no-op.
func MigrateFile(path string, dryRun bool) (*MigrateResult, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// Refuse an unclosed block. Migrating it takes the !had branch below and prepends a
	// SECOND, closed block declaring type: note, demoting the author's real declaration to
	// body prose and reporting success - which turns one missing line into a note that
	// authoritatively lies about itself. writeNoteChecked cannot catch it: the output it
	// inspects has perfectly valid frontmatter. This is exactly what `mesh lint` used to
	// recommend for such a note.
	if UnterminatedFrontmatter(string(data)) {
		return nil, fmt.Errorf("%s: %w", path, ErrUnterminatedFrontmatter)
	}
	fmText, body, had := SplitFrontmatter(string(data))
	fm, raw, err := ParseFrontmatter([]byte(fmText))
	if err != nil {
		return nil, err
	}
	res := &MigrateResult{Path: path}
	var add []string

	if _, ok := raw["id"]; !ok || strings.TrimSpace(fm.ID) == "" {
		id := Slugify(strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)))
		add = append(add, "id: "+id)
		res.Actions = append(res.Actions, "added id "+id)
	}
	if _, ok := raw["type"]; !ok {
		add = append(add, "type: note")
		res.Actions = append(res.Actions, "added type note")
	}
	if _, ok := raw["when"]; !ok {
		when, src := fm.Updated, "updated"
		if when == "" {
			when, src = fileMtimeDate(path), "file mtime"
		}
		add = append(add, `when: "`+when+`"`)
		res.Actions = append(res.Actions, "set when from "+src)
	}
	if _, ok := raw["related"]; !ok {
		if links := relatedLinks(body); len(links) > 0 {
			block := "related:"
			for _, l := range links {
				block += "\n  - " + l
			}
			add = append(add, block)
			res.Actions = append(res.Actions, "lifted "+strconv.Itoa(len(links))+" related links")
		}
	}

	if fm.Type.RequiresFlywheel() {
		for _, e := range fm.Validate() {
			if strings.HasPrefix(e, "do ") || strings.HasPrefix(e, "dont ") || strings.HasPrefix(e, "why ") {
				res.Issues = append(res.Issues, e)
			}
		}
	}

	if len(add) == 0 {
		return res, nil // already clean: idempotent no-op
	}
	res.Changed = true
	if dryRun {
		return res, nil
	}

	newFM := strings.Join(add, "\n")
	if fmText != "" {
		newFM += "\n" + fmText
	}
	out := "---\n" + newFM + "\n---\n" + body
	if !had {
		out = "---\n" + newFM + "\n---\n\n" + body
	}
	if err := writeNoteChecked(path, out); err != nil {
		return nil, err
	}
	return res, nil
}

// BackfillScopeFile stamps `scope: <scope>` onto a note that declares no scope yet,
// idempotently (a note that already has a scope is left untouched). Unlabeled notes
// already behave as dev by the EffectiveScopes fail-safe; this just makes that
// explicit so a note can be deliberately relabeled into another scope later.
func BackfillScopeFile(path, scope string, dryRun bool) (*MigrateResult, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// Same refusal as MigrateFile: this writer carries the identical !had branch, so it
	// corrupts an unclosed-frontmatter note the same way. Fixing only one of the two is
	// the half-migration this repo keeps paying for.
	if UnterminatedFrontmatter(string(data)) {
		return nil, fmt.Errorf("%s: %w", path, ErrUnterminatedFrontmatter)
	}
	fmText, body, had := SplitFrontmatter(string(data))
	_, raw, err := ParseFrontmatter([]byte(fmText))
	if err != nil {
		return nil, err
	}
	res := &MigrateResult{Path: path}
	if _, ok := raw["scope"]; ok {
		return res, nil // already labeled: idempotent no-op
	}
	if strings.TrimSpace(scope) == "" {
		scope = DefaultScope
	}
	res.Changed = true
	res.Actions = append(res.Actions, "added scope "+scope)
	if dryRun {
		return res, nil
	}
	newFM := "scope: " + scope
	if fmText != "" {
		newFM += "\n" + fmText
	}
	out := "---\n" + newFM + "\n---\n" + body
	if !had {
		out = "---\n" + newFM + "\n---\n\n" + body
	}
	if err := writeNoteChecked(path, out); err != nil {
		return nil, err
	}
	return res, nil
}

// BackfillRelatedFile adds `related:` entries to a note that carries none, so an
// orphaned note joins the graph instead of floating beside it. Idempotent in the way
// that matters: a note that already declares `related` is left alone, because an
// author's own links are the ground truth and a derived list must never overwrite them.
//
// It goes through writeNoteChecked like every other writer here: a rewritten block
// that will not parse removes the note from search and the graph silently, which is a
// strictly worse outcome than the disconnection this is trying to fix.
func BackfillRelatedFile(path string, related []string, dryRun bool) (*MigrateResult, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	fmText, body, had := SplitFrontmatter(string(data))
	if !had {
		return nil, noFrontmatterErr(path, string(data))
	}
	_, raw, err := ParseFrontmatter([]byte(fmText))
	if err != nil {
		return nil, err
	}
	res := &MigrateResult{Path: path}
	if v, ok := raw["related"]; ok {
		// Present but empty counts as "already handled": an empty list is how a note
		// records that its links were considered and there are none.
		if l, isList := v.([]any); !isList || len(l) > 0 {
			return res, nil
		}
	}
	var clean []string
	seen := map[string]bool{}
	for _, r := range related {
		if r = strings.TrimSpace(r); r != "" && !seen[r] {
			seen[r] = true
			clean = append(clean, r)
		}
	}
	if len(clean) == 0 {
		return res, nil
	}
	res.Changed = true
	res.Actions = append(res.Actions, fmt.Sprintf("added %d related link(s)", len(clean)))
	if dryRun {
		return res, nil
	}
	var b strings.Builder
	b.WriteString("related:\n")
	for _, r := range clean {
		b.WriteString("    - " + r + "\n")
	}
	out := "---\n" + b.String() + fmText + "\n---\n" + body
	if err := writeNoteChecked(path, out); err != nil {
		return nil, err
	}
	return res, nil
}

// BackfillBodyFile repairs a note whose body is still the TODO skeleton an older Mesh
// scaffolded (the fixed bodyTemplate text, byte-for-byte) while its do/dont/why content
// already sits in frontmatter, unread by anyone looking at the body. It reuses
// bodySections, the SAME type-to-heading mapping renderBody uses for brand-new notes (see
// scaffold.go), so a repaired note converges on exactly the body CreateNote would have
// written today instead of drifting into a second, hand-maintained mapping.
//
// Only decision/gotcha/post-mortem are fed by do/dont/why (bodySections returns nil for
// every other type, so BackfillBodyFile is a no-op there). Within those, a section is only
// touched when its body content is STILL the placeholder comment verbatim AND the mapped
// frontmatter field has real content (Unfilled decides "real" the same way lint and the
// scaffold do). An author who already replaced a placeholder with their own prose keeps it
// exactly as written, even if it disagrees with the frontmatter field: this backfill fills
// gaps, it never overwrites judgment. post-mortem's "What happened" has no mapped field
// (bodySections leaves it nil) and is never touched, on purpose, matching renderBody.
//
// Idempotent: once a section holds real content it no longer matches its placeholder
// verbatim, so a second run finds nothing left to do.
func BackfillBodyFile(path string, dryRun bool) (*MigrateResult, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	fmText, body, had := SplitFrontmatter(string(data))
	if !had {
		return nil, noFrontmatterErr(path, string(data))
	}
	fm, _, err := ParseFrontmatter([]byte(fmText))
	if err != nil {
		return nil, err
	}
	res := &MigrateResult{Path: path}
	sections := bodySections(fm.Type)
	if sections == nil {
		return res, nil // not a flywheel type: body is not fed by do/dont/why
	}

	newBody := body
	var filled []string
	for _, s := range sections {
		if s.field == nil {
			continue
		}
		v := strings.TrimSpace(s.field(fm))
		if Unfilled(v) {
			continue // frontmatter itself has nothing real to offer yet
		}
		old := "## " + s.heading + "\n" + s.placeholder
		if !strings.Contains(newBody, old) {
			continue // not still the verbatim placeholder: authored already, or heading absent
		}
		newBody = strings.Replace(newBody, old, "## "+s.heading+"\n"+v, 1)
		filled = append(filled, s.heading)
	}
	if len(filled) == 0 {
		return res, nil // idempotent no-op
	}
	res.Changed = true
	res.Actions = append(res.Actions, "filled body section(s): "+strings.Join(filled, ", "))
	if dryRun {
		return res, nil
	}
	out := "---\n" + fmText + "\n---\n" + newBody
	if err := writeNoteChecked(path, out); err != nil {
		return nil, err
	}
	return res, nil
}

// noFrontmatterErr names which of the two shapes a had=false file actually is. They need
// different fixes and "no frontmatter block" describes only one of them: it sends an author
// off to add a block that is already there, when the real repair is one missing closing ---.
func noFrontmatterErr(path, content string) error {
	if UnterminatedFrontmatter(content) {
		return fmt.Errorf("%s: %w", path, ErrUnterminatedFrontmatter)
	}
	return fmt.Errorf("%s: no frontmatter block; refusing to rewrite", path)
}

// writeNoteChecked is the single guarded write path for a note Mesh authors itself. It
// re-parses the rendered content and refuses to write when the frontmatter would not
// come back out, because an unparseable frontmatter block removes the note from search
// and the graph with no warning at all. CreateNote has enforced this since the drop was
// diagnosed; the migrate writers did not, so `mesh migrate` could still prepend a block
// that broke the YAML (an unquoted value carrying a colon, an existing block it merges
// with) and leave the note silently invisible. Both writers now go through it, so the
// invariant is "Mesh never writes a note that would vanish", not "CreateNote does not".
func writeNoteChecked(path, content string) error {
	fmStr, _, had := SplitFrontmatter(content)
	if !had {
		return fmt.Errorf("%s: rendered note has no frontmatter block; refusing to write a note the index would drop", path)
	}
	if _, _, err := ParseFrontmatter([]byte(fmStr)); err != nil {
		return fmt.Errorf("%s: rewritten frontmatter is invalid YAML (the index would drop this note, invisible to search and the graph): %w", path, err)
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

func fileMtimeDate(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return Now().Format("2006-01-02")
	}
	return fi.ModTime().Format("2006-01-02")
}

// relatedLinks collects the [[basenames]] under a "## Related" (or "## See also")
// section, deduped and in order.
//
// It reads the same stripped body the parser reads. A raw regex over the section lifted
// [[links]] out of HTML comments and code spans, and frontmatter related: is
// authoritative, so migrate promoted commented-out guidance into a real edge that no
// parser fix can suppress: `mesh new` followed by `mesh migrate` manufactured the exact
// broken link the scaffold had just stopped shipping.
func relatedLinks(body string) []string {
	clean, _ := StripNonContent(body)
	var out []string
	seen := map[string]bool{}
	inRelated := false
	for _, line := range strings.Split(clean, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "## ") {
			h := strings.ToLower(strings.TrimSpace(t[3:]))
			inRelated = h == "related" || h == "see also"
			continue
		}
		if !inRelated {
			continue
		}
		for _, m := range wikilinkTarget.FindAllStringSubmatch(line, -1) {
			name := strings.TrimSuffix(strings.TrimSpace(m[1]), ".md")
			if name != "" && !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
	}
	return out
}
