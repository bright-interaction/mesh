// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"strings"
	"testing"
)

// Editing a field the embedder puts into the vector chunk (title/do/dont/why/tags) must
// change retrievalHash, so the stale pre-edit vector is excluded until a re-embed. This
// is the fix for the "corrected a gotcha's do:, but the old vector kept being served"
// bug: the hash is stamped onto each vector as note_hash and the staleness check keys on
// it, so if the hash ignored these fields the correction would never invalidate.
func TestRetrievalHashCoversEmbeddingFields(t *testing.T) {
	base := `---
id: h
type: gotcha
title: Original title
do: use sqlite
dont: use cgo
why: static binary
tags: [a, b]
---
# body
same body text
`
	parse := func(src string) *ParsedNote {
		pn, err := Parse("vault/h.md", []byte(src))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		return pn
	}
	h0 := retrievalHash(parse(base))

	edits := map[string]string{
		"title": strings.Replace(base, "Original title", "New title", 1),
		"do":    strings.Replace(base, "do: use sqlite", "do: NEVER use sqlite", 1),
		"dont":  strings.Replace(base, "dont: use cgo", "dont: use raw C", 1),
		"why":   strings.Replace(base, "why: static binary", "why: reproducible builds", 1),
		"tags":  strings.Replace(base, "tags: [a, b]", "tags: [a, c]", 1),
	}
	for field, src := range edits {
		if retrievalHash(parse(src)) == h0 {
			t.Errorf("editing %q did not change retrievalHash; its vector would stay stale", field)
		}
	}
}

func TestChunkText(t *testing.T) {
	src := []byte(`---
id: chunk-demo
type: gotcha
title: Modernc cannot load C extensions
do: prefer pure-Go drivers
why: cgo breaks the static single-binary promise
tags: [sqlite, cgo]
---

# Modernc cannot load C extensions

## Symptom
<!-- internal note -->
The load_extension call returns an error at runtime.

## Cause
modernc.org/sqlite is a pure-Go transpile with no dlopen.

## Fix
Compile the extension logic in Go instead.
`)
	pn, err := Parse("vault/gotchas/chunk-demo.md", src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	chunks := ChunkText(pn)

	// Header chunk carries the flywheel + tags so the institutional memory is
	// embeddable even though it lives in frontmatter, not the body.
	if len(chunks) < 2 {
		t.Fatalf("want a header chunk plus section chunks, got %d", len(chunks))
	}
	header := chunks[0]
	for _, want := range []string{"Modernc cannot load C extensions", "prefer pure-Go drivers", "cgo breaks", "sqlite"} {
		if !strings.Contains(header, want) {
			t.Errorf("header chunk missing %q\ngot: %s", want, header)
		}
	}

	// Each body section becomes its own chunk, every one prefixed with the title
	// so an isolated section is still self-describing to the embedder.
	joined := strings.Join(chunks[1:], "\n===\n")
	for _, want := range []string{"Symptom", "Cause", "Fix", "dlopen"} {
		if !strings.Contains(joined, want) {
			t.Errorf("section chunks missing %q", want)
		}
	}
	for i, c := range chunks[1:] {
		if !strings.Contains(c, "Modernc cannot load C extensions") {
			t.Errorf("section chunk %d missing title context: %s", i, c)
		}
	}

	// HTML comments are stripped from chunks (no index noise).
	if strings.Contains(joined, "internal note") {
		t.Error("html comment leaked into a chunk")
	}
}
