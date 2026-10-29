package main

import (
	"encoding/json"
	"testing"
	"time"
)

func TestGUIProfileProjectionPrivateNotesAndOwnFieldBoundary(t *testing.T) {
	s := testStore(t)
	for _, n := range []string{"card-alice", "card-bob", "card-admin"} {
		registerUser(t, s, n)
	}
	h := newHub()
	h.Attach(s)
	if _, err := s.db.Exec(`UPDATE users SET role='admin' WHERE name='card-admin'`); err != nil {
		t.Fatal(err)
	}
	for _, x := range []struct{ owner, target, note string }{{"card-alice", "card-bob", "alice-private"}, {"card-bob", "card-alice", "bob-private"}} {
		if err := guiProfileMutation(s, x.owner, "", "/note", x.target, x.note); err != nil {
			t.Fatal(err)
		}
	}
	for _, x := range []struct{ viewer, target, note string }{{"card-alice", "card-bob", "alice-private"}, {"card-bob", "card-alice", "bob-private"}, {"card-admin", "card-bob", ""}} {
		p, err := guiProfileSnapshot(s, h, x.viewer, x.target)
		if err != nil || p.Note != x.note || p.Self {
			t.Fatal(p, err)
		}
		raw, _ := json.Marshal(p)
		var fields map[string]any
		if err = json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"settings", "auth_epoch", "password", "ip", "sessions", "keys", "must_reset"} {
			if _, ok := fields[key]; ok {
				t.Fatal("private account field leaked", key)
			}
		}
	}
	if err := guiProfileMutation(s, "card-alice", "", "/bio", "card-bob", "foreign edit"); err == nil {
		t.Fatal("foreign profile edited")
	}
	if err := guiProfileMutation(s, "card-alice", "", "/bio", "card-alice", "own bio"); err != nil {
		t.Fatal(err)
	}
	a, _ := s.User("card-alice")
	b, _ := s.User("card-bob")
	if a.Bio != "own bio" || b.Bio != "" {
		t.Fatal(a.Bio, b.Bio)
	}
	if err := guiProfileMutation(s, "card-alice", "", "/unnote", "card-bob", ""); err != nil {
		t.Fatal(err)
	}
	if s.Relations("card-bob", "note")["card-alice"] != "bob-private" {
		t.Fatal("foreign private note deleted")
	}
	for _, cmd := range []string{"/follow", "/friend", "/ignore", "/note", "/unfriend"} {
		if err := guiProfileMutation(s, "card-alice", "", cmd, "card-alice", "x"); err == nil {
			t.Fatal("self relation accepted", cmd)
		}
	}
}

