// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Bright Interaction AB

package merge

import (
	"testing"
	"time"
)

func TestBasePathRoundTrip(t *testing.T) {
	now := time.Date(2026, 6, 16, 0, 0, 0, 0, time.UTC)
	losing := []byte("the losing version body")
	cases := []struct {
		name string
		base string
		user string
	}{
		{"simple", "notes/topic.md", "alice"},
		{"root", "x.md", "bob"},
		{"nested", "deep/dir/area/x.md", "carol"},
		{"hyphen user", "notes/topic.md", "alice-smith"},
		{"hyphen stem lookalike", "notes/a-sync-conflict-b.md", "dave"},
		{"dotted stem", "notes/x.y.md", "erin"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sib := SiblingPath(c.base, now, c.user, losing)
			got, ok := BasePath(sib)
			if !ok {
				t.Fatalf("BasePath(%q) ok=false", sib)
			}
			if got != c.base {
				t.Fatalf("round trip: %q -> %q -> %q (want %q)", c.base, sib, got, c.base)
			}
		})
	}
}

func TestBasePathRejects(t *testing.T) {
	// A plain note is not a sibling.
	if _, ok := BasePath("notes/topic.md"); ok {
		t.Fatal("plain note should not parse as a sibling")
	}
	// A doubly-nested sibling name is garbage: recovered base still looks like a
	// sibling, so refuse rather than mis-resolve.
	if _, ok := BasePath("notes/x.sync-conflict-20260616-alice-aaaaaaaaaaaaaaaa.sync-conflict-20260616-bob-bbbbbbbbbbbbbbbb.md"); ok {
		t.Fatal("doubly-nested sibling should be refused")
	}
	// A crafted empty-stem sibling must not reconstruct a hidden "notes/.md" base.
	if base, ok := BasePath("notes/.sync-conflict-20260616-bob-bbbbbbbbbbbbbbbb.md"); ok {
		t.Fatalf("empty-stem sibling should be refused, got base %q", base)
	}
	if base, ok := BasePath(".sync-conflict-20260616-bob-bbbbbbbbbbbbbbbb.md"); ok {
		t.Fatalf("root empty-stem sibling should be refused, got base %q", base)
	}
}
