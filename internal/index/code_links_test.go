// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

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

// A bare PascalCase word that names a method on many types (Server here) must NOT link:
// an unqualified token only links when it resolves to exactly one symbol. A genuinely
// unique bare name (StartTailnetProxy) still links.
func TestLinkBareTokenRequiresUniqueSymbol(t *testing.T) {
	dir := t.TempDir()
	note := "---\nid: ambi\ntype: note\n---\n# notes\nWe touch the `Server` and also `StartTailnetProxy`.\n"
	if err := os.WriteFile(filepath.Join(dir, "n.md"), []byte(note), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := Reindex(store, dir); err != nil {
		t.Fatal(err)
	}
	// Two packages each define a Server type (ambiguous), one unique StartTailnetProxy.
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\n\ntype Server struct{}\nfunc StartTailnetProxy() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "b.go"), []byte("package b\n\ntype Server struct{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReindexCode(store, []string{root}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LinkNotesToCode(dir); err != nil {
		t.Fatal(err)
	}
	syms, _ := store.SymbolsForNote("ambi")
	for _, s := range syms {
		if s.Symbol == "Server" {
			t.Errorf("ambiguous bare token 'Server' linked to a symbol; it names Server in two packages and must not link")
		}
	}
	// The unique name must still link.
	found := false
	for _, s := range syms {
		if s.Symbol == "StartTailnetProxy" {
			found = true
		}
	}
	if !found {
		t.Errorf("unique bare token 'StartTailnetProxy' did not link; got %+v", syms)
	}
}

// A note that names a symbol only in its TITLE (no backticks in the body) still links.
func TestLinkFromTitle(t *testing.T) {
	dir := t.TempDir()
	// Title names renderMDSafe; the body never backticks it.
	note := "---\nid: xss-note\ntype: gotcha\ntitle: renderMDSafe must sanitize ingested note HTML\n---\n# renderMDSafe must sanitize ingested note HTML\nUntrusted note bodies need the safe renderer to avoid stored XSS.\n"
	if err := os.WriteFile(filepath.Join(dir, "xss.md"), []byte(note), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := Reindex(store, dir); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "render.go"), []byte("package x\n\nfunc renderMDSafe() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReindexCode(store, []string{root}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LinkNotesToCode(dir); err != nil {
		t.Fatal(err)
	}
	notes, _ := store.NotesForSymbolName("renderMDSafe")
	if len(notes) != 1 || notes[0].NoteID != "xss-note" {
		t.Fatalf("title-only reference did not link: %+v", notes)
	}
}
