// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Bright Interaction AB

package tui

import "github.com/charmbracelet/lipgloss"

// Dark, monochrome-with-one-accent palette, matching `mesh ui`. The accent marks
// focus + tier-0; community/type stays muted so the content reads first.
var (
	colorAccent = lipgloss.Color("#f5f2ec") // near-white: focus + selection
	colorDim    = lipgloss.Color("#3a342e") // unfocused borders
	colorMuted  = lipgloss.Color("#a8a199")
	colorFaint  = lipgloss.Color("#6e665c")
	colorTier0  = lipgloss.Color("#e0a86b") // warm marker for decisions/gotchas/post-mortems
	colorErr    = lipgloss.Color("#e0746b")
)

func paneStyle(focused bool) lipgloss.Style {
	color := colorDim
	if focused {
		color = colorAccent
	}
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(color).Padding(0, 1)
}

var (
	titleStyle    = lipgloss.NewStyle().Foreground(colorFaint).Bold(true)
	selectedStyle = lipgloss.NewStyle().Foreground(colorAccent).Bold(true)
	mutedStyle    = lipgloss.NewStyle().Foreground(colorMuted)
	faintStyle    = lipgloss.NewStyle().Foreground(colorFaint)
	tier0Style    = lipgloss.NewStyle().Foreground(colorTier0)
	headerStyle   = lipgloss.NewStyle().Foreground(colorAccent).Bold(true).Padding(0, 1)
	footerStyle   = lipgloss.NewStyle().Foreground(colorFaint).Padding(0, 1)
	errStyle      = lipgloss.NewStyle().Foreground(colorErr)
)
