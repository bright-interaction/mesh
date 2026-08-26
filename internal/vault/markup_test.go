// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"slices"
	"strings"
	"testing"
)

// TestStripNonContent pins the scanner itself. Blanking, not deleting: every case also
// asserts the length is unchanged, because line numbers and columns in the graph are
// taken from the stripped text.
func TestStripNonContent(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		want     string
		wantOpen int
	}{
		{
			name: "a body with nothing to strip is returned as is",
			body: "plain [[a]] text\n",
			want: "plain [[a]] text\n",
		},
		{
			name: "a whole line comment blanks to spaces",
			body: "<!-- x -->\n",
			want: "          \n",
		},
		{
			name: "only the commented span is blanked",
			body: "a <!-- b --> c\n",
			want: "a            c\n",
		},
		{
			name: "a comment spanning lines blanks all of them",
			body: "a <!-- b\nc\n--> d\n",
			want: "a       \n \n    d\n",
		},
		{
			name: "a comment can close and reopen on one line",
			body: "<!-- a --> b <!-- c -->\n",
			want: "           b           \n",
		},
		{
			name:     "a line-start comment that never closes hides the rest of the file",
			body:     "a\n<!-- b\nc\n",
			want:     "a\n      \n \n",
			wantOpen: 2,
		},
		{
			name: "a mid-line comment that never closes is literal text",
			body: "use the <!-- marker\nc\n",
			want: "use the <!-- marker\nc\n",
		},
		{
			name: "abrupt closing empty comments close",
			body: "<!--> a\n<!---> b\n<!----> c\n",
			want: "      a\n       b\n        c\n",
		},
		{
			name: "an unpaired backtick does not blank to end of line",
			body: "the ` char, then [[a]]\n",
			want: "the ` char, then [[a]]\n",
		},
		{
			name: "a paired code span is blanked",
			body: "run `mesh index` now\n",
			want: "run              now\n",
		},
		{
			name: "a double backtick span is one code span",
			body: "see ``[[hidden]]`` then [[visible]]\n",
			want: "see                then [[visible]]\n",
		},
		{
			name: "an unpaired backtick inside a comment cannot erase the close",
			body: "<!-- prefer the ` char -->\n[[a]]\n",
			want: "                          \n[[a]]\n",
		},
		{
			name: "a backtick before an opener does not hide the opener",
			body: "see ` here <!--\nb\n-->\n[[a]]\n",
			want: "see ` here     \n \n   \n[[a]]\n",
		},
		{
			name: "a comment marker inside a code span is inert",
			body: "an `<!--` in prose\n[[a]]\n",
			want: "an        in prose\n[[a]]\n",
		},
		{
			name: "a fence is blanked and its markers are inert",
			body: "```\n<!-- [[a]]\n```\n[[b]]\n",
			want: "   \n          \n   \n[[b]]\n",
		},
		{
			name: "four-space indented backticks do not swallow later prose",
			body: "    ```\n[[visible]]\n    ```\n[[also-visible]]\n",
			want: "       \n[[visible]]\n       \n[[also-visible]]\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, open := StripNonContent(tc.body)
			if got != tc.want {
				t.Errorf("StripNonContent(%q)\n = %q\nwant %q", tc.body, got, tc.want)
			}
			if open != tc.wantOpen {
				t.Errorf("StripNonContent(%q) open = %d, want %d", tc.body, open, tc.wantOpen)
			}
			if len(got) != len(tc.body) {
				t.Errorf("length changed: %d, want %d", len(got), len(tc.body))
			}
		})
	}
}

// TestStripNonContentUnterminatedOpeners exercises the rescan: proving a mid-line
// marker literal happens only at the end of the file, and doing it can expose a later
// marker that was hidden behind the first one.
func TestStripNonContentUnterminatedOpeners(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		want     string
		wantOpen int
	}{
		{
			name: "two mid-line openers, both literal",
			body: "a <!-- one\nb <!-- two\n[[link]]\n",
			want: "a <!-- one\nb <!-- two\n[[link]]\n",
		},
		{
			name: "a mid-line opener closed on a later line still hides",
			body: "x <!-- a\n<!-- b -->\nc [[link]]\n",
			want: "x       \n          \nc [[link]]\n",
		},
		{
			name: "a literal opener does not eat the fence below it",
			body: "x <!-- oops\n```\ncode [[fenced]]\n```\n[[link]]\n",
			want: "x <!-- oops\n   \n               \n   \n[[link]]\n",
		},
		{
			name:     "an indented opener still starts a block",
			body:     "  <!-- never closed\n[[hidden]]\n",
			want:     "                   \n          \n",
			wantOpen: 1,
		},
		{
			name:     "a body that is nothing but an opener",
			body:     "<!--",
			want:     "    ",
			wantOpen: 1,
		},
		{
			name: "an empty body",
			body: "",
			want: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, open := StripNonContent(tc.body)
			if got != tc.want {
				t.Errorf("StripNonContent(%q)\n = %q\nwant %q", tc.body, got, tc.want)
			}
			if open != tc.wantOpen {
				t.Errorf("StripNonContent(%q) open = %d, want %d", tc.body, open, tc.wantOpen)
			}
			if len(got) != len(tc.body) {
				t.Errorf("length changed: %d, want %d", len(got), len(tc.body))
			}
		})
	}
}

