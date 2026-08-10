// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"fmt"
	"log/slog"
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
// Two mappings feed it. bodySections covers decision/gotcha/post-mortem, whose do/dont/why
// come from the author. referenceSections covers entity and note, where a HUMAN scaffold
// has no such fields but an AGENT write (mesh_write_entity, mesh_append_note) does: those
// pages were a silent no-op here until 2026-08-10, so 18 of them in the live vault kept a
// pure TODO skeleton with their whole substance sitting in frontmatter. Within either
// mapping, a section is only
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
		// Not a flywheel type. A reference page (entity, note) is not fed by do/dont/why
		// when a HUMAN scaffolds it, but an agent writing one through mesh_write_entity /
		// mesh_append_note does supply that prose, and it landed in frontmatter with the
		// body left as a pure TODO skeleton on 18 pages in the live vault.
		sections = referenceSections(fm.Type)
	}
	if sections == nil {
		return res, nil // nothing in frontmatter maps to this type's body
	}

	newBody, filled := fillBodySections(body, fm, sections)
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

// fillBodySections projects each section's mapped frontmatter field into the body, and
// reports which headings it touched. It is shared by the create path (renderBody) and the
// backfill (BackfillBodyFile) so a page written today and a page repaired years later end
// up byte-identical; they had no reason to agree before, and a projection that only ran at
// create time is exactly how 18 live pages kept a skeleton body forever.
func fillBodySections(body string, fm *Frontmatter, sections []bodySection) (string, []string) {
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
		heading := "## " + s.heading + "\n"
		// A section can be in one of three states, and only the first two may be touched.
		// Absent, so the note predates this heading existing (every gotcha and post-mortem
		// written before Related was added to their templates). Present but still holding a
		// placeholder, which includes placeholders this template used to emit and no longer
		// does. Or present and authored, which is judgment and is never overwritten.
		switch {
		case !strings.Contains(newBody, heading):
			newBody = appendSection(newBody, s.heading, v)
			filled = append(filled, s.heading+" (added)")
		default:
			replaced := false
			for _, ph := range append([]string{s.placeholder}, retiredPlaceholders[s.heading]...) {
				old := heading + ph
				if strings.Contains(newBody, old) {
					newBody = strings.Replace(newBody, old, heading+v, 1)
					replaced = true
					break
				}
			}
			if !replaced {
				continue // authored already; leave it exactly as written
			}
			filled = append(filled, s.heading)
		}
	}
	return newBody, filled
}

// retiredPlaceholders are placeholder texts a template USED to emit for a heading. A note
// scaffolded before the wording changed still carries the old string, and without this it
// reads as authored prose and is skipped forever, so the backfill would quietly miss
// exactly the notes that need it most: the oldest ones.
var retiredPlaceholders = map[string][]string{
	"Related": {
		"<!-- linked notes from the related: field render in the graph -->",
		// The entity/concept templates word their Related prompt differently, so without
		// this every entity page kept the prompt instead of its links.
		"<!-- link the concepts it uses and the decisions that shaped it: `[[note-id]]` (also fill related: above) -->",
		"<!-- `[[note-id]]` -->",
	},
}

// appendSection adds a heading the note never had. It goes at the end of the body but
// BEFORE any trailing provenance comment (<!-- authored by ... -->), which every
// scaffolded note carries as its last line; appending past the signature would read as an
// afterthought bolted on rather than part of the note.
//
// It also lands before a trailing "## Related", because Related is the note's closing
// link list in every template and a section bolted on after it reads as an appendix to the
// links rather than part of the note. Without this a repaired plain note came out
// Overview, Related, Do, Don't.
func appendSection(body, heading, content string) string {
	section := "## " + heading + "\n" + content + "\n"
	trimmed := strings.TrimRight(body, "\n")

	// Match the provenance line specifically, not any trailing comment: a template's last
	// line is often a placeholder comment (tmplNote closes on the Related prompt), and
	// treating that as the signature tore the Related section in half.
	tail := ""
	if i := strings.LastIndex(trimmed, "\n"+provenancePrefix); i >= 0 && strings.HasSuffix(trimmed, "-->") {
		tail = trimmed[i+1:]
		trimmed = strings.TrimRight(trimmed[:i], "\n")
	}
	if i := lastTrailingRelated(trimmed); i >= 0 {
		head := strings.TrimRight(trimmed[:i], "\n")
		rel := strings.TrimRight(trimmed[i:], "\n")
		trimmed = head + "\n\n" + strings.TrimRight(section, "\n") + "\n\n" + rel
		section = ""
	}
	out := trimmed
	if section != "" {
		out = trimmed + "\n\n" + strings.TrimRight(section, "\n")
	}
	if tail != "" {
		return out + "\n\n" + tail + "\n"
	}
	return out + "\n"
}

