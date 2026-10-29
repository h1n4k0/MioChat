package main

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

func TestTUICompositionNormalSavePublishAndCtrlCFailureRetention(t *testing.T) {
	s, h := featureFixture(t)
	m := testModel(t, s, h, "alice")
	m.startCompose("post", "normal TUI")
	m.editor.SetValue("first saved body")
	m.composeKey(tea.KeyMsg{Type: tea.KeyEsc})
	if m.composeKind != "" {
		t.Fatal("normal save-close failed", m.notice)
	}
	m.startCompose("post", "")
	if m.editor.Value() != "first saved body" {
		t.Fatal("reopen lost saved draft")
	}
	m.editor.SetValue("normal published body")
	before := storageScalar(t, s, `SELECT count(*) FROM objects`)
	m.composeKey(tea.KeyMsg{Type: tea.KeyF4})
	if m.composeKind != "" || storageScalar(t, s, `SELECT count(*) FROM objects`) == before {
		t.Fatal("normal TUI publication rejected", m.notice)
	}
	if body := storageScalar(t, s, `SELECT body FROM compositions WHERE user='alice' AND room='lobby' AND kind='post'`); body != "" {
		t.Fatal("published draft was not atomically cleared", body)
	}
	m.startCompose("post", "normal TUI")
	m.editor.SetValue("unacknowledged body")
	if _, err := s.db.Exec(`CREATE TRIGGER compose_exit_fault BEFORE UPDATE ON compositions BEGIN SELECT RAISE(IGNORE);END`); err != nil {
		t.Fatal(err)
	}
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	m = updated.(model)
	if cmd != nil || m.composeKind != "post" || m.editor.Value() != "unacknowledged body" {
		t.Fatal("Ctrl+C lost failed-save text")
	}
	m.composeKey(tea.KeyMsg{Type: tea.KeyF9})
	m.composeKey(tea.KeyMsg{Type: tea.KeyF9}) // repeated/held first key never discards
	if m.composeKind == "" {
		t.Fatal("repeated first key confirmed discard")
	}
	m.composeKey(tea.KeyMsg{Type: tea.KeyF2}) // cancels discard arm
	m.composeKey(tea.KeyMsg{Type: tea.KeyF10})
	if m.composeKind == "" {
		t.Fatal("intervening key did not cancel destructive confirmation")
	}
	m.composeKey(tea.KeyMsg{Type: tea.KeyF9})
	m.composeKey(tea.KeyMsg{Type: tea.KeyF10})
	if m.composeKind != "" || m.editor.Value() != "" {
		t.Fatal("explicit local discard did not close")
	}
	if body := storageScalar(t, s, `SELECT body FROM compositions WHERE user='alice' AND room='lobby' AND kind='post'`); body != "" {
		t.Fatal("discard modified durable draft", body)
	}
}

func TestTUICompositionConflictChoicesBindCapturedRemoteVersion(t *testing.T) {
	for _, choice := range []string{"keep", "reload", "reload-revoked", "keep-newer"} {
		t.Run(choice, func(t *testing.T) {
			s, h := featureFixture(t)
			a, b := testModel(t, s, h, "alice"), testModel(t, s, h, "alice")
			a.startCompose("post", "choices")
			b.startCompose("post", "choices")
			a.editor.SetValue("local unsaved text")
			b.editor.SetValue("remote first text")
			if err := b.saveCompositionDraft(); err != nil {
				t.Fatal(err)
			}
			if err := a.saveCompositionDraft(); err == nil {
				t.Fatal("expected stale CAS")
			}
			a.composeKey(tea.KeyMsg{Type: tea.KeyF6})
			if a.composeRemote == nil || a.editor.Value() != "local unsaved text" {
				t.Fatal("first preview changed local text")
			}
			a.composeKey(tea.KeyMsg{Type: tea.KeyF6}) // held/read repeated must never reload
			if a.editor.Value() != "local unsaved text" {
				t.Fatal("repeated read key discarded local text")
			}
			switch choice {
			case "keep-newer":
				b.editor.SetValue("remote second text")
				if err := b.saveCompositionDraft(); err != nil {
					t.Fatal(err)
				}
				a.composeKey(tea.KeyMsg{Type: tea.KeyF7})
				if a.composeRemote != nil || !a.composeConflict || a.editor.Value() != "local unsaved text" {
					t.Fatal("stale keep auto-retried or lost local text")
				}
				if storageScalar(t, s, `SELECT body FROM compositions WHERE user='alice' AND room='lobby' AND kind='post'`) != "remote second text" {
					t.Fatal("keep overwrote unseen newer draft")
				}
			case "reload-revoked":
				if _, err := s.db.Exec(`UPDATE sessions SET revoked=1 WHERE id=?`, a.client.ID); err != nil {
					t.Fatal(err)
				}
				a.composeKey(tea.KeyMsg{Type: tea.KeyF8})
				if a.editor.Value() != "local unsaved text" || a.composeRemote != nil {
					t.Fatal("reload ignored fresh session revocation")
				}
			case "reload":
				a.composeKey(tea.KeyMsg{Type: tea.KeyF8})
				if a.editor.Value() != "remote first text" || a.composeConflict || a.composeRemote != nil {
					t.Fatal("explicit reload failed", a.notice)
				}
			case "keep":
				a.composeKey(tea.KeyMsg{Type: tea.KeyF7})
				if a.composeConflict || a.composeRemote != nil || storageScalar(t, s, `SELECT body FROM compositions WHERE user='alice' AND room='lobby' AND kind='post'`) != "local unsaved text" {
					t.Fatal("explicit fixed-version keep failed", a.notice)
				}
			}
		})
	}
}

