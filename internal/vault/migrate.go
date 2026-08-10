// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
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
	if fm.Type == TypeEntity {
		var extra []string
		newBody, extra = repairEntityPage(newBody, fm)
		filled = append(filled, extra...)
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

// Commit is one resolved commit behind an id a note mentions. Times and subjects come
// from the repository, never from the note, so a wrong id in the prose cannot invent a
// timeline entry: it simply does not resolve.
type Commit struct {
	ID      string
	When    time.Time
	Subject string
}

// CommitResolver looks an id up in a repository. It returns ok=false for anything that is
// not a real commit, which is also what filters the many hex-looking words in prose
// ("deadbeef", "0e5f4a1 in the log", a truncated hash that no longer exists).
type CommitResolver func(id string) (Commit, bool)

var (
	shaLike  = regexp.MustCompile(`\b[0-9a-f]{7,40}\b`)
	isoDate  = regexp.MustCompile(`\b20[0-9]{2}-[01][0-9]-[0-3][0-9]\b`)
	whatHapp = "## What happened\n" + whatHappenedPlaceholder
)

const whatHappenedPlaceholder = "<!-- TODO: the incident, plainly -->"

// BackfillTimelineFile fills a post-mortem's "What happened" section from evidence the
// note already carries, and from nothing else.
//
// That section is the one part of a post-mortem no frontmatter field feeds, so it stayed a
// TODO on 88 notes: do/dont/why carry judgments (a recommendation, a warning, a reason),
// not a chronology. Reconstructing the narrative from those three would be invention. What
// IS reconstructible, and verifiable, is when things happened: the note's own recorded
// date, the commit ids it names resolved against the repository for their real committer
// time and subject, and the other dates its prose refers to. That is what goes in.
//
// Only a section still holding its exact placeholder is touched, so an authored "What
// happened" is never overwritten, and the result no longer matches the placeholder, so
// re-running is a no-op. resolve may be nil, in which case the section is built from dates
// alone.
func BackfillTimelineFile(path string, resolve CommitResolver, dryRun bool) (*MigrateResult, error) {
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
	if fm.Type != TypePostMortem || !strings.Contains(body, whatHapp) {
		return res, nil
	}

	section := buildTimeline(string(data), fm, resolve)
	if section == "" {
		return res, nil
	}
	newBody := strings.Replace(body, whatHapp, "## What happened\n"+section, 1)
	res.Changed = true
	res.Actions = append(res.Actions, "built What happened from the note's own dates and commit ids")
	if dryRun {
		return res, nil
	}
	if err := writeNoteChecked(path, "---\n"+fmText+"\n---\n"+newBody); err != nil {
		return nil, err
	}
	return res, nil
}

// buildTimeline renders the section. Everything in it is either quoted from the note's
// frontmatter or read out of the repository; there is no sentence here that asserts
// anything the sources do not.
func buildTimeline(whole string, fm *Frontmatter, resolve CommitResolver) string {
	var commits []Commit
	if resolve != nil {
		seen := map[string]bool{}
		for _, id := range shaLike.FindAllString(whole, -1) {
			if seen[id] {
				continue
			}
			seen[id] = true
			if c, ok := resolve(id); ok {
				commits = append(commits, c)
			}
		}
		sort.Slice(commits, func(i, j int) bool { return commits[i].When.Before(commits[j].When) })
	}

	// Dedupe commits that resolve to the same object through a short and a long id.
	var uniq []Commit
	byFull := map[string]bool{}
	for _, c := range commits {
		if byFull[c.ID] {
			continue
		}
		byFull[c.ID] = true
		uniq = append(uniq, c)
	}
	commits = uniq

	recorded := strings.TrimSpace(fm.When)
	created := strings.TrimSpace(fm.Created)
	if recorded == "" {
		recorded = created
	}

	// A date is only worth listing if it is not already on screen: not the date this note
	// records, and not one a commit row already carries. Without this the thinnest notes
	// came out saying "Recorded 2026-07-28" and then "Other dates this note refers to:
	// 2026-07-28", which reads like a bug.
	covered := map[string]bool{recorded: true, created: true}
	for _, c := range commits {
		covered[c.When.Format("2006-01-02")] = true
	}
	var others []string
	seenDate := map[string]bool{}
	for _, d := range isoDate.FindAllString(whole, -1) {
		if seenDate[d] || covered[d] {
			continue
		}
		seenDate[d] = true
		others = append(others, d)
	}
	sort.Strings(others)

	var b strings.Builder
	if recorded != "" {
		b.WriteString("Recorded " + recorded)
		if created != "" && created != recorded {
			b.WriteString(" (note created " + created + ")")
		}
		if a := strings.TrimSpace(fm.Agent); a != "" {
			b.WriteString(" by " + a)
		}
		b.WriteString(".\n")
	}
	if len(commits) > 0 {
		b.WriteString("\nThe commits this note names, with the times and subjects git has for them:\n\n")
		b.WriteString("| committed | commit | subject |\n|---|---|---|\n")
		for _, c := range commits {
			b.WriteString("| " + c.When.Format("2006-01-02 15:04 -07:00") + " | `" + c.ID + "` | " + escapePipes(c.Subject) + " |\n")
		}
	}
	if len(others) > 0 {
		b.WriteString("\nOther dates this note refers to: " + strings.Join(others, ", ") + ".\n")
	}
	if b.Len() == 0 {
		return ""
	}
	if len(commits) == 0 && len(others) == 0 {
		b.WriteString("\nThis note names no commit ids and no other dates, so that one timestamp is the " +
			"whole of the recoverable chronology. The incident narrative was never written down here, " +
			"and it is not reconstructible from the note's own fields, so it is absent rather than " +
			"reconstructed.\n")
		return b.String()
	}
	b.WriteString("\nAssembled from this note's frontmatter, the commit ids in its own text resolved " +
		"against the repository, and the dates in its prose. The incident narrative is not " +
		"recoverable from those sources and is deliberately absent rather than reconstructed.\n")
	return b.String()
}

// escapePipes keeps a commit subject from breaking the markdown table it sits in.
func escapePipes(s string) string { return strings.ReplaceAll(s, "|", `\|`) }

// repairEntityPage finishes what fillBodySections starts on an entity page, for the two
// parts of tmplEntity the section machinery cannot reach.
//
// The lead gets the first sentence of `why`, which is extraction and not authoring: an
// entity's why opens by saying what the thing is, which is exactly what the one-liner
// asks for, and FirstSentence refuses anything that does not look like a clean sentence.
//
// "How it works" and "Key facts" are DROPPED when they still hold their prompt, because
// no field feeds them and nothing ever will on an agent-written page. Keeping a heading
// whose body is a TODO comment renders an empty section forever; writing one from
// inference would be worse. The template still emits both for `mesh new`.
func repairEntityPage(body string, fm *Frontmatter) (string, []string) {
	// Both repairs are for a page that HAS content: an agent wrote it, its substance is in
	// why, and nothing will ever revisit it. A page with no why is a fresh `mesh new`
	// scaffold a human is about to fill, and every prompt on it is still doing its job.
	if Unfilled(strings.TrimSpace(fm.Why)) {
		return body, nil
	}
	var did []string
	if strings.Contains(body, entityLeadPlaceholder) {
		if lead := FirstSentence(fm.Why); lead != "" {
			body = strings.Replace(body, entityLeadPlaceholder, "**One-liner.** "+lead, 1)
			did = append(did, "One-liner (from why)")
		}
	}
	for _, h := range unsourcedEntityHeadings {
		if trimmed, ok := dropPlaceholderSection(body, h); ok {
			body = trimmed
			did = append(did, h+" (dropped, nothing sources it)")
		}
	}
	return body, did
}

// dropPlaceholderSection removes a "## <heading>" section whose entire content is a single
// TODO comment. It refuses anything else, so a section a human has written is never
// touched, and it never removes a heading that has real prose or nested content under it.
func dropPlaceholderSection(body, heading string) (string, bool) {
	start := sectionStart(body, heading)
	if start < 0 {
		return body, false
	}
	rest := body[start:]
	nl := strings.Index(rest, "\n")
	if nl < 0 {
		return body, false
	}
	inner := rest[nl+1:]
	end := strings.Index(inner, "\n## ")
	tail := ""
	if end >= 0 {
		tail = inner[end+1:]
		inner = inner[:end]
	}
	if strings.TrimSpace(inner) == "" {
		return body, false
	}
	for _, line := range strings.Split(strings.TrimSpace(inner), "\n") {
		l := strings.TrimSpace(line)
		if l == "" {
			continue
		}
		if !strings.HasPrefix(l, "<!--") || !strings.HasSuffix(l, "-->") {
			return body, false // authored content: leave the whole section alone
		}
	}
	head := strings.TrimRight(body[:start], "\n")
	if tail == "" {
		return head + "\n", true
	}
	return head + "\n\n" + tail, true
}

// sectionStart returns the offset of a "## <heading>" line, or -1.
func sectionStart(body, heading string) int {
	h := "## " + heading + "\n"
	if strings.HasPrefix(body, h) {
		return 0
	}
	if i := strings.Index(body, "\n"+h); i >= 0 {
		return i + 1
	}
	return -1
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
