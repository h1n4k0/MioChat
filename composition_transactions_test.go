package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func TestCompositionSessionCASNormalDraftCanonicalAndPublication(t *testing.T) {
	for _, kind := range []string{"post", "art"} {
		t.Run(kind, func(t *testing.T) {
			s, h := featureFixture(t)
			m := testModel(t, s, h, "alice")
			initial, err := s.CompositionForSession(t.Context(), "alice", m.client.ID, "lobby", kind)
			if err != nil || initial.Exists || initial.Version != 0 {
				t.Fatal("absent0 must be a real snapshot", initial, err)
			}
			m.startCompose(kind, "canonical title")
			if m.composeKind != kind || m.composeVersion <= 0 {
				t.Fatal("normal editor did not reserve", m.notice)
			}
			raw := "\x1b[31mA\tB\r\n中文\u200b\n"
			stored, err := s.SaveCompositionForSession(t.Context(), "alice", m.client.ID, "lobby", kind, m.composeVersion, "canonical title", raw, false)
			if err != nil || stored.Body != "A    B\n中文" {
				t.Fatal("canonical server draft", stored, err)
			}
			// The textarea sanitizes pasted controls itself. Exercise its actual
			// raw/saved bookkeeping with a representable trailing newline instead
			// of pretending ANSI bytes survive that widget's sanitizer unchanged.
			m.startCompose(kind, "canonical title")
			m.editor.SetValue("TUI body\n")
			if err = m.saveCompositionDraft(); err != nil {
				t.Fatal(err)
			}
			first := m.composeVersion
			if err = m.saveCompositionDraft(); err != nil || m.composeVersion != first {
				t.Fatal("canonical mismatch caused perpetual raw autosaves", m.composeVersion, first, err)
			}
			snapshot, err := s.CompositionForSession(t.Context(), "alice", m.client.ID, "lobby", kind)
			if err != nil || snapshot.Body != "TUI body" || snapshot.Version != first {
				t.Fatal("canonical actual TUI draft", snapshot, err)
			}
			if kind == "art" {
				if _, _, err = s.PublishCompositionForSession(t.Context(), "alice", m.client.ID, "lobby", kind, first, "canonical title", "中文"); err == nil {
					t.Fatal("art publication accepted non-ASCII")
				}
			}
			id, clear, err := s.PublishCompositionForSession(t.Context(), "alice", m.client.ID, "lobby", kind, first, "canonical title", "A\tB\n")
			if err != nil || id <= 0 || !clear.Exists || clear.Body != "" || clear.Version <= first {
				t.Fatal("normal atomic publication", id, clear, err)
			}
			object, err := s.Object("alice", id)
			if err != nil || object.Kind != kind || !strings.Contains(object.Payload, "A    B") {
				t.Fatal("canonical object", object, err)
			}
		})
	}
}

func TestCompositionLoadReserveConflictTombstoneAndExternalWrites(t *testing.T) {
	s, h := featureFixture(t)
	m := testModel(t, s, h, "alice")
	zero, err := s.CompositionForSession(t.Context(), "alice", m.client.ID, "lobby", "post")
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.ReserveCompositionForSession(t.Context(), "alice", m.client.ID, "lobby", "post", zero.Version, "first")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReserveCompositionForSession(t.Context(), "alice", m.client.ID, "lobby", "post", zero.Version, "stale prefill"); !errors.Is(err, ErrCompositionConflict) {
		t.Fatal("load/reserve overwrite", err)
	}
	if _, err = s.db.Exec(`DELETE FROM compositions WHERE user='alice' AND room='lobby' AND kind='post'`); err != nil {
		t.Fatal(err)
	}
	absent, err := s.CompositionForSession(t.Context(), "alice", m.client.ID, "lobby", "post")
	if err != nil || absent.Exists || absent.Version <= first.Version {
		t.Fatal("delete lost version tombstone", absent, err)
	}
	if _, err = s.db.Exec(`INSERT INTO compositions VALUES('alice','lobby','post','external','newer external text',1)`); err != nil {
		t.Fatal("legacy six-column insert", err)
	}
	recreated, err := s.CompositionForSession(t.Context(), "alice", m.client.ID, "lobby", "post")
	if err != nil || recreated.Version <= absent.Version || recreated.Body != "newer external text" {
		t.Fatal(recreated, err)
	}
	if _, err = s.SaveCompositionForSession(t.Context(), "alice", m.client.ID, "lobby", "post", first.Version, "old", "old", false); !errors.Is(err, ErrCompositionConflict) {
		t.Fatal("ABA accepted old editor", err)
	}
}

