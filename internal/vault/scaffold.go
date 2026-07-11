// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
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
	id, path := uniquePath(dir, base)
	date := Now().Format("2006-01-02")

	fm := &Frontmatter{
		ID:         id,
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
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return nil, err
	}
	return &CreateResult{Path: path, ID: id, When: date, TODOs: fm.Validate()}, nil
}

func uniquePath(dir, base string) (id, path string) {
	id = base
	path = filepath.Join(dir, id+".md")
	for n := 2; fileExists(path); n++ {
		id = fmt.Sprintf("%s-%d", base, n)
		path = filepath.Join(dir, id+".md")
	}
	return id, path
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

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
	b.WriteString(bodyTemplate(fm.Type))
	if by != "" {
		b.WriteString("\n<!-- authored by ")
		b.WriteString(by)
		b.WriteString(" -->\n")
	}
	return b.String(), nil
}

func bodyTemplate(t NoteType) string {
	switch t {
	case TypeDecision:
		return tmplDecision
	case TypeGotcha:
		return tmplGotcha
	case TypePostMortem:
		return tmplPostMortem
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

const tmplDecision = `## Context
<!-- TODO: what forced a decision; the situation and constraints -->

## Decision
<!-- TODO: what was decided, stated plainly -->

## Consequences
<!-- TODO: trade-offs accepted; what this makes easy or hard -->

## Related
<!-- linked notes from the related: field render in the graph -->
`

const tmplGotcha = `## Symptom
<!-- TODO: how the problem shows up -->

## Cause
<!-- TODO: the root cause -->

## Fix
<!-- TODO: the resolution or workaround -->
`

const tmplPostMortem = `## What happened
<!-- TODO: the incident, plainly -->

## Impact
<!-- TODO: who or what was affected and for how long -->

## Root cause
<!-- TODO: the underlying cause, not just the trigger -->

## Follow-ups
<!-- TODO: concrete actions, owners, dates -->
`

const tmplNote = `## Overview
<!-- TODO: the substance of this note -->

## Related
<!-- linked notes from the related: field render in the graph -->
`

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
<!-- link the concepts it uses and the decisions that shaped it: [[note-id]] (also fill related: above) -->
`

const tmplConcept = `**One-liner.** <!-- TODO: the idea in one sentence -->

## The idea
<!-- TODO: what it is and why it matters -->

## How it works
<!-- TODO: the mechanism, the model, the steps -->

## How it applies to our work
<!-- TODO: where we use it, with [[links]] to the entities that apply it -->

## Related
<!-- [[note-id]] -->
`

const tmplMap = `**One-liner.** <!-- TODO: the domain this maps, in one sentence -->

<!-- One paragraph of context, then grouped links. A map is mostly links: if you are
     writing long prose here it wants to be a concept instead. -->

## <!-- TODO: a section per sub-area -->
- [[note-id]] - <!-- why it matters -->
`
