// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestVersionReadsCancelWhileConnectionIsBusy(t *testing.T) {
	root := writeVault(t)
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, _, err := ReindexFull(s, root); err != nil {
		t.Fatal(err)
	}
	hashes, err := s.NoteHashes()
	if err != nil {
		t.Fatal(err)
	}
	s.readDB.SetMaxOpenConns(1)
	conn, err := s.readDB.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for _, tc := range []struct {
		name string
		run  func(context.Context) error
	}{
		{"snapshot", func(ctx context.Context) error {
			_, _, err := s.LoadGraphAtNoteVersionContext(ctx, "a", "a.md", hashes["a.md"])
			return err
		}},
		{"final version", func(ctx context.Context) error {
			_, err := s.NoteVersionMatchesContext(ctx, "a", "a.md", hashes["a.md"])
			return err
		}},
		{"fingerprints", func(ctx context.Context) error { _, err := s.NoteHashesContext(ctx); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			if err := tc.run(ctx); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("got %v, want deadline", err)
			}
		})
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	g, matched, err := s.loadGraphAtNoteVersionContext(ctx, "a", "a.md", hashes["a.md"], cancel)
	if !errors.Is(err, context.Canceled) || g != nil || matched {
		t.Fatalf("canceled snapshot was published: graph=%v matched=%v err=%v", g != nil, matched, err)
	}
	g, matched, err = s.LoadGraphAtNoteVersionContext(context.Background(), "a", "a.md", hashes["a.md"])
	if err != nil || !matched || g == nil {
		t.Fatalf("reader did not recover: matched=%v err=%v", matched, err)
	}
}
