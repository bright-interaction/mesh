// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An agent writing notes naturally cross-references its OWN memory files, which live in a
// different store and can never resolve here. One vault had 27 dangling links to a single
// such slug, and the pattern kept returning after each cleanup because "resolves to
// nothing" reads like a typo and sends the author hunting for a note that was never going
// to exist. The message has to name the cause.
func TestForeignStoreLinkExplainsItself(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("src.md", "---\nid: src\ntype: note\ntitle: Src\n---\n\n"+
		"# Src\n\nSee [[feedback_verify_before_shipping]] and [[project_mesh_open_core]].\n"+
		"Also [[some-missing-note]].\n")

	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_, notes, err := ReindexFull(st, dir)
	if err != nil {
		t.Fatal(err)
	}
	_, issues := BuildGraph(notes)

	var memoryMsgs, plainMsgs []string
	for _, is := range issues {
		if is.Kind != "broken-link" {
			continue
		}
		if strings.Contains(is.Msg, "Claude memory filename") {
			memoryMsgs = append(memoryMsgs, is.Msg)
		} else {
			plainMsgs = append(plainMsgs, is.Msg)
		}
	}

	if len(memoryMsgs) != 2 {
		t.Errorf("both memory-store slugs should be named as such, got %d: %v", len(memoryMsgs), memoryMsgs)
	}
	for _, m := range memoryMsgs {
		if !strings.Contains(m, "different store") || !strings.Contains(m, "drop the brackets") {
			t.Errorf("message should say where it lives and what to do, got: %s", m)
		}
	}
	// An ordinary dangling link is NOT a memory slug and must keep the plain message,
	// otherwise the specific advice becomes noise attached to real typos.
	if len(plainMsgs) != 1 || !strings.Contains(plainMsgs[0], "resolves to nothing") {
		t.Errorf("a plain dangling link should keep the plain message, got: %v", plainMsgs)
	}
}

func TestForeignStoreIDShape(t *testing.T) {
	// Real slugs from the estate's memory directory.
	memory := []string{
		"feedback_verify_before_shipping",
		"project_mesh_open_core",
		"reference_mesh_code_index",
		"feedback_incomplete_migrations",
		"user_writing_voice",
	}
	for _, s := range memory {
		if !foreignStoreID.MatchString(s) {
			t.Errorf("%q is a memory filename and should be recognised", s)
		}
	}

	// Vault ids are kebab-case, so they must never be mistaken for one. The last two
	// matter most: a prefix word alone, or a single underscore-free token, is not the
	// memory shape and misreporting it would send an author down the wrong path.
	vault := []string{
		"mesh-open-core",
		"deep-codebase-audit-playbook",
		"feedback",
		"project",
		"some-missing-note",
		"keyvault",
		"pare-verified-sie-4e-bas-2025-26-swedish-moms-2026-spec",
	}
	for _, s := range vault {
		if foreignStoreID.MatchString(s) {
			t.Errorf("%q is a vault id and must NOT be reported as a memory filename", s)
		}
	}
}
