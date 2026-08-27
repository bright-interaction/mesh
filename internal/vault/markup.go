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
func StripNonContent(body string) (string, int) { return stripMarkup(body, true, true) }

// UnterminatedFence reports whether body ends inside a fenced code block. It uses the
// same scanner as StripNonContent rather than duplicating its character, run-length,
// container-prefix, and info-string matching rules. An open fence blanks the appended
// probe line; balanced markup leaves that line visible.
func UnterminatedFence(body string) bool {
	const probe = "meshunterminatedfenceprobe"
	clean, openComment := StripNonContent(body + "\n" + probe + "\n")
	if openComment > 0 {
		return false // the existing unterminated-comment diagnostic owns this case
	}
	at := len(strings.Split(body, "\n"))
	lines := strings.Split(clean, "\n")
	return at >= len(lines) || strings.TrimSpace(lines[at]) != probe
}

// StripComments blanks only the HTML comments, keeping code text intact. Search and
// embeddings want `mesh index --workers 4` to be findable, so code is content there
// even though it is not a place to read links from. Comment, fence and code-span
// boundaries are detected exactly as in StripNonContent, so both agree on which markers
// are real markup and neither can see a comment the other one does not.
func StripComments(body string) (string, int) { return stripMarkup(body, false, false) }

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
			run := backtickRun(line, i)
			b.WriteString(line[i : i+run])
			i += run
			continue
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
func stripMarkup(body string, blankFences, blankSpans bool) (string, int) {
	lines := strings.Split(body, "\n")
	out := make([]string, len(lines))
	// Openers proven to be literal text by a scan that ran off the end of the file, keyed
	// by (line, column). See the unterminated-comment handling in stripLines.
	var literal map[[2]int]bool
	for {
		ln, col, atLineStart := stripLines(lines, out, blankFences, blankSpans, literal)
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
func stripLines(lines, out []string, blankFences, blankSpans bool, literal map[[2]int]bool) (openLine, openCol int, atLineStart bool) {
	var b strings.Builder
	inFence, inComment := false, false
	spanRun := 0
	var fenceChar byte
	var fenceRun, fenceQuoteDepth, fenceListIndent int
	for li, line := range lines {
		ln := li + 1
		if !inComment && spanRun == 0 {
			// A fence marker inside an open comment is itself commented out, and a comment
			// marker inside a fence is sample text, so the two states cannot toggle each
			// other: inComment is only ever set below, where inFence is false.
			trimmed, fenceEligible := fenceCandidate(line)
			quoteDepth := blockquoteDepth(line)
			// A fence nested in a blockquote ends when the quote container ends, even if
			// the author omitted a closing marker. Carrying it into ordinary prose hid the
			// rest of the note.
			if inFence && fenceQuoteDepth > 0 && quoteDepth < fenceQuoteDepth {
				inFence, fenceChar, fenceRun, fenceQuoteDepth, fenceListIndent = false, 0, 0, 0, 0
			}
			// A fence opened inside a list item is contained by that item. Once a
			// non-blank line loses the item's content indentation, the list (and the
			// unclosed fence with it) has ended; ordinary top-level prose is visible.
			if inFence && fenceListIndent > 0 && listContainerEnded(line, fenceQuoteDepth, fenceListIndent) {
				inFence, fenceChar, fenceRun, fenceQuoteDepth, fenceListIndent = false, 0, 0, 0, 0
			}
			// The list marker can make the item's content indentation wider than the
			// three spaces allowed before a root fence (for example "10. ```"). Reuse
			// the opener's saved container indentation when looking for its closer.
			if inFence && fenceListIndent > 0 {
				if candidate, ok := listContainedFenceCandidate(line, fenceQuoteDepth, fenceListIndent); ok {
					trimmed, fenceEligible = candidate, true
				}
			}
			// CommonMark: a fence closes only on the SAME character, at least as long as
			// the opener, with no info string. Toggling on any run of three inverted the
			// state for a nested block, so an outer ````markdown fence containing an inner
			// ```ts opener CLOSED the outer fence at that inner line, and everything the
			// documentation block was quoting became real graph content: phantom heading
			// nodes, phantom reference edges and phantom tags built from an example. With
			// an odd count the inverse happened and real prose below was hidden.
			if ch, run, info := fenceMarker(trimmed); fenceEligible && run > 0 {
				switch {
				case !inFence:
					inFence, fenceChar, fenceRun, fenceQuoteDepth = true, ch, run, quoteDepth
					fenceListIndent = listFenceContentIndent(line)
					out[li] = keepOrBlank(line, blankFences)
					continue
				case ch == fenceChar && run >= fenceRun && !info:
					inFence, fenceChar, fenceRun, fenceQuoteDepth, fenceListIndent = false, 0, 0, 0, 0
					out[li] = keepOrBlank(line, blankFences)
					continue
				default:
					// A shorter run, a different char, or an info string inside an open
					// fence is CONTENT of that fence, not a delimiter.
					out[li] = keepOrBlank(line, blankFences)
					continue
				}
			}
			if inFence {
				out[li] = keepOrBlank(line, blankFences)
				continue
			}
			if indentedCodeLine(line) && !listContinuationLine(lines, li) {
				out[li] = keepOrBlank(line, blankFences)
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
			if spanRun > 0 {
				if line[i] == '`' && backtickRun(line, i) == spanRun {
					if blankSpans {
						writeSpaces(&b, spanRun)
					} else {
						b.WriteString(line[i : i+spanRun])
					}
					i += spanRun
					spanRun = 0
					continue
				}
				if blankSpans {
					b.WriteByte(' ')
				} else {
					b.WriteByte(line[i])
				}
				i++
				continue
			}
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
			if strings.HasPrefix(line[i:], commentOpen) && !markdownEscapedAt(line, i) && !literal[[2]int{ln, i}] {
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
				if markdownEscapedAt(line, i) {
					b.WriteByte(line[i])
					i++
					continue
				}
				if end := closingBacktick(line, i); end >= 0 {
					if blankSpans {
						writeSpaces(&b, end+1-i)
					} else {
						b.WriteString(line[i : end+1])
					}
					i = end + 1
					continue
				}
				run := backtickRun(line, i)
				if hasClosingBacktick(lines, li+1, run) {
					if blankSpans {
						writeSpaces(&b, run)
					} else {
						b.WriteString(line[i : i+run])
					}
					i += run
					spanRun = run
					continue
				}
				// An unpaired backtick is a literal character, not a code span that swallows
				// the rest of the line: a span has to close, and CommonMark renders the
				// leftover backtick as itself. Blanking to end of line on an odd count is
				// what erased the --> in "<!-- the ` char -->" and hid the file below it.
				b.WriteString(line[i : i+run])
				i += run
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

// closingBacktick returns the final index of the backtick run that closes the code span
// opened at i, or -1 when the line has no run of exactly the same length. A shorter or
// longer run is content inside the span under CommonMark; matching only the next single
// backtick made a code example such as [[example]] leak a phantom graph link.
func closingBacktick(line string, i int) int {
	openRun := backtickRun(line, i)
	for j := i + openRun; j < len(line); {
		if line[j] != '`' {
			j++
			continue
		}
		closeRun := backtickRun(line, j)
		if closeRun == openRun {
			return j + closeRun - 1
		}
		j += closeRun
	}
	return -1
}

func backtickRun(line string, i int) int {
	j := i
	for j < len(line) && line[j] == '`' {
		j++
	}
	return j - i
}

// hasClosingBacktick reports whether a code-span delimiter continues onto a later
// line of the same Markdown paragraph. Blank lines and block openers end a paragraph;
// without this look-ahead an unmatched opener was treated as literal before the scanner
// reached its valid closer, leaking wikilinks from multiline code spans.
func hasClosingBacktick(lines []string, startLine, wantRun int) bool {
	for li := startLine; li < len(lines); li++ {
		line := lines[li]
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			return false
		}
		if strings.HasPrefix(trimmed, commentOpen) || atxHeadingLine(trimmed) {
			return false
		}
		// A list marker starts a new block. A backtick left open in the previous
		// item is literal text; it cannot turn the next item's contents into code.
		if _, ok := listItemContent(line, 3); ok {
			return false
		}
		if candidate, ok := fenceCandidate(line); ok {
			if _, run, _ := fenceMarker(candidate); run > 0 {
				return false
			}
		}
		for i := 0; i < len(line); {
			if line[i] != '`' {
				i++
				continue
			}
			run := backtickRun(line, i)
			if run == wantRun {
				return true
			}
			i += run
		}
	}
	return false
}

func atxHeadingLine(trimmed string) bool {
	i := 0
	for i < len(trimmed) && trimmed[i] == '#' {
		i++
	}
	return i > 0 && i <= 6 && i < len(trimmed) && trimmed[i] == ' '
}

func markdownEscapedAt(line string, pos int) bool {
	backslashes := 0
	for pos > 0 && line[pos-1] == '\\' {
		backslashes++
		pos--
	}
	return backslashes%2 == 1
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

// StripFencesAndComments blanks fenced code blocks and HTML comments but KEEPS inline code
// spans, for callers that care whether a token was written as an example or as a reference.
//
// A path in a fence is a command you could run ("npx vitest src/lib/utils.test.ts"). A path
// in backticks mid-sentence is how anyone writes a filename in prose, so treating those as
// examples too would suppress the real citations the caller is looking for. mesh health's
// dead-ref pass wants exactly this split: it lost a genuine finding (a procedure step
// pointing at a deleted script) when it briefly used StripNonContent instead.
func StripFencesAndComments(body string) (string, int) { return stripMarkup(body, true, false) }

// fenceCandidate removes blockquote containers and the indentation CommonMark permits
// before a fence. Four spaces (or a leading tab) form an indented code block instead;
// treating that line as a fence can hide every real paragraph below it.
func fenceCandidate(line string) (string, bool) {
	rest, _, ok := stripFenceContainers(line)
	if !ok {
		return "", false
	}
	// A fenced block may open on the same line as its list marker. Continuation
	// and closing lines are already handled by stripFenceContainers because a
	// list item's normal content indentation is at most three spaces here.
	if content, list := listItemContent(rest, 3); list {
		rest = content
	}
	return strings.TrimSpace(rest), true
}

func blockquoteDepth(line string) int {
	_, depth, _ := stripFenceContainers(line)
	return depth
}

// listFenceContentIndent reports the indentation subsequent lines must keep to remain
// inside the list item that opened a fence. Zero means the fence is not list-contained.
func listFenceContentIndent(line string) int {
	rest, _, ok := stripFenceContainers(line)
	if !ok {
		return 0
	}
	_, indent, ok := listItemDetails(rest, 3)
	if !ok {
		return 0
	}
	return indent
}

func listContainerEnded(line string, quoteDepth, contentIndent int) bool {
	if strings.TrimSpace(line) == "" {
		return false // blank lines do not themselves close a list item
	}
	rest, ok := lineAfterBlockquotes(line, quoteDepth)
	return !ok || leadingIndentColumns(rest) < contentIndent
}

func listContainedFenceCandidate(line string, quoteDepth, contentIndent int) (string, bool) {
	rest, ok := lineAfterBlockquotes(line, quoteDepth)
	if !ok {
		return "", false
	}
	i, columns := 0, 0
	for i < len(rest) && columns < contentIndent {
		switch rest[i] {
		case ' ':
			columns++
		case '\t':
			columns += 4 - columns%4
		default:
			return "", false
		}
		i++
	}
	if columns < contentIndent {
		return "", false
	}
	rest = rest[i:]
	indent := 0
	for indent < len(rest) && rest[indent] == ' ' {
		indent++
	}
	if indent > 3 || indent < len(rest) && rest[indent] == '\t' {
		return "", false
	}
	return strings.TrimSpace(rest), true
}

// lineAfterBlockquotes removes exactly depth quote containers. List indentation is
// measured inside the quote that owns it, not after any deeper quote on the same line.
func lineAfterBlockquotes(line string, depth int) (string, bool) {
	rest := line
	for d := 0; d < depth; d++ {
		i := 0
		for i < len(rest) && i < 4 && rest[i] == ' ' {
			i++
		}
		if i > 3 || i >= len(rest) || rest[i] != '>' {
			return rest, false
		}
		rest = rest[i+1:]
		if len(rest) > 0 && (rest[0] == ' ' || rest[0] == '\t') {
			rest = rest[1:]
		}
	}
	return rest, true
}

func leadingIndentColumns(line string) int {
	columns := 0
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case ' ':
			columns++
		case '\t':
			columns += 4 - columns%4
		default:
			return columns
		}
	}
	return columns
}

func prefixColumns(line string) int {
	columns := 0
	for i := 0; i < len(line); i++ {
		if line[i] == '\t' {
			columns += 4 - columns%4
		} else {
			columns++
		}
	}
	return columns
}

func stripFenceContainers(line string) (string, int, bool) {
	rest := line
	depth := 0
	for {
		i := 0
		for i < len(rest) && rest[i] == ' ' {
			i++
		}
		if i > 3 || i < len(rest) && rest[i] == '\t' {
			return "", depth, false
		}
		rest = rest[i:]
		if len(rest) == 0 || rest[0] != '>' {
			return rest, depth, true
		}
		depth++
		rest = rest[1:]
		if len(rest) > 0 && (rest[0] == ' ' || rest[0] == '\t') {
			rest = rest[1:]
		}
	}
}

func indentedCodeLine(line string) bool {
	rest := line
	for {
		spaces := 0
		for spaces < len(rest) && rest[spaces] == ' ' {
			spaces++
		}
		if spaces >= 4 || spaces < len(rest) && rest[spaces] == '\t' {
			return true
		}
		rest = rest[spaces:]
		if len(rest) == 0 || rest[0] != '>' {
			return false
		}
		rest = rest[1:]
		if len(rest) > 0 && (rest[0] == ' ' || rest[0] == '\t') {
			rest = rest[1:]
		}
	}
}

// listItemContent returns the text following a CommonMark bullet or ordered-list
// marker. maxIndent is the indentation permitted before the marker; callers use three
// for a marker at the current container level and a larger value only when checking
// whether an otherwise indented line is nested beneath an earlier list item.
func listItemContent(line string, maxIndent int) (string, bool) {
	content, _, ok := listItemDetails(line, maxIndent)
	return content, ok
}

func listItemDetails(line string, maxIndent int) (contentText string, contentIndent int, ok bool) {
	i := 0
	for i < len(line) && line[i] == ' ' {
		i++
	}
	if i > maxIndent || i < len(line) && line[i] == '\t' {
		return "", 0, false
	}

	markerEnd := i
	switch {
	case markerEnd < len(line) && (line[markerEnd] == '-' || line[markerEnd] == '+' || line[markerEnd] == '*'):
		markerEnd++
	default:
		digits := 0
		for markerEnd < len(line) && line[markerEnd] >= '0' && line[markerEnd] <= '9' && digits < 10 {
			markerEnd++
			digits++
		}
		if digits == 0 || digits > 9 || markerEnd >= len(line) || line[markerEnd] != '.' && line[markerEnd] != ')' {
			return "", 0, false
		}
		markerEnd++
	}
	if markerEnd >= len(line) || line[markerEnd] != ' ' && line[markerEnd] != '\t' {
		return "", 0, false
	}

	content := markerEnd
	for content < len(line) && (line[content] == ' ' || line[content] == '\t') {
		content++
	}
	// One through four spaces after a marker are list padding. With five or more,
	// CommonMark treats the extra four as an indented code block instead.
	if content-markerEnd > 4 {
		return "", 0, false
	}
	return line[content:], prefixColumns(line[:content]), true
}

// listContinuationLine distinguishes visible list content from root-level indented
// code. Indented code inside a list starts four columns beyond the item's content
// indentation; anything less remains ordinary list prose (including a nested marker).
func listContinuationLine(lines []string, line int) bool {
	current := lines[line]
	quoteDepth := blockquoteDepth(current)
	rest, ok := lineAfterBlockquotes(current, quoteDepth)
	if !ok {
		return false
	}
	indent := leadingIndentColumns(rest)
	if indent < 4 {
		return false
	}
	consecutiveBlanks := 0
	for previous := line - 1; previous >= 0; previous-- {
		if strings.TrimSpace(lines[previous]) == "" {
			consecutiveBlanks++
			if consecutiveBlanks >= 2 {
				return false
			}
			continue
		}
		consecutiveBlanks = 0
		if blockquoteDepth(lines[previous]) != quoteDepth {
			return false
		}
		previousRest, ok := lineAfterBlockquotes(lines[previous], quoteDepth)
		if !ok {
			return false
		}
		previousIndent := leadingIndentColumns(previousRest)
		if _, contentIndent, item := listItemDetails(previousRest, previousIndent); item {
			return indent >= contentIndent && indent < contentIndent+4
		}
		if previousIndent == 0 {
			return false
		}
	}
	return false
}

// fenceMarker reports the fence character, its run length and whether an info string
// follows, for a trimmed line. run is 0 when the line is not a fence marker at all.
//
// Split out because "starts with three backticks" is not what CommonMark means by a
// fence: the closer must match the opener's character and be at least as long, and a
// closer may not carry an info string. Treating every run of three as a toggle is what
// let a nested documentation fence invert the state.
func fenceMarker(trimmed string) (ch byte, run int, info bool) {
	if len(trimmed) < 3 {
		return 0, 0, false
	}
	c := trimmed[0]
	if c != '`' && c != '~' {
		return 0, 0, false
	}
	i := 0
	for i < len(trimmed) && trimmed[i] == c {
		i++
	}
	if i < 3 {
		return 0, 0, false
	}
	rest := strings.TrimSpace(trimmed[i:])
	// A backtick fence's info string may not contain a backtick (CommonMark), which is
	// what keeps an inline span like `` `a` `` from reading as a fence.
	if c == '`' && strings.ContainsRune(rest, '`') {
		return 0, 0, false
	}
	return c, i, rest != ""
}
