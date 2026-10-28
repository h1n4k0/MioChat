package main

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

func (m model) usableViewport() bool { return m.w >= 40 && m.h >= 24 }

func (m model) smallViewportView() string {
	status := "请放大终端窗口，Ctrl+C 退出。"
	if m.emergencyDraft != nil {
		status = "草稿尚未保存，请保持连接并放大窗口。"
	} else if m.positionUnknown {
		status = "草稿加载失败，请保持连接并放大窗口。"
	}
	return fmt.Sprintf("MioChat\n窗口至少需要 40×24，当前 %d×%d。\n%s", m.w, m.h, status)
}

// Every screen owns the same bounded cell grid, including its empty rows.
// Padding is ordinary text: no per-frame clear-screen escape or flicker.
func terminalFrame(text string, width, height int) string {
	width, height = normalizeTerminalSize(width, height)
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "")
	rows := strings.Split(text, "\n")
	blank := strings.Repeat(" ", width)
	frame := make([]string, height)
	for i := range frame {
		if i >= len(rows) {
			frame[i] = blank
			continue
		}
		row := ansi.Truncate(strings.ReplaceAll(rows[i], "\t", "    "), width, "")
		frame[i] = row + strings.Repeat(" ", max(0, width-ansi.StringWidth(row)))
	}
	return strings.Join(frame, "\n")
}

func (m *model) reflowInputWidths() {
	m.input.Width = max(1, m.w-8)
	m.input.SetCursor(m.input.Position())
	m.panelFilter.Width = max(1, m.w-4)
	m.panelFilter.SetCursor(m.panelFilter.Position())
}
