package main

import (
	"context"
	"io"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

func TestFormFocusPromptBlinksMovesAndNeverEntersCredentials(t *testing.T) {
	s := testStore(t)
	m := newModel(s, newHub(), context.Background(), io.Discard, "xterm-256color", 100, 30, "192.0.2.10", func() {})
	m.fields[0].SetValue("中文用户")
	m.fields[1].SetValue("secret-current-password")
	m.frame = 0
	if !strings.HasPrefix(ansi.Strip(m.formInput(m.fields[0], 30)), "> ") {
		t.Fatal("selected login field has no visible prompt")
	}
	m.frame = 1
	if strings.HasPrefix(ansi.Strip(m.formInput(m.fields[0], 30)), "> ") {
		t.Fatal("focus prompt did not enter its blink-off frame")
	}
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = updated.(model)
	m.frame = 2
	if m.fields[0].Focused() || !m.fields[1].Focused() || !strings.HasPrefix(ansi.Strip(m.formInput(m.fields[1], 30)), "> ") {
		t.Fatal("prompt did not follow keyboard focus")
	}
	if strings.Contains(m.View(), "secret-current-password") {
		t.Fatal("focus prompt exposed the password")
	}
	if m.fields[0].Value() != "中文用户" || m.fields[1].Value() != "secret-current-password" {
		t.Fatal("display prompt changed credential values")
	}
	for _, h := range m.hits() {
		if h.action == "field" && h.value == "0" {
			updated, _ = m.Update(tea.MouseMsg{X: h.x + 1, Y: h.y, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
			m = updated.(model)
			break
		}
	}
	if !m.fields[0].Focused() || m.fields[1].Focused() {
		t.Fatal("prompt did not follow mouse focus")
	}
	// Reduced-motion mode retains a steady visible focus indicator.
	m.settings["motion"] = "off"
	m.frame = 1
	if !strings.HasPrefix(ansi.Strip(m.formInput(m.fields[0], 30)), "> ") {
		t.Fatal("reduced-motion mode lost the focus indicator")
	}
	for _, width := range []int{40, 60, 100} {
		m.w, m.h = width, 24
		for _, line := range strings.Split(m.View(), "\n") {
			if ansi.StringWidth(line) > width {
				t.Fatal("focus prompt caused an authentication form overflow", width)
			}
		}
	}
}
