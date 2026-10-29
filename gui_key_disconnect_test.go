package main

import (
	"context"
	"testing"
	"time"
)

func TestGUIDeviceKeyDisconnectBeforeCommitDoesNotBind(t *testing.T) {
	s := testStore(t)
	owner := registerUser(t, s, "cancel-key-owner")
	h := newHub()
	device := transportSigner(t)
	f, transport := guiOverSignedSSH(t, s, h, device)
	f.read("hello", "")
	f.send(guiRequest{Type: "login", RequestID: "login", Username: owner.Name, Password: "test-password-123"})
	f.read("auth", "login")
	f.read("state", "")
	if roster := f.read("state", ""); roster.Status != "roster" {
		t.Fatal("expected join roster before writer barrier")
	}
	var sessionContext context.Context
	h.mu.Lock()
	for client := range h.clients {
		if client.Name == owner.Name {
			sessionContext = client.Ctx
		}
	}
	h.mu.Unlock()
	if sessionContext == nil {
		t.Fatal("missing authenticated synthetic SSH session")
	}
	writer, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	if _, err = writer.Exec("UPDATE users SET bio=bio WHERE name=?", owner.Name); err != nil {
		t.Fatal(err)
	}
	waits := s.db.Stats().WaitCount
	f.send(guiRequest{Type: "bind_key", RequestID: "pending-key", Body: "must not commit"})
	deadline := time.After(4 * time.Second)
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for s.db.Stats().WaitCount <= waits || len(h.AuthSlots) == 0 {
		select {
		case <-tick.C:
		case <-deadline:
			t.Fatal("key bind did not reach blocked commit")
		}
	}
	transport.Close()
	select {
	case <-sessionContext.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("SSH disconnect did not cancel key binding context")
	}
	if err = writer.Rollback(); err != nil {
		t.Fatal(err)
	}
	deadline = time.After(3 * time.Second)
	for len(h.AuthSlots) != 0 {
		select {
		case <-tick.C:
		case <-deadline:
			t.Fatal("cancelled key bind did not release authentication slot")
		}
	}
	keys, err := s.SSHKeys(owner.Name)
	if err != nil || len(keys) != 0 {
		t.Fatal("disconnected bind created durable credentials", err)
	}
}
