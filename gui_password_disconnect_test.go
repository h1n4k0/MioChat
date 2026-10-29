package main

import (
	"context"
	"encoding/json"
	"errors"
	gossh "golang.org/x/crypto/ssh"
	"net"
	"testing"
	"time"
)

func TestGUIPasswordDisconnectBeforeCommitKeepsCredentials(t *testing.T) {
	s := testStore(t)
	u := registerUser(t, s, "disconnect-fixture")
	if err := s.RecordSession(&Client{ID: "retained-other-device", Name: u.Name, IP: "192.0.2.122", AuthEpoch: u.AuthEpoch, Ctx: t.Context()}); err != nil {
		t.Fatal(err)
	}
	h := newHub()
	signer := transportSigner(t)
	server := makeServer(s, h, signer)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go server.Serve(listener)
	t.Cleanup(func() { h.stopUI(); server.Close(); h.uiTasks.Wait() })
	client, err := gossh.Dial("tcp", listener.Addr().String(), &gossh.ClientConfig{User: "chat", Auth: []gossh.AuthMethod{transportAnonymousAuth()}, HostKeyCallback: gossh.FixedHostKey(signer.PublicKey()), Timeout: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	sshSession, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sshSession.Close()
	in, err := sshSession.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := sshSession.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = sshSession.RequestSubsystem(guiSubsystemName); err != nil {
		t.Fatal(err)
	}
	responses := make(chan guiResponse, 64)
	go func() {
		defer close(responses)
		d := json.NewDecoder(out)
		for {
			var r guiResponse
			if d.Decode(&r) != nil {
				return
			}
			responses <- r
		}
	}()
	f := guiWireFixture{t: t, writer: json.NewEncoder(in), responses: responses}
	f.read("hello", "")
	f.send(guiRequest{Type: "login", RequestID: "auth", Username: u.Name, Password: "test-password-123"})
	f.read("auth", "auth")
	f.read("state", "")
	// Drain the guaranteed join-presence roster before taking the writer;
	// otherwise that read, rather than the password transaction, is blocked.
	if roster := f.read("state", ""); roster.Status != "roster" {
		t.Fatal("expected join roster before commit barrier")
	}
	var serverContext context.Context
	h.mu.Lock()
	for c := range h.clients {
		if c.Name == u.Name {
			serverContext = c.Ctx
		}
	}
	h.mu.Unlock()
	if serverContext == nil {
		t.Fatal("missing authenticated fixture context")
	}
	// Hold the single writer so the real password operation reaches BeginTx
	// after hashing but cannot commit before the deliberate SSH disconnect.
	blocker, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback()
	if _, err = blocker.Exec("UPDATE users SET bio=bio WHERE name=?", u.Name); err != nil {
		t.Fatal(err)
	}
	before := s.db.Stats().WaitCount
	f.send(guiRequest{Type: "change_password", RequestID: "change", Password: "test-password-123", NewPassword: "not-committed-fixture", Confirm: "not-committed-fixture"})
	deadline := time.After(4 * time.Second)
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for s.db.Stats().WaitCount <= before || len(h.AuthSlots) == 0 {
		select {
		case <-tick.C:
		case <-deadline:
			t.Fatal("password operation did not reach blocked commit")
		}
	}
	client.Close()
	select {
	case <-serverContext.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("SSH disconnect did not cancel server context")
	}
	if err = blocker.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Authenticate(u.Name, "test-password-123"); err != nil {
		t.Fatal("disconnect changed old credentials", err)
	}
	if _, err = s.Authenticate(u.Name, "not-committed-fixture"); err == nil {
		t.Fatal("cancelled replacement became usable")
	}
	current, err := s.User(u.Name)
	if err != nil || current.AuthEpoch != u.AuthEpoch {
		t.Fatal("cancelled operation changed credential epoch", err)
	}
	if !s.SessionValid("retained-other-device") {
		t.Fatal("cancelled change revoked another device")
	}
}

func TestCancelledPasswordContextCannotCommitPreparedProof(t *testing.T) {
	s := testStore(t)
	u := registerUser(t, s, "cancelled-proof-user")
	if err := s.RecordSession(&Client{ID: "proof-fixture", Name: u.Name, IP: "192.0.2.123", AuthEpoch: u.AuthEpoch, Ctx: t.Context()}); err != nil {
		t.Fatal(err)
	}
	_, proof, err := s.verifyPassword(u.Name, "test-password-123")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err = s.commitPasswordChangeContext(ctx, proof, []byte("not-a-real-hash"), []byte("fixture-salt"), "proof-fixture"); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled prepared commit accepted", err)
	}
	if _, err = s.Authenticate(u.Name, "test-password-123"); err != nil {
		t.Fatal("cancelled proof mutated credentials", err)
	}
}
