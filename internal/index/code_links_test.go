package index

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLinkNotesToCode(t *testing.T) {
	dir := t.TempDir()
	// A note that references a distinctive symbol (RecordReuse) plus generic words
	// (Open, git) that must NOT link.
	note := "---\nid: note-x\ntype: gotcha\n---\n# Reuse note\nThe `RecordReuse` call counts a fetch as reuse. Do not confuse with `Open` or `git`.\n"
	if err := os.WriteFile(filepath.Join(dir, "n.md"), []byte(note), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := Reindex(store, dir); err != nil { // index the note
		t.Fatal(err)
	}

	// Seed the code index with RecordReuse + Open.
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "reuse.go"), []byte("package x\n\nfunc RecordReuse() {}\nfunc Open() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReindexCode(store, []string{root}, nil); err != nil {
		t.Fatal(err)
	}

	n, err := store.LinkNotesToCode(dir)
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("no note<->code links created")
	}
	base := filepath.Base(root)
	recordID := "code:" + base + "/reuse.go#RecordReuse"
	notes, _ := store.NotesForSymbol(recordID)
	if len(notes) != 1 || notes[0].NoteID != "note-x" {
		t.Fatalf("NotesForSymbol(RecordReuse) = %+v, want [note-x]", notes)
	}
	// Generic short names must not link.
	if o, _ := store.NotesForSymbol("code:" + base + "/reuse.go#Open"); len(o) != 0 {
		t.Fatalf("generic 'Open' wrongly linked: %+v", o)
	}
	// Reverse direction.
	syms, _ := store.SymbolsForNote("note-x")
	if len(syms) != 1 || syms[0].Symbol != "RecordReuse" {
		t.Fatalf("SymbolsForNote(note-x) = %+v, want [RecordReuse]", syms)
	}
	if c := store.NoteCountForSymbol(recordID); c != 1 {
		t.Fatalf("NoteCountForSymbol = %d, want 1", c)
	}
}
