// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package textdiff

import (
	"strings"
	"testing"
)

func TestUnifiedBasicAddRemove(t *testing.T) {
	a := []byte("alpha\nbeta\ngamma\n")
	b := []byte("alpha\nbeta CHANGED\ngamma\ndelta\n")
	out := Unified(a, b, Options{})
	for _, want := range []string{"@@", "-beta", "+beta CHANGED", "+delta", " alpha", " gamma"} {
		if !strings.Contains(out, want) {
			t.Fatalf("diff missing %q:\n%s", want, out)
		}
	}
	// The unchanged "alpha" must be context (space prefix), never a +/- line.
	if strings.Contains(out, "-alpha") || strings.Contains(out, "+alpha") {
		t.Fatalf("context line alpha shown as a change:\n%s", out)
	}
}

func TestUnifiedHunkHeaders(t *testing.T) {
	// Two far-apart changes should produce two @@ hunks, not one giant span.
	var a, b strings.Builder
	for i := 0; i < 40; i++ {
		a.WriteString("same\n")
		b.WriteString("same\n")
	}
	out := Unified(
		[]byte("HEAD-A\n"+a.String()+"TAIL-A\n"),
		[]byte("HEAD-B\n"+b.String()+"TAIL-B\n"),
		Options{},
	)
	if n := strings.Count(out, "@@ -"); n != 2 {
		t.Fatalf("want 2 hunks for two far-apart changes, got %d:\n%s", n, out)
	}
}

func TestUnifiedIdentical(t *testing.T) {
	x := []byte("one\ntwo\nthree\n")
	if out := Unified(x, x, Options{}); out != "" {
		t.Fatalf("identical inputs should diff to empty, got:\n%s", out)
	}
}

func TestUnifiedBounded(t *testing.T) {
	var a, b strings.Builder
	for i := 0; i < lineCeiling+10; i++ {
		a.WriteString("a\n")
		b.WriteString("b\n")
	}
	out := Unified([]byte(a.String()), []byte(b.String()), Options{})
	if !strings.HasPrefix(out, "(note too large") {
		t.Fatalf("oversize input should degrade to a summary, got:\n%s", out[:min(120, len(out))])
	}
}

func TestUnifiedMaxLinesCap(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 50; i++ {
		b.WriteString("new line\n")
	}
	out := Unified([]byte("base\n"), []byte(b.String()), Options{MaxLines: 5})
	if !strings.Contains(out, "truncated") {
		t.Fatalf("MaxLines cap should truncate, got:\n%s", out)
	}
}

func TestUnifiedSanitizesControl(t *testing.T) {
	// A note carrying an ANSI escape + a lone CR must not reach the terminal raw.
	mine := []byte("base\n\x1b[31mFAKE PROMPT\x1b[0m\rmore\n")
	out := Unified([]byte("base\n"), mine, Options{})
	if strings.ContainsRune(out, 0x1b) || strings.ContainsRune(out, '\r') {
		t.Fatalf("diff leaked a control byte to output:\n%q", out)
	}
	if !strings.Contains(out, "FAKE PROMPT") {
		t.Fatalf("sanitizer dropped visible text:\n%s", out)
	}
}

func TestSanitizeKeepsTabAndText(t *testing.T) {
	if got := Sanitize("a\tb"); got != "a\tb" {
		t.Fatalf("tab should survive: %q", got)
	}
	if Sanitize("x\x07y") == "x\x07y" {
		t.Fatal("BEL should be replaced")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
