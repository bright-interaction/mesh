// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package shellpath

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestQuote(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"/home/t/notes", "/home/t/notes"}, // the common case stays bare
		{"", "''"},                         // empty must not vanish into nothing
		{"/home/t/My Notes", "'/home/t/My Notes'"},
		{"/home/t/it's", `'/home/t/it'\''s'`},
		{"/home/t/$HOME", "'/home/t/$HOME'"},
		{"/home/t/a*b", "'/home/t/a*b'"},
		{"~/Documents", "'~/Documents'"}, // ~ must not stay bare: it would expand
	} {
		if got := Quote(tc.in); got != tc.want {
			t.Errorf("Quote(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The assertion that matters is not the string shape, it is that a real shell hands the
// path back as ONE argument. Anything else is a command the user pastes and watches
// half-run against the wrong directory.
func TestQuotedPathSurvivesARealShell(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	for _, p := range []string{
		"/tmp/plain",
		"/tmp/My Notes",
		"/tmp/it's a vault",
		"/tmp/two  spaces",
		"/tmp/$HOME and `backticks`",
		"/tmp/star*glob?",
		filepath.Join("/tmp", "trailing "),
	} {
		// printf %s with one directive: a path that splits into N arguments prints
		// only the first, so the comparison catches splitting AND expansion.
		out, err := exec.Command("sh", "-c", "printf %s "+Quote(p)).Output()
		if err != nil {
			t.Errorf("sh rejected %q (quoted %s): %v", p, Quote(p), err)
			continue
		}
		if got := string(out); got != p {
			t.Errorf("shell received %q, want %q (quoted as %s)", got, p, Quote(p))
		}
	}
}

// A vault path with a space is the case that motivated this: macOS puts notes under
// ~/Documents and iCloud Drive, both of which contain spaces upstream.
func TestPrintedRemedyIsOneArgument(t *testing.T) {
	root := "/Users/t/Library/Mobile Documents/com~apple~CloudDocs/vault"
	remedy := "mesh index " + Quote(root)
	if !strings.Contains(remedy, "'") {
		t.Fatalf("remedy %q left a spaced path bare; pasting it indexes the wrong directory", remedy)
	}
	out, err := exec.Command("sh", "-c", "set -- "+Quote(root)+`; printf %s "$#"`).Output()
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "1" {
		t.Errorf("the pasted remedy expanded to %s arguments, want 1", out)
	}
}
