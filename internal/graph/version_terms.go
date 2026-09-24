// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package graph

import (
	"regexp"
	"strings"
	"unicode"
)

// This recognizes version-shaped literals, not SemVer ranges or ordering. Keep
// the optional v and the complete prerelease/build suffix; do not infer that a
// different spelling is equivalent. No persisted index or model is required.
var versionLiteral = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9a-z-]+(?:\.[0-9a-z-]+)*)?(?:\+[0-9a-z-]+(?:\.[0-9a-z-]+)*)?$`)

const maxVersionLiteralBytes = 128
const maxVersionLiteralTokens = 8

func appendVersionLiterals(out []string, text string) []string {
	if !hasDottedNumber(text) {
		return out
	}
	// Whole candidate fields prevent extracting v1.2.3 from xv1.2.3,
	// v1.2.30, or a longer suffix. Sentence punctuation is not part of a version.
	for field := range strings.FieldsFuncSeq(text, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '.' && r != '-' && r != '+'
	}) {
		field = strings.Trim(field, ".")
		if len(field) == 0 || len(field) > maxVersionLiteralBytes || (field[0] != 'v' && (field[0] < '0' || field[0] > '9')) {
			continue
		}
		if versionLiteral.MatchString(field) && literalTokenCount(field) <= maxVersionLiteralTokens {
			// Ranker term counts outlive this scan. Do not retain the entire
			// normalized document through a tiny substring map key.
			out = append(out, strings.Clone(field))
		}
	}
	return out
}

// Most searchable prose contains sentence dots but no version. Skip the field
// scan entirely unless a dot has ASCII digits on both sides (required by the
// literal grammar), without allocating another word list or normalized copy.
func hasDottedNumber(text string) bool {
	for offset := 0; offset < len(text); {
		i := strings.IndexByte(text[offset:], '.')
		if i < 0 {
			return false
		}
		i += offset
		if i > 0 && i+1 < len(text) && text[i-1] >= '0' && text[i-1] <= '9' && text[i+1] >= '0' && text[i+1] <= '9' {
			return true
		}
		offset = i + 1
	}
	return false
}

// Ordinary terms are alphanumeric runs; additional version literals contain
// ASCII separators that FTS5 splits. Count runs, not bytes, to bound phrase work.
func literalTokenCount(term string) int {
	if !strings.ContainsAny(term, ".-+") {
		return 1
	}
	count, inToken := 0, false
	for _, r := range term {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if !inToken {
				count++
			}
			inToken = true
		} else {
			inToken = false
		}
	}
	return count
}
