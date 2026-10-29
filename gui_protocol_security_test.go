package main

import (
	"bufio"
	"encoding/json"
	gossh "golang.org/x/crypto/ssh"
	"io"
	"net"
	"testing"
	"time"
)

// Real SSH, disposable loopback listener, in-memory host key and test-only Store.
// This never uses the user's SSH configuration, credentials or chat server.
func TestGUISubsystemRealSSHAuthenticationAndRevocation(t *testing.T) {
	store := testStore(t)
	registerUser(t, store, "gui-local")
	hub := newHub()
	signer := transportSigner(t)
	server := makeServer(store, hub, signer)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go server.Serve(listener)
	t.Cleanup(func() { hub.stopUI(); server.Close(); hub.uiTasks.Wait() })
	client, err := gossh.Dial("tcp", listener.Addr().String(), &gossh.ClientConfig{User: "chat", Auth: []gossh.AuthMethod{transportAnonymousAuth()}, HostKeyCallback: gossh.FixedHostKey(signer.PublicKey()), Timeout: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	in, err := session.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := session.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = session.RequestSubsystem(guiSubsystemName); err != nil {
		t.Fatal(err)
	}
	responses := make(chan guiResponse, 32)
	go func() {
		defer close(responses)
		decoder := json.NewDecoder(bufio.NewReader(out))
		for {
			var response guiResponse
			if decoder.Decode(&response) != nil {
				return
			}
			responses <- response
		}
	}()
	read := func(kind, id string) guiResponse {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case response, ok := <-responses:
				if !ok {
					t.Fatal("SSH subsystem ended early")
				}
				if response.Type == kind && (id == "" || response.RequestID == id) {
					return response
				}
			case <-deadline:
				t.Fatalf("no %s response for %s", kind, id)
			}
		}
	}
	writer := json.NewEncoder(in)
	if read("hello", "").Protocol != guiProtocolVersion {
		t.Fatal("wrong protocol")
	}
	if _, err = store.db.Exec("UPDATE user_meta SET must_reset=1 WHERE name=?", "gui-local"); err != nil {
		t.Fatal(err)
	}
	if err = writer.Encode(guiRequest{Type: "login", RequestID: "reset-required", Username: "gui-local", Password: "test-password-123"}); err != nil {
		t.Fatal(err)
	}
	if auth := read("auth", "reset-required"); auth.Status != "password_reset_required" {
		t.Fatal("reset login must be restricted", auth)
	}
	if reset := read("reset_required", ""); len(reset.Rooms) != 0 || len(reset.Messages) != 0 {
		t.Fatal("reset login leaked content")
	}
	if err = writer.Encode(guiRequest{Type: "sync", RequestID: "restricted"}); err != nil {
		t.Fatal(err)
	}
	read("error", "restricted")
	var joined int
	if err = store.db.QueryRow("SELECT count(*) FROM sessions WHERE user=?", "gui-local").Scan(&joined); err != nil || joined != 1 {
		t.Fatal("missing restricted session", err)
	}
	if err = writer.Encode(guiRequest{Type: "change_password", RequestID: "change", Password: "test-password-123", NewPassword: "changed-fixture-123", Confirm: "changed-fixture-123"}); err != nil {
		t.Fatal(err)
	}
	read("password_changed", "change")
	read("state", "")
	if err = writer.Encode(guiRequest{Type: "send_message", RequestID: "send-1", Room: "lobby", Body: "local fixture only"}); err != nil {
		t.Fatal(err)
	}
	sent := read("sent", "send-1")
	if sent.Message == nil {
		t.Fatal("missing message")
	}
	read("message", "")
	if err = store.UpdateMessage("gui-local", sent.Message.ID, "edited local fixture", false); err != nil {
		t.Fatal(err)
	}
	hub.MessageChanged(sent.Message.ID)
	updated := read("message_updated", "")
	if updated.Message == nil || updated.Message.Edited == 0 || updated.RequestID != "" {
		t.Fatal("edit must be a distinct asynchronous event")
	}
	var sessionID string
	if err = store.db.QueryRow("SELECT id FROM sessions WHERE user=? AND closed=0", "gui-local").Scan(&sessionID); err != nil {
		t.Fatal(err)
	}
	if err = store.RevokeSession("gui-local", sessionID); err != nil {
		t.Fatal(err)
	}
	hub.Refresh()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-responses:
			if !ok {
				return
			}
		case <-timeout:
			t.Fatal("revoked GUI SSH session was not closed")
			return
		}
	}
}

func TestGUIRejectsUncorrelatedAndOversizedRequests(t *testing.T) {
	for _, frame := range []string{"{\"type\":\"sync\"}\n", string(make([]byte, guiMaxFrameBytes+1)) + "\n"} {
		t.Run("bounded-frame", func(t *testing.T) {
			store := testStore(t)
			hub := newHub()
			inputR, inputW := io.Pipe()
			outputR, outputW := io.Pipe()
			defer inputR.Close()
			defer inputW.Close()
			defer outputR.Close()
			defer outputW.Close()
			go io.Copy(io.Discard, outputR)
			done := make(chan error, 1)
			go func() {
				done <- runGUIProtocol(t.Context(), inputR, outputW, "192.0.2.33", "local-test", func() {}, store, hub)
			}()
			go func() { io.WriteString(inputW, frame); inputW.Close() }()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("invalid request accepted")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("invalid frame did not terminate")
			}
		})
	}
}
