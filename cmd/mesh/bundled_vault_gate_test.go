// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/retrieve"
	"github.com/bright-interaction/mesh/internal/vault"
)

// The bundled sample vault (vault/, 14 committed notes) is a PUBLISHED artifact: the
// mirror ships it, and it is the first thing a new user or a demo points Mesh at. That
// makes it the one vault in the world that has to be zero-debt, and the gates below hold
// it to that standard.
//
// This does NOT change `mesh lint` policy, and must not. For a user's vault a broken
// link is a NOTICE, not an error, on purpose: lint once counted authoring debt as
// defects, reported 1118 "problems" on a vault with zero broken retrieval, and the
// number got ignored. That policy stays. What changes here is the standard applied to
// OUR OWN sample, which nobody is mid-thought on and which has no excuse for a dangling
// link. Everything lint would call a notice is a failure here, and nowhere else.
//
// The failures these gates were written against, all live on 2026-08-26:
//   - vault/.mesh/mesh.db was stamped index schema v1 against a binary expecting v6, so
//     every read-only surface answered "status: BROKEN" on a vault that was fine. That
//     is why nothing here reads the committed .mesh directory: the gate builds its own.
//   - a bare [[wikilinks]] written as a SYNTAX EXAMPLE in a note's do: field parsed as a
//     real link (frontmatter prose links became edges) and resolved to nothing. Write
//     such examples in backticks: `[[wikilinks]]`.
//   - an unterminated code fence hid a dangling link from BuildGraph AND from `mesh
//     lint`, which reported "14 files, 0 errors, 0 notices" over a vault that had an
//     unresolvable link in it. See fenceLeftOpen.

// bundledVaultNoteFloor is the smallest note count that still means "the sample vault is
// really here". Without a floor every gate below passes vacuously the day vault/ is
// moved, emptied or excluded from the walk: zero notes parse cleanly, and zero notes
// produce zero graph issues. 14 notes are committed today. This is deliberately a FLOOR
// and not an exact pin: adding a sample note is a legitimate act that should not red CI
// with a confusing count mismatch, while an emptied or relocated vault still fails loudly.
const bundledVaultNoteFloor = 10

// distinctiveVaultNoteID is a note whose subject appears nowhere else in the sample
// vault, so a search for its vocabulary that does NOT return it means retrieval is
// broken rather than merely ranked differently. The retrieval test asserts this note
// EXISTS before asserting it is findable, so renaming the fixture reports a fixture
// change instead of blaming retrieval.
const distinctiveVaultNoteID = "modernc-cannot-load-c-extensions"

// copyBundledVault copies the committed sample vault's markdown into a fresh temp dir,
// preserving the folder layout (basename collisions across folders are themselves a
// graph issue, so the layout is part of what is under test).
//
// It copies through vault.Walk, which is the same view the indexer takes: markdown only,
// dot-directories skipped. So vault/.mesh never travels, and no gate here can pass or
// fail because of a stale committed index. That is the point - the index is derived
// state that git does not carry, and the vault must be provably clean without it.
func copyBundledVault(t *testing.T) string {
	t.Helper()

	src := filepath.Join(moduleRoot(t), "vault")
	info, err := os.Stat(src)
	if err != nil || !info.IsDir() {
		t.Fatalf("the bundled sample vault is missing at %s (%v).\n"+
			"It is a committed, published asset. If it moved, move this gate with it; "+
			"do not let the gate go quiet.", src, err)
	}

	files, err := vault.Walk(src)
	if err != nil {
		t.Fatalf("walk %s: %v", src, err)
	}
	if len(files) < bundledVaultNoteFloor {
		t.Fatalf("the bundled vault holds %d notes, below the floor of %d.\n"+
			"Either the vault lost its notes or this gate is walking the wrong directory. "+
			"Both make every assertion below vacuous, so this is a failure, not a skip.",
			len(files), bundledVaultNoteFloor)
	}

	dst := t.TempDir()
	for _, f := range files {
		rel, err := filepath.Rel(src, f)
		if err != nil {
			t.Fatalf("relativize %s against %s: %v", f, src, err)
		}
		out := filepath.Join(dst, rel)
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", out, err)
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if err := os.WriteFile(out, b, 0o600); err != nil {
			t.Fatalf("write %s: %v", out, err)
		}
	}
	return dst
}