func TestCompositionSameVersionWritersOnlyOneWins(t *testing.T) {
	s, h := featureFixture(t)
	m := testModel(t, s, h, "alice")
	m.startCompose("post", "race")
	done := make(chan error, 2)
	for _, body := range []string{"first", "second"} {
		go func(body string) {
			_, err := s.SaveCompositionForSession(t.Context(), "alice", m.client.ID, "lobby", "post", m.composeVersion, "race", body, false)
			done <- err
		}(body)
	}
	wins, conflicts := 0, 0
	for range 2 {
		err := <-done
		if err == nil {
			wins++
		} else if errors.Is(err, ErrCompositionConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatal(wins, conflicts)
	}
}

func TestCompositionWriteFaultsNeverCommitObjectOrDraft(t *testing.T) {
	for _, fault := range []string{"draft-ignore", "version-ignore", "object-ignore", "object-rewrite", "version-exhausted"} {
		t.Run(fault, func(t *testing.T) {
			s, h := featureFixture(t)
			m := testModel(t, s, h, "alice")
			m.startCompose("post", "fault")
			m.editor.SetValue("original to retain")
			if err := m.saveCompositionDraft(); err != nil {
				t.Fatal(err)
			}
			trigger := map[string]string{
				"draft-ignore":      `CREATE TRIGGER composition_fault BEFORE UPDATE ON compositions BEGIN SELECT RAISE(IGNORE);END`,
				"version-ignore":    `CREATE TRIGGER composition_fault BEFORE UPDATE ON composition_versions BEGIN SELECT RAISE(IGNORE);END`,
				"object-ignore":     `CREATE TRIGGER composition_fault BEFORE INSERT ON objects WHEN NEW.kind='post' BEGIN SELECT RAISE(IGNORE);END`,
				"object-rewrite":    `CREATE TRIGGER composition_fault AFTER UPDATE ON compositions WHEN NEW.body='' BEGIN UPDATE objects SET title='changed by synthetic trigger' WHERE id=(SELECT max(id) FROM objects);END`,
				"version-exhausted": `UPDATE composition_versions SET version=9223372036854775807 WHERE user='alice' AND room='lobby' AND kind='post'`,
			}[fault]
			if _, err := s.db.Exec(trigger); err != nil {
				t.Fatal(err)
			}
			if fault == "version-exhausted" {
				m.composeVersion = 1<<63 - 1
			}
			beforeVersion := storageScalar(t, s, `SELECT version FROM composition_versions WHERE user='alice' AND room='lobby' AND kind='post'`)
			beforeObjects := storageScalar(t, s, `SELECT count(*) FROM objects`)
			id, snapshot, err := s.PublishCompositionForSession(t.Context(), "alice", m.client.ID, "lobby", "post", m.composeVersion, "fault", "changed publish body")
			if err == nil || id != 0 || snapshot.Exists {
				t.Fatal("fault falsely committed", id, snapshot, err)
			}
			if body := storageScalar(t, s, `SELECT body FROM compositions WHERE user='alice' AND room='lobby' AND kind='post'`); body != "original to retain" {
				t.Fatal("draft partial commit", body)
			}
			if storageScalar(t, s, `SELECT count(*) FROM objects`) != beforeObjects || storageScalar(t, s, `SELECT version FROM composition_versions WHERE user='alice' AND room='lobby' AND kind='post'`) != beforeVersion {
				t.Fatal("object/version partial commit")
			}
		})
	}
}

func TestCompositionSuspendedOwnerSaveNeverReadsOrCreatesRemoteDraft(t *testing.T) {
	s, h := featureFixture(t)
	if err := s.CreateRoomAccess("private-compose", "alice", true); err != nil {
		t.Fatal(err)
	}
	if err := s.RoomMember("alice", "private-compose", "bob", "invite"); err != nil {
		t.Fatal(err)
	}
	m := testModel(t, s, h, "bob")
	if err := m.move("private-compose", ""); err != nil {
		t.Fatal(err)
	}
	m.startCompose("post", "private")
	v := m.composeVersion
	if err := s.RoomMember("alice", "private-compose", "bob", "remove"); err != nil {
		t.Fatal(err)
	}
	out, err := s.SaveCompositionForSession(t.Context(), "bob", m.client.ID, "private-compose", "post", v, "private", "only submitted local body", true)
	if err != nil || out.Body != "only submitted local body" || out.Version <= v {
		t.Fatal(out, err)
	}
	if out, err = s.CompositionForSession(t.Context(), "bob", m.client.ID, "private-compose", "post"); err == nil || out.Body != "" {
		t.Fatal("revoked read", out, err)
	}
	if out, err = s.SaveCompositionForSession(t.Context(), "bob", m.client.ID, "private-compose", "art", 0, "new", "cannot reserve", true); err == nil || out.Exists {
		t.Fatal("suspended created missing row", out, err)
	}
	if out, err = s.SaveCompositionForSession(t.Context(), "bob", m.client.ID, "private-compose", "post", v, "stale", "stale", true); !errors.Is(err, ErrCompositionConflict) || out.Body != "" {
		t.Fatal("stale suspended leaked snapshot", out, err)
	}
}

func TestCompositionQueuedActualTUIRechecksSessionKeyAndRoom(t *testing.T) {
	for _, action := range []string{"save", "publish"} {
		for _, change := range []string{"session", "key", "private", "reset", "epoch", "cancel"} {
			t.Run(action+"/"+change, func(t *testing.T) {
				s, h := featureFixture(t)
				m := testModel(t, s, h, "alice")
				m.startCompose("post", "queued")
				m.editor.SetValue("queued private input")
				var keyID int64
				if change == "key" {
					key, err := s.BindSSHKey("alice", "synthetic composition", testSSHKeyText(testSSHKeySigner(t).PublicKey()))
					if err != nil {
						t.Fatal(err)
					}
					keyID = key.ID
					if _, err = s.db.Exec(`INSERT INTO session_credentials(session_id,key_id) VALUES(?,?)`, m.client.ID, keyID); err != nil {
						t.Fatal(err)
					}
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				m.ctx = ctx
				before := storageScalar(t, s, `SELECT count(*) FROM objects`)
				beforeVersion := m.composeVersion
				tx, err := s.db.Begin()
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				waits := s.db.Stats().WaitCount
				done := make(chan error, 1)
				go func() {
					if action == "save" {
						done <- m.saveCompositionDraft()
					} else {
						m.composeKey(tea.KeyMsg{Type: tea.KeyF4})
						if m.composeKind == "" {
							done <- errors.New("queued publish closed editor")
						} else {
							done <- errors.New(m.notice)
						}
					}
				}()
				deadline := time.Now().Add(3 * time.Second)
				for s.db.Stats().WaitCount <= waits {
					if time.Now().After(deadline) {
						t.Fatal("composition writer did not queue")
					}
					time.Sleep(time.Millisecond)
				}
				switch change {
				case "session":
					_, err = tx.Exec(`UPDATE sessions SET revoked=1 WHERE id=?`, m.client.ID)
				case "key":
					_, err = tx.Exec(`UPDATE ssh_keys SET revoked=1 WHERE id=?`, keyID)
				case "private":
					_, err = tx.Exec(`UPDATE room_meta SET private=1 WHERE name='lobby'`)
				case "reset":
					_, err = tx.Exec(`UPDATE user_meta SET must_reset=1 WHERE name='alice'`)
				case "epoch":
					err = changedOne(tx.Exec(`INSERT INTO credential_versions(user,epoch) VALUES('alice',1) ON CONFLICT(user) DO UPDATE SET epoch=epoch+1`))
				case "cancel":
					cancel()
				}
				if err != nil {
					t.Fatal(err)
				}
				if err = tx.Commit(); err != nil {
					t.Fatal(err)
				}
				err = <-done
				if change == "private" {
					// Real TUI navigation preserves only its preexisting own draft
					// after ACL loss; that suspended save is allowed, not a publish.
					if m.composeKind != "post" || m.editor.Value() != "queued private input" || storageScalar(t, s, `SELECT count(*) FROM objects`) != before {
						t.Fatal("private fallback leaked/published/retargeted")
					}
					if err == nil && m.composeVersion <= beforeVersion {
						t.Fatal("suspended success without stored version")
					}
					return
				}
				if err == nil {
					t.Fatal("queued stale operation succeeded")
				}
				if m.composeKind != "post" || m.composeVersion != beforeVersion || m.editor.Value() != "queued private input" || storageScalar(t, s, `SELECT count(*) FROM objects`) != before {
					t.Fatal("queued invalidation changed local or durable state", fmt.Sprint(m.composeVersion))
				}
			})
		}
	}
}
