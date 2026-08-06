// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestHealthFindingsDoNotLeakTheVaultRoot. mesh_health is the THIRD surface with the same
// shape as the two the sweep already closed: a vault-relative "path" field sitting next to
// a raw error string. The path reads as handled, so the detail beside it was never looked
// at. It is: the dropped-note errors come from ParseFile, which calls os.ReadFile with the
// ABSOLUTE path, so an unreadable note answered
// detail = "open <abs vault root>/decisions/x.md: permission denied" to any hosted caller
// with a write role.
//
// The vault root in the fixture CONTAINS A SPACE, the estate's real shape, so a scrubber
// that only tokenizes on whitespace cannot pass this.
func TestHealthFindingsDoNotLeakTheVaultRoot(t *testing.T) {
	cases := []struct {
		name string
		// setup lays out the vault and returns the fragment the detail must KEEP, so a
		// scrub that blanks the message cannot pass.
		setup     func(t *testing.T, vaultRoot string) string
		wantIssue string
	}{
		{
			name: "an unreadable note carries a filesystem error",
			setup: func(t *testing.T, vaultRoot string) string {
				if os.Geteuid() == 0 {
					t.Skip("the fixture makes a file unreadable, which root ignores")
				}
				p := filepath.Join(vaultRoot, "decisions", "unreadable.md")
				writeNote(t, p, "unreadable")
				if err := os.Chmod(p, 0o000); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(p, 0o644) })
				return "unreadable.md"
			},
			wantIssue: "unparseable",
		},
		{
			// The other half of the guard: a duplicate-id quarantine names only
			// vault-relative paths, so the scrub must leave it completely alone. Without
			// this case, deleting the detail entirely would still pass.
			name: "a duplicate id quarantine keeps its whole message",
			setup: func(t *testing.T, vaultRoot string) string {
				writeNote(t, filepath.Join(vaultRoot, "decisions", "a.md"), "dup")
				writeNote(t, filepath.Join(vaultRoot, "decisions", "b.md"), "dup")
				return `duplicate note id "dup": already claimed by "decisions/a.md"`
			},
			wantIssue: "duplicate-id",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "Automation HQ", "vault")
			if err := os.MkdirAll(filepath.Join(dir, "decisions"), 0o755); err != nil {
				t.Fatal(err)
			}
			writeNote(t, filepath.Join(dir, "decisions", "seed.md"), "seed")
			keep := tc.setup(t, dir)

			srv, err := NewServer(dir)
			if err != nil {
				t.Fatalf("NewServer: %v", err)
			}
			if err := srv.WaitReady(); err != nil {
				t.Fatalf("WaitReady: %v", err)
			}
			t.Cleanup(func() { srv.Close() })
			if _, err := srv.reconcileOnce(true); err != nil {
				t.Fatalf("reconcile: %v", err)
			}

			out, rerr := srv.toolHealth(WithLocalOperator(context.Background()), json.RawMessage(`{}`))
			if rerr != nil {
				t.Fatalf("mesh_health failed: %s", rerr.Message)
			}
			res, ok := out.(map[string]any)
			if !ok {
				t.Fatalf("tool result is not an object: %#v", out)
			}
			detail, path := findingOf(t, toolText(t, res), tc.wantIssue)

			assertNoVaultRoot(t, detail, dir)
			if !strings.Contains(detail, keep) {
				t.Fatalf("the finding lost the part that makes it actionable (%q): %q", keep, detail)
			}
			// The path field beside it must stay vault-relative, which is what made this
			// leak look handled in the first place.
			if filepath.IsAbs(path) {
				t.Fatalf("the finding's path is absolute: %q", path)
			}
			assertNoVaultRoot(t, path, dir)
		})
	}
}

func writeNote(t *testing.T, path, id string) {
	t.Helper()
	body := "---\nid: " + id + "\ntype: decision\nwhen: 2026-01-01\ndo: x\ndont: y\nwhy: fixture\n---\n# " + id + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// findingOf pulls the detail + path of the first finding with the given issue kind.
func findingOf(t *testing.T, payload map[string]any, issue string) (string, string) {
	t.Helper()
	raw, ok := payload["findings"].([]any)
	if !ok {
		t.Fatalf("mesh_health returned no findings array: %#v", payload)
	}
	for _, f := range raw {
		m, ok := f.(map[string]any)
		if !ok || m["issue"] != issue {
			continue
		}
		detail, _ := m["detail"].(string)
		path, _ := m["path"].(string)
		if detail == "" {
			t.Fatalf("the %s finding has no detail, so there is nothing to scrub: %#v", issue, m)
		}
		return detail, path
	}
	t.Fatalf("no %s finding in %#v", issue, payload)
	return "", ""
}
