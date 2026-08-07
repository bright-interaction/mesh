// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package ingest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// durabilityConn serves one doc whose body the test controls, on a deterministic path, so
// a second Run upserts over the note the first Run wrote.
type durabilityConn struct{ body string }

func (c *durabilityConn) Name() string { return "fake" }
func (c *durabilityConn) Key() string  { return "fake:durability" }
func (c *durabilityConn) Pull(_ context.Context, _ time.Time) ([]Doc, bool, error) {
	return []Doc{{ExternalID: "doc-1", Title: "Imported doc", Body: c.body, CreatedAt: "2026-01-01"}}, false, nil
}

// Run landed every imported note with a bare os.WriteFile: O_TRUNC, then write, no fsync.
// Imports are deterministic upserts, so the second pull of an item rewrites a note that is
// already indexed and already synced to the team. Truncating it first opens a window where
// the note reads as EMPTY to the index daemon and to `mesh sync --watch`, and pushes that
// emptiness to the hub; the missing fsync means a power cut can leave it empty for good.
//
// The observable half of the fix is that the note is REPLACED rather than truncated in
// place: file identity changes and a second link to the original still holds the original
// bytes. (The fsync itself cannot be reproduced in a unit test and is pinned by cmd/mesh's
// TestEveryNoteByteWriterFsyncs.)
func TestImportUpsertReplacesInsteadOfTruncating(t *testing.T) {
	vaultRoot := t.TempDir()
	c := &durabilityConn{body: "first version of the imported body"}
	if _, err := Run(context.Background(), vaultRoot, c, time.Time{}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	dir := filepath.Join(vaultRoot, "imported", "fake")
	path := filepath.Join(dir, "fake-doc-1.md")
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("first run wrote no note at %s: %v", path, err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "observer.hardlink")
	linked := os.Link(path, link) == nil // not supported everywhere; only asserted when it is

	c.body = "second version of the imported body"
	res, err := Run(context.Background(), vaultRoot, c, time.Time{})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if res.Written != 1 {
		t.Fatalf("written = %d, want 1", res.Written)
	}

	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, after) {
		t.Errorf("the imported note was rewritten IN PLACE (same file identity), so the upsert still " +
			"truncates a live, indexed, already-synced note before writing it. Want a temp file " +
			"renamed over it.")
	}
	if linked {
		old, err := os.ReadFile(link)
		if err != nil {
			t.Fatal(err)
		}
		if string(old) != string(first) {
			t.Errorf("the original file was mutated in place, so the previous content was destroyed " +
				"before the new content landed")
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "second version of the imported body") {
		t.Errorf("the upsert did not land the new body:\n%s", got)
	}
	if countMD(t, dir) != 1 {
		t.Errorf("re-pull left %d notes, want 1", countMD(t, dir))
	}
	// A stranded temp file in an imported folder is walked as a note, so it would be
	// indexed and synced as a duplicate of the note it was a copy of.
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".mesh-") {
			t.Errorf("temp file %s left behind in %s", e.Name(), dir)
		}
	}
}
