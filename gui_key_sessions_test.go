package main

import (
	"testing"

	gossh "golang.org/x/crypto/ssh"
)

func TestGUIDeviceKeyLoginSharesThreeSessionLimit(t *testing.T) {
	s := testStore(t)
	registerUser(t, s, "limit-admin")
	owner := registerUser(t, s, "limit-key-owner")
	device := transportSigner(t)
	if _, err := s.BindSSHKey(owner.Name, "temporary", string(gossh.MarshalAuthorizedKey(device.PublicKey()))); err != nil {
		t.Fatal(err)
	}
	h := newHub()
	connect := guiSSHConnections(t, s, h)
	for i := 0; i < 4; i++ {
		f, _ := connect(device)
		f.read("hello", "")
		f.send(guiRequest{Type: "login_key", RequestID: "key"})
		if i < 3 {
			f.read("auth", "key")
			f.read("state", "")
		} else {
			f.read("error", "key")
		}
	}
	h.mu.Lock()
	live := len(h.clients)
	h.mu.Unlock()
	if live != 3 {
		t.Fatalf("key auth bypassed common live-session cap: %d", live)
	}
	sessions, err := s.Sessions(owner.Name, owner.Name)
	if err != nil {
		t.Fatal(err)
	}
	active := 0
	for _, session := range sessions {
		if !session.Revoked && session.Closed == 0 {
			active++
		}
	}
	if active != 3 {
		t.Fatalf("key auth created extra durable session: %d", active)
	}
}

func TestGUIDeviceKeyPasswordChangeAndForcedResetSessionBoundaries(t *testing.T) {
	s := testStore(t)
	admin := registerUser(t, s, "password-key-admin")
	owner := registerUser(t, s, "password-key-owner")
	// A name containing "admin" grants no authority. Prove the ordinary
	// synthetic role is rejected before explicitly configuring this fixture.
	if _, err := s.ResetPassword(admin.Name, owner.Name); err == nil {
		t.Fatal("ordinary account gained reset authority")
	}
	unchanged, err := s.Authenticate(owner.Name, "test-password-123")
	if err != nil || unchanged.MustReset || unchanged.AuthEpoch != owner.AuthEpoch {
		t.Fatal("denied reset changed credentials", err)
	}
	if _, err := s.db.Exec("UPDATE users SET role='admin' WHERE name=?", admin.Name); err != nil {
		t.Fatal(err)
	}
	configured, err := s.User(admin.Name)
	if err != nil || configured.Role != "admin" {
		t.Fatal("synthetic admin setup failed", err)
	}
	device := transportSigner(t)
	key, err := s.BindSSHKey(owner.Name, "temporary", string(gossh.MarshalAuthorizedKey(device.PublicKey())))
	if err != nil {
		t.Fatal(err)
	}
	h := newHub()
	connect := guiSSHConnections(t, s, h)
	current, _ := connect(device)
	current.read("hello", "")
	current.send(guiRequest{Type: "login_key", RequestID: "key"})
	current.read("auth", "key")
	current.read("state", "")
	var currentID string
	h.mu.Lock()
	for client := range h.clients {
		currentID = client.ID
	}
	h.mu.Unlock()
	other, _ := connect(nil)
	other.read("hello", "")
	other.send(guiRequest{Type: "login", RequestID: "other", Username: owner.Name, Password: "test-password-123"})
	other.read("auth", "other")
	other.read("state", "")
	var otherID string
	h.mu.Lock()
	for client := range h.clients {
		if client.ID != currentID {
			otherID = client.ID
		}
	}
	h.mu.Unlock()
	if currentID == "" || otherID == "" {
		t.Fatal("missing synthetic sessions")
	}
	current.send(guiRequest{Type: "change_password", RequestID: "change", Password: "test-password-123", NewPassword: "changed-synthetic-only", Confirm: "changed-synthetic-only"})
	current.read("password_changed", "change")
	current.read("state", "")
	if !s.SessionValid(currentID) || s.SessionValid(otherID) {
		t.Fatal("ordinary password change did not retain only current key-authenticated session")
	}
	if keys, err := s.SSHKeys(owner.Name); err != nil || len(keys) != 1 || keys[0].ID != key.ID {
		t.Fatal("ordinary password change revoked device key", err)
	}
	again, _ := connect(device)
	again.read("hello", "")
	again.send(guiRequest{Type: "login_key", RequestID: "again"})
	again.read("auth", "again")
	again.read("state", "")
	before, err := s.User(owner.Name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ResetPassword(admin.Name, owner.Name); err != nil {
		t.Fatal(err)
	}
	h.Enforce()
	after, err := s.User(owner.Name)
	if err != nil || !after.MustReset || after.AuthEpoch <= before.AuthEpoch {
		t.Fatal("forced reset did not advance restricted credential epoch", err)
	}
	if s.SessionValid(currentID) {
		t.Fatal("forced reset retained prior current session")
	}
	if keys, err := s.SSHKeys(owner.Name); err != nil || len(keys) != 0 {
		t.Fatal("forced reset retained active device keys", err)
	}
	denied, _ := connect(device)
	denied.read("hello", "")
	denied.send(guiRequest{Type: "login_key", RequestID: "after-reset"})
	denied.read("error", "after-reset")
}
