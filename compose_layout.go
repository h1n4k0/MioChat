package main

import (
	tea "github.com/charmbracelet/bubbletea"
	"strings"
)

// Projection uses the same wrapped controls as composeView and composeMouse.
// The upstream bounded-frame fix must not remove CAS conflict/discard actions.
func (m model) composeControls() (string, []hit) {
	rows, controls := compositionControlLayout(m.w)
	firstY := m.h - len(rows) - 1
	var hits []hit
	for _, control := range controls {
		y := firstY + control.y
		if width := min(control.width, m.w-control.x); width > 0 && y >= 0 && y < m.h {
			hits = append(hits, hit{control.x, y, width, 1, "compose", (tea.KeyMsg{Type: control.key}).String()})
		}
	}
	return m.color(strings.Join(rows, "\n"), m.accent(), false), hits
}
