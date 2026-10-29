package main

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestGUIPresenceOwnAvatarAFKBadgesAndBack(t *testing.T) {
	s := testStore(t)
	a := registerUser(t, s, "presence-a")
	b := registerUser(t, s, "presence-b")
	h := newHub()
	h.Attach(s)
	s.Award(b.Name, "first-message")
	for _, kind := range []string{"camp", "cat", "robot", "planet", "flower", "ident"} {
		r, err := guiExecuteAction(s, h, a, guiScope{room: "lobby"}, 0, guiRequest{Body: "/avatar " + kind, Username: b.Name})
		if err != nil || r.profile == nil || r.profile.Name != a.Name || r.profile.AvatarKind != kind || r.profile.Avatar == "" {
			t.Fatal(kind, r.profile, err)
		}
	}
	if err := guiProfileMutation(s, a.Name, "", "/avatar", b.Name, "cat"); err == nil {
		t.Fatal("foreign avatar edited")
	}
	if err := guiProfileMutation(s, a.Name, "", "/avatar", a.Name, "invalid"); err == nil {
		t.Fatal("invalid avatar accepted")
	}
	for _, value := range []string{"", strings.Repeat("界", 180)} {
		r, err := guiExecuteAction(s, h, a, guiScope{room: "lobby"}, 0, guiRequest{Body: "/afk " + value})
		want := "AFK · 暂时离开"
		if value != "" {
			want = cleanText("AFK · "+value, 160)
		}
		if err != nil || r.profile == nil || r.profile.Status != want {
			t.Fatal(r.profile, err)
		}
	}
	r, err := guiExecuteAction(s, h, a, guiScope{room: "lobby"}, 0, guiRequest{Body: "/badges #" + fmt.Sprint(b.UID)})
	if err != nil || r.profile == nil || r.profile.Name != b.Name || !slices.Contains(r.profile.Badges, "first-message") {
		t.Fatal(r.profile, err)
	}
	r, err = guiExecuteAction(s, h, a, guiScope{peer: b.Name}, 0, guiRequest{Body: "/back"})
	fresh, _ := s.User(a.Name)
	other, _ := s.User(b.Name)
	if err != nil || r.scope != (guiScope{room: "lobby"}) || fresh.Status != "" || other.Status != b.Status || other.Avatar != b.Avatar {
		t.Fatal(r.scope, fresh.Status, other, err)
	}
}

func TestGUIPokeCooldownPolicyAndAtomicNotification(t *testing.T) {
	for _, policy := range []string{"dnd", "poke", "block", "self", "notify-fail", "allow"} {
		t.Run(policy, func(t *testing.T) {
			s := testStore(t)
			registerUser(t, s, "poke-a")
			registerUser(t, s, "poke-b")
			target := "poke-b"
			switch policy {
			case "dnd":
				_ = s.Setting(target, "dnd", "on")
			case "poke":
				_ = s.Setting(target, "poke", "off")
			case "block":
				_ = s.Relation(target, "poke-a", "block", "", false)
			case "self":
				target = "poke-a"
			case "notify-fail":
				if _, err := s.db.Exec(`CREATE TRIGGER reject_poke BEFORE INSERT ON notifications BEGIN SELECT RAISE(ABORT,'synthetic rollback'); END`); err != nil {
					t.Fatal(err)
				}
			}
			_, err := s.pokeForSession("poke-a", target, "")
			var limits, notices int
			_ = s.db.QueryRow(`SELECT count(*) FROM poke_limits`).Scan(&limits)
			_ = s.db.QueryRow(`SELECT count(*) FROM notifications WHERE kind='poke'`).Scan(&notices)
			if policy != "allow" {
				if err == nil || limits != 0 || notices != 0 {
					t.Fatal("failed poke left effects", err, limits, notices)
				}
				return
			}
			if err != nil || limits != 1 || notices != 1 {
				t.Fatal(err, limits, notices)
			}
			if err = s.Poke("poke-a", target); err == nil {
				t.Fatal("terminal/native cooldown split")
			}
			if _, err = s.db.Exec(`UPDATE poke_limits SET at=?`, time.Now().Unix()-31); err != nil {
				t.Fatal(err)
			}
			if err = s.Poke("poke-a", target); err != nil {
				t.Fatal("expired cooldown", err)
			}
		})
	}
}