// fenceLeftOpen reports whether a note body ends inside an unterminated code fence.
//
// It does NOT re-implement CommonMark fence grammar - fence matching is char-, run-length-
// and info-string-sensitive (internal/vault/markup.go stripLines), and a second copy of
// those rules would drift from the real one. Instead it asks the parser's own scanner:
// append a sentinel line and strip with the same vault.StripNonContent the parser uses.
// If the body ends inside an open fence the sentinel is swallowed as fence content; if
// the body is balanced the sentinel survives as ordinary text.
//
// Why the bundled vault needs this at all: an unterminated fence hides everything below
// it from the graph AND from search, with no diagnostic anywhere. StripNonContent reports
// an unterminated COMMENT (the parser turns that into an unterminated-comment issue) but
// there is no fence counterpart. Reproduced 2026-08-26: with an unclosed ``` above a
// [[this-note-does-not-exist]], BuildGraph reported nothing and `mesh lint ./vault` said
// "14 files, 0 errors, 0 notices" over a vault holding an unresolvable link. Without this
// check the zero-debt gate passes vacuously in exactly that case.
func fenceLeftOpen(body string) bool {
	clean, openComment := vault.StripNonContent(body + "\n" + fenceProbeSentinel + "\n")
	if openComment > 0 {
		// An unterminated COMMENT swallows the sentinel too, but the parser already
		// raises unterminated-comment for that, so it is not this check's to report.
		return false
	}
	// StripNonContent preserves line structure byte-for-byte, so the sentinel sits at a
	// known line index. Check THAT line rather than searching the whole body: a note that
	// merely mentions the sentinel word would otherwise mask a real open fence below it,
	// which is a guard that can be defeated by its own subject matter.
	at := len(strings.Split(body, "\n"))
	lines := strings.Split(clean, "\n")
	if at >= len(lines) {
		return false
	}
	return strings.TrimSpace(lines[at]) != fenceProbeSentinel
}

const fenceProbeSentinel = "meshbundledvaultfenceprobe"

// TestFenceLeftOpenAcrossMarkupShapes ablates fenceLeftOpen itself. It is the newest idea
// in this file and the only check here with no counterpart anywhere else in Mesh, so it
// gets its own table: a false NEGATIVE lets the zero-debt gate pass over a hidden dangling
// link, and a false POSITIVE reds CI on an innocent note.
//
// The sentinel-collision case is why the check reads one line instead of searching the
// body - with strings.Contains it returned false on a genuinely open fence.
func TestFenceLeftOpenAcrossMarkupShapes(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"clean prose", "# Title\n\nsome prose\n", false},
		{"balanced fence", "# T\n\n```go\ncode\n```\n\nafter\n", false},
		{"unterminated backtick fence", "# T\n\n```\ncode\n", true},
		{"unterminated tilde fence", "# T\n\n~~~\ncode\n", true},
		{"balanced tilde fence", "# T\n\n~~~\ncode\n~~~\n", false},
		// The EVEN nesting below survives a naive "any run of 3 toggles" implementation, so
		// it defends nothing on its own. The ODD one is the killer: outer opened, inner is
		// content, outer closed. Under naive toggling the state ends open and this returns
		// true against want false, which is the historic bug markup.go:110-116 describes.
		{"nested fences, inner is content, outer closed", "# T\n\n````markdown\n```ts\nx\n````\n\nafter\n", false},
		{"nested fences, outer left open", "# T\n\n````markdown\n```ts\nx\n```\n", true},
		{"indented fence, balanced", "# T\n\n   ```\ncode\n   ```\n\nafter\n", false},
		{"indented fence, unterminated", "# T\n\n   ```\ncode\n", true},
		{"empty body", "", false},
		{"inline code span", "a `b` c\n", false},
		// Named for what it actually asserts: an unpaired backtick must not be read as a
		// fence opener. It cannot test span line-locality - this function only ever reads
		// one line, and the scanner's spans never cross lines.
		{"unpaired backtick does not open a fence", "a `b\n", false},
		{"CRLF balanced", "# T\r\n\r\n```\r\ncode\r\n```\r\n", false},
		{"CRLF unterminated", "# T\r\n\r\n```\r\ncode\r\n", true},
		{"unterminated comment belongs to the comment check", "# T\n\n<!-- open\n", false},
		{"body mentioning the sentinel word", "# T\n\n" + fenceProbeSentinel + " appears here\n", false},
		{"sentinel word above a genuinely open fence", "# T\n\n" + fenceProbeSentinel + "\n\n```\ncode\n", true},
	}
	for _, tc := range cases {
		if got := fenceLeftOpen(tc.body); got != tc.want {
			t.Errorf("fenceLeftOpen(%q) = %v, want %v [%s]", tc.body, got, tc.want, tc.name)
		}
	}
}

