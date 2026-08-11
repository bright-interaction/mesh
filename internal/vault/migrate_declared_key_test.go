// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// blankDeclarations are the ways a note can carry a key that declares nothing. YAML resolves
// every null spelling to nil and the quoted ones to a blank string, so "the key is present"
// and "the author said something" are different questions, and every writer here asked the
// wrong one on at least one key.
var blankDeclarations = []struct {
	name  string
	value string // what follows the colon, including its leading space
}{
	{"bare", ""},
	{"trailing space", "  "},
	{"tilde", " ~"},
	{"null", " null"},
	{"Null", " Null"},
	{"NULL", " NULL"},
	{"empty string", ` ""`},
	{"blank string", ` "   "`},
}

const relatedBody = "# Alpha\n\nbody\n\n## Related\n- [[beta]]\n- [[gamma]]\n"

func writeNoteFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "alpha-note.md")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// topLevelFrontmatterKeys lists a note's top-level frontmatter keys IN ORDER, duplicates
// included. It reads the block as a yaml.Node rather than a map on purpose: a map silently
// answers "one" for a note that declares the same key twice, which is the exact shape these
// guards exist to catch.
func topLevelFrontmatterKeys(t *testing.T, note []byte) []string {
	t.Helper()
	fmText, _, had := SplitFrontmatter(string(note))
	if !had {
		t.Fatalf("note has no frontmatter block:\n%s", note)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(fmText), &doc); err != nil {
		t.Fatalf("frontmatter is not YAML: %v\n%s", err, fmText)
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		t.Fatalf("frontmatter is not a mapping:\n%s", fmText)
	}
	var keys []string
	for i := 0; i+1 < len(doc.Content[0].Content); i += 2 {
		keys = append(keys, doc.Content[0].Content[i].Value)
	}
	return keys
}

func countKey(keys []string, key string) int {
	n := 0
	for _, k := range keys {
		if k == key {
			n++
		}
	}
	return n
}

// carriesValue is the property every one of these writers has to honor, stated independently
// of how they implement it: the key is there AND it says something.
func carriesValue(raw map[string]any, key string) bool {
	v, ok := raw[key]
	if !ok || v == nil {
		return false
	}
	s, isString := v.(string)
	return !isString || strings.TrimSpace(s) != ""
}

