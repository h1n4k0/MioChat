package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

func assertDisplayFrame(t *testing.T, m model) {
	t.Helper()
	rows := strings.Split(m.View(), "\n")
	if len(rows) != m.h {
		t.Fatalf("frame has %d physical rows, want %d", len(rows), m.h)
	}
	for i, row := range rows {
		if width := ansi.StringWidth(row); width != m.w {
			t.Fatalf("row %d has %d cells, want %d: %q", i, width, m.w, ansi.Strip(row))
		}
	}
}

func TestSSHFirstFrameWaitsForRendererDimensions(t *testing.T) {
	s := testStore(t)
	m := newModel(s, newHub(), context.Background(), io.Discard, "xterm-256color", 100, 30, "192.0.2.10", func() {})
	m.awaitingTerminalSize = true
	if m.View() != "" || len(m.hits()) != 0 {
		t.Fatal("uninitialized renderer received a visible or interactive frame")
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = updated.(model)
	if m.awaitingTerminalSize || !strings.Contains(m.View(), "TERMINAL SOCIAL CLUB") {
		t.Fatal("initial identical PTY size did not release the first frame")
	}
	assertDisplayFrame(t, m)
}

func TestSmallTerminalsKeepTheirPhysicalGridAndBlockHiddenControls(t *testing.T) {
	s, h := featureFixture(t)
	base := testModel(t, s, h, "alice")
	for _, size := range [][2]int{{1, 1}, {8, 4}, {20, 10}, {39, 23}} {
		t.Run(fmt.Sprintf("%dx%d", size[0], size[1]), func(t *testing.T) {
			m := base
			m.openMenu(false)
			m.input.SetValue("保留输入")
			updated, _ := m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
			m = updated.(model)
			if m.w != size[0] || m.h != size[1] {
				t.Fatal("physical terminal was enlarged into automatic wrapping")
			}
			assertDisplayFrame(t, m)
			if len(m.hits()) != 0 || len(m.panelHits()) != 0 || len(m.keyDialogHits()) != 0 {
				t.Fatal("invisible controls have active hit targets")
			}
			updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
			m = updated.(model)
			if m.input.Value() != "保留输入" || m.panelTitle == "" {
				t.Fatal("small window activated an invisible action")
			}
			_, quit := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
			if quit == nil {
				t.Fatal("small window could not exit")
			}
		})
	}
}

func TestLightPaletteChangesEveryRowIncludingBlankRows(t *testing.T) {
	s := testStore(t)
	m := newModel(s, newHub(), context.Background(), io.Discard, "xterm-256color", 100, 30, "192.0.2.10", func() {})
	m.renderer.SetColorProfile(termenv.TrueColor)
	m.user.Theme = "light"
	rows := strings.Split(m.View(), "\n")
	for i, row := range rows {
		if !strings.Contains(row, "\x1b[48;2;246;248;250m") {
			t.Fatalf("row %d does not carry its own light background", i)
		}
	}
	m.user.Theme = "aurora"
	dark := strings.Split(m.View(), "\n")
	for i := range rows {
		if rows[i] == dark[i] {
			t.Fatalf("palette change leaves row %d indistinguishable to the differential renderer", i)
		}
	}
}

func TestEveryDisplayModeHasOneBoundedCanvas(t *testing.T) {
	s, h := featureFixture(t)
	base := testModel(t, s, h, "alice")
	for _, size := range [][2]int{{40, 24}, {60, 24}, {80, 24}, {100, 30}, {140, 40}, {320, 120}} {
		for _, mode := range []string{"login", "register", "invite", "chat", "long-chat-header", "menu", "notifications", "key-prompt", "compose", "preview", "password"} {
			t.Run(fmt.Sprintf("%dx%d/%s", size[0], size[1], mode), func(t *testing.T) {
				m := base
				updated, _ := m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
				m = updated.(model)
				switch mode {
				case "login", "register", "invite":
					m.logged = false
					m.register = mode != "login"
					m.formInvite = mode == "invite"
				case "long-chat-header":
					m.user.Name, m.peer = strings.Repeat("中", 20), strings.Repeat("文", 20)
				case "menu":
					m.openMenu(false)
				case "notifications":
					m.openPanel("通知中心", "notifications", nil, []panelAction{{strings.Repeat("中文通知正文", 20), "/help", false}})
				case "key-prompt":
					m.keyPrompt = true
				case "compose", "preview":
					m.startCompose("post", "中文标题")
					m.editor.SetValue("短预览")
					m.composePreview = mode == "preview"
				case "password":
					m.command("/passwd")
				}
				assertDisplayFrame(t, m)
			})
		}
	}
}

func TestVisibleComposeCancelCannotPublish(t *testing.T) {
	for _, preview := range []bool{false, true} {
		t.Run(fmt.Sprintf("preview=%t", preview), func(t *testing.T) {
			s, h := featureFixture(t)
			m := testModel(t, s, h, "alice")
			m.startCompose("post", "还未同意发布")
			m.editor.SetValue("只应保存为私有草稿")
			m.composePreview = preview
			found := false
			for y, row := range strings.Split(m.composeView(), "\n") {
				row = ansi.Strip(row)
				if at := strings.Index(row, "[Esc 取消]"); at >= 0 {
					found = true
					x := ansi.StringWidth(row[:at]) + 2
					m.composeMouse(tea.MouseMsg{X: x, Y: y, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
					break
				}
			}
			if !found {
				t.Fatal("cancel button is not visible")
			}
			var published int
			if err := s.db.QueryRow(`SELECT count(*) FROM objects`).Scan(&published); err != nil {
				t.Fatal(err)
			}
			if published != 0 || m.composeKind != "" {
				t.Fatalf("visible Cancel must close without publishing: objects=%d editor=%q", published, m.composeKind)
			}
			_, saved := s.Composition("alice", "lobby", "post")
			if saved != "只应保存为私有草稿" {
				t.Fatal("cancel lost the owner's draft")
			}
		})
	}
}

func TestOpenPanelFilterReflowsWhenTerminalShrinks(t *testing.T) {
	s, h := featureFixture(t)
	m := testModel(t, s, h, "alice")
	m.openMenu(false)
	m.panelFilter.SetValue(strings.Repeat("x", 70))
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 24})
	m = updated.(model)
	if m.panelFilter.Width > m.w-2 {
		t.Fatal("open filter retained the old terminal width", m.panelFilter.Width, m.w)
	}
	assertDisplayFrame(t, m)
}

func TestWrappedPanelActionsShareVisibleMouseRows(t *testing.T) {
	s, h := featureFixture(t)
	m := testModel(t, s, h, "alice")
	m.w, m.h = 40, 24
	var actions []panelAction
	for i := 0; i < 30; i++ {
		actions = append(actions, panelAction{fmt.Sprintf("动作%02d %s 末尾标记", i, strings.Repeat("中文内容", 9)), fmt.Sprintf("selected-%02d", i), true})
	}
	m.openPanel("长通知", "test", []string{"说明文字"}, actions)
	m.panelSelected = 25
	m.keepPanelSelection()
	if !strings.Contains(ansi.Strip(m.View()), "末尾标记") {
		t.Fatal("selected action lost the wrapped label tail")
	}
	var selected []hit
	for _, hit := range m.panelHits() {
		if hit.value == "25" {
			selected = append(selected, hit)
		}
		if hit.y < 4 || hit.y >= m.h-2 || hit.x < 0 || hit.x+hit.w > m.w {
			t.Fatal("panel hit does not belong to the visible content", hit)
		}
	}
	if len(selected) < 2 {
		t.Fatal("wrapped action has no matching multiline hit regions")
	}
	last := selected[len(selected)-1]
	m.mouseInteractions(tea.MouseMsg{X: last.x + 1, Y: last.y, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	if m.input.Value() != "selected-25" || m.panelTitle != "" {
		t.Fatal("wrapped action's last visible row activated the wrong item", m.input.Value())
	}
}
