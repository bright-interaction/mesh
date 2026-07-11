// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package tui

import (
	"fmt"
	"strings"

	"github.com/bright-interaction/mesh/internal/retrieve"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const (
	focusNotes = iota
	focusResults
	focusPreview
)

// App is the parent bubbletea model: three panes (notes list, search results,
// preview) with focus arbitration and a single keymap.
type App struct {
	be            Backend
	width, height int
	focus         int
	help          bool
	lastErr       string

	notes              []NoteRef
	notesCur, notesOff int

	search             textinput.Model
	editing            bool
	cards              []retrieve.Card
	cardsCur, cardsOff int
	lastQuery          string

	preview viewport.Model
	curID   string // note id currently in the preview

	colW     [3]int // inner content widths of the three panes
	contentH int    // inner content height
}

// NewApp builds the model over a Backend.
func NewApp(be Backend) *App {
	ti := textinput.New()
	ti.Placeholder = "search the vault"
	ti.Prompt = "/ "
	a := &App{be: be, focus: focusNotes, search: ti, preview: viewport.New(0, 0)}
	a.notes = be.Notes()
	return a
}

// Run opens the vault and runs the TUI in the alternate screen.
func Run(vaultRoot string) error {
	be, closeFn, err := NewLocalBackend(vaultRoot)
	if err != nil {
		return err
	}
	defer closeFn()
	if len(be.Notes()) == 0 {
		fmt.Println("no notes indexed yet. run: mesh index")
		return nil
	}
	_, err = tea.NewProgram(NewApp(be), tea.WithAltScreen()).Run()
	return err
}

func (a *App) Init() tea.Cmd { return nil }

func (a *App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		a.width, a.height = msg.Width, msg.Height
		a.layout()
		if a.curID == "" && len(a.notes) > 0 {
			a.loadPreview(a.notes[a.notesCur].ID)
		}
		return a, nil
	case tea.KeyMsg:
		return a.handleKey(msg)
	}
	return a, nil
}

func (a *App) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	if k == "ctrl+c" { // raw mode delivers ^C as a keystroke, not a signal: always quit
		return a, tea.Quit
	}
	if a.help {
		if k == "q" {
			return a, tea.Quit
		}
		if k == "?" || k == "esc" {
			a.help = false
		}
		return a, nil
	}
	// While editing the search box, all keys go to the input except esc/enter.
	if a.editing {
		switch k {
		case "esc":
			a.editing = false
			a.search.Blur()
		case "enter":
			a.editing = false
			a.search.Blur()
			a.runSearch(a.search.Value())
		default:
			var cmd tea.Cmd
			a.search, cmd = a.search.Update(msg)
			return a, cmd
		}
		return a, nil
	}
	switch k {
	case "q", "ctrl+c":
		return a, tea.Quit
	case "?":
		a.help = true
	case "tab":
		a.focus = (a.focus + 1) % 3
	case "shift+tab":
		a.focus = (a.focus + 2) % 3
	case "/":
		a.focus = focusResults
		a.editing = true
		return a, a.search.Focus()
	case "up", "k":
		a.move(-1)
	case "down", "j":
		a.move(+1)
	case "pgup":
		a.move(-10)
	case "pgdown":
		a.move(+10)
	case "enter":
		a.openSelected()
	}
	return a, nil
}

// move navigates the focused list or scrolls the preview.
func (a *App) move(delta int) {
	switch a.focus {
	case focusNotes:
		if len(a.notes) == 0 {
			return
		}
		a.notesCur = clamp(a.notesCur+delta, 0, len(a.notes)-1)
		a.notesOff = scrollWindow(a.notesCur, a.notesOff, a.listRows()-1) // minus the NOTES title row
		a.loadPreview(a.notes[a.notesCur].ID)
	case focusResults:
		if len(a.cards) == 0 {
			return
		}
		a.cardsCur = clamp(a.cardsCur+delta, 0, len(a.cards)-1)
		a.cardsOff = scrollWindow(a.cardsCur, a.cardsOff, a.listRows()-2)
		a.loadPreview(a.cards[a.cardsCur].NoteID)
	case focusPreview:
		if delta < 0 {
			a.preview.LineUp(-delta)
		} else {
			a.preview.LineDown(delta)
		}
	}
}

