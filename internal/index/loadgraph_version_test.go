// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func TestExpectedNoteVersionAndLoadedGraphShareOneSnapshot(t *testing.T) {
	root := t.TempDir()
	notePath := filepath.Join(root, "published.md")
	if err := os.WriteFile(notePath, []byte("---\nid: published\ntype: note\n---\n# Published\nexpected bytes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	owner, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if _, err := Reindex(owner, root); err != nil {
		t.Fatal(err)
	}
	hash, err := owner.NoteRetrievalHash("note:published")
	if err != nil {
		t.Fatal(err)
	}
	reader, err := OpenReadOnly(root)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	g, matched, err := reader.loadGraphAtNoteVersion("published", "published.md", hash, func() {
		// Commit the exact turnover that used to fit between point validation and
		// LoadGraph. WAL lets this writer commit while the reader retains its snapshot.
		if werr := owner.Write(func(tx *sql.Tx) error {
			if _, err := tx.Exec(`DELETE FROM edges WHERE source = 'note:published' OR target = 'note:published'`); err != nil {
				return err
			}
			if _, err := tx.Exec(`DELETE FROM nodes WHERE id = 'note:published' OR note_id = 'published'`); err != nil {
				return err
			}
			_, err := tx.Exec(`DELETE FROM notes WHERE id = 'published'`)
			return err
		}); werr != nil {
			t.Fatal(werr)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if !matched {
		t.Fatal("expected version was present at the snapshot boundary")
	}
	if _, ok := g.Node("note:published"); !ok {
		t.Fatal("success returned a graph from after the expected note was removed")
	}
	if current, err := reader.NoteRetrievalHash("note:published"); err != nil || current != "" {
		t.Fatalf("test turnover did not commit: hash=%q err=%v", current, err)
	}
	if current, err := reader.NoteVersionMatches("published", "published.md", hash); err != nil || current {
		t.Fatalf("post-snapshot validation accepted the removed row: current=%v err=%v", current, err)
	}
}
