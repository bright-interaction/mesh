// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Now returns the current time and is overridable in tests.
var Now = func() time.Time { return time.Now() }

// NewNoteSpec is the irreducible, judgment-only input an author provides. Mesh
// derives everything else: id, timestamps, placement, filename, skeleton.
type NewNoteSpec struct {
	Type     NoteType
	Title    string
	Do       string
	Dont     string
	Why      string
	Related  []string
	Tags     []string
	Status   string
	Severity string
	By       string
	// Provenance (all optional; recorded in the note's frontmatter).
	Author     string
	Agent      string
	Source     string
	SourceURL  string
	Confidence string
	ReviewBy   string
	ImportedAt string
	// Scope is the access-control partition(s) to stamp on the new note. Empty leaves
	// the note unlabeled (= dev-only by the EffectiveScopes default).
	Scope []string
}

// CreateResult reports what Mesh filled in and what the author still must.
type CreateResult struct {
	Path  string
	ID    string
	When  string
	TODOs []string
}

// DirForType maps a note type to its vault subdirectory.
func DirForType(t NoteType) string {
	switch t {
	case TypeDecision:
		return "decisions"
	case TypeGotcha:
		return "gotchas"
	case TypePostMortem:
		return "post-mortems"
	case TypeEntity:
		return "entities"
	case TypeConcept:
		return "concepts"
	case TypeMap:
		return "maps"
	default:
		return "notes"
	}
}

