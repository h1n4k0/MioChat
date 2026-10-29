package main

import (
	gossh "golang.org/x/crypto/ssh"
	"testing"
	"time"
)

func TestGUIForcedResetHasNoChatAuthorityUntilPasswordChanged(t *testing.T) {
	s := testStore(t)
	admin := registerUser(t, s, "gui-reset-admin")
	victim := registerUser(t, s, "gui-reset-user")
	if _, err := s.db.Exec("UPDATE users SET role='admin' WHERE name=?", admin.Name); err != nil {
		t.Fatal(err)
	}
	signer := transportSigner(t)
	if _, err := s.BindSSHKey(victim.Name, "synthetic key", string(gossh.MarshalAuthorizedKey(signer.PublicKey()))); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordSession(&Client{ID: "pre-reset", Name: victim.Name, IP: "192.0.2.110", AuthEpoch: victim.AuthEpoch, Ctx: t.Context()}); err != nil {
		t.Fatal(err)
	}
	temporary, err := s.ResetPassword(admin.Name, victim.Name)
	if err != nil {
		t.Fatal(err)
	}
	if s.SessionValid("pre-reset") {
		t.Fatal("forced reset retained previous session")
	}
	if keys, err := s.SSHKeys(victim.Name); err != nil || len(keys) != 0 {
		t.Fatal("forced reset retained device keys", err)
	}
	h := newHub()
	f := newGUIWireFixture(t, s, h)
	f.send(guiRequest{Type: "login", RequestID: "auth", Username: victim.Name, Password: temporary})
	if r := f.read("auth", "auth"); r.Status != "password_reset_required" {
		t.Fatal(r)
	}
	if r := f.read("reset_required", ""); len(r.Messages) != 0 || len(r.Rooms) != 0 {
		t.Fatal("reset-only response carried chat content")
	}
	m := mustSend(t, s, admin.Name, "lobby", "", "synthetic content hidden until reset")
	h.Broadcast(m)
	f.send(guiRequest{Type: "send_message", RequestID: "denied", Room: "lobby", Body: "must not be sent"})
	deadline := time.After(4 * time.Second)
	for {
		select {
		case r, ok := <-f.responses:
			if !ok {
				t.Fatal("restricted connection ended unexpectedly")
			}
			if r.Type == "state" || r.Type == "message" || r.Type == "message_updated" {
				t.Fatal("restricted connection received content", r.Type)
			}
			if r.RequestID == "denied" {
				if r.Type != "error" {
					t.Fatal(r)
				}
				goto restrictedVerified
			}
		case <-deadline:
			t.Fatal("restricted action timed out")
		}
	}
restrictedVerified:
	f.send(guiRequest{Type: "change_password", RequestID: "change", Password: temporary, NewPassword: "synthetic-next-password", Confirm: "synthetic-next-password"})
	f.read("password_changed", "change")
	if r := f.read("state", ""); r.User != victim.Name || len(r.Rooms) == 0 {
		t.Fatal("change did not enable ordinary app state")
	}
	u, err := s.User(victim.Name)
	if err != nil || u.MustReset {
		t.Fatal("MustReset did not clear", err)
	}
}

func TestGUIPasswordChangeKeepsCurrentSessionAndDeviceKeys(t *testing.T) {
	s := testStore(t)
	u := registerUser(t, s, "gui-password-user")
	if err := s.RecordSession(&Client{ID: "other-device", Name: u.Name, IP: "192.0.2.111", AuthEpoch: u.AuthEpoch, Ctx: t.Context()}); err != nil {
		t.Fatal(err)
	}
	key := transportSigner(t)
	if _, err := s.BindSSHKey(u.Name, "synthetic device", string(gossh.MarshalAuthorizedKey(key.PublicKey()))); err != nil {
		t.Fatal(err)
	}
	h := newHub()
	f := newGUIWireFixture(t, s, h)
	f.send(guiRequest{Type: "login", RequestID: "auth", Username: u.Name, Password: "test-password-123"})
	f.read("auth", "auth")
	f.read("state", "")
	var current string
	if err := s.db.QueryRow("SELECT id FROM sessions WHERE user=? AND client='native-fixture/gpui'", u.Name).Scan(&current); err != nil {
		t.Fatal(err)
	}
	f.send(guiRequest{Type: "change_password", RequestID: "mismatch", Password: "test-password-123", NewPassword: "different-fixture-123", Confirm: "wrong-confirmation"})
	f.read("error", "mismatch")
	if !s.SessionValid("other-device") || !s.SessionValid(current) {
		t.Fatal("failed confirmation changed sessions")
	}
	f.send(guiRequest{Type: "change_password", RequestID: "change", Password: "test-password-123", NewPassword: "new-fixture-password", Confirm: "new-fixture-password"})
	f.read("password_changed", "change")
	f.read("state", "")
	if !s.SessionValid(current) || s.SessionValid("other-device") {
		t.Fatal("incorrect password-change session preservation")
	}
	if keys, err := s.SSHKeys(u.Name); err != nil || len(keys) != 1 {
		t.Fatal("ordinary password change revoked device keys", err)
	}
	if _, err := s.Authenticate(u.Name, "test-password-123"); err == nil {
		t.Fatal("old password still valid")
	}
	if _, err := s.Authenticate(u.Name, "new-fixture-password"); err != nil {
		t.Fatal("new password not valid", err)
	}
}
