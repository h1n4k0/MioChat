package main

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

type panelRow struct {
	text   string
	action int
}

type panelLayout struct {
	rows                  []panelRow
	width, height, offset int
}

// The same wrapped rows drive rendering, scrolling and every mouse target.
func (m model) panelLayout() panelLayout {
	layout := panelLayout{width: max(1, m.w-4), height: max(1, m.h-6)}
	for _, line := range m.panelLines {
		for _, row := range strings.Split(ansi.Hardwrap(line, layout.width, true), "\n") {
			layout.rows = append(layout.rows, panelRow{row, -1})
		}
	}
	if len(layout.rows) > 0 {
		layout.rows = append(layout.rows, panelRow{"", -1})
	}
	actions := m.filteredActions()
	for i, action := range actions {
		prefix := "  "
		if i == m.panelSelected {
			prefix = "> "
		}
		for _, row := range strings.Split(ansi.Hardwrap(prefix+action.Label, layout.width, true), "\n") {
			layout.rows = append(layout.rows, panelRow{m.color(row, m.accent(), i == m.panelSelected), i})
		}
	}
	if len(m.panelActions) > 0 && len(actions) == 0 {
		layout.rows = append(layout.rows, panelRow{"没有匹配项", -1})
	}
	layout.offset = max(0, min(m.panelOffset, max(0, len(layout.rows)-layout.height)))
	return layout
}
