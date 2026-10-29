package main

import (
	gossh "golang.org/x/crypto/ssh"
	"testing"
	"time"
)

func TestGUIPreferencesValidationOwnershipAndRevokedSession(t *testing.T) {
	s := testStore(t)
	registerUser(t, s, "pref-alice")
	registerUser(t, s, "pref-bob")
	h := newHub()
	h.Attach(s)
	u, _ := s.User("pref-alice")
	if _, err := s.db.Exec(`UPDATE user_meta SET settings='{"custom":"keep"}' WHERE name=?`, u.Name); err != nil {
		t.Fatal(err)
	}
	run := func(cmd string) (guiActionResult, error) {
		return guiExecuteAction(s, h, u, guiScope{room: "lobby"}, 0, guiRequest{Body: cmd, Username: "pref-bob"})
	}
	for _, key := range []string{"poke", "motion", "bell", "dnd", "compact", "ascii", "welcome", "contrast", "key_reminder"} {
		for _, value := range []string{"on", "off"} {
			r, err := run("/pref " + key + " " + value)
			if err != nil || r.preferences == nil || r.preferences.User != u.Name || r.preferences.Settings[key] != value {
				t.Fatal(key, value, r, err)
			}
		}
	}
	if s.Settings("pref-bob")["dnd"] != "off" || s.Settings("pref-bob")["key_reminder"] != "on" {
		t.Fatal("client actor replaced session owner")
	}
	if s.Settings(u.Name)["custom"] != "keep" {
		t.Fatal("extension setting lost")
	}
	if p, err := guiPreferencesSnapshot(s, u.Name); err != nil || len(p.Settings) != 9 {
		t.Fatal("GUI exposed unsupported settings", p, err)
	}
	for _, cmd := range []string{"/pref dnd true", "/pref unknown on", "/pref dnd on extra", "/dnd", "/dnd on extra", "/theme evil", "/theme aurora extra"} {
		if _, err := run(cmd); err == nil {
			t.Fatal("invalid preference accepted", cmd)
		}
	}
	for _, theme := range []string{"aurora", "sakura", "amber", "mono", "light", "contrast"} {
		r, err := run("/theme " + theme)
		if err != nil || r.preferences.Theme != theme {
			t.Fatal(theme, r, err)
		}
	}
	c := &Client{ID: "preferences-revoked-fixture", Name: u.Name, Room: "lobby", Events: make(chan Event, 32), Close: func() {}, AuthEpoch: u.AuthEpoch}
	if err := h.Join(c); err != nil {
		t.Fatal(err)
	}
	defer h.Leave(c)
	if err := s.RevokeSession(u.Name, c.ID); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{"/dnd on", "/theme aurora"} {
		if _, err := guiExecuteAction(s, h, u, guiScope{room: "lobby"}, 0, guiRequest{Body: cmd, sessionID: c.ID}); err == nil {
			t.Fatal("revoked durable session mutated preferences")
		}
	}
	if s.Settings(u.Name)["dnd"] != "off" {
		t.Fatal("denied mutation changed DND")
	}
	if _, err := s.db.Exec(`UPDATE user_meta SET must_reset=1 WHERE name=?`, u.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := run("/pref dnd on"); err == nil {
		t.Fatal("stale reset-only actor changed preferences")
	}
	if _, err := guiPreferencesSnapshot(s, u.Name); err == nil {
		t.Fatal("reset-only preference snapshot exposed")
	}
}

func TestGUIPreferencesRevocationWhileWaitingForTransaction(t *testing.T) {
	for _, kind := range []string{"preference", "theme"} {
		t.Run(kind, func(t *testing.T) {
			s := testStore(t)
			u := registerUser(t, s, "race-preferences")
			client := &Client{ID: "preferences-race-session", Name: u.Name, IP: "192.0.2.91", AuthEpoch: u.AuthEpoch, Ctx: t.Context()}
			if err := s.RecordSession(client); err != nil {
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
				if kind == "preference" {
					done <- s.settingForSession(u.Name, "dnd", "on", client.ID)
				} else {
					done <- guiSetTheme(s, u.Name, "sakura", client.ID)
				}
			}()
			tick := time.NewTicker(5 * time.Millisecond)
			defer tick.Stop()
			deadline := time.After(3 * time.Second)
			for s.db.Stats().WaitCount <= waits {
				select {
				case <-tick.C:
				case <-deadline:
					t.Fatal("operation did not reach blocked transaction")
				}
			}
			if _, err = blocker.Exec(`UPDATE sessions SET revoked=1 WHERE id=?`, client.ID); err != nil {
				t.Fatal(err)
			}
			if err = blocker.Commit(); err != nil {
				t.Fatal(err)
			}
			select {
			case err = <-done:
				if err == nil {
					t.Fatal("revocation during transaction wait was ignored")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("pending operation did not finish")
			}
			fresh, _ := s.User(u.Name)
			if s.Settings(u.Name)["dnd"] != "off" || fresh.Theme != u.Theme {
				t.Fatal("revoked operation changed preferences")
			}
		})
	}
}

func TestGUIKeyReminderPasswordLoginUsesOwnPreferencesAndKeys(t *testing.T) {
	for _, mode := range []string{"default", "disabled", "bound", "revoked", "reset"} {
		t.Run(mode, func(t *testing.T) {
			s := testStore(t)
			registerUser(t, s, "reminder-owner")
			registerUser(t, s, "reminder-foreign")
			if _, err := s.BindSSHKey("reminder-foreign", "foreign synthetic", string(gossh.MarshalAuthorizedKey(transportSigner(t).PublicKey()))); err != nil {
				t.Fatal(err)
			}
			if mode == "disabled" {
				if err := s.Setting("reminder-owner", "key_reminder", "off"); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "bound" || mode == "revoked" {
				key, err := s.BindSSHKey("reminder-owner", "synthetic", string(gossh.MarshalAuthorizedKey(transportSigner(t).PublicKey())))
				if err != nil {
					t.Fatal(err)
				}
				if mode == "revoked" {
					if err := s.RevokeSSHKey("reminder-owner", key.ID); err != nil {
						t.Fatal(err)
					}
				}
			}
			if mode == "reset" {
				if _, err := s.db.Exec(`UPDATE user_meta SET must_reset=1 WHERE name='reminder-owner'`); err != nil {
					t.Fatal(err)
				}
			}
			f := newGUIWireFixture(t, s, newHub())
			f.send(guiRequest{Type: "login", RequestID: "login", Username: "reminder-owner", Password: "test-password-123"})
			auth := f.read("auth", "login")
			want := mode == "default" || mode == "revoked"
			if auth.KeyReminder != want {
				t.Fatal("wrong reminder gate", mode, auth)
			}
			if mode == "reset" {
				if auth.Preferences != nil {
					t.Fatal("reset login exposed preferences")
				}
				f.read("reset_required", "")
			} else {
				if auth.Preferences == nil || auth.Preferences.User != "reminder-owner" {
					t.Fatal(auth)
				}
				f.read("state", "")
			}
		})
	}
}

func TestGUIPreferencesWireTwoDeviceSyncAndActorIsolation(t *testing.T) {
	s := testStore(t)
	registerUser(t, s, "sync-alice")
	registerUser(t, s, "sync-bob")
	h := newHub()
	a := newGUIWireFixture(t, s, h)
	other := newGUIWireFixture(t, s, h)
	b := newGUIWireFixture(t, s, h)
	for _, x := range []struct {
		f    guiWireFixture
		user string
	}{{a, "sync-alice"}, {other, "sync-alice"}, {b, "sync-bob"}} {
		x.f.send(guiRequest{Type: "login", RequestID: "login", Username: x.user, Password: "test-password-123"})
		x.f.read("auth", "login")
		x.f.read("state", "")
	}
	a.send(guiRequest{Type: "preferences", RequestID: "load", Username: "sync-bob"})
	if p := a.read("preferences", "load").Preferences; p == nil || p.User != "sync-alice" {
		t.Fatal("foreign preference read", p)
	}
	a.send(guiRequest{Type: "command", RequestID: "dnd", Room: "lobby", Body: "/dnd on", Username: "sync-bob"})
	if r := a.read("action", "dnd"); r.Preferences == nil || r.Preferences.Settings["dnd"] != "on" {
		t.Fatal(r)
	}
	if r := other.read("preferences", ""); r.Preferences == nil || r.Preferences.User != "sync-alice" || r.Preferences.Settings["dnd"] != "on" {
		t.Fatal("second device did not receive preference", r)
	}
	b.send(guiRequest{Type: "preferences", RequestID: "own", Username: "sync-alice"})
	if p := b.read("preferences", "own").Preferences; p == nil || p.User != "sync-bob" || p.Settings["dnd"] != "off" {
		t.Fatal("foreign mutation leaked", p)
	}
	p, err := s.PositionWithError("sync-alice", "room:lobby")
	if err != nil || p.Draft != "" {
		t.Fatal("preference operation touched draft", p, err)
	}
}