func TestTUICompositionPointerRecoveryUsesActualControlRanges(t *testing.T) {
	for _, width := range []int{20, 40, 80, 120} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			s, h := featureFixture(t)
			m := testModel(t, s, h, "alice")
			m.w, m.h = width, 10
			m.startCompose("post", "pointer")
			m.editor.SetValue("local to discard")
			rows, controls := compositionControlLayout(m.w)
			for _, row := range rows {
				if ansi.StringWidth(row) > width {
					t.Fatal("wrapped control overflow", row)
				}
			}
			// Upstream's bounded-frame policy intentionally disables controls
			// below 40x24. Hidden compose controls must not discard a draft.
			for _, size := range [][2]int{{width, 10}, {width, 24}} {
				if size[0] >= 40 && size[1] >= 24 {
					continue
				}
				updated, _ := m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
				m = updated.(model)
				if m.usableViewport() || len(m.hits()) != 0 || !strings.Contains(ansi.Strip(m.View()), "窗口至少") {
					t.Fatal("small viewport did not hide composition controls", m.View())
				}
				for _, control := range controls {
					y := len(strings.Split(m.composeView(), "\n")) - 1 - len(rows) + control.y
					updated, _ := m.Update(tea.MouseMsg{X: control.x, Y: y, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
					m = updated.(model)
					if m.editor.Value() != "local to discard" || m.composeKind == "" || m.composeDiscardArmed {
						t.Fatal("hidden small-viewport pointer changed local draft")
					}
				}
			}
			updated, _ := m.Update(tea.WindowSizeMsg{Width: max(40, width), Height: 24})
			m = updated.(model)
			if !m.usableViewport() || m.editor.Value() != "local to discard" {
				t.Fatal("resize did not retain editable local draft")
			}
			if len(strings.Split(m.composeView(), "\n")) > m.h {
				t.Fatal("controls pushed outside terminal", m.composeView())
			}
			click := func(key tea.KeyType) {
				name := (tea.KeyMsg{Type: key}).String()
				for _, hit := range m.hits() {
					if hit.action == "compose" && hit.value == name {
						view := strings.Split(ansi.Strip(m.View()), "\n")
						if hit.y < 0 || hit.y >= len(view) || !strings.Contains(view[hit.y], strings.ToUpper(name)) {
							t.Fatal("pointer range not on the rendered control", name, hit, m.View())
						}
						updated, _ := m.Update(tea.MouseMsg{X: hit.x, Y: hit.y, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
						m = updated.(model)
						return
					}
				}
				t.Fatal("missing TUI pointer control")
			}
			click(tea.KeyF6)
			click(tea.KeyF6)
			if m.editor.Value() != "local to discard" {
				t.Fatal("repeated F6 pointer discarded local text")
			}
			click(tea.KeyF9)
			if !m.composeDiscardArmed || m.composeKind == "" {
				t.Fatal("F9 pointer did not arm local-only discard")
			}
			click(tea.KeyF9)
			if m.composeKind == "" {
				t.Fatal("repeated pointer F9 discarded")
			}
			click(tea.KeyF10)
			if m.composeKind != "" {
				t.Fatal("F10 pointer failed to confirm")
			}
		})
	}
}
