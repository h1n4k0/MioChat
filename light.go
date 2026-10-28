package main

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/muesli/termenv"
)

var sgrPattern = regexp.MustCompile("\x1b\\[([0-9;]*)m")

// Nested SGR resets must restore the light palette instead of the client's dark
// default. Extended RGB components are consumed together, never as SGR opcodes.
func (m model) lightPalette(text string) string {
	fg, bg := "\x1b[30m", "\x1b[47m"
	switch m.renderer.ColorProfile() {
	case termenv.TrueColor:
		fg = "\x1b[38;2;36;41;47m"
		bg = "\x1b[48;2;246;248;250m"
	case termenv.ANSI256:
		fg = "\x1b[38;5;235m"
		bg = "\x1b[48;5;255m"
	case termenv.Ascii:
		return text
	}
	base := "\x1b[0m" + fg + bg
	painted := sgrPattern.ReplaceAllStringFunc(text, func(sequence string) string {
		raw := strings.TrimSuffix(strings.TrimPrefix(sequence, "\x1b["), "m")
		if raw == "" {
			return base
		}
		params := strings.Split(raw, ";")
		var result strings.Builder
		for i := 0; i < len(params); i++ {
			op, _ := strconv.Atoi(params[i])
			switch {
			case op == 0:
				result.WriteString(base)
			case op == 39:
				result.WriteString(fg)
			case op == 49 || op >= 40 && op <= 47 || op >= 100 && op <= 107:
				result.WriteString(bg)
			case op == 38 || op == 48 || op == 58:
				end := i + 1
				if end < len(params) {
					if params[end] == "5" {
						end = min(len(params), i+3)
					} else if params[end] == "2" {
						end = min(len(params), i+5)
					} else {
						end++
					}
				}
				if op == 48 {
					result.WriteString(bg)
				} else {
					result.WriteString("\x1b[" + strings.Join(params[i:end], ";") + "m")
				}
				i = end - 1
			default:
				result.WriteString("\x1b[" + params[i] + "m")
			}
		}
		return result.String()
	})
	// A differential renderer may skip an unchanged blank row. Encode its
	// palette explicitly so switching light/dark also repaints empty cells.
	rows := strings.Split(painted, "\n")
	for i, row := range rows {
		rows[i] = base + row + "\x1b[0m"
	}
	return strings.Join(rows, "\n")
}
