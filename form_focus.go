package main

import "github.com/charmbracelet/bubbles/textinput"

// Render focus outside the value so the prompt can never enter a username,
// password or pasted key label. Labels remain highlighted between blink frames.
func (m model) formInput(field textinput.Model, width int) string {
	prompt := "  "
	if field.Focused() && (m.settings["motion"] == "off" || m.frame%2 == 0) {
		prompt = m.color("> ", m.accent(), true)
	}
	field.Width = max(1, width-2)
	return prompt + field.View()
}

func (m model) formLabel(label string, field textinput.Model) string {
	if field.Focused() {
		return m.color(label, m.accent(), true)
	}
	return m.color(label, "#93A6BC", false)
}
