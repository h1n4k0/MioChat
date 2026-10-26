package main

import (
	"errors"
	"sync"
	"testing"
)

func storageScalar(t *testing.T, s *Store, query string, values ...any) string {
	t.Helper()
	var result string
	if err := s.db.QueryRow(query, values...).Scan(&result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestSensitiveMutationsRollbackWhenAuditFails(t *testing.T) {
	for _, failure := range []string{"SELECT RAISE(ABORT,'test audit failure');", "SELECT RAISE(IGNORE);"} {
		for _, name := range []string{"role", "config", "sanction", "session", "room-setting", "room-invite", "room-remove", "room-transfer", "invite-code", "report"} {
			t.Run(name+"/"+failure, func(t *testing.T) {
				s, _ := featureFixture(t)
				if err := s.CreateRoomAccess("atomic-room", "alice", true); err != nil {
					t.Fatal(err)
				}
				if err := s.RoomMember("alice", "atomic-room", "bob", "invite"); err != nil {
					t.Fatal(err)
				}
				if err := s.RecordSession(&Client{ID: "atomic-session", Name: "alice"}); err != nil {
					t.Fatal(err)
				}
				message := mustSend(t, s, "alice", "lobby", "", "reported synthetic content")
				if err := s.Report("eve", message.ID, "synthetic reason"); err != nil {
					t.Fatal(err)
				}
				var report int64
				if err := s.db.QueryRow(`SELECT id FROM reports LIMIT 1`).Scan(&report); err != nil {
					t.Fatal(err)
				}
				var operation func() error
				var query string
				switch name {
				case "role":
					operation = func() error { return s.SetRole("owner", "alice", "admin") }
					query = `SELECT role FROM users WHERE name='alice'`
				case "config":
					operation = func() error { return s.SetConfig("admin", "registration", "closed") }
					query = `SELECT value FROM config WHERE key='registration'`
				case "sanction":
					operation = func() error { return s.Sanction("admin", "user", "bob", "", "1d", "synthetic", false) }
					query = `SELECT count(*) FROM sanctions`
				case "session":
					operation = func() error { return s.RevokeSession("alice", "atomic-session") }
					query = `SELECT revoked FROM sessions WHERE id='atomic-session'`
				case "room-setting":
					operation = func() error { return s.RoomSetting("alice", "atomic-room", "readonly", "on") }
					query = `SELECT readonly FROM room_meta WHERE name='atomic-room'`
				case "room-invite":
					operation = func() error { return s.RoomMember("alice", "atomic-room", "eve", "invite") }
					query = `SELECT count(*) FROM members WHERE room='atomic-room' AND user='eve'`
				case "room-remove":
					operation = func() error { return s.RoomMember("alice", "atomic-room", "bob", "remove") }
					query = `SELECT count(*) FROM members WHERE room='atomic-room' AND user='bob'`
				case "room-transfer":
					operation = func() error { return s.RoomSetting("alice", "atomic-room", "transfer", "bob") }
					query = `SELECT owner FROM rooms WHERE name='atomic-room'`
				case "invite-code":
					operation = func() error {
						code, err := s.InviteCode("admin", "1d", 2)
						if code != "" {
							t.Error("failed transaction returned an active invitation")
						}
						return err
					}
					query = `SELECT count(*) FROM invites`
				case "report":
					operation = func() error { return s.ResolveReport("admin", report, "resolved") }
					query = `SELECT status FROM reports LIMIT 1`
				}
				before := storageScalar(t, s, query)
				notifications := storageScalar(t, s, `SELECT count(*) FROM notifications`)
				audits := storageScalar(t, s, `SELECT count(*) FROM audit`)
				if _, err := s.db.Exec(`CREATE TRIGGER reject_sensitive_audit BEFORE INSERT ON audit BEGIN ` + failure + ` END`); err != nil {
					t.Fatal(err)
				}
				if err := operation(); err == nil {
					t.Fatal("sensitive mutation succeeded without an audit record")
				}
				if after := storageScalar(t, s, query); after != before {
					t.Fatalf("failed mutation changed state: %q -> %q", before, after)
				}
				if storageScalar(t, s, `SELECT count(*) FROM notifications`) != notifications || storageScalar(t, s, `SELECT count(*) FROM audit`) != audits {
					t.Fatal("failed mutation left side effects")
				}
			})
		}
	}
}

func TestSettingsAtomicKeysAndPreservedUnknownPreferences(t *testing.T) {
	s, _ := featureFixture(t)
	if _, err := s.db.Exec(`UPDATE user_meta SET settings='{"custom":"keep"}' WHERE name='alice'`); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 30; attempt++ {
		if err := s.Setting("alice", "dnd", "off"); err != nil {
			t.Fatal(err)
		}
		if err := s.Setting("alice", "key_reminder", "on"); err != nil {
			t.Fatal(err)
		}
		var workers sync.WaitGroup
		start := make(chan struct{})
		for _, setting := range [][2]string{{"dnd", "on"}, {"key_reminder", "off"}} {
			workers.Add(1)
			go func(key, value string) {
				defer workers.Done()
				<-start
				if err := s.Setting("alice", key, value); err != nil {
					t.Error(err)
				}
			}(setting[0], setting[1])
		}
		close(start)
		workers.Wait()
		settings := s.Settings("alice")
		if settings["dnd"] != "on" || settings["key_reminder"] != "off" || settings["custom"] != "keep" {
			t.Fatal("independent preference updates overwrote each other")
		}
	}
}

func TestPositionCompareAndSwapPreservesOtherDeviceDraft(t *testing.T) {
	s, _ := featureFixture(t)
	left, right := s.Position("alice", "room:lobby"), s.Position("alice", "room:lobby")
	left.Draft, left.LastRead = "first device draft", 90
	version, err := s.SavePositionVersioned("alice", "room:lobby", left)
	if err != nil || version != 1 {
		t.Fatal("first position save failed", version, err)
	}
	right.Draft = "stale second device draft"
	if _, err = s.SavePositionVersioned("alice", "room:lobby", right); !errors.Is(err, ErrPositionConflict) {
		t.Fatal("stale device silently overwrote draft", err)
	}
	current := s.Position("alice", "room:lobby")
	if current.Draft != left.Draft || current.LastRead != 90 || current.Version != 1 {
		t.Fatal("conflict modified persisted position", current)
	}
	current.Draft, current.LastRead = "explicit current version", 12
	if version, err = s.SavePositionVersioned("alice", "room:lobby", current); err != nil || version != 2 {
		t.Fatal(version, err)
	}
	if s.Position("alice", "room:lobby").LastRead != 90 {
		t.Fatal("read progress moved backwards")
	}
}

func TestConflictDraftsArePrivateAndSurviveSessionClosure(t *testing.T) {
	s, _ := featureFixture(t)
	for _, client := range []*Client{{ID: "alice-device", Name: "alice"}, {ID: "bob-device", Name: "bob"}} {
		if err := s.RecordSession(client); err != nil {
			t.Fatal(err)
		}
	}
	p := Position{Draft: "private unsent text", ReplyTo: 19, Version: 3}
	if err := s.SaveConflictDraft("alice", "room:lobby", "bob-device", p); err == nil {
		t.Fatal("account used another user's session for a private draft")
	}
	s.EndSession("alice-device")
	if err := s.SaveConflictDraft("alice", "room:lobby", "alice-device", p); err != nil {
		t.Fatal("closed session could not preserve its own pending text", err)
	}
	if _, exists, err := s.ConflictDraft("bob", "room:lobby", "alice-device"); err != nil || exists {
		t.Fatal("another account read a saved conflict", err)
	}
	if err := s.RemoveConflictDraft("bob", "room:lobby", "alice-device"); err != nil {
		t.Fatal(err)
	}
	actual, exists, err := s.ConflictDraft("alice", "room:lobby", "alice-device")
	if err != nil || !exists || actual.Draft != p.Draft || actual.ReplyTo != p.ReplyTo || actual.Version != p.Version {
		t.Fatal("owner draft changed or disappeared", actual, exists, err)
	}
	items, err := s.ConflictDrafts("alice")
	if err != nil || len(items) != 1 || items[0].SessionID != "alice-device" {
		t.Fatal("owner could not list retained text", items, err)
	}
	if err = s.RemoveConflictDraft("alice", "room:lobby", "alice-device"); err != nil {
		t.Fatal(err)
	}
}
