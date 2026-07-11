// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

// Package textdiff renders a bounded, hand-rolled unified line diff (stdlib only,
// no diff library). It backs `mesh conflicts diff` and `mesh curator show`, which
// show a parked loser against the current note. Bounded by design: it refuses to
// run the O(n*m) LCS on very large inputs, degrading to a line-count summary so a
// hostile or huge note cannot hang the CLI. Reusable by a future TUI view.
package textdiff

import (
	"fmt"
	"strings"
)

// Options tunes Unified. Zero values are sensible (3 lines of context, a 400
// changed-line cap, no color, hunks only).
type Options struct {
	Context  int  // equal lines of context around a change (default 3)
	Full     bool // emit the whole file as one hunk, not just changed regions
	MaxLines int  // cap on emitted +/- lines before truncating (default 400)
	Color    bool // ANSI-color the +/- lines (the caller gates this on a TTY)
}

const (
	defaultContext  = 3
	defaultMaxLines = 400
	lineCeiling     = 3000 // above this on either side, summarize instead of LCS
)

const (
	ansiRed   = "\x1b[31m"
	ansiGreen = "\x1b[32m"
	ansiReset = "\x1b[0m"
)

type tag int

const (
	eq tag = iota
	del
	add
)

type dline struct {
	t    tag
	text string
	aNum int // 1-based line number in a (0 for an add)
	bNum int // 1-based line number in b (0 for a del)
}

// Unified returns a unified diff of a (the base) vs b (mine), or "" if identical.
func Unified(a, b []byte, opts Options) string {
	if opts.Context <= 0 {
		opts.Context = defaultContext
	}
	if opts.MaxLines <= 0 {
		opts.MaxLines = defaultMaxLines
	}
	al := splitLines(a)
	bl := splitLines(b)
	if len(al) > lineCeiling || len(bl) > lineCeiling {
		return summary(al, bl)
	}
	lines := diffLines(al, bl)
	return format(lines, len(al), opts)
}

func splitLines(b []byte) []string {
	s := strings.TrimSuffix(string(b), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// diffLines computes the line-level LCS edit script (an O(n*m) table, bounded by
// the caller's lineCeiling) and walks it into a flat eq/del/add sequence.
func diffLines(a, b []string) []dline {
	n, m := len(a), len(b)
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}
	var out []dline
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			out = append(out, dline{eq, a[i], i + 1, j + 1})
			i++
			j++
		case dp[i+1][j] >= dp[i][j+1]:
			out = append(out, dline{del, a[i], i + 1, 0})
			i++
		default:
			out = append(out, dline{add, b[j], 0, j + 1})
			j++
		}
	}
	for ; i < n; i++ {
		out = append(out, dline{del, a[i], i + 1, 0})
	}
	for ; j < m; j++ {
		out = append(out, dline{add, b[j], 0, j + 1})
	}
	return out
}

func format(lines []dline, aTotal int, opts Options) string {
	var changes []int
	for idx, l := range lines {
		if l.t != eq {
			changes = append(changes, idx)
		}
	}
	if len(changes) == 0 {
		return "" // identical
	}

	// Group changes whose flat-list gap is small enough to share context into one
	// hunk; far-apart changes get separate hunks.
	type span struct{ start, end int } // [start,end) in lines
	var hunks []span
	if opts.Full {
		hunks = []span{{0, len(lines)}}
	} else {
		gi := 0
		for gi < len(changes) {
			gstart, gend := changes[gi], changes[gi]
			gj := gi + 1
			for gj < len(changes) && changes[gj]-gend <= 2*opts.Context+1 {
				gend = changes[gj]
				gj++
			}
			hs := gstart - opts.Context
			if hs < 0 {
				hs = 0
			}
			he := gend + opts.Context + 1
			if he > len(lines) {
				he = len(lines)
			}
			hunks = append(hunks, span{hs, he})
			gi = gj
		}
	}

	var sb strings.Builder
	emitted := 0
	for _, h := range hunks {
		if emitted >= opts.MaxLines {
			fmt.Fprintf(&sb, "... diff truncated at %d changed lines (use --full to see all)\n", opts.MaxLines)
			break
		}
		aStart, aLen, bStart, bLen := 0, 0, 0, 0
		for _, l := range lines[h.start:h.end] {
			if l.aNum != 0 {
				if aStart == 0 {
					aStart = l.aNum
				}
				aLen++
			}
			if l.bNum != 0 {
				if bStart == 0 {
					bStart = l.bNum
				}
				bLen++
			}
		}
		fmt.Fprintf(&sb, "@@ -%s +%s @@\n", rng(aStart, aLen), rng(bStart, bLen))
		for _, l := range lines[h.start:h.end] {
			text := Sanitize(l.text) // note bytes are untrusted: never let raw ESC reach a TTY
			switch l.t {
			case eq:
				fmt.Fprintf(&sb, " %s\n", text)
			case del:
				writeColored(&sb, "-", text, ansiRed, opts.Color)
				emitted++
			case add:
				writeColored(&sb, "+", text, ansiGreen, opts.Color)
				emitted++
			}
			if emitted >= opts.MaxLines {
				fmt.Fprintf(&sb, "... diff truncated at %d changed lines (use --full to see all)\n", opts.MaxLines)
				return sb.String()
			}
		}
	}
	return sb.String()
}

// Sanitize replaces C0/C1 control bytes (except tab) with the Unicode
// replacement char so untrusted note content cannot inject ANSI escapes, cursor
// moves, or a fake prompt when the diff is printed to a real terminal. Exported
// so callers can scrub hub-supplied labels (e.g. a note path) the same way.
func Sanitize(s string) string {
	if !strings.ContainsFunc(s, isControl) {
		return s
	}
	return strings.Map(func(r rune) rune {
		if isControl(r) {
			return '�'
		}
		return r
	}, s)
}

func isControl(r rune) bool {
	if r == '\t' {
		return false
	}
	return r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f)
}

func writeColored(sb *strings.Builder, prefix, text, color string, on bool) {
	if on {
		fmt.Fprintf(sb, "%s%s%s%s\n", color, prefix, text, ansiReset)
		return
	}
	fmt.Fprintf(sb, "%s%s\n", prefix, text)
}

// rng formats a unified-diff line range; a zero-length side shows start 0.
func rng(start, count int) string {
	if count == 0 {
		return "0,0"
	}
	if count == 1 {
		return fmt.Sprintf("%d", start)
	}
	return fmt.Sprintf("%d,%d", start, count)
}

// summary degrades the diff for oversize inputs: a multiset line-count delta with
// no O(n*m) work, so a 1 MiB note never blows up the diff.
func summary(a, b []string) string {
	ac := map[string]int{}
	for _, l := range a {
		ac[l]++
	}
	bc := map[string]int{}
	for _, l := range b {
		bc[l]++
	}
	onlyA, onlyB := 0, 0
	for l, c := range ac {
		if d := c - bc[l]; d > 0 {
			onlyA += d
		}
	}
	for l, c := range bc {
		if d := c - ac[l]; d > 0 {
			onlyB += d
		}
	}
	return fmt.Sprintf("(note too large to render a full diff: %d lines in base, %d in mine; %d line(s) only in base, %d only in mine)\n",
		len(a), len(b), onlyA, onlyB)
}