// TestStripCommentsKeepsCode: search and embeddings index code, so only comments come
// out there. The markers are still read by the same state machine, which is the point:
// the two strippers can never disagree about where a comment starts.
func TestStripCommentsKeepsCode(t *testing.T) {
	body := "run `mesh index` <!-- hidden -->\n```\nfenced [[x]]\n```\n"
	want := "run `mesh index`                \n```\nfenced [[x]]\n```\n"
	got, open := StripComments(body)
	if got != want {
		t.Errorf("StripComments\n = %q\nwant %q", got, want)
	}
	if open != 0 {
		t.Errorf("open = %d, want 0", open)
	}
	// The unterminated case is the one a `(?s)<!--.*?-->` regex cannot see at all.
	got, open = StripComments("head\n<!-- secretword\ntail\n")
	if strings.Contains(got, "secretword") || strings.Contains(got, "tail") {
		t.Errorf("StripComments = %q, kept text the parser treats as hidden", got)
	}
	if open != 2 {
		t.Errorf("open = %d, want 2", open)
	}
}

func TestStripCodeSpans(t *testing.T) {
	tests := []struct{ line, want string }{
		{"no code here", "no code here"},
		{"a `b` c", "a     c"},
		{"a ``[[hidden]]`` c", "a                c"},
		{"a ``one ` tick`` c", "a                c"},
		{"a ` b c", "a ` b c"},
		{"`x` and `y`", "    and    "},
		{"## `code` heading", "##        heading"},
	}
	for _, tc := range tests {
		if got := StripCodeSpans(tc.line); got != tc.want {
			t.Errorf("StripCodeSpans(%q) = %q, want %q", tc.line, got, tc.want)
		}
	}
}

func TestStripNonContentAdversarialMarkdownBlocks(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		hidden     string
		mustRemain string
	}{
		{
			name:       "multiline code span",
			body:       "before `code starts\n[[hidden-multiline]]\nand ends` after [[visible]]\n",
			hidden:     "[[hidden-multiline]]",
			mustRemain: "[[visible]]",
		},
		{
			name:       "blockquoted fence",
			body:       "> ```md\n> [[hidden-quote]]\n> ```\n[[visible]]\n",
			hidden:     "[[hidden-quote]]",
			mustRemain: "[[visible]]",
		},
		{
			name:       "indented code block",
			body:       "    [[hidden-indented]]\n\n[[visible]]\n",
			hidden:     "[[hidden-indented]]",
			mustRemain: "[[visible]]",
		},
		{
			name:       "nested list indentation is visible content",
			body:       "- parent\n    - [[visible-child]]\n",
			mustRemain: "[[visible-child]]",
		},
		{
			name:       "list continuation indentation is visible content",
			body:       "- parent\n    continued [[visible-child]]\n",
			mustRemain: "[[visible-child]]",
		},
		{
			name:       "backtick fence inside a bullet list",
			body:       "- ```md\n  [[hidden-list-fence]]\n  ```\n[[visible-after-list]]\n",
			hidden:     "[[hidden-list-fence]]",
			mustRemain: "[[visible-after-list]]",
		},
		{
			name:       "unterminated list fence ends with its container",
			body:       "- ```md\n  [[hidden-list-fence]]\n[[visible-after-list]]\n",
			hidden:     "[[hidden-list-fence]]",
			mustRemain: "[[visible-after-list]]",
		},
		{
			name:       "wide ordered-list indent can close its fence",
			body:       "10. ```md\n    [[hidden-wide-list-fence]]\n    ```\n    continuation [[visible-same-item]]\n",
			hidden:     "[[hidden-wide-list-fence]]",
			mustRemain: "[[visible-same-item]]",
		},
		{
			name:       "tilde fence inside an ordered list",
			body:       "1. ~~~md\n   [[hidden-ordered-fence]]\n   ~~~\n[[visible-after-ordered]]\n",
			hidden:     "[[hidden-ordered-fence]]",
			mustRemain: "[[visible-after-ordered]]",
		},
		{
			name:       "two blank lines end list continuation context",
			body:       "- parent\n\n\n    [[hidden-root-code]]\n[[visible-after-code]]\n",
			hidden:     "[[hidden-root-code]]",
			mustRemain: "[[visible-after-code]]",
		},
		{
			name:       "an unmatched code tick cannot span two list items",
			body:       "- `literal opener\n- [[visible-next-item]]`\n",
			mustRemain: "[[visible-next-item]]",
		},
		{
			name:       "escaped HTML comment opener is visible",
			body:       "\\<!-- literal [[visible]] -->\n",
			mustRemain: "[[visible]]",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := StripNonContent(tc.body)
			if tc.hidden != "" && strings.Contains(got, tc.hidden) {
				t.Errorf("hidden markup leaked through: %q", got)
			}
			if !strings.Contains(got, tc.mustRemain) {
				t.Errorf("visible content was hidden: %q", got)
			}
			if len(got) != len(tc.body) || strings.Count(got, "\n") != strings.Count(tc.body, "\n") {
				t.Errorf("layout changed: got %d bytes/%d lines, want %d/%d", len(got), strings.Count(got, "\n"), len(tc.body), strings.Count(tc.body, "\n"))
			}
		})
	}
}

