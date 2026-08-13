// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/pflag"
)

// `mesh mcp` is NOT a read-only surface any more, and the docs have to stop saying it is.
//
// It elects itself the vault's owning writer and opens the index WRITABLE, which means
// ensureSchema runs on it: index a vault at the current schema_version, stamp the file
// back one version, start `mesh mcp`, and the index is dropped and rebuilt. That is
// correct behaviour (the index is derived from the notes, nothing is lost), but the
// README told the reader the opposite, listing `mesh mcp` among the surfaces that "open
// the index read-only and cannot write it". A reader who believes that expects starting
// their agent to be inert against a stale index, and reaches for the wrong diagnosis
// when their index is suddenly rebuilt.
//
// This guard is a sentence-level check rather than a search for one banned string,
// because the same claim was written two different ways in one file and a fix that
// landed on one wording is exactly how this estate loses the other.

// readOnlyClaimSentences returns every sentence in text that both claims something is
// read-only and names `mesh mcp`. Whitespace is collapsed first so a claim that wraps
// across lines is still one sentence.
func readOnlyClaimSentences(text string) []string {
	flat := strings.Join(strings.Fields(text), " ")
	var bad []string
	for _, s := range strings.Split(flat, ". ") {
		low := strings.ToLower(s)
		if !strings.Contains(low, "read-only") && !strings.Contains(low, "read only") {
			continue
		}
		if strings.Contains(low, "mesh mcp") {
			bad = append(bad, strings.TrimSpace(s))
		}
	}
	return bad
}

func TestDocsDoNotCallMeshMCPReadOnly(t *testing.T) {
	root := filepath.Join("..", "..")
	// The vault/ tree is sample knowledge content, not Mesh's own documentation, and
	// _archive is by definition history.
	skipDirs := map[string]bool{"vault": true, "_archive": true, "node_modules": true}

	var scanned int
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		scanned++
		for _, s := range readOnlyClaimSentences(string(b)) {
			rel, _ := filepath.Rel(root, path)
			t.Errorf("%s describes `mesh mcp` as read-only:\n  %s\n\n"+
				"`mesh mcp` elects itself the owning writer and opens the index writable, so it can "+
				"(and does) rebuild the index on a schema-version change. Name the surfaces that are "+
				"genuinely read-only (`mesh doctor`, `mesh ui` without --own-index, the TUI) and say "+
				"what `mesh mcp` actually does.", rel, s)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk docs: %v", err)
	}
	if scanned < 5 {
		t.Fatalf("only %d markdown files scanned; this guard is not looking at the docs it claims to", scanned)
	}
}

// TestMCPHelpDoesNotPromiseReadOnly covers the other place the claim could live: the
// command's own help, which is what a user reads when they never open the README.
func TestMCPHelpDoesNotPromiseReadOnly(t *testing.T) {
	c := mcpCmd()
	texts := []string{c.Short, c.Long}
	c.Flags().VisitAll(func(f *pflag.Flag) { texts = append(texts, f.Usage) })
	for _, s := range texts {
		low := strings.ToLower(s)
		if strings.Contains(low, "read-only") || strings.Contains(low, "read only") {
			t.Errorf("`mesh mcp` help calls itself read-only:\n  %s", s)
		}
	}
	// Guard the guard: mcpCmd must still be the command this test thinks it is.
	if !strings.HasPrefix(c.Use, "mcp") {
		t.Fatalf("mcpCmd().Use = %q; this test is pointed at the wrong command", c.Use)
	}
}
