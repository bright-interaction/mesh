// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package merge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestLongSiblingNamesRemainWritableReversibleAndDistinct(t *testing.T) {
	for _, stem := range []string{strings.Repeat("a", 170), strings.Repeat("a", 252), strings.Repeat("界", 84), "." + strings.Repeat("b", 246)} {
		for _, user := range []string{"alice", "upsert-guard-0123456789abcdef0123456789abcdef-0", strings.Repeat("long-user", 1000)} {
			base := filepath.Join("notes", stem+".md")
			sib := SiblingPath(base, testTime, user, []byte("mine"))
			for _, part := range strings.Split(filepath.ToSlash(sib), "/") {
				if len(part) > 240 || !utf8.ValidString(part) {
					t.Fatalf("invalid component (%d bytes): %q", len(part), part)
				}
			}
			if got, ok := BasePath(sib); !ok || got != filepath.ToSlash(base) {
				t.Fatalf("base lost: %q -> %q (%v), want %q", sib, got, ok, base)
			}
			if sib != SiblingPath(base, testTime, user, []byte("mine")) ||
				sib == SiblingPath(base, testTime, user, []byte("different")) ||
				sib == SiblingPath(base, testTime, user+"-other", []byte("mine")) ||
				sib == SiblingPath(filepath.Join("notes", stem+"b.md"), testTime, user, []byte("mine")) {
				t.Fatal("sibling identity is not stable and distinct")
			}
			abs := filepath.Join(t.TempDir(), sib)
			if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(abs, []byte("mine"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestBasePathRejectsMalformedOverflow(t *testing.T) {
	for _, p := range []string{
		"notes/a.sync-conflict-base-v1/plain.md",
		"notes/.sync-conflict-base-v1/b.sync-conflict-20260101-user-1234.md",
		"notes/a.sync-conflict-base-v1/b.sync-conflict-base-v1/c.sync-conflict-20260101-user-1234.md",
	} {
		if b, ok := BasePath(p); ok {
			t.Fatalf("accepted %q as %q", p, b)
		}
	}
}