// openSelected drills the selected list row into the preview and focuses it.
func (a *App) openSelected() {
	switch a.focus {
	case focusNotes:
		if len(a.notes) > 0 {
			a.loadPreview(a.notes[a.notesCur].ID)
			a.focus = focusPreview
		}
	case focusResults:
		if len(a.cards) > 0 {
			a.loadPreview(a.cards[a.cardsCur].NoteID)
			a.focus = focusPreview
		}
	}
}

func (a *App) runSearch(q string) {
	q = strings.TrimSpace(q)
	a.lastQuery = q
	a.cards, a.cardsCur, a.cardsOff = nil, 0, 0
	if q == "" {
		return
	}
	cards, err := a.be.Search(q)
	if err != nil {
		a.lastErr = err.Error()
		return
	}
	a.lastErr = ""
	a.cards = cards
	if len(cards) > 0 {
		a.loadPreview(cards[0].NoteID)
	}
}

func (a *App) loadPreview(id string) {
	if id == "" || id == a.curID {
		return
	}
	d, err := a.be.Note(id)
	if err != nil {
		a.lastErr = err.Error()
		return
	}
	a.lastErr = ""
	a.curID = id
	a.preview.SetContent(a.renderDetail(d))
	a.preview.GotoTop()
}

// ---- layout ----
func (a *App) layout() {
	if a.width <= 0 || a.height <= 0 {
		return
	}
	a.contentH = a.height - 2 // header + footer
	if a.contentH < 5 {
		a.contentH = 5
	}
	const overhead = 4 // each pane: border (2) + padding (2)
	notesOuter := a.width * 26 / 100
	resultsOuter := a.width * 32 / 100
	previewOuter := a.width - notesOuter - resultsOuter
	a.colW = [3]int{notesOuter - overhead, resultsOuter - overhead, previewOuter - overhead}
	for i := range a.colW {
		if a.colW[i] < 8 {
			a.colW[i] = 8
		}
	}
	a.search.Width = a.colW[focusResults] - 3
	a.preview.Width = a.colW[focusPreview]
	a.preview.Height = a.contentH - 2 // border top/bottom
}

func (a *App) listRows() int { return a.contentH - 2 } // minus border; pane title sits inside

// ---- view ----
func (a *App) View() string {
	if a.width == 0 {
		return "starting mesh tui..."
	}
	if a.help {
		return a.helpView()
	}
	header := headerStyle.Render("mesh") + faintStyle.Render(fmt.Sprintf("  %d notes", len(a.notes)))
	row := lipgloss.JoinHorizontal(lipgloss.Top, a.notesView(), a.resultsView(), a.previewView())
	return lipgloss.JoinVertical(lipgloss.Left, header, row, a.footerView())
}

func (a *App) notesView() string {
	innerW := a.colW[focusNotes]
	rows := a.listRows() - 1 // title line
	var b strings.Builder
	b.WriteString(titleStyle.Render("NOTES") + "\n")
	end := min(a.notesOff+rows, len(a.notes))
	for i := a.notesOff; i < end; i++ {
		n := a.notes[i]
		line := truncate(n.Title, innerW-2)
		var row string
		if i == a.notesCur {
			row = selectedStyle.Render("> " + line)
		} else {
			row = mutedStyle.Render("  " + line)
		}
		b.WriteString(capWidth(row, innerW) + "\n")
	}
	return paneStyle(a.focus == focusNotes).Width(innerW).Height(a.contentH - 2).Render(b.String())
}

