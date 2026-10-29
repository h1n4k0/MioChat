package main

import (
	"strings"
	"testing"
	"time"
)

func TestGUIRoomManagementPermissionsTransferAndPreferences(t *testing.T) {
	s := testStore(t)
	for _, name := range []string{"manage-owner", "manage-mod", "manage-reader", "manage-admin"} {
		registerUser(t, s, name)
	}
	if _, err := s.db.Exec(`UPDATE users SET role='user'; UPDATE users SET role='admin' WHERE name='manage-admin'`); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRoomAccess("managed-private", "manage-owner", true); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"manage-mod", "manage-reader"} {
		if err := s.RoomMember("manage-owner", "managed-private", name, "invite"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RoomMember("manage-owner", "managed-private", "manage-mod", "mod"); err != nil {
		t.Fatal(err)
	}
	h := newHub()
	h.Attach(s)
	run := func(actor, body string) (guiActionResult, error) {
		u, _ := s.User(actor)
		return guiExecuteAction(s, h, u, guiScope{room: "managed-private"}, 0, guiRequest{Body: body, Username: "manage-owner"})
	}
	for _, actor := range []string{"manage-reader", "manage-admin"} {
		for _, cmd := range []string{"/roomset rules bad", "/roommod manage-reader", "/archive"} {
			if _, err := run(actor, cmd); err == nil {
				t.Fatal("unauthorized management", actor, cmd)
			}
		}
	}
	for _, cmd := range []string{"/roomset private off", "/archive", "/roomset transfer manage-reader", "/roommod manage-reader"} {
		if _, err := run("manage-mod", cmd); err == nil {
			t.Fatal("moderator exceeded authority", cmd)
		}
	}
	for _, cmd := range []string{"/roomset category native", "/roomset rules actual rules", "/roomset announcement actual notice", "/roomset slow 10", "/roomset readonly on"} {
		if _, err := run("manage-mod", cmd); err != nil {
			t.Fatal(cmd, err)
		}
	}
	r, err := run("manage-owner", "/members")
	if err != nil || !r.panel.Details.CanOwn || !r.panel.Details.CanAssignMods {
		t.Fatal(r.panel, err)
	}
	for _, m := range r.panel.Members {
		if m.Name == "manage-mod" && !m.CanUnmod {
			t.Fatal(m)
		}
		if m.Name == "manage-reader" && !m.CanMod {
			t.Fatal(m)
		}
	}
	for _, cmd := range []string{"/roommod manage-reader", "/unmod manage-reader", "/archive", "/unarchive"} {
		if _, err := run("manage-owner", cmd); err != nil {
			t.Fatal(cmd, err)
		}
	}
	if _, err = s.SavePositionVersioned("manage-reader", "room:managed-private", Position{Draft: "preserved settings draft", Anchor: 1, Offset: 2}); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{"/roompin on", "/subscribe on", "/roommute on"} {
		r, err = run("manage-reader", cmd)
		if err != nil {
			t.Fatal(err)
		}
	}
	p, err := s.PositionWithError("manage-reader", "room:managed-private")
	if err != nil || !p.Pinned || !p.Subscribed || !p.Muted || p.Draft != "preserved settings draft" || p.Version != 1 || p.Anchor != 1 || p.Offset != 2 {
		t.Fatal(p, err)
	}
	prefs, err := guiPreferencesSnapshot(s, "manage-reader")
	if err != nil || !prefs.Rooms["managed-private"].Muted {
		t.Fatal(prefs, err)
	}
	other, err := guiPreferencesSnapshot(s, "manage-owner")
	if err != nil || other.Rooms["managed-private"].Pinned {
		t.Fatal("other owner preferences changed", other, err)
	}
	for _, cmd := range []string{"/roomset slow -1", "/roomset slow 3601", "/roomset private yes", "/roommute yes", "/archive extra"} {
		if _, err := run("manage-owner", cmd); err == nil {
			t.Fatal("bad setting accepted", cmd)
		}
	}
	if _, err = run("manage-owner", "/roomset transfer manage-reader"); err != nil {
		t.Fatal(err)
	}
	if _, err = run("manage-owner", "/roomset rules stale owner"); err == nil {
		t.Fatal("old owner retained management")
	}
	if _, err = run("manage-reader", "/leave"); err == nil {
		t.Fatal("new owner left without transfer")
	}
	if _, err = run("manage-owner", "/leave"); err != nil {
		t.Fatal("transferred owner could not leave", err)
	}
}

func TestGUIRoomManagementRechecksSessionAndACLAfterWait(t *testing.T) {
	for _, kind := range []string{"setting-session", "member-session", "leave-session", "pref-session", "setting-role", "pref-access"} {
		t.Run(kind, func(t *testing.T) {
			s := testStore(t)
			registerUser(t, s, "race-owner")
			u := registerUser(t, s, "race-room-mod")
			registerUser(t, s, "race-room-peer")
			if _, err := s.db.Exec(`UPDATE users SET role='user'`); err != nil {
				t.Fatal(err)
			}
			if err := s.CreateRoomAccess("race-private", "race-owner", true); err != nil {
				t.Fatal(err)
			}
			if err := s.RoomMember("race-owner", "race-private", u.Name, "mod"); err != nil {
				t.Fatal(err)
			}
			c := &Client{ID: "room-management-session", Name: u.Name, IP: "192.0.2.95", AuthEpoch: u.AuthEpoch, Ctx: t.Context()}
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
				switch kind {
				case "member-session":
					done <- s.roomMemberForSession(u.Name, "race-private", "race-room-peer", "invite", c.ID)
				case "leave-session":
					done <- s.leaveRoomForSession(u.Name, "race-private", c.ID)
				case "pref-session", "pref-access":
					done <- guiRoomPreference(s, u.Name, c.ID, "race-private", "pinned", true)
				default:
					done <- s.roomSettingForSession(u.Name, "race-private", "rules", "changed", c.ID)
				}
			}()
			deadline := time.Now().Add(3 * time.Second)
			for s.db.Stats().WaitCount <= waits {
				if time.Now().After(deadline) {
					t.Fatal("mutation did not wait")
				}
				time.Sleep(5 * time.Millisecond)
			}
			switch kind {
			case "setting-role":
				_, err = blocker.Exec(`UPDATE members SET role='member' WHERE room='race-private' AND user=?`, u.Name)
			case "pref-access":
				_, err = blocker.Exec(`DELETE FROM members WHERE room='race-private' AND user=?`, u.Name)
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
					t.Fatal("stale authority wrote")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("mutation hung")
			}
			r, _ := s.RoomInfo("race-private")
			var prefs int
			_ = s.db.QueryRow(`SELECT count(*) FROM preferences WHERE user=?`, u.Name).Scan(&prefs)
			if r.Rules != "" || prefs != 0 || s.CanRead("race-room-peer", "race-private") {
				t.Fatal("denied mutation left effects", r, prefs)
			}
			if kind != "pref-access" && !s.CanRead(u.Name, "race-private") {
				t.Fatal("denied leave changed membership")
			}
		})
	}
}