// migrateAddableKeys asks MigrateFile itself which frontmatter keys it can add, by running it
// over a note that declares nothing at all. The guards below walk that list rather than a
// hand-written one, so a fifth key added to MigrateFile later is covered on the day it lands
// instead of on the day it bites: four keys got this wrong and a fifth was fixed alone.
func migrateAddableKeys(t *testing.T) []string {
	t.Helper()
	p := writeNoteFile(t, relatedBody)
	if _, err := MigrateFile(p, false); err != nil {
		t.Fatalf("migrating a note with no frontmatter at all: %v", err)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	keys := topLevelFrontmatterKeys(t, data)
	for _, want := range []string{"id", "type", "when", "related"} {
		if !slices.Contains(keys, want) {
			t.Fatalf("MigrateFile added %v, which does not include %q; the discovery fixture no longer exercises every key", keys, want)
		}
	}
	return keys
}

// TestMigrateFileKeysAreNilAware walks every key MigrateFile can add and asserts each one
// reads a blank declaration as "nothing declared".
//
// Presence was read as a value on all four, in two different wrong ways. `id` was the only
// key whose add block fired while the key was still present, and nothing removed the old
// line, so `id: ~` produced a DUPLICATE id and yaml.v3 refuses a duplicate mapping key: the
// note failed every `mesh migrate --apply` run forever with no way for an operator to fix it
// from the tool. `type`, `when` and `related` went the other way and skipped the note
// entirely, so `mesh migrate` reported clean while `mesh lint` reported [missing when] on the
// same file, permanently, with nothing to reconcile them.
func TestMigrateFileKeysAreNilAware(t *testing.T) {
	for _, key := range migrateAddableKeys(t) {
		for _, blank := range blankDeclarations {
			t.Run(key+"/"+blank.name, func(t *testing.T) {
				p := writeNoteFile(t, "---\ntitle: Alpha\n"+key+":"+blank.value+"\n---\n"+relatedBody)

				res, err := MigrateFile(p, false)
				if err != nil {
					t.Fatalf("MigrateFile refused a note whose %q declares nothing: %v", key, err)
				}
				if !res.Changed {
					t.Fatalf("MigrateFile reported no change for a note whose %q declares nothing; lint still calls that note broken and no tool reconciles them", key)
				}

				out, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				fmText, _, had := SplitFrontmatter(string(out))
				if !had {
					t.Fatalf("the migrated note lost its frontmatter block:\n%s", out)
				}
				_, raw, err := ParseFrontmatter([]byte(fmText))
				if err != nil {
					t.Fatalf("the migrated note no longer parses, so the index would drop it from search and the graph: %v\n%s", err, out)
				}
				if n := countKey(topLevelFrontmatterKeys(t, out), key); n != 1 {
					t.Errorf("frontmatter declares %q %d times, want exactly 1:\n%s", key, n, fmText)
				}
				if !carriesValue(raw, key) {
					t.Errorf("%q still declares nothing after a migration that claimed to set it:\n%s", key, fmText)
				}

				res2, err := MigrateFile(p, false)
				if err != nil {
					t.Fatalf("re-migrating: %v", err)
				}
				if res2.Changed {
					t.Errorf("second migrate must be a no-op, got %v", res2.Actions)
				}
			})
		}
	}
}

// TestBackfillsAreNilAware is the same property for the two backfills that own a key each.
// A blank `scope:` counted as labeled, so `mesh scope backfill --apply sales` silently
// no-op'd and the note stayed dev-only. A blank `related:` counted as declared for the null
// spellings, and the line-editing dropper disagreed with the nil test that decided to edit,
// so the rewrite produced two `related` keys and `--wire-orphans --apply` reported the note
// as a failure instead of wiring it.
func TestBackfillsAreNilAware(t *testing.T) {
	for _, blank := range blankDeclarations {
		t.Run("scope/"+blank.name, func(t *testing.T) {
			p := writeNoteFile(t, "---\nid: alpha\ntype: note\nwhen: \"2026-01-01\"\nscope:"+blank.value+"\n---\n# Alpha\n")

			res, err := BackfillScopeFile(p, "sales", false)
			if err != nil {
				t.Fatalf("BackfillScopeFile refused a note whose scope declares nothing: %v", err)
			}
			if !res.Changed {
				t.Fatalf("a blank scope: counted as already labeled, so the backfill no-op'd and the note stays dev-only")
			}
			out, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			fmText, _, _ := SplitFrontmatter(string(out))
			fm, _, err := ParseFrontmatter([]byte(fmText))
			if err != nil {
				t.Fatalf("the backfilled note no longer parses: %v\n%s", err, out)
			}
			if got := fm.EffectiveScopes(); !slices.Equal(got, []string{"sales"}) {
				t.Errorf("EffectiveScopes = %v, want [sales]:\n%s", got, fmText)
			}
			if n := countKey(topLevelFrontmatterKeys(t, out), "scope"); n != 1 {
				t.Errorf("frontmatter declares scope %d times, want exactly 1:\n%s", n, fmText)
			}
		})

		t.Run("related/"+blank.name, func(t *testing.T) {
			p := writeNoteFile(t, "---\nid: alpha\ntype: gotcha\nrelated:"+blank.value+"\ntags:\n    - mesh\n---\n# Alpha\n")

			res, err := BackfillRelatedFile(p, []string{"beta", "gamma"}, false)
			if err != nil {
				t.Fatalf("BackfillRelatedFile refused a note whose related declares nothing: %v", err)
			}
			if !res.Changed {
				t.Fatalf("a blank related: counted as the author's own link list, so the orphan was never wired")
			}
			out, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			fmText, _, _ := SplitFrontmatter(string(out))
			fm, _, err := ParseFrontmatter([]byte(fmText))
			if err != nil {
				t.Fatalf("the wired note no longer parses: %v\n%s", err, out)
			}
			if got := []string(fm.Related); !slices.Equal(got, []string{"beta", "gamma"}) {
				t.Errorf("related = %v, want [beta gamma]:\n%s", got, fmText)
			}
			keys := topLevelFrontmatterKeys(t, out)
			if n := countKey(keys, "related"); n != 1 {
				t.Errorf("frontmatter declares related %d times, want exactly 1:\n%s", n, fmText)
			}
			if !slices.Contains(keys, "tags") {
				t.Errorf("dropping the blank related also took the note's other keys with it:\n%s", fmText)
			}
		})
	}
}

// TestMigratedValuesAreEncodedNotFormatted covers the other half of hand-building YAML lines:
// the value side. A `## Related` wikilink was spliced into the block verbatim, so
// "[[Deploy: staging]]" became a nested mapping, "[[*star]]" an unknown alias, and "[[&amp]]"
// an anchor whose value silently vanished; a synthesized `when: "<value>"` closed early on any
// value carrying a quote. Each one made the whole block unparseable, and writeNoteChecked
// could only refuse the write: the note was reported as a migration failure every run.
func TestMigratedValuesAreEncodedNotFormatted(t *testing.T) {
	t.Run("lifted wikilinks survive whatever YAML makes of them", func(t *testing.T) {
		p := writeNoteFile(t, "# Alpha\n\n## Related\n- [[Deploy: staging]]\n- [[*star]]\n- [[&amp]]\n- [[plain-note]]\n")
		if _, err := MigrateFile(p, false); err != nil {
			t.Fatalf("lifting wikilinks into related: %v", err)
		}
		out, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		fmText, _, _ := SplitFrontmatter(string(out))
		fm, _, err := ParseFrontmatter([]byte(fmText))
		if err != nil {
			t.Fatalf("the migrated note no longer parses: %v\n%s", err, out)
		}
		want := []string{"Deploy: staging", "*star", "&amp", "plain-note"}
		if got := []string(fm.Related); !slices.Equal(got, want) {
			t.Errorf("related = %q, want %q:\n%s", got, want, fmText)
		}
	})

	t.Run("a when carrying a quote does not close the block", func(t *testing.T) {
		p := writeNoteFile(t, "---\ntype: entity\nupdated: 'he said \"ship it\" on friday'\n---\n# Alpha\nbody\n")
		if _, err := MigrateFile(p, false); err != nil {
			t.Fatalf("migrating a note whose updated carries a quote: %v", err)
		}
		out, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		fmText, _, _ := SplitFrontmatter(string(out))
		fm, _, err := ParseFrontmatter([]byte(fmText))
		if err != nil {
			t.Fatalf("the migrated note no longer parses: %v\n%s", err, out)
		}
		if fm.When != `he said "ship it" on friday` {
			t.Errorf("when = %q, want the updated value verbatim:\n%s", fm.When, fmText)
		}
	})

	t.Run("an operator scope that is not a plain scalar", func(t *testing.T) {
		p := writeNoteFile(t, "---\nid: alpha\ntype: note\nwhen: \"2026-01-01\"\n---\n# Alpha\n")
		if _, err := BackfillScopeFile(p, "[unclosed", false); err != nil {
			t.Fatalf("backfilling an awkward scope: %v", err)
		}
		out, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		fmText, _, _ := SplitFrontmatter(string(out))
		fm, _, err := ParseFrontmatter([]byte(fmText))
		if err != nil {
			t.Fatalf("the backfilled note no longer parses: %v\n%s", err, out)
		}
		if got := fm.EffectiveScopes(); !slices.Equal(got, []string{"[unclosed"}) {
			t.Errorf("EffectiveScopes = %v, want [[unclosed]:\n%s", got, fmText)
		}
	})
}

// A declared value is still the author's, and none of the nil-awareness above may touch it.
func TestDeclaredValuesSurviveTheBackfills(t *testing.T) {
	t.Run("an explicit empty list is a decision", func(t *testing.T) {
		p := writeNoteFile(t, "---\nid: alpha\ntype: gotcha\nrelated: []\n---\n# Alpha\n")
		res, err := BackfillRelatedFile(p, []string{"derived"}, false)
		if err != nil || res.Changed {
			t.Fatalf("`related: []` records that the links were considered and there are none; Changed=%v err=%v", res.Changed, err)
		}
	})

	t.Run("a labeled scope is left alone", func(t *testing.T) {
		p := writeNoteFile(t, "---\nid: alpha\ntype: note\nwhen: \"2026-01-01\"\nscope: sales\n---\n# Alpha\n")
		res, err := BackfillScopeFile(p, "dev", false)
		if err != nil || res.Changed {
			t.Fatalf("a labeled note must not be relabeled; Changed=%v err=%v", res.Changed, err)
		}
	})

	t.Run("declared migrate keys are not re-added", func(t *testing.T) {
		p := writeNoteFile(t, "---\nid: alpha\ntype: gotcha\nwhen: \"2026-01-01\"\nrelated:\n    - mine\n---\n"+relatedBody)
		res, err := MigrateFile(p, false)
		if err != nil || res.Changed {
			t.Fatalf("a fully declared note must be an idempotent no-op; Changed=%v actions=%v err=%v", res.Changed, res.Actions, err)
		}
	})
}
