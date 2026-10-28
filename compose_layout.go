package main

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

func (m model) composeControls() (string, []hit) {
	x, y := 1, max(0, m.h-3)
	var labels []string
	var hits []hit
	for _, button := range []struct{ label, key string }{{"[F2 预览/编辑]", "f2"}, {"[F4 发布]", "f4"}, {"[Esc 取消]", "esc"}} {
		width := ansi.StringWidth(button.label)
		labels = append(labels, button.label)
		if visible := min(width, m.w-x); visible > 0 && y < m.h {
			hits = append(hits, hit{x, y, visible, 1, "compose", button.key})
		}
		x += width + 2
	}
	return m.fitLine(m.color(" "+strings.Join(labels, "  "), m.accent(), false), m.w), hits
}
