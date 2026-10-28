package main

const (
	minTerminalWidth  = 1
	minTerminalHeight = 1
	maxTerminalWidth  = 320
	maxTerminalHeight = 120
	maxTerminalCells  = maxTerminalWidth * maxTerminalHeight
	maxFeedMessages   = 500
	maxCachedMessages = 500
)

// Dimensions originate in unauthenticated SSH requests. Bound them before any
// viewport, padding or row allocation, including the initial login form.
func normalizeTerminalSize(width, height int) (int, int) {
	width = max(minTerminalWidth, min(maxTerminalWidth, width))
	height = max(minTerminalHeight, min(maxTerminalHeight, height))
	if width*height > maxTerminalCells {
		height = max(minTerminalHeight, maxTerminalCells/width)
	}
	return width, height
}