// bundledVaultDebt parses every note in a copied vault and returns everything wrong with
// it: parse failures, and every graph/frontmatter/filename/fence problem folded into one
// Issue list so a single reporting loop covers them all.
//
// The frontmatter and filename checks mirror `mesh lint` (cmd/mesh/main.go, the
// pn.FM.Validate() and isKebab loop) because BuildGraph does not model them: an
// `invalid type` passes the graph cleanly, silently drops the note out of tier-0
// (retrieve.tier0Types), and CI has no `mesh lint` step to catch it. The difference from
// lint is severity, not detection - lint splits these into errors and notices, and here
// every one of them fails.
func bundledVaultDebt(t *testing.T, root string) ([]index.FileError, []index.Issue) {
	t.Helper()

	files, err := vault.Walk(root)
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	notes, ferrs := index.ParseFiles(files, 0)
	for _, pn := range notes {
		if rel, err := filepath.Rel(root, pn.Path); err == nil {
			pn.Path = rel
		}
	}
	_, issues := index.BuildGraph(notes)

	// Parse stamps its own per-file Issues (unterminated-comment) with the ABSOLUTE path,
	// before the relativization above can reach them, so those arrive looking nothing like
	// the graph-level ones. A gate whose entire value is naming the committed note to fix
	// must not print a temp directory that stopped existing before a human read CI.
	for i := range issues {
		if rel, err := filepath.Rel(root, issues[i].Path); err == nil && !strings.HasPrefix(rel, "..") {
			issues[i].Path = rel
		}
	}

	for _, pn := range notes {
		for _, e := range pn.FM.Validate() {
			if e == "missing id" {
				continue // BuildGraph already reports this one, as missing-id
			}
			issues = append(issues, index.Issue{Path: pn.Path, Kind: "frontmatter", Msg: e})
		}
		if base := filepath.Base(pn.Path); !isKebab(base) && !isConventionalDoc(base) {
			issues = append(issues, index.Issue{Path: pn.Path, Kind: "filename", Msg: "filename is not kebab-case"})
		}
		// The body is only half the surface. BuildGraph extracts wikilinks from do/dont/why
		// through the same vault.StripNonContent (internal/index/parse_md.go), so an
		// unterminated fence inside one of those YAML scalars hides a dangling link exactly
		// the way a body fence does - and do: is precisely where this vault's original
		// defect lived. Verified: a `do: |` block carrying an unclosed fence above a
		// [[dangling-link]] produced 0 links, 0 issues and a clean bill of health.
		for _, f := range []struct{ where, text string }{
			{"body", pn.Body}, {"do:", pn.FM.Do}, {"dont:", pn.FM.Dont}, {"why:", pn.FM.Why},
		} {
			if fenceLeftOpen(f.text) {
				issues = append(issues, index.Issue{Path: pn.Path, Kind: "unterminated-fence",
					Msg: "a code fence is opened and never closed in the " + f.where +
						", so everything below it is hidden from the graph and from search " +
						"- and no linter reports it; close the fence"})
			}
		}
	}
	return ferrs, issues
}

// TestBundledVaultIsZeroDebt is the gate: every committed sample note parses, and nothing
// about the vault is wrong - no broken link, no duplicate id, no missing id, no ambiguous
// link key, no invalid frontmatter, no unterminated fence. Notices included, which is
// exactly where this is stricter than `mesh lint`'s exit code and deliberately so.
func TestBundledVaultIsZeroDebt(t *testing.T) {
	root := copyBundledVault(t)
	ferrs, issues := bundledVaultDebt(t, root)

	for _, fe := range ferrs {
		rel, err := filepath.Rel(root, fe.Path)
		if err != nil {
			rel = fe.Path
		}
		t.Errorf("bundled vault note does not parse: %s: %v", rel, fe.Err)
	}
	for _, is := range issues {
		t.Errorf("bundled vault debt [%s] %s: %s", is.Kind, is.Path, is.Msg)
	}

	if len(ferrs) > 0 || len(issues) > 0 {
		t.Fatalf("the bundled sample vault carries %d parse error(s) and %d problem(s); "+
			"it ships to users and must carry zero.\n"+
			"`mesh lint ./vault` names most of them (it is the same detection, with softer "+
			"severity), but NOT an unterminated fence - this gate is the only thing that "+
			"catches that one.\n"+
			"A [[link]] that resolves to nothing is either a note that should exist, or a "+
			"syntax example that belongs in backticks: `[[like-this]]`.",
			len(ferrs), len(issues))
	}
}