func TestGUIProfileBlockedRequestsAndAtomicNotification(t *testing.T) {
	s := testStore(t)
	registerUser(t, s, "relation-a")
	registerUser(t, s, "relation-b")
	if err := guiProfileMutation(s, "relation-b", "", "/ignore", "relation-a", ""); err != nil {
		t.Fatal(err)
	}
	if err := guiProfileMutation(s, "relation-a", "", "/friend", "relation-b", ""); err == nil {
		t.Fatal("blocked recipient got request")
	}
	if len(s.PendingFriends("relation-b")) != 0 || s.UnreadNotifications("relation-b") != 0 {
		t.Fatal("blocked action left data")
	}
	if err := guiProfileMutation(s, "relation-b", "", "/unignore", "relation-a", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER reject_relation_notice BEFORE INSERT ON notifications BEGIN SELECT RAISE(FAIL,'synthetic failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := guiProfileMutation(s, "relation-a", "", "/friend", "relation-b", ""); err == nil {
		t.Fatal("request survived failed notification")
	}
	if len(s.PendingFriends("relation-b")) != 0 {
		t.Fatal("request transaction was partial")
	}
	if _, err := s.db.Exec(`DROP TRIGGER reject_relation_notice`); err != nil {
		t.Fatal(err)
	}
	if err := guiProfileMutation(s, "relation-a", "", "/friend", "relation-b", ""); err != nil {
		t.Fatal(err)
	}
	if err := guiCenterMutation(s, "relation-b", "", "/accept", "relation-a"); err != nil {
		t.Fatal(err)
	}
	if err := guiProfileMutation(s, "relation-a", "", "/unfriend", "relation-b", ""); err != nil {
		t.Fatal(err)
	}
	if len(s.Relations("relation-a", "friend")) != 0 || len(s.Relations("relation-b", "friend")) != 0 {
		t.Fatal("unfriend was not reciprocal")
	}
}

func TestGUIProfileMutationsRecheckSessionAfterWait(t *testing.T) {
	for _, command := range []string{"/bio", "/note", "/friend", "/ignore", "/unfriend"} {
		t.Run(command, func(t *testing.T) {
			s := testStore(t)
			u := registerUser(t, s, "profile-race")
			registerUser(t, s, "profile-peer")
			s.Relation(u.Name, "profile-peer", "request", "", false)
			if err := s.AcceptFriend("profile-peer", u.Name, true); err != nil {
				t.Fatal(err)
			}
			if err := guiProfileMutation(s, u.Name, "", "/note", "profile-peer", "old"); err != nil {
				t.Fatal(err)
			}
			c := &Client{ID: "profile-race-session", Name: u.Name, IP: "192.0.2.93", AuthEpoch: u.AuthEpoch, Ctx: t.Context()}
			if err := s.RecordSession(c); err != nil {
				t.Fatal(err)
			}
			blocker, err := s.db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback()
			waits := s.db.Stats().WaitCount
			done := make(chan error, 1)
			target := "profile-peer"
			if command == "/bio" {
				target = u.Name
			}
			go func() { done <- guiProfileMutation(s, u.Name, c.ID, command, target, "new") }()
			tick := time.NewTicker(5 * time.Millisecond)
			defer tick.Stop()
			deadline := time.After(3 * time.Second)
			for s.db.Stats().WaitCount <= waits {
				select {
				case <-tick.C:
				case <-deadline:
					t.Fatal("mutation did not wait")
				}
			}
			if _, err = blocker.Exec(`UPDATE sessions SET revoked=1 WHERE id=?`, c.ID); err != nil {
				t.Fatal(err)
			}
			if err = blocker.Commit(); err != nil {
				t.Fatal(err)
			}
			select {
			case err = <-done:
				if err == nil {
					t.Fatal("revoked session wrote")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("mutation hung")
			}
			fresh, _ := s.User(u.Name)
			if fresh.Bio != u.Bio || s.Relations(u.Name, "note")["profile-peer"] != "old" || len(s.Relations(u.Name, "friend")) != 1 || len(s.Relations(u.Name, "block")) != 0 || len(s.PendingFriends("profile-peer")) != 0 {
				t.Fatal("revoked operation changed data")
			}
		})
	}
}

func TestGUIProfileWireOwnerSpoofSyncAndDraft(t *testing.T) {
	s := testStore(t)
	registerUser(t, s, "profile-wire")
	registerUser(t, s, "profile-other")
	if _, err := s.SavePositionVersioned("profile-wire", "room:lobby", Position{Draft: "unchanged profile draft"}); err != nil {
		t.Fatal(err)
	}
	h := newHub()
	a := newGUIWireFixture(t, s, h)
	b := newGUIWireFixture(t, s, h)
	for _, f := range []guiWireFixture{a, b} {
		f.send(guiRequest{Type: "login", RequestID: "login", Username: "profile-wire", Password: "test-password-123"})
		f.read("auth", "login")
		f.read("state", "")
		f.send(guiRequest{Type: "profile", RequestID: "profile", Body: "profile-other", Username: "profile-other"})
		r := f.read("profile", "profile")
		if r.Profile == nil || r.Profile.Viewer != "profile-wire" || r.Profile.Self {
			t.Fatal(r)
		}
	}
	a.send(guiRequest{Type: "command", RequestID: "note", Room: "lobby", Body: "/note profile-other private note", Username: "profile-other"})
	a.read("action", "note")
	if r := b.read("profile", ""); r.Profile == nil || r.Profile.Note != "private note" || r.Profile.Viewer != "profile-wire" {
		t.Fatal("second device stale", r)
	}
	a.send(guiRequest{Type: "command", RequestID: "block", Room: "lobby", Body: "/ignore profile-other"})
	a.read("action", "block")
	b.send(guiRequest{Type: "preferences", RequestID: "blocked"})
	p := b.read("preferences", "blocked").Preferences
	if p == nil || len(p.Blocked) != 1 || p.Blocked[0] != "profile-other" {
		t.Fatal(p)
	}
	d, err := s.PositionWithError("profile-wire", "room:lobby")
	if err != nil || d.Draft != "unchanged profile draft" || d.Version != 1 {
		t.Fatal("draft changed", d, err)
	}
}
