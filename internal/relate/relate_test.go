// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package relate

import (
	"reflect"
	"testing"

	"github.com/bright-interaction/mesh/internal/retrieve"
)

func cards(ids ...string) []retrieve.Card {
	out := make([]retrieve.Card, 0, len(ids))
	for _, id := range ids {
		out = append(out, retrieve.Card{NoteID: id})
	}
	return out
}

func TestSelect(t *testing.T) {
	tags := map[string][]string{
		"same-thread":  {"mesh", "auth"},
		"also-thread":  {"auth"},
		"off-topic":    {"seo"},
		"untagged":     nil,
		"other-thread": {"billing"},
	}
	tagsFor := func(id string) []string { return tags[id] }

	for _, tc := range []struct {
		name     string
		cards    []retrieve.Card
		selfID   string
		selfTags []string
		max      int
		want     []string
	}{
		{
			name:     "top hit is taken on rank alone even with no tag overlap",
			cards:    cards("off-topic", "other-thread"),
			selfTags: []string{"mesh"},
			max:      3,
			want:     []string{"off-topic"},
		},
		{
			name:     "lower ranks need a shared tag",
			cards:    cards("same-thread", "off-topic", "also-thread"),
			selfTags: []string{"mesh", "auth"},
			max:      3,
			want:     []string{"same-thread", "also-thread"},
		},
		{
			name:     "self is never linked",
			cards:    cards("me", "same-thread"),
			selfID:   "me",
			selfTags: []string{"mesh"},
			max:      3,
			want:     []string{"same-thread"},
		},
		{
			name:     "duplicate cards collapse to one link",
			cards:    cards("same-thread", "same-thread", "also-thread"),
			selfTags: []string{"auth"},
			max:      3,
			want:     []string{"same-thread", "also-thread"},
		},
		{
			name:     "an untagged source note gets the top hit and nothing else",
			cards:    cards("same-thread", "also-thread"),
			selfTags: nil,
			max:      3,
			want:     []string{"same-thread"},
		},
		{
			name:     "an untagged candidate cannot pass the gate below rank 1",
			cards:    cards("same-thread", "untagged", "also-thread"),
			selfTags: []string{"auth"},
			max:      3,
			want:     []string{"same-thread", "also-thread"},
		},
		{
			name:     "max caps the result",
			cards:    cards("same-thread", "also-thread"),
			selfTags: []string{"auth"},
			max:      1,
			want:     []string{"same-thread"},
		},
		{
			name:     "max of zero derives nothing",
			cards:    cards("same-thread"),
			selfTags: []string{"auth"},
			max:      0,
			want:     nil,
		},
		{
			name:     "blank note ids are skipped",
			cards:    cards("", "same-thread"),
			selfTags: []string{"auth"},
			max:      2,
			want:     []string{"same-thread"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Select(tc.cards, tagsFor, tc.selfID, tc.selfTags, tc.max)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Select() = %v, want %v", got, tc.want)
			}
		})
	}
}

// Tag matching must not care about case or padding: frontmatter is hand-written and
// "Mesh" / " mesh" are the same tag to everyone except a string compare.
func TestSelectTagMatchIsNormalized(t *testing.T) {
	tagsFor := func(string) []string { return []string{" MESH "} }
	got := Select(cards("first", "second"), tagsFor, "", []string{"mesh"}, 2)
	want := []string{"first", "second"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Select() = %v, want %v (tag match should be case/space insensitive)", got, want)
	}
}