// TestBundledVaultGateCatchesInjectedDebt ablates the gate above. Each case plants one
// specific defect in a COPY of the vault and requires the gate to notice it. Without
// this, TestBundledVaultIsZeroDebt is a test that cannot fail, and a green run over a
// rotting vault is worse than no gate at all.
//
// The fence case is the one that matters most: it is the only defect here that nothing
// else in Mesh detects, and it is how the gate passed vacuously before this test existed.
func TestBundledVaultGateCatchesInjectedDebt(t *testing.T) {
	cases := []struct {
		name   string
		needle string                   // pick the first victim containing this ("" = first file)
		mutate func(orig string) string // content mutation; nil when renaming instead
		rename string                   // new basename, for the filename case

		wantKind string // Issue.Kind expected; empty means expect a PARSE failure instead
		wantMsg  string // substring the issue message must carry; optional

		// forbidKind/forbidMsg assert an issue that must NOT appear. This is how a case
		// proves the mechanism it is named for, rather than passing on a side effect.
		forbidKind string
		forbidMsg  string
	}{
		{
			name:     "unresolved wikilink",
			mutate:   func(o string) string { return o + "\nSee [[missing-note]] for the rest.\n" },
			wantKind: "broken-link",
			wantMsg:  "missing-note",
		},
		{
			// The forbid is the point: this case must fail because the fence HID the link,
			// not merely because a link was dangling. If StripNonContent ever stopped
			// blanking fenced content, broken-link would fire and the premise in this
			// file's header would be silently false while the case stayed green.
			name:       "unterminated fence in the body hiding a dangling link",
			mutate:     func(o string) string { return o + "\n```\nSee [[also-missing]].\n" },
			wantKind:   "unterminated-fence",
			wantMsg:    "body",
			forbidKind: "broken-link",
			forbidMsg:  "also-missing",
		},
		{
			// The other half of the surface. BuildGraph reads links out of do/dont/why, so
			// a fence in a block scalar hides them exactly like a body fence - verified:
			// before the fix this produced 0 links, 0 issues and a clean bill of health.
			name:   "unterminated fence in a do: block scalar",
			needle: "\ntype: decision\n",
			mutate: func(o string) string {
				return strings.Replace(o, "\ndo: ",
					"\ndo: |\n  step one\n  ```\n  See [[hidden-in-do]].\nignored_do: ", 1)
			},
			wantKind:   "unterminated-fence",
			wantMsg:    "do:",
			forbidKind: "broken-link",
			forbidMsg:  "hidden-in-do",
		},
		{
			name:     "invalid frontmatter type",
			needle:   "\ntype: decision\n",
			mutate:   func(o string) string { return strings.Replace(o, "\ntype: decision\n", "\ntype: decisions\n", 1) },
			wantKind: "frontmatter",
			wantMsg:  "invalid type",
		},
		{
			// Renaming, not editing: isKebab was the one check in bundledVaultDebt that
			// could be deleted outright with every test still green.
			name:     "filename that is not kebab-case",
			rename:   "Not_Kebab_Case.md",
			wantKind: "filename",
			wantMsg:  "kebab",
		},
		{
			// YAML that is genuinely malformed, INSIDE a properly closed block. The earlier
			// fixture left the block unterminated, so Parse bailed before ParseFrontmatter
			// ran and the YAML was never parsed at all: the whole yaml-error branch could be
			// deleted with this case still green. Both branches now get their own case.
			name:   "frontmatter YAML that does not parse",
			mutate: func(string) string { return "---\nid: [unclosed\ntitle: 'broken\n---\n\n# x\n" },
		},
		{
			name:   "frontmatter block that is never closed",
			mutate: func(string) string { return "---\nid: fine\ntype: decision\ntitle: T\n" },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := copyBundledVault(t)

			// The copy must start clean, so anything found after the edit is the edit's doing.
			if ferrs, issues := bundledVaultDebt(t, root); len(ferrs) != 0 || len(issues) != 0 {
				t.Fatalf("precondition: the copied vault should start clean, got %d parse error(s) and %d issue(s): %v",
					len(ferrs), len(issues), issues)
			}

			files, err := vault.Walk(root)
			if err != nil {
				t.Fatalf("walk %s: %v", root, err)
			}
			sort.Strings(files) // deterministic victim, independent of walk order

			// Pick a victim that can actually carry the defect. Taking files[0] blindly made
			// the frontmatter case report "the mutation did nothing" - blaming the fixture -
			// the moment a note sorting first did not happen to be a decision.
			victim := files[0]
			if tc.needle != "" {
				victim = ""
				for _, f := range files {
					b, rerr := os.ReadFile(f)
					if rerr != nil {
						t.Fatalf("read %s: %v", f, rerr)
					}
					if strings.Contains(string(b), tc.needle) {
						victim = f
						break
					}
				}
				if victim == "" {
					t.Fatalf("no bundled note contains %q, so this case cannot run and is not "+
						"silently passing", tc.needle)
				}
			}

			if tc.rename != "" {
				renamed := filepath.Join(filepath.Dir(victim), tc.rename)
				if err := os.Rename(victim, renamed); err != nil {
					t.Fatalf("rename %s: %v", victim, err)
				}
				victim = renamed
			} else {
				orig, rerr := os.ReadFile(victim)
				if rerr != nil {
					t.Fatalf("read %s: %v", victim, rerr)
				}
				mutated := tc.mutate(string(orig))
				if mutated == string(orig) {
					t.Fatalf("the mutation did not change %s, so this case proves nothing "+
						"(the note's shape probably changed under the fixture)", filepath.Base(victim))
				}
				if err := os.WriteFile(victim, []byte(mutated), 0o600); err != nil {
					t.Fatalf("write %s: %v", victim, err)
				}
			}

			wantPath, err := filepath.Rel(root, victim)
			if err != nil {
				t.Fatalf("relativize victim: %v", err)
			}

			ferrs, issues := bundledVaultDebt(t, root)

			if tc.wantKind == "" {
				// Assert WHICH file failed. Without it, a collateral failure elsewhere in the
				// vault satisfies the case and the named branch can rot untested.
				for _, fe := range ferrs {
					if strings.HasSuffix(filepath.ToSlash(fe.Path), filepath.ToSlash(wantPath)) {
						return
					}
				}
				t.Fatalf("planted unparseable frontmatter in %s and the gate reported no parse error "+
					"for THAT file (parse errors seen: %v); the len(ferrs) branch is not ablated",
					wantPath, ferrs)
			}

			// Path binding matters: without it, three separate misattribution mutants -
			// every issue reported against notes[0] - survive the whole table, and the day
			// the vault really breaks CI names a note that is fine.
			for _, is := range issues {
				if is.Kind == tc.forbidKind && tc.forbidKind != "" &&
					(tc.forbidMsg == "" || strings.Contains(is.Msg, tc.forbidMsg)) {
					t.Fatalf("case %q passed for the WRONG REASON: it reported a %q issue (%s), which "+
						"means the defect was detected by a different mechanism than the one this "+
						"case exists to prove", tc.name, is.Kind, is.Msg)
				}
			}
			for _, is := range issues {
				if is.Kind == tc.wantKind &&
					(tc.wantMsg == "" || strings.Contains(is.Msg, tc.wantMsg)) &&
					filepath.ToSlash(is.Path) == filepath.ToSlash(wantPath) {
					return
				}
			}
			t.Fatalf("the zero-debt gate is vacuous for %q: the defect was planted in %s and the gate "+
				"reported no %q issue attributed to that path. Issues seen: %v\n"+
				"Fix the gate before trusting it - a guard that cannot fail proves nothing.",
				tc.name, wantPath, tc.wantKind, issues)
		})
	}
}

