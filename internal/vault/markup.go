// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import "strings"

const (
	commentOpen  = "<!--"
	commentClose = "-->"
)

// StripNonContent blanks every span of a markdown body that a reader never sees: fenced
// code blocks, inline code spans, and HTML comments. The result has the same byte
// length and the same line structure as the input (hidden bytes become spaces), so line
// numbers, columns and heading parsing are unaffected and the visible text on a line is
// still parsed normally.
//
// The second return value is the 1-based line of an HTML comment that opens and is
// never closed, or 0 when the body ends outside a comment. That case hides everything
// below the marker, so the caller should surface it rather than let a note quietly lose
// its tail.
//
// This is the single reader of markdown markup in Mesh. Everything that needs to know
// what a note actually says goes through it: the parser (links, tags, headings), the
// FTS body, the embedding chunks, and the related: lift in migrate. They used to strip
// with three different rules and disagreed about what a note contained.
func StripNonContent(body string) (string, int) { return stripMarkup(body, true) }

// StripComments blanks only the HTML comments, keeping code text intact. Search and
// embeddings want `mesh index --workers 4` to be findable, so code is content there
// even though it is not a place to read links from. Comment, fence and code-span
// boundaries are detected exactly as in StripNonContent, so both agree on which markers
// are real markup and neither can see a comment the other one does not.
func StripComments(body string) (string, int) { return stripMarkup(body, false) }

// StripCodeSpans blanks the inline code spans in a single line, for callers that work
// line by line on an already comment-stripped body.
func StripCodeSpans(line string) string {
	if strings.IndexByte(line, '`') < 0 {
		return line
	}
	var b strings.Builder
	b.Grow(len(line))
	for i := 0; i < len(line); {
		if line[i] == '`' {
			if end := closingBacktick(line, i); end >= 0 {
				writeSpaces(&b, end+1-i)
				i = end + 1
				continue
			}
		}
		b.WriteByte(line[i])
		i++
	}
	return b.String()
}

// stripMarkup walks the body once, left to right, tracking fences, code spans and HTML
// comments in ONE state machine. Running two independent passes in sequence is what
// makes them corrupt each other: a code-span pass blanks the closing --> out of
// "<!-- use the ` char -->" and the comment then runs to the end of the file, while a
// comment pass first turns "`<!--`" in prose into a real opener. Neither ordering is
// safe, because each pass has to see the other's markers to know they are inert.
//
// blankCode selects what is erased once the scan knows where everything is: the
// comments only, or the code as well. The state machine is identical either way.
func stripMarkup(body string, blankCode bool) (string, int) {
	lines := strings.Split(body, "\n")
	out := make([]string, len(lines))
	// Openers proven to be literal text by a scan that ran off the end of the file, keyed
	// by (line, column). See the unterminated-comment handling in stripLines.
	var literal map[[2]int]bool
	for {
		ln, col, atLineStart := stripLines(lines, out, blankCode, literal)
		if ln == 0 || atLineStart {
			return strings.Join(out, "\n"), ln
		}
		if literal == nil {
			literal = map[[2]int]bool{}
		}
		literal[[2]int{ln, col}] = true
	}
}