func (a *App) resultsView() string {
	innerW := a.colW[focusResults]
	var b strings.Builder
	b.WriteString(a.search.View() + "\n")
	if len(a.cards) == 0 {
		hint := "press / to search"
		if a.lastQuery != "" {
			hint = "no matches for " + truncate(a.lastQuery, innerW-12)
		}
		b.WriteString(faintStyle.Render(hint))
		return paneStyle(a.focus == focusResults).Width(innerW).Height(a.contentH - 2).Render(b.String())
	}
	rows := a.listRows() - 2 // search line + spacing
	end := min(a.cardsOff+rows, len(a.cards))
	for i := a.cardsOff; i < end; i++ {
		c := a.cards[i]
		marker := "  "
		if c.Tier0 {
			marker = tier0Style.Render("* ")
		}
		title := truncate(c.Title, innerW-4)
		var row string
		if i == a.cardsCur {
			row = marker + selectedStyle.Render(title)
		} else {
			row = marker + mutedStyle.Render(title)
		}
		b.WriteString(capWidth(row, innerW) + "\n")
	}
	return paneStyle(a.focus == focusResults).Width(innerW).Height(a.contentH - 2).Render(b.String())
}

func (a *App) previewView() string {
	return paneStyle(a.focus == focusPreview).Width(a.colW[focusPreview]).Height(a.contentH - 2).Render(a.preview.View())
}

func (a *App) renderDetail(d NoteDetail) string {
	w := a.colW[focusPreview]
	var b strings.Builder
	head := d.Title
	if head == "" {
		head = d.ID
	}
	b.WriteString(selectedStyle.Render(head) + "\n")
	meta := d.Path
	if d.Type != "" {
		meta = d.Type + "  " + d.Path
	}
	b.WriteString(faintStyle.Render(meta) + "\n\n")
	b.WriteString(strings.TrimRight(d.Body, "\n"))
	if len(d.Neighbors) > 0 {
		b.WriteString("\n\n" + titleStyle.Render("NEIGHBORS") + "\n")
		for _, n := range d.Neighbors {
			arrow := "->"
			if n.Dir == "in" {
				arrow = "<-"
			}
			b.WriteString(mutedStyle.Render(fmt.Sprintf("%s %s ", arrow, n.Rel)) + faintStyle.Render(n.Title) + "\n")
		}
	}
	return lipgloss.NewStyle().Width(w).Render(b.String())
}

func (a *App) footerView() string {
	binds := "[tab] pane   [j/k] move   [/] search   [enter] open   [?] help   [q] quit"
	bar := footerStyle.Render(binds)
	if a.lastErr != "" {
		bar += "  " + errStyle.Render(a.lastErr)
	}
	return bar
}

func (a *App) helpView() string {
	lines := []string{
		titleStyle.Render("mesh tui"),
		"",
		"  tab / shift+tab   cycle panes (notes, results, preview)",
		"  j / k, up / down  move in a list, or scroll the preview",
		"  pgup / pgdown     jump by ten",
		"  /                 search the vault (enter runs, esc cancels)",
		"  enter             open the selected note in the preview",
		"  ?                 toggle this help",
		"  q                 quit",
		"",
		faintStyle.Render("  the notes list is hub-first; results are the same ranked"),
		faintStyle.Render("  cards the agent gets; the preview shows the note + its links."),
	}
	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colorAccent).Padding(1, 2)
	return lipgloss.Place(a.width, a.height, lipgloss.Center, lipgloss.Center, box.Render(strings.Join(lines, "\n")))
}

// ---- helpers ----
func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// scrollWindow keeps the cursor visible within a window of `rows`, returning the
// new top offset.
func scrollWindow(cur, off, rows int) int {
	if rows < 1 {
		rows = 1
	}
	if cur < off {
		return cur
	}
	if cur >= off+rows {
		return cur - rows + 1
	}
	return off
}

// capWidth hard-truncates a (possibly styled) line to w display cells, so a
// wide-character title can never overflow the pane and wrap the column.
func capWidth(s string, w int) string {
	if w < 1 {
		return ""
	}
	return lipgloss.NewStyle().MaxWidth(w).Render(s)
}

func truncate(s string, w int) string {
	if w < 1 {
		return ""
	}
	r := []rune(s)
	if len(r) <= w {
		return s
	}
	if w <= 1 {
		return string(r[:w])
	}
	return string(r[:w-1]) + "…"
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