// TestBundledVaultCleanIndexAnswersSearch is the retrieval half. Parsing clean is not the
// same as being retrievable: the sample vault's job is to answer a query the moment
// someone points Mesh at it, from an index built here and now.
func TestBundledVaultCleanIndexAnswersSearch(t *testing.T) {
	root := copyBundledVault(t)

	// index.Open creates .mesh under the temp root, so this is a cold index at this
	// binary's schema version - never the committed one, whatever state it is in.
	st, err := index.Open(root)
	if err != nil {
		t.Fatalf("open a fresh index over the bundled vault: %v", err)
	}
	defer st.Close()

	_, notes, err := index.ReindexFull(st, root)
	if err != nil {
		t.Fatalf("index the bundled vault from cold: %v", err)
	}
	if len(notes) < bundledVaultNoteFloor {
		t.Fatalf("a cold index of the bundled vault holds %d notes, below the floor of %d", len(notes), bundledVaultNoteFloor)
	}

	g, err := st.LoadGraph()
	if err != nil {
		t.Fatalf("load the graph back out of the fresh index: %v", err)
	}
	// retrieve.New, not NewFromEnv: no reranker, no embeddings, no environment. The gate
	// measures the core lexical+graph path that every install has.
	r := retrieve.New(st, g)

	t.Run("representative query returns well-formed ranked cards", func(t *testing.T) {
		// The sample vault's actual subject matter: retrieval design decisions.
		const query = "rerank embeddings fusion retrieval index"
		cards, err := r.Retrieve(context.Background(), query, retrieve.Options{Limit: 10, NoRerank: true})
		if err != nil {
			t.Fatalf("search a freshly indexed bundled vault: %v", err)
		}
		if len(cards) == 0 {
			t.Fatalf("query %q returned no cards from a clean index of %d notes; retrieval is dead on the shipped sample", query, len(notes))
		}
		// Card well-formedness and rank order are both satisfied by the graph-BM25 arm
		// ALONE, so without this the subtest passes with the SQLite FTS half returning
		// nothing at all. A populated snippet is the measured discriminator: normally the
		// top card carries one, and with the FTS arm stubbed out every snippet goes empty.
		// Only the top card - a 1-hop expansion card legitimately has no snippet.
		if cards[0].Snippet == "" {
			t.Errorf("top card (%s) has an empty snippet: the FTS arm contributed nothing, "+
				"so this test is passing on the graph arm alone and does not prove a clean "+
				"index answers a search end-to-end", cards[0].NoteID)
		}
		for i, c := range cards {
			if c.NoteID == "" || c.Path == "" {
				t.Errorf("card %d is unusable: NoteID=%q Path=%q", i, c.NoteID, c.Path)
				continue
			}
			if _, err := os.Stat(filepath.Join(root, c.Path)); err != nil {
				t.Errorf("card %d (%s) points at %s, which is not a file in the vault: %v", i, c.NoteID, c.Path, err)
			}
		}
		// Ranked means non-increasing score (retrieve.sortCards). Checked in its own pass
		// so a malformed card above cannot skip the comparison at its index.
		for i := 1; i < len(cards); i++ {
			if cards[i].Score > cards[i-1].Score {
				t.Errorf("cards are not ranked: card %d scores %v above card %d at %v",
					i, cards[i].Score, i-1, cards[i-1].Score)
			}
		}
	})

	t.Run("a distinctive term finds its note", func(t *testing.T) {
		// Assert the FIXTURE first. Links resolve by lowercased basename, so renaming only
		// the frontmatter id leaves lint and the graph clean - and without this check the
		// gate would blame retrieval for a fixture edit.
		present := false
		for _, pn := range notes {
			if pn.FM != nil && pn.FM.ID == distinctiveVaultNoteID {
				present = true
				break
			}
		}
		if !present {
			t.Fatalf("fixture changed: no note in the bundled vault has id %q any more. "+
				"This is a fixture problem, not a retrieval problem - point the constant at "+
				"another note whose vocabulary is unique in the vault.", distinctiveVaultNoteID)
		}

		// Deliberately NOT an assertion about rank 1: fusion weights get tuned, and a gate
		// that pins the winner would flap on every tuning change. Presence in the result
		// set is the honest claim - this vocabulary belongs to exactly one note.
		const query = "modernc sqlite C extension"
		cards, err := r.Retrieve(context.Background(), query, retrieve.Options{Limit: 10, NoRerank: true})
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		for _, c := range cards {
			if c.NoteID == distinctiveVaultNoteID {
				return
			}
		}
		var got []string
		for _, c := range cards {
			got = append(got, c.NoteID)
		}
		t.Fatalf("query %q did not return %s anywhere in %d cards (got %v); "+
			"that note is the only one in the vault about this, so retrieval is not matching, "+
			"not merely ranking differently", query, distinctiveVaultNoteID, len(cards), got)
	})
}
