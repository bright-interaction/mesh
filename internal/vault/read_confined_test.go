// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestConfinedReadPoliciesAndByteLimits(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "group"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "group", "note.md"), []byte("payload"), 0600); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.md"), []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{"link": filepath.Join(outside, "secret.md"), "alias": outside} {
		if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	for name, read := range map[string]func(context.Context, string, string, int) ([]byte, error){
		"joined": ReadConfinedFile, "caller_owned_wait": ReadConfinedFileContext,
	} {
		t.Run(name, func(t *testing.T) {
			for _, limit := range []int{0, len("payload")} {
				body, err := read(nil, root, "group/note.md", limit)
				if err != nil || string(body) != "payload" {
					t.Fatalf("normal confined read: %q, %v", body, err)
				}
			}
			if body, err := read(context.Background(), root, "group/note.md", 6); body != nil || !errors.Is(err, ErrConfinedFileTooLarge) {
				t.Fatalf("limit did not reject rather than truncate: %q, %v", body, err)
			}
			for _, path := range []string{"link", "alias/secret.md", "group", "../secret.md", filepath.Join(outside, "secret.md")} {
				if body, err := read(context.Background(), root, path, 0); body != nil || err == nil {
					t.Fatalf("unsafe path %q returned content", path)
				}
			}
			if _, err := read(context.Background(), root, "group/note.md", -1); err == nil {
				t.Fatal("negative limit accepted")
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if body, err := read(ctx, root, "group/note.md", 0); body != nil || !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled read returned content: %q, %v", body, err)
			}
		})
	}
}
