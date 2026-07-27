// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import "testing"

// A notice is only worth printing if acting on it is an improvement. README.md and
// CLAUDE.md are supposed to shout, so telling an author to rename them is noise that
// trains people to skim past the notices that do matter.
func TestConventionalDocsAreNotFlaggedAsBadlyNamed(t *testing.T) {
	exempt := []string{
		"README.md",
		"CLAUDE.md",
		"ORGANIZATION.md",
		"LICENSE.md",
		"CONTRIBUTING.md",
		"AGENTS.md",
		"CHANGELOG.md",
		"NOTICE.md",
	}
	for _, name := range exempt {
		if !isConventionalDoc(name) {
			t.Errorf("%s is a conventional all-caps document and must not be reported as badly named", name)
		}
	}

	// Real notes must still be held to the kebab rule; the exemption is for SHOUTING
	// names only, not for any name that merely contains a capital.
	notExempt := []string{
		"My Note.md",
		"Readme.md",
		"SomeNote.md",
		"mixedCASE.md",
		"dockyard-vault-read-route-gap.md",
	}
	for _, name := range notExempt {
		if isConventionalDoc(name) {
			t.Errorf("%s is an ordinary note filename and must still be checked against the kebab rule", name)
		}
	}
}

func TestIsKebabStillRejectsRealOffenders(t *testing.T) {
	bad := []string{"My Note.md", "Some_Note.md", "UPPER lower.md"}
	for _, name := range bad {
		if isKebab(name) {
			t.Errorf("isKebab(%q) = true, want false", name)
		}
	}
	good := []string{"a-note.md", "dockyard-vault-read-route-gap.md", "note.md"}
	for _, name := range good {
		if !isKebab(name) {
			t.Errorf("isKebab(%q) = false, want true", name)
		}
	}
}