// TestRelatedLinksReadsOnlyVisibleLinks: migrate lifts this section into frontmatter
// related:, which is authoritative and cannot be filtered later by the parser. Lifting a
// commented-out or backticked example manufactured a broken link out of the skeleton
// Mesh had just written, undoing the scaffold fix inside the same binary.
func TestRelatedLinksReadsOnlyVisibleLinks(t *testing.T) {
	tests := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "a real link is lifted",
			body: "## Related\n- [[real-note]]\n",
			want: []string{"real-note"},
		},
		{
			name: "see also is the same section",
			body: "## See also\n[[a]] and [[b]]\n",
			want: []string{"a", "b"},
		},
		{
			name: "a commented placeholder is not a link",
			body: "## Related\n<!-- link the decisions that shaped it: `[[note-id]]` -->\n",
		},
		{
			name: "a code span is an example, not a link",
			body: "## Related\nwrite `[[example]]` to link a note\n",
		},
		{
			name: "a fenced block is not a link",
			body: "## Related\n```\n[[fenced]]\n```\n",
		},
		{
			name: "a multi line comment hides the whole section",
			body: "## Related\n<!--\n[[hidden]]\n-->\n",
		},
		{
			name: "visible links next to hidden ones are still lifted",
			body: "## Related\n- [[real-note]] <!-- [[hidden]] -->\n",
			want: []string{"real-note"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := relatedLinks(tc.body); !slices.Equal(got, tc.want) {
				t.Errorf("relatedLinks = %v, want %v", got, tc.want)
			}
		})
	}
}

// FuzzStripMarkupPreservesLayout exercises arbitrary combinations of fences,
// comments, escapes, backtick runs, Unicode, and malformed bytes. All scanners
// deliberately blank bytes rather than delete them because graph source locations
// are measured against their output.
func FuzzStripMarkupPreservesLayout(f *testing.F) {
	for _, seed := range []string{
		"plain [[note]]\n",
		"<!-- open\nsecret\n",
		"> ```md\n> `code` <!-- comment -->\n> ```\n",
		"before ``code ` ticks`` after\n",
		"\\<!-- literal --> and \\\\`code`\n",
		"# Räksmörgås 東京\n",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, body string) {
		for name, strip := range map[string]func(string) (string, int){
			"non-content": StripNonContent,
			"comments":    StripComments,
		} {
			got, _ := strip(body)
			if len(got) != len(body) {
				t.Fatalf("%s changed byte length from %d to %d", name, len(body), len(got))
			}
			if strings.Count(got, "\n") != strings.Count(body, "\n") {
				t.Fatalf("%s changed newline count", name)
			}
		}
		// StripCodeSpans is explicitly a single-line helper; exercise each line
		// independently so a backtick on one line cannot treat a later line as its
		// closer (the document scanner above owns multiline spans).
		for _, line := range strings.Split(body, "\n") {
			if got := StripCodeSpans(line); len(got) != len(line) {
				t.Fatalf("code-span stripper changed line length")
			}
		}
	})
}
