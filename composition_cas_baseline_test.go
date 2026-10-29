package main

import (
	"fmt"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// Identical test source is intended for the old actual TUI entry points and
// the future CAS implementation; no new helper/type is referenced here.
func TestTUICompositionStaleEditorNeverOverwritesOrPublishes(t *testing.T) {
	for _, kind := range []string{"post", "art"} {
		for _, action := range []string{"save", "close", "publish"} {
			t.Run(kind+"/"+action, func(t *testing.T) {
				s, h := featureFixture(t)
				a, b := testModel(t, s, h, "alice"), testModel(t, s, h, "alice")
				a.startCompose(kind, "synthetic title")
				b.startCompose(kind, "synthetic title")
				if a.composeKind != kind || b.composeKind != kind {
					t.Fatal("fixture editor did not open")
				}
				a.editor.SetValue("older unsaved local text")
				b.editor.SetValue("newer other-device text")
				if err := b.saveCompositionDraft(); err != nil {
					t.Fatal("fixture newer save", err)
				}
				before := storageScalar(t, s, `SELECT count(*) FROM objects`)
				switch action {
				case "save":
					if err := a.saveCompositionDraft(); err == nil {
						t.Error("COMPOSITION_STALE_SAVE_FALSE_SUCCESS")
					}
				case "close":
					a.composeKey(tea.KeyMsg{Type: tea.KeyEsc})
					if a.composeKind == "" {
						t.Error("COMPOSITION_STALE_CLOSE_LOST_CONFLICT")
					}
				case "publish":
					a.composeKey(tea.KeyMsg{Type: tea.KeyF4})
					if storageScalar(t, s, `SELECT count(*) FROM objects`) != before {
						t.Error("COMPOSITION_STALE_PUBLISH_COMMITTED")
					}
					if a.composeKind == "" {
						t.Error("COMPOSITION_STALE_PUBLISH_CLOSED_EDITOR")
					}
				}
				var body string
				if err := s.db.QueryRow(`SELECT body FROM compositions WHERE user='alice' AND room='lobby' AND kind=?`, kind).Scan(&body); err != nil {
					t.Fatal("fixture draft read", err)
				}
				if body != "newer other-device text" {
					t.Errorf("COMPOSITION_STALE_REMOTE_LOST: %s body=%q", action, body)
				}
				if a.editor.Value() != "older unsaved local text" {
					t.Error("COMPOSITION_LOCAL_INPUT_LOST")
				}
			})
		}
	}
}

func TestTUICompositionPublishAndClearAreOneCommit(t *testing.T) {
	for _, kind := range []string{"post", "art"} {
		t.Run(kind, func(t *testing.T) {
			s, h := featureFixture(t)
			m := testModel(t, s, h, "alice")
			m.startCompose(kind, "synthetic atomic title")
			m.editor.SetValue("synthetic body to retain")
			if err := m.saveCompositionDraft(); err != nil {
				t.Fatal("fixture save", err)
			}
			if _, err := s.db.Exec(fmt.Sprintf(`CREATE TRIGGER composition_clear_fault BEFORE UPDATE ON compositions WHEN NEW.user='alice' AND NEW.kind='%s' AND NEW.body='' BEGIN SELECT RAISE(IGNORE); END`, kind)); err != nil {
				t.Fatal("fixture trigger", err)
			}
			before := storageScalar(t, s, `SELECT count(*) FROM objects`)
			m.composeKey(tea.KeyMsg{Type: tea.KeyF4})
			if after := storageScalar(t, s, `SELECT count(*) FROM objects`); after != before {
				t.Error("COMPOSITION_PUBLISH_CLEAR_PARTIAL_COMMIT")
			}
			if m.composeKind == "" || m.editor.Value() != "synthetic body to retain" {
				t.Error("COMPOSITION_CLEAR_FAILURE_DISCARDED_EDITOR")
			}
			var body string
			if err := s.db.QueryRow(`SELECT body FROM compositions WHERE user='alice' AND room='lobby' AND kind=?`, kind).Scan(&body); err != nil || body != "synthetic body to retain" {
				t.Fatal("draft was not retained", err, body)
			}
		})
	}
}