// Slugify turns arbitrary text into a kebab-case slug. Used for ids, filenames,
// and heading anchors.
func Slugify(s string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// CreateNote writes a new note with everything derivable already filled: id from
// the title (collision-suffixed), when/created auto-stamped, placed in the
// type's subdirectory, with a type-specific body skeleton. The author only fills
// the judgment fields. Returns the path and any flywheel fields still to fill.
func CreateNote(root string, spec NewNoteSpec) (*CreateResult, error) {
	if !spec.Type.Valid() {
		return nil, fmt.Errorf("invalid type %q", spec.Type)
	}
	title := strings.TrimSpace(spec.Title)
	if title == "" {
		return nil, fmt.Errorf("title is required")
	}
	base := Slugify(title)
	if base == "" {
		base = "note"
	}
	dir := filepath.Join(root, DirForType(spec.Type))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	date := Now().Format("2006-01-02")

	fm := &Frontmatter{
		Type:       spec.Type,
		Title:      title,
		When:       date,
		Created:    date,
		Related:    normalizeLinks(spec.Related),
		Tags:       normalizeTags(spec.Tags),
		Status:     spec.Status,
		Severity:   spec.Severity,
		Author:     strings.TrimSpace(spec.Author),
		Agent:      strings.TrimSpace(spec.Agent),
		Source:     strings.TrimSpace(spec.Source),
		SourceURL:  strings.TrimSpace(spec.SourceURL),
		Confidence: strings.TrimSpace(spec.Confidence),
		ReviewBy:   strings.TrimSpace(spec.ReviewBy),
		ImportedAt: strings.TrimSpace(spec.ImportedAt),
		Scope:      normalizeTags(spec.Scope),
	}
	if spec.Type.RequiresFlywheel() {
		fm.Do = orTODO(spec.Do)
		fm.Dont = orTODO(spec.Dont)
		fm.Why = orTODO(spec.Why)
	} else {
		fm.Do, fm.Dont, fm.Why = spec.Do, spec.Dont, spec.Why
	}

	// Claim the filename by CREATING it, and let the id follow the claim. The old shape
	// picked a free path with os.Stat and then wrote it with os.WriteFile, which is
	// check-then-act: two agents writing back the same title concurrently both saw the
	// base path free, both rendered `id: <base>`, and the second write TRUNCATED the
	// first. Both got a success receipt naming a note that no longer existed - silent
	// loss of exactly the write-back the flywheel exists to capture, and reachable from
	// any concurrent caller (mesh mcp --http, the hub's /mcp, internal/web/pending_api).
	// O_EXCL makes the claim atomic, so a loser sees ErrExist and takes the next suffix.
	for n := 1; n <= maxIDAttempts; n++ {
		id := base
		if n > 1 {
			id = fmt.Sprintf("%s-%d", base, n)
		}
		path := filepath.Join(dir, id+".md")
		fm.ID = id

		content, err := renderNote(fm, spec.By)
		if err != nil {
			return nil, err
		}
		// Guard: a note whose frontmatter does not re-parse would be silently dropped
		// from the index (invalid YAML removes it from search and the graph with no
		// warning). yaml.Marshal quotes values correctly today, so this should never
		// fire, but it makes that a hard invariant: Mesh's own tools never write a note
		// that would vanish.
		if err := validateRoundTrip(content, id); err != nil {
			return nil, err
		}

		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if errors.Is(err, os.ErrExist) {
			continue // taken, by an earlier note or by a concurrent writer
		}
		if err != nil {
			return nil, err
		}
		if _, err := f.Write([]byte(content)); err != nil {
			f.Close()
			return nil, err
		}
		if err := f.Close(); err != nil {
			return nil, err
		}
		return &CreateResult{Path: path, ID: id, When: date, TODOs: fm.Validate()}, nil
	}
	return nil, fmt.Errorf("could not claim a free id for %q after %d attempts", base, maxIDAttempts)
}

// maxIDAttempts bounds the suffix search so a pathological directory cannot spin forever.
const maxIDAttempts = 1000

func orTODO(s string) string {
	if strings.TrimSpace(s) == "" {
		return "TODO"
	}
	return s
}

func normalizeLinks(in []string) StringList {
	var out StringList
	for _, s := range in {
		s = strings.TrimSpace(strings.Trim(strings.TrimSpace(s), "[]"))
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func normalizeTags(in []string) StringList {
	var out StringList
	seen := map[string]bool{}
	for _, s := range in {
		s = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "#")))
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// validateRoundTrip re-parses a freshly rendered note to prove its frontmatter is
// valid YAML and the id survives the round trip. Broken frontmatter would make the
// note invisible to search and the graph with no warning, so a note that fails this
// is never written to disk.
func validateRoundTrip(content, wantID string) error {
	fmStr, _, had := SplitFrontmatter(content)
	if !had {
		return fmt.Errorf("rendered note has no frontmatter block")
	}
	parsed, _, err := ParseFrontmatter([]byte(fmStr))
	if err != nil {
		return fmt.Errorf("rendered note has invalid frontmatter (would be dropped by the index): %w", err)
	}
	if parsed.ID != wantID {
		return fmt.Errorf("rendered note id %q does not round-trip (want %q)", parsed.ID, wantID)
	}
	return nil
}

func renderNote(fm *Frontmatter, by string) (string, error) {
	y, err := yaml.Marshal(fm)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("---\n")
	b.Write(y)
	b.WriteString("---\n\n# ")
	b.WriteString(fm.Title)
	b.WriteString("\n\n")
	b.WriteString(renderBody(fm))
	if by != "" {
		b.WriteString("\n<!-- authored by ")
		b.WriteString(by)
		b.WriteString(" -->\n")
	}
	return b.String(), nil
}

// renderBody builds a note's body. For the three flywheel types (decision, gotcha,
// post-mortem) the body is PROJECTED from the same do/dont/why the author already put in
// frontmatter, via bodySections below, so a filled note reads as prose under real
// headings instead of a TODO skeleton sitting under a wall of YAML. Frontmatter is left
// untouched by this: retrieve.Card and internal/index read do/dont/why from there, so
// those fields stay exactly as CreateNote set them regardless of what the body ends up
// saying. Every other type (entity/concept/map/note) is not fed by do/dont/why at all
// (see NewNoteSpec/CreateNote), so its body is still the fixed bodyTemplate skeleton.
func renderBody(fm *Frontmatter) string {
	if sections := bodySections(fm.Type); sections != nil {
		return renderSections(fm, sections)
	}
	return bodyTemplate(fm.Type)
}

func bodyTemplate(t NoteType) string {
	if sections := bodySections(t); sections != nil {
		// Total for every type: an empty *Frontmatter reproduces the plain skeleton,
		// so this stays a single source of truth instead of a second copy of the text.
		return renderSections(&Frontmatter{}, sections)
	}
	switch t {
	case TypeEntity:
		return tmplEntity
	case TypeConcept:
		return tmplConcept
	case TypeMap:
		return tmplMap
	default:
		return tmplNote
	}
}

// bodySection is one heading of a flywheel note's body: its title, the placeholder shown
// while nothing is authored, and the frontmatter field (if any) that fills it once the
// author supplies real content.
type bodySection struct {
	heading     string
	placeholder string
	field       func(fm *Frontmatter) string
}

func fieldDo(fm *Frontmatter) string   { return fm.Do }
func fieldDont(fm *Frontmatter) string { return fm.Dont }
func fieldWhy(fm *Frontmatter) string  { return fm.Why }

// bodySections returns the ordered body sections for a type that is fed by do/dont/why,
// or nil for a type that is not (entity/concept/map/note keep the fixed skeleton from
// bodyTemplate).
//
// The mapping is the SAME shape for all three flywheel types, because do/dont/why mean
// the same three things everywhere in Mesh (ORGANIZATION.md: a decision/gotcha's
// "required body" is literally "**Do:** ... **Don't:** ... **Why:** ..."):
//
//   - why is the causal, explanatory field -> the type's causal section
//     (Context / Cause / Root cause).
//   - do is the actionable recommendation -> the type's action section
//     (Decision / Fix / Follow-ups).
//   - dont is the cautionary field, which in practice describes what goes wrong when the
//     advice is ignored -> the type's consequence section
//     (Consequences / Symptom / Impact).
//
// post-mortem has a fourth section, "What happened", with no corresponding field: it is
// the plain narrative of the incident, and do/dont/why carry judgments (a recommendation,
// a warning, a reason), not a timeline. It keeps its TODO placeholder even when the other
// three sections are filled, on purpose: inventing a fourth field's content from the
// other three would be fabrication, and an honest gap is better than invented history
// (see the post-mortem on the 65 title-only note stubs: guidance that cannot be sourced
// from the note itself must never be authored on the note's behalf).
func bodySections(t NoteType) []bodySection {
	switch t {
	case TypeDecision:
		return []bodySection{
			{"Context", "<!-- TODO: what forced a decision; the situation and constraints -->", fieldWhy},
			{"Decision", "<!-- TODO: what was decided, stated plainly -->", fieldDo},
			{"Consequences", "<!-- TODO: trade-offs accepted; what this makes easy or hard -->", fieldDont},
			{"Related", "<!-- linked notes from the related: field render in the graph -->", nil},
		}
	case TypeGotcha:
		return []bodySection{
			{"Symptom", "<!-- TODO: how the problem shows up -->", fieldDont},
			{"Cause", "<!-- TODO: the root cause -->", fieldWhy},
			{"Fix", "<!-- TODO: the resolution or workaround -->", fieldDo},
		}
	case TypePostMortem:
		return []bodySection{
			{"What happened", "<!-- TODO: the incident, plainly -->", nil},
			{"Impact", "<!-- TODO: who or what was affected and for how long -->", fieldDont},
			{"Root cause", "<!-- TODO: the underlying cause, not just the trigger -->", fieldWhy},
			{"Follow-ups", "<!-- TODO: concrete actions, owners, dates -->", fieldDo},
		}
	default:
		return nil
	}
}

// renderSections fills each heading with its mapped frontmatter field when the author
// supplied real content, and its placeholder comment otherwise. Unfilled is the SAME
// predicate lint and the indexer use for "the author left this blank" (empty, or the
// literal "TODO" sentinel the scaffold writes), so a section and its frontmatter field can
// never disagree about whether they are filled.
func renderSections(fm *Frontmatter, sections []bodySection) string {
	parts := make([]string, len(sections))
	for i, s := range sections {
		content := s.placeholder
		if s.field != nil {
			if v := strings.TrimSpace(s.field(fm)); !Unfilled(v) {
				content = v
			}
		}
		parts[i] = "## " + s.heading + "\n" + content
	}
	return strings.Join(parts, "\n\n") + "\n"
}

const tmplNote = `## Overview
<!-- TODO: the substance of this note -->

## Related
<!-- linked notes from the related: field render in the graph -->
`

// linkPlaceholder shows the wikilink syntax in backticks, so it reads as the example it
// is rather than as a link. A bare [[note-id]] in a template made every note Mesh
// scaffolds ship a real link to a note nobody had written, so the author inherited a
// broken-link notice from the skeleton itself.
const linkPlaceholder = "`[[note-id]]`"

// tmplEntity / tmplConcept / tmplMap follow the LLM-wiki (Karpathy) shape: a bold
// one-line card up top (the atomic summary that also becomes the retrieval snippet),
// then a few dense, type-specific sections, then Related links. The AI fills the
// <!-- TODO --> guidance; everything else is derived.

const tmplEntity = `**One-liner.** <!-- TODO: what this is and why it matters, in one sentence -->

## What it does
<!-- TODO: the core function, plainly -->

## How it works
<!-- TODO: architecture, the moving parts, where it runs -->

## Key facts
<!-- TODO: the non-obvious things a teammate needs (constraints, gotchas, status, repo) -->

## Related
<!-- link the concepts it uses and the decisions that shaped it: ` + linkPlaceholder + ` (also fill related: above) -->
`

const tmplConcept = `**One-liner.** <!-- TODO: the idea in one sentence -->

## The idea
<!-- TODO: what it is and why it matters -->

## How it works
<!-- TODO: the mechanism, the model, the steps -->

## How it applies to our work
<!-- TODO: where we use it, with ` + linkPlaceholder + ` links to the entities that apply it -->

## Related
<!-- ` + linkPlaceholder + ` -->
`

const tmplMap = `**One-liner.** <!-- TODO: the domain this maps, in one sentence -->

<!-- One paragraph of context, then grouped links. A map is mostly links: if you are
     writing long prose here it wants to be a concept instead. -->

## <!-- TODO: a section per sub-area -->
- ` + linkPlaceholder + ` - <!-- why it matters -->
`
