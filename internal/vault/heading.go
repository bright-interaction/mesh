// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import "strings"

// ATXHeading is the heading information shared by the index and MCP fetch path.
// Text preserves the authored Markdown for graph labels, while VisibleText removes
// destinations that a Markdown renderer does not show. Anchor is derived from that
// visible text, so a link's URL cannot become part of a section address.
type ATXHeading struct {
	Level       int
	Text        string
	VisibleText string
	Anchor      string
}

// ParseATXHeading parses one CommonMark ATX heading from two length-preserving views of
// the same source line. markerLine decides whether the # marker is active (callers mask
// code and comments there); textLine supplies the visible heading text (callers may keep
// inline-code contents there). ATX markers may be indented by at most three spaces and
// the delimiter after the hashes may be either a space or a tab.
func ParseATXHeading(markerLine, textLine string) (ATXHeading, bool) {
	indent := 0
	for indent < len(markerLine) && indent < 4 && markerLine[indent] == ' ' {
		indent++
	}
	if indent > 3 {
		return ATXHeading{}, false
	}

	i := indent
	for i < len(markerLine) && markerLine[i] == '#' {
		i++
	}
	level := i - indent
	if level == 0 || level > 6 || i >= len(markerLine) || (markerLine[i] != ' ' && markerLine[i] != '\t') {
		return ATXHeading{}, false
	}
	if i > len(textLine) {
		return ATXHeading{}, false
	}

	// Keep the established treatment of optional closing hashes. The structural and
	// display views have identical byte lengths, so the marker-derived offset is valid
	// even when the heading contains multibyte Unicode text.
	text := strings.TrimSpace(strings.TrimRight(textLine[i:], "# "))
	if text == "" {
		return ATXHeading{}, false
	}
	visible := headingLinkLabels(text)
	return ATXHeading{Level: level, Text: text, VisibleText: visible, Anchor: Slugify(visible)}, true
}

// headingLinkLabels removes inline and reference-link destinations while preserving the
// label a Markdown reader sees. It intentionally leaves malformed links untouched: on
// malformed input, retaining authored text is safer than deleting an arbitrary suffix.
func headingLinkLabels(s string) string {
	var out strings.Builder
	for i := 0; i < len(s); {
		if s[i] != '[' || markdownPunctuationEscaped(s, i) {
			out.WriteByte(s[i])
			i++
			continue
		}

		labelEnd := matchingMarkdownDelimiter(s, i, '[', ']')
		if labelEnd < 0 || labelEnd+1 >= len(s) {
			out.WriteByte(s[i])
			i++
			continue
		}

		destinationEnd := -1
		switch s[labelEnd+1] {
		case '(':
			destinationEnd = matchingMarkdownDelimiter(s, labelEnd+1, '(', ')')
		case '[':
			destinationEnd = matchingMarkdownDelimiter(s, labelEnd+1, '[', ']')
		}
		if destinationEnd < 0 {
			out.WriteByte(s[i])
			i++
			continue
		}

		out.WriteString(headingLinkLabels(s[i+1 : labelEnd]))
		i = destinationEnd + 1
	}
	return out.String()
}

func matchingMarkdownDelimiter(s string, start int, open, close byte) int {
	depth := 0
	for i := start; i < len(s); i++ {
		if markdownPunctuationEscaped(s, i) {
			continue
		}
		switch s[i] {
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func markdownPunctuationEscaped(s string, pos int) bool {
	backslashes := 0
	for pos > 0 && s[pos-1] == '\\' {
		backslashes++
		pos--
	}
	return backslashes%2 == 1
}