func TestGUIRoomManagementWirePreferencesAndPrivateRevocation(t *testing.T) {
	s := testStore(t)
	registerUser(t, s, "management-wire")
	registerUser(t, s, "management-reader")
	if _, err := s.db.Exec(`UPDATE users SET role='user'`); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRoomAccess("management-public", "management-wire", false); err != nil {
		t.Fatal(err)
	}
	_, err := s.SavePositionVersioned("management-reader", "room:management-public", Position{Draft: "revoked draft survives"})
	if err != nil {
		t.Fatal(err)
	}
	h := newHub()
	a, b, c := newGUIWireFixture(t, s, h), newGUIWireFixture(t, s, h), newGUIWireFixture(t, s, h)
	for _, item := range []struct {
		f    guiWireFixture
		name string
	}{{a, "management-wire"}, {b, "management-reader"}, {c, "management-reader"}} {
		item.f.send(guiRequest{Type: "login", RequestID: "login", Username: item.name, Password: "test-password-123"})
		item.f.read("auth", "login")
		item.f.read("state", "")
		item.f.send(guiRequest{Type: "select_room", RequestID: "select", Room: "management-public"})
		item.f.read("state", "select")
	}
	b.send(guiRequest{Type: "command", RequestID: "muted", Room: "management-public", Body: "/roommute on"})
	b.read("action", "muted")
	c.send(guiRequest{Type: "preferences", RequestID: "readprefs"})
	if p := c.read("preferences", "readprefs").Preferences; p == nil || !p.Rooms["management-public"].Muted {
		t.Fatal(p)
	}
	for _, f := range []guiWireFixture{b, c} {
		f.send(guiRequest{Type: "sync", RequestID: "barrier"})
		f.read("state", "barrier")
	}
	a.send(guiRequest{Type: "command", RequestID: "private", Room: "management-public", Body: "/roomset private on"})
	a.read("action", "private")
	for _, f := range []guiWireFixture{b, c} {
		if r := f.read("state", ""); r.Room != "lobby" {
			t.Fatal(r)
		}
		f.send(guiRequest{Type: "preferences", RequestID: "hidden"})
		p := f.read("preferences", "hidden").Preferences
		if p == nil {
			t.Fatal("missing preferences")
		}
		if _, ok := p.Rooms["management-public"]; ok {
			t.Fatal("inaccessible preference scope leaked")
		}
	}
	var draft string
	_ = s.db.QueryRow(`SELECT draft FROM preferences WHERE user='management-reader' AND scope='room:management-public'`).Scan(&draft)
	if !strings.Contains(draft, "survives") {
		t.Fatal("draft lost", draft)
	}
}