// provenancePrefix opens the "<!-- authored by X -->" line renderNote closes every
// scaffolded note with.
const provenancePrefix = "<!-- authored by "

// lastTrailingRelated returns the byte offset of a "## Related" heading that is the LAST
// section of body, or -1. A Related section with other headings after it is not the
// closing link list, so it is left alone.
func lastTrailingRelated(body string) int {
	i := strings.LastIndex(body, "\n## Related\n")
	if i < 0 {
		if strings.HasPrefix(body, "## Related\n") {
			i = 0
		} else {
			return -1
		}
	} else {
		i++ // skip the leading newline
	}
	if strings.Contains(body[i+len("## Related\n"):], "\n## ") {
		return -1
	}
	return i
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
	return WriteNoteAtomic(path, []byte(content))
}

// WriteNoteAtomic replaces a note's bytes durably: the content goes to a temp file in
// the same directory, is fsynced, and is then renamed over the note, after which the
// directory entry is fsynced too.
//
// It replaces a plain os.WriteFile, which was neither atomic nor durable here. WriteFile
// opens with O_TRUNC, so the note is EMPTY from the truncate until the write lands: a
// crash in that window leaves nothing behind, and a concurrent reader (the indexer, the
// sync daemon, an editor) can observe the empty or half-written file even with no crash
// at all. That is the write path behind every bulk in-place rewrite of a whole vault
// (`mesh migrate --apply`, `mesh scope backfill --apply`, `mesh structure --fill-bodies
// --apply` and `--wire-orphans --apply`), none of which takes a backup first.
//
// The rename gives crash-atomicity and the two fsyncs give durability, in that order:
// the data must reach the device BEFORE the rename publishes the name, or the rename can
// point at blocks that were never written; the directory must be fsynced AFTER, or the
// new entry itself can be lost. Exported because internal/ingest lands imported notes
// through it too, so the whole module has ONE durable note writer instead of a sixth
// hand-rolled copy that the next durability pass has to find again.
//
// The destination's current permission bits are preserved when it already exists, since
// a rename installs the temp file's mode: without this a note the author chmod'd 0600
// would come back 0644.
func WriteNoteAtomic(path string, content []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	perm := os.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		perm = fi.Mode().Perm()
		// A rename replaces the destination without ever opening it, so temp+rename would
		// cheerfully overwrite a note the author chmod'd read-only, which the os.WriteFile
		// this replaced refused with EACCES. That refusal is the only protection an author
		// has against a bulk `--apply` pass touching a note they locked, and losing it
		// silently would be a durability fix that costs data control. Ask the destination
		// the same question a direct write would, before anything is created.
		w, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		w.Close()
	}
	tmp, err := os.CreateTemp(dir, ".mesh-note-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	// CreateTemp makes the file 0600; match the mode the note is meant to land with.
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	// Flush the data to the device BEFORE the rename publishes the name.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	syncDir(dir)
	return nil
}

// syncDir fsyncs a directory so a rename (or a freshly created note) that just landed in
// it survives a power cut. Best effort on purpose: opening or fsyncing a directory handle
// is a no-op or an error on some platforms and network filesystems (Windows cannot open
// one at all), and that is not a failed write, so the bytes still count as written and
// the refusal is only logged. The data itself is already fsynced by the caller.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		slog.Debug("vault: cannot open the note directory to fsync it", "dir", dir, "err", err)
		return
	}
	if err := d.Sync(); err != nil {
		slog.Debug("vault: directory fsync not supported here", "dir", dir, "err", err)
	}
	d.Close()
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
