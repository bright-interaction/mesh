// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Excerpt formatting, matching what FTS5's snippet() used to emit so a card looks
// the same as it always did: matched terms wrapped in brackets, " ... " where text
// was cut away.
const (
	excerptOpen     = "["
	excerptClose    = "]"
	excerptEllipsis = " ... "
)

// excerptScanChars bounds how much of a matched note's body is pulled back from
// SQLite to build the excerpt. Every real note fits; a multi-megabyte one (a pasted
// deploy log, a concatenated transcript) contributes only its first 64k characters
// to the excerpt while still matching and ranking on its whole body.
const excerptScanChars = 65536

// SQL's bounded prefix can itself end inside a version. At the character cap,
// conservatively discard its last field rather than treating an unseen suffix
// as absent. Shorter bodies are untouched; the fixed scan bound does not grow.
func trimExcerptScanTail(text string) string {
	if len(text) < excerptScanChars || utf8.RuneCountInString(text) < excerptScanChars {
		return text
	}
	boundary := strings.LastIndexFunc(text, func(r rune) bool {
		return !continuesExcerptField(r) && r != '.'
	})
	if boundary < 0 {
		return ""
	}
	return text[:boundary]
}

// buildExcerpt renders the result snippet in Go rather than asking SQLite for
// snippet().
//
// snippet() is the reason mesh_search could hang forever. FTS5 scores every match
// INSTANCE inside a matched document to choose the best window, at roughly the
// square of the number of occurrences in that one document: measured with the system
// sqlite3 binary against a Mesh-written index, 5,087 occurrences cost 0.11s, 20,150
// cost 1.56s, 80,106 cost 25.2s, and an 11 MB note of repeated text never returned at
// all. That is ordinary user data, not an attack, and it cannot be interrupted:
// sqlite3_interrupt only takes effect between VDBE opcodes, and one snippet() call is
// one opcode, so cancelling the agent tool call or the HTTP request did not stop the
// burn either.
//
// Building the window here is linear in the text scanned and bounded by
// excerptScanChars, so the cost per hit is fixed no matter how repetitive the note.
// terms are the tokenized query terms (already deduplicated and capped by
// graph.TokenizeQuery); width is the number of tokens in the window. With no term
// present in the scanned prefix the head of the text is returned, the same fallback
// snippet() applied when a match lived in another column.
func buildExcerpt(text string, terms []string, width int) string {
	if text == "" || width <= 0 {
		return ""
	}
	spans := tokenSpans(text)
	if len(spans) == 0 {
		return strings.TrimSpace(text)
	}
	match := make(map[string]bool, len(terms))
	phrases := make(map[string][][]string)
	for _, t := range terms {
		if !strings.Contains(t, ".") {
			match[t] = true
			continue
		}
		// The query tokenizer retains bounded version literals. FTS5 matches
		// these as token phrases; highlight the same full sequence, including
		// single-digit components, without invoking SQLite's costly snippet().
		var parts []string
		for _, sp := range tokenSpans(t) {
			parts = append(parts, t[sp.start:sp.end])
		}
		if len(parts) > 0 {
			phrases[parts[0]] = append(phrases[parts[0]], parts)
		}
	}

	hit := -1
	hits := make([]bool, len(spans))
	phraseEnds := make(map[int]int)
	for i, sp := range spans {
		word := strings.ToLower(text[sp.start:sp.end])
		if match[word] {
			hits[i] = true
		}
		for _, phrase := range phrases[word] {
			if i+len(phrase) > len(spans) {
				continue
			}
			matched := true
			for j := 1; j < len(phrase); j++ {
				next := spans[i+j]
				if strings.ToLower(text[next.start:next.end]) != phrase[j] {
					matched = false
					break
				}
			}
			if matched && completeExcerptPhrase(text, sp.start, spans[i+len(phrase)-1].end) {
				if end := i + len(phrase) - 1; end > phraseEnds[i] {
					phraseEnds[i] = end
				}
				for j := range phrase {
					hits[i+j] = true
				}
			}
		}
		if hits[i] && hit < 0 {
			hit = i
		}
	}

	// Centre the window on the first match, or start at the head when nothing in the
	// scanned prefix matched.
	lo := 0
	if hit > 0 {
		lo = hit - width/2
		if lo < 0 {
			lo = 0
		}
	}
	hi := lo + width
	if hi > len(spans) {
		hi = len(spans)
		if lo = hi - width; lo < 0 {
			lo = 0
		}
	}
	// A token window can end after the stable core but before -rc.1, or
	// start inside a longer dotted field. Omit partial fields instead of
	// manufacturing a complete version or growing the bounded window.
	for lo < hi && !excerptFieldStart(text, spans[lo].start) {
		lo++
	}
	for hi > lo && !excerptFieldEnd(text, spans[hi-1].end) {
		hi--
	}
	if lo == hi {
		return excerptEllipsis
	}

	var b strings.Builder
	if lo > 0 {
		b.WriteString(excerptEllipsis)
	}
	for i := lo; i < hi; i++ {
		// Keep the original inter-token text (spaces, punctuation) so the excerpt reads
		// as prose rather than as a bag of words.
		if i > lo {
			b.WriteString(text[spans[i-1].end:spans[i].start])
		}
		if end, ok := phraseEnds[i]; ok && end < hi {
			// Preserve literal spelling inside one bracket pair. Separate brackets
			// around each component would hide its identity from cheap confidence
			// routing when the version appears only in the snippet, not the title.
			b.WriteString(excerptOpen)
			b.WriteString(text[spans[i].start:spans[end].end])
			b.WriteString(excerptClose)
			i = end
			continue
		}
		// Even ordinary highlighting can split an invalid longer field into
		// a valid version, e.g. 1.2.3.[41]. Keep partial fields unbracketed.
		if hits[i] && completeExcerptPhrase(text, spans[i].start, spans[i].end) {
			b.WriteString(excerptOpen)
			b.WriteString(text[spans[i].start:spans[i].end])
			b.WriteString(excerptClose)
			continue
		}
		b.WriteString(text[spans[i].start:spans[i].end])
	}
	if hi < len(spans) {
		b.WriteString(excerptEllipsis)
	}
	return b.String()
}