func TestGUIPresenceRechecksRevocationAndRecipientPolicyAfterWait(t *testing.T) {
	for _, action := range []string{"/avatar", "/afk", "/back", "poke-revoke", "poke-dnd", "poke-block"} {
		t.Run(action, func(t *testing.T) {
			s := testStore(t)
			u := registerUser(t, s, "presence-race")
			registerUser(t, s, "presence-peer")
			c := &Client{ID: "presence-session", Name: u.Name, IP: "192.0.2.94", AuthEpoch: u.AuthEpoch, Ctx: t.Context()}
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
			go func() {
				if strings.HasPrefix(action, "poke-") {
					_, e := s.pokeForSession(u.Name, "presence-peer", c.ID)
					done <- e
				} else {
					done <- guiProfileMutation(s, u.Name, c.ID, action, u.Name, "cat")
				}
			}()
			deadline := time.Now().Add(3 * time.Second)
			for s.db.Stats().WaitCount <= waits {
				if time.Now().After(deadline) {
					t.Fatal("mutation did not wait")
				}
				time.Sleep(5 * time.Millisecond)
			}
			switch action {
			case "poke-dnd":
				_, err = blocker.Exec(`UPDATE user_meta SET settings='{"dnd":"on"}' WHERE name='presence-peer'`)
			case "poke-block":
				_, err = blocker.Exec(`INSERT INTO relations VALUES('presence-peer','presence-race','block','',0)`)
			default:
				_, err = blocker.Exec(`UPDATE sessions SET revoked=1 WHERE id=?`, c.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = blocker.Commit(); err != nil {
				t.Fatal(err)
			}
			select {
			case err = <-done:
				if err == nil {
					t.Fatal("stale authority accepted")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("mutation hung")
			}
			fresh, _ := s.User(u.Name)
			var n int
			_ = s.db.QueryRow(`SELECT count(*) FROM poke_limits`).Scan(&n)
			if fresh.Avatar != u.Avatar || fresh.Status != u.Status || n != 0 {
				t.Fatal("denied operation changed state")
			}
		})
	}
}

func TestGUIPresenceWireSyncMentionAndDraft(t *testing.T) {
	s := testStore(t)
	registerUser(t, s, "presence-wire")
	peer := registerUser(t, s, "presence-target")
	_, err := s.SavePositionVersioned("presence-wire", "room:lobby", Position{Draft: "kept draft", ReplyTo: 0})
	if err != nil {
		t.Fatal(err)
	}
	h := newHub()
	a, b := newGUIWireFixture(t, s, h), newGUIWireFixture(t, s, h)
	for _, f := range []guiWireFixture{a, b} {
		f.send(guiRequest{Type: "login", RequestID: "login", Username: "presence-wire", Password: "test-password-123"})
		f.read("auth", "login")
		f.read("state", "")
		f.send(guiRequest{Type: "profile", RequestID: "own"})
		f.read("profile", "own")
	}
	a.send(guiRequest{Type: "command", RequestID: "avatar", Room: "lobby", Body: "/avatar ident", Username: peer.Name})
	a.read("action", "avatar")
	if r := b.read("profile", ""); r.Profile == nil || r.Profile.AvatarKind != "ident" {
		t.Fatal(r)
	}
	a.send(guiRequest{Type: "command", RequestID: "afk", Room: "lobby", Body: "/afk away"})
	a.read("action", "afk")
	if r := b.read("profile", ""); r.Profile == nil || r.Profile.Status != "AFK · away" {
		t.Fatal(r)
	}
	a.send(guiRequest{Type: "command", RequestID: "mention", Room: "lobby", Body: fmt.Sprintf("/mention #%d", peer.UID), Username: peer.Name})
	if r := a.read("action", "mention"); r.Mention != peer.Name || r.Message != nil || r.Room != "lobby" {
		t.Fatal(r)
	}
	p, err := s.PositionWithError("presence-wire", "room:lobby")
	if err != nil || p.Draft != "kept draft" || p.Version != 1 {
		t.Fatal(p, err)
	}
}
