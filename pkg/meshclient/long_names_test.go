// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package meshclient

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/merge"
	"github.com/bright-interaction/mesh/internal/vault"
)

func TestLongNoteGuardedUpsertDeleteAndEditorRace(t *testing.T) {
	for _, stem := range []string{strings.Repeat("a", 170), strings.Repeat("a", 247), strings.Repeat("界", 82)} {
		t.Run(stem[:6], func(t *testing.T) {
			root := t.TempDir()
			rel := "notes/" + stem + ".md"
			old, incoming := []byte("old\n"), []byte("hub\n")
			write(t, root, rel, string(old))
			ok, sib, err := replaceFileDurable(root, rel, true, contentHash(old), incoming)
			if err != nil || !ok || sib != "" {
				t.Fatalf("upsert: %v %q %v", ok, sib, err)
			}
			got, err := os.ReadFile(filepath.Join(root, rel))
			if err != nil || string(got) != string(incoming) {
				t.Fatalf("upsert bytes: %q %v", got, err)
			}
			ok, sib, err = removeFileDurable(root, rel, contentHash(incoming))
			if err != nil || !ok || sib != "" {
				t.Fatalf("delete: %v %q %v", ok, sib, err)
			}
			if _, err := os.Stat(filepath.Join(root, rel)); !os.IsNotExist(err) {
				t.Fatalf("delete left base: %v", err)
			}

			write(t, root, rel, "editor\n")
			ok, sib, err = replaceFileDurable(root, rel, true, contentHash(old), incoming)
			if err != nil || ok || sib == "" {
				t.Fatalf("race: %v %q %v", ok, sib, err)
			}
			if base, valid := merge.BasePath(sib); !valid || filepath.ToSlash(base) != rel {
				t.Fatalf("lost base: %q %v", base, valid)
			}
			got, err = os.ReadFile(filepath.Join(root, rel))
			if err != nil || string(got) != "editor\n" {
				t.Fatalf("editor bytes lost: %q %v", got, err)
			}
			got, err = os.ReadFile(filepath.Join(root, sib))
			if err != nil || string(got) != string(incoming) {
				t.Fatalf("hub bytes lost: %q %v", got, err)
			}
			notes, err := vault.Walk(root)
			if err != nil || len(notes) != 1 {
				t.Fatalf("conflict leaked into notes: %v %v", notes, err)
			}
			siblings, err := vault.WalkConflictSiblings(root)
			if err != nil || len(siblings) != 1 || siblings[0] != filepath.Join(root, sib) {
				t.Fatalf("conflict undiscoverable: %v %v", siblings, err)
			}
		})
	}
}