// Do not insert brackets inside a version-shaped field. FTS may match the stable
// core of a prerelease, but [v1.2.3]-rc.1 would create a false standalone version
// when confidence routing tokenizes the snippet. Only outer sentence dots may
// be ignored, matching the whole-field tokenizer's boundary handling.
func completeExcerptPhrase(text string, start, end int) bool {
	return excerptFieldStart(text, start) && excerptFieldEnd(text, end)
}

func excerptFieldStart(text string, start int) bool {
	for start > 0 && text[start-1] == '.' {
		start--
	}
	if start > 0 {
		r, _ := utf8.DecodeLastRuneInString(text[:start])
		if continuesExcerptField(r) {
			return false
		}
	}
	return true
}

func excerptFieldEnd(text string, end int) bool {
	for end < len(text) && text[end] == '.' {
		end++
	}
	if end < len(text) {
		r, _ := utf8.DecodeRuneInString(text[end:])
		if continuesExcerptField(r) {
			return false
		}
	}
	return true
}

func continuesExcerptField(r rune) bool {
	// Be conservative around combining marks: graph.Tokenize normalizes NFC,
	// so a mark may join its neighboring letter before field recognition.
	return unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsMark(r) || r == '-' || r == '+'
}

// span is one token's byte range within the scanned text.
type span struct{ start, end int }

// tokenSpans locates the alphanumeric runs in text, using the same rune classes as
// graph.Tokenize and FTS5's unicode61 tokenizer so a term that matched in SQLite is
// the same run that gets bracketed here. Unlike the tokenizer it keeps short tokens
// and stopwords: they are not query terms, but they are part of the prose the reader
// sees.
func tokenSpans(text string) []span {
	spans := make([]span, 0, len(text)/6+1)
	start := -1
	for i, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 {
			spans = append(spans, span{start, i})
			start = -1
		}
	}
	if start >= 0 {
		spans = append(spans, span{start, len(text)})
	}
	return spans
}
