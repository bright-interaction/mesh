package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/retrieve"
	tea "github.com/charmbracelet/bubbletea"
)

// ---- LocalBackend over a real indexed vault ----

func TestLocalBackend(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body string) {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("hub.md", "---\nid: hub\ntype: note\nwhen: 2026-01-01\ntags: [core]\n---\n# Hub\nthe storage decision links [[alpha]] and [[beta]]\n")
	write("alpha.md", "---\nid: alpha\ntype: note\nwhen: 2026-01-01\n---\n# Alpha\nsee [[beta]]\n")
	write("beta.md", "---\nid: beta\ntype: note\nwhen: 2026-01-01\n---\n# Beta\nleaf\n")

	be, closeFn, err := NewLocalBackend(dir)
	if err != nil {
		t.Fatalf("NewLocalBackend: %v", err)
	}
	defer closeFn()

	notes := be.Notes()
	if len(notes) != 3 {
		t.Fatalf("want 3 notes, got %d", len(notes))
	}
	if notes[0].ID != "hub" {
		t.Fatalf("notes should be hub-first (highest degree), got %q", notes[0].ID)
	}

	cards, err := be.Search("storage decision")
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) == 0 {
		t.Fatal("search returned no cards")
	}

	d, err := be.Note("hub")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d.Body, "# Hub") {
		t.Fatalf("note body missing: %q", d.Body)
	}
	if len(d.Neighbors) == 0 {
		t.Fatal("hub should have neighbors (alpha, beta)")
	}
	for _, n := range d.Neighbors {
		if n.ID == "hub" {
			t.Fatal("a note must not be its own neighbor")
		}
	}
}

// ---- App model, driven headlessly (no TTY) ----

type stubBackend struct{ notes []NoteRef }

func (s stubBackend) Notes() []NoteRef { return s.notes }
func (s stubBackend) Search(q string) ([]retrieve.Card, error) {
	if q == "" {
		return nil, nil
	}
	return []retrieve.Card{{NoteID: "b", Title: "Beta", Tier0: true}, {NoteID: "a", Title: "Alpha"}}, nil
}
func (s stubBackend) Note(id string) (NoteDetail, error) {
	return NoteDetail{ID: id, Title: "T-" + id, Path: id + ".md", Body: "# " + id + "\nbody line", Neighbors: []NeighborRef{{ID: "x", Title: "X", Rel: "references", Dir: "out"}}}, nil
}

func runes(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

func TestAppNavigationHeadless(t *testing.T) {
	be := stubBackend{notes: []NoteRef{{ID: "a", Title: "Alpha", Degree: 5}, {ID: "b", Title: "Beta", Degree: 3}, {ID: "c", Title: "Gamma", Degree: 1}}}
	var a tea.Model = NewApp(be)
	send := func(m tea.Msg) tea.Cmd {
		next, cmd := a.Update(m)
		a = next
		return cmd
	}
	app := func() *App { return a.(*App) }

	send(tea.WindowSizeMsg{Width: 120, Height: 40})
	if app().View() == "" {
		t.Fatal("view empty after sizing")
	}
	if app().curID != "a" {
		t.Fatalf("preview should auto-load the first note, got %q", app().curID)
	}

	send(tea.KeyMsg{Type: tea.KeyTab})
	if app().focus != focusResults {
		t.Fatalf("tab should focus results, got %d", app().focus)
	}
	send(tea.KeyMsg{Type: tea.KeyShiftTab})
	if app().focus != focusNotes {
		t.Fatal("shift+tab should return to notes")
	}

	send(runes("j"))
	if app().notesCur != 1 || app().curID != "b" {
		t.Fatalf("j should move + live-load preview: cur=%d id=%q", app().notesCur, app().curID)
	}

	send(runes("/"))
	if !app().editing || app().focus != focusResults {
		t.Fatal("/ should focus results and enter editing")
	}
	send(runes("beta"))
	send(tea.KeyMsg{Type: tea.KeyEnter})
	if app().editing {
		t.Fatal("enter should exit editing")
	}
	if len(app().cards) != 2 || app().curID != "b" {
		t.Fatalf("search should load cards + preview first hit: cards=%d id=%q", len(app().cards), app().curID)
	}

	// q while editing must type into the search box, not quit.
	send(runes("/"))
	send(runes("q"))
	if !strings.Contains(app().search.Value(), "q") {
		t.Fatalf("q while editing must type into search, not quit (value=%q)", app().search.Value())
	}
	send(tea.KeyMsg{Type: tea.KeyEsc})

	send(runes("?"))
	if !app().help || app().View() == "" {
		t.Fatal("? should open a non-empty help view")
	}
	send(tea.KeyMsg{Type: tea.KeyEsc})
	if app().help {
		t.Fatal("esc should close help")
	}

	if cmd := send(runes("q")); cmd == nil {
		t.Fatal("q should return a quit command")
	}
}

func isQuit(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	_, ok := cmd().(tea.QuitMsg)
	return ok
}

func TestCtrlCQuitsEverywhere(t *testing.T) {
	be := stubBackend{notes: []NoteRef{{ID: "a", Title: "A"}}}
	// In raw mode ctrl+c is a keystroke; it must quit from every mode.
	for _, setup := range []func(*App){
		func(a *App) {},                                                          // base
		func(a *App) { a.editing = true },                                        // editing search
		func(a *App) { a.help = true },                                           // help open
	} {
		a := NewApp(be)
		a.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
		setup(a)
		_, cmd := a.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
		if !isQuit(cmd) {
			t.Fatal("ctrl+c must quit from every mode")
		}
	}
}

func TestNotesCursorStaysVisible(t *testing.T) {
	notes := make([]NoteRef, 100)
	for i := range notes {
		notes[i] = NoteRef{ID: string(rune('a' + i%26)), Title: "note"}
	}
	var a tea.Model = NewApp(stubBackend{notes: notes})
	a.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	app := a.(*App)
	for i := 0; i < 60; i++ {
		a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	}
	rows := app.listRows() - 1 // notesView renders this many rows under the title
	if app.notesCur < app.notesOff || app.notesCur >= app.notesOff+rows {
		t.Fatalf("cursor %d out of the visible window [%d, %d)", app.notesCur, app.notesOff, app.notesOff+rows)
	}
}

func TestAppEmptySearchNoPanic(t *testing.T) {
	a := NewApp(stubBackend{notes: []NoteRef{{ID: "a", Title: "A"}}})
	a.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	a.runSearch("") // empty query clears, no crash
	if len(a.cards) != 0 {
		t.Fatal("empty query should clear cards")
	}
	_ = a.View()
}