// stripLines fills out with the stripped lines and reports where an HTML comment was
// still open when the body ended: its 1-based line, its byte column, and whether the
// marker was the first thing on its line.
//
// That last flag decides what an unterminated <!-- means, and CommonMark draws the line
// in the same place. A marker at the start of a line opens an HTML block, which ends at
// --> or at the end of the document, so the rest of the file really is hidden from the
// reader and the graph should not hold links out of it. A marker in the middle of a
// line is inline raw HTML, which only matches when it is terminated: unterminated, it
// renders as the literal text it is, the note below it is perfectly visible, and hiding
// it would drop content nobody asked to hide. Whether the marker terminates is only
// known at the end of the file, so the caller marks that opener literal and rescans.
func stripLines(lines, out []string, blankCode bool, literal map[[2]int]bool) (openLine, openCol int, atLineStart bool) {
	var b strings.Builder
	inFence, inComment := false, false
	for li, line := range lines {
		ln := li + 1
		if !inComment {
			// A fence marker inside an open comment is itself commented out, and a comment
			// marker inside a fence is sample text, so the two states cannot toggle each
			// other: inComment is only ever set below, where inFence is false.
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
				inFence = !inFence
				out[li] = keepOrBlank(line, blankCode)
				continue
			}
			if inFence {
				out[li] = keepOrBlank(line, blankCode)
				continue
			}
			if strings.IndexByte(line, '`') < 0 && !strings.Contains(line, commentOpen) {
				out[li] = line // nothing to strip: the overwhelmingly common line
				continue
			}
		}
		b.Reset()
		b.Grow(len(line))
		for i := 0; i < len(line); {
			if inComment {
				if strings.HasPrefix(line[i:], commentClose) {
					writeSpaces(&b, len(commentClose))
					i += len(commentClose)
					inComment = false
					openLine, openCol, atLineStart = 0, 0, false
					continue
				}
				b.WriteByte(' ')
				i++
				continue
			}
			if strings.HasPrefix(line[i:], commentOpen) && !literal[[2]int{ln, i}] {
				if n := abruptClose(line[i:]); n > 0 {
					writeSpaces(&b, n)
					i += n
					continue
				}
				openLine, openCol = ln, i
				// The prefix of the ORIGINAL line, not of the stripped one: on
				// "<!-- a --> <!-- b" the second marker is genuinely mid-line, and CommonMark
				// agrees (the HTML block that line opens is closed by the --> on it).
				atLineStart = strings.TrimSpace(line[:i]) == ""
				inComment = true
				writeSpaces(&b, len(commentOpen))
				i += len(commentOpen)
				continue
			}
			if line[i] == '`' {
				if end := closingBacktick(line, i); end >= 0 {
					if blankCode {
						writeSpaces(&b, end+1-i)
					} else {
						b.WriteString(line[i : end+1])
					}
					i = end + 1
					continue
				}
				// An unpaired backtick is a literal character, not a code span that swallows
				// the rest of the line: a span has to close, and CommonMark renders the
				// leftover backtick as itself. Blanking to end of line on an odd count is
				// what erased the --> in "<!-- the ` char -->" and hid the file below it.
				b.WriteByte(line[i])
				i++
				continue
			}
			b.WriteByte(line[i])
			i++
		}
		out[li] = b.String()
	}
	return openLine, openCol, atLineStart
}

// abruptClose reports the length of an abrupt-closing empty comment at the start of s,
// or 0 when s does not start with one. <!--> and <!---> are complete, empty comments in
// both the HTML spec (the comment-start and comment-start-dash states close on >) and
// CommonMark. Reading them as an opener left a comment that nothing could close and
// swallowed the remainder of the file. <!----> is NOT one of these: it is a normal
// empty comment that the --> at its end closes.
func abruptClose(s string) int {
	switch {
	case strings.HasPrefix(s, "<!-->"):
		return len("<!-->")
	case strings.HasPrefix(s, "<!--->"):
		return len("<!--->")
	}
	return 0
}

// closingBacktick returns the index of the backtick that closes the code span opened at
// i, or -1 when the line has none. Code spans do not carry across lines here (the body
// is scanned line by line), which is why an unpaired backtick is literal text.
func closingBacktick(line string, i int) int {
	j := strings.IndexByte(line[i+1:], '`')
	if j < 0 {
		return -1
	}
	return i + 1 + j
}

func keepOrBlank(line string, blank bool) string {
	if !blank {
		return line
	}
	var b strings.Builder
	b.Grow(len(line))
	writeSpaces(&b, len(line))
	return b.String()
}

func writeSpaces(b *strings.Builder, n int) {
	const pad = "                                " // 32 spaces
	for n > len(pad) {
		b.WriteString(pad)
		n -= len(pad)
	}
	b.WriteString(pad[:n])
}
