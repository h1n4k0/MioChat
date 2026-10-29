package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strconv"
	"testing"
	"time"
)

type guiWireFixture struct {
	t         *testing.T
	writer    *json.Encoder
	responses <-chan guiResponse
	done      <-chan error
}

func newGUIWireFixture(t *testing.T, store *Store, hub *Hub) guiWireFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	responses := make(chan guiResponse, 256)
	go func() {
		err := runGUIProtocol(ctx, inR, outW, "192.0.2.70", "native-fixture", func() { cancel(); inR.Close() }, store, hub)
		if err != nil {
			// Preserve the actual failure when the reader only observes EOF.
			t.Logf("GUI protocol fixture stopped: %v", err)
		}
		done <- err
		outW.Close()
	}()
	go func() {
		defer close(responses)
		d := json.NewDecoder(bufio.NewReader(outR))
		for {
			var r guiResponse
			if d.Decode(&r) != nil {
				return
			}
			select {
			case responses <- r:
			case <-ctx.Done():
				return
			}
		}
	}()
	t.Cleanup(func() {
		cancel()
		inR.Close()
		inW.Close()
		outR.Close()
		outW.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("protocol fixture did not stop")
		}
	})
	f := guiWireFixture{t: t, writer: json.NewEncoder(inW), responses: responses, done: done}
	if f.read("hello", "").Protocol != 1 {
		t.Fatal("wrong hello")
	}
	return f
}
func (f guiWireFixture) send(r guiRequest) {
	f.t.Helper()
	if err := f.writer.Encode(r); err != nil {
		f.t.Fatal(err)
	}
}
func (f guiWireFixture) read(kind, id string) guiResponse {
	f.t.Helper()
	timeout := time.After(4 * time.Second)
	for {
		select {
		case r, ok := <-f.responses:
			if !ok {
				f.t.Fatal("protocol ended early")
			}
			if r.Type == kind && (id == "" || r.RequestID == id) {
				return r
			}
		case <-timeout:
			f.t.Fatalf("missing %s %s", kind, id)
		}
	}
}
func TestGUINativeWireDMCorrelationAndReply(t *testing.T) {
	s := testStore(t)
	registerUser(t, s, "wire-alice")
	registerUser(t, s, "wire-bob")
	h := newHub()
	f := newGUIWireFixture(t, s, h)
	f.send(guiRequest{Type: "command", RequestID: "unauth", Room: "lobby", Body: "/create forbidden"})
	f.read("error", "unauth")
	if _, err := s.Room("forbidden"); err == nil {
		t.Fatal("unauthenticated action mutated store")
	}
	f.send(guiRequest{Type: "login", RequestID: "auth", Username: "wire-alice", Password: "test-password-123"})
	f.read("auth", "auth")
	f.read("state", "")
	f.send(guiRequest{Type: "command", RequestID: "dm", Room: "lobby", Body: "/dm wire-bob"})
	if r := f.read("action", "dm"); r.Room != "dm:wire-bob" {
		t.Fatal(r)
	}
	f.read("state", "")
	f.send(guiRequest{Type: "send_message", RequestID: "stale", Room: "lobby", Body: "must not send"})
	f.read("error", "stale")
	f.send(guiRequest{Type: "send_message", RequestID: "dm-send", Room: "dm:wire-bob", Body: "wire DM only"})
	r := f.read("sent", "dm-send")
	if r.Message == nil || r.Message.Sender != "wire-alice" || r.Message.Recipient != "wire-bob" || r.Message.Room != "dm:wire-bob" {
		t.Fatal(r)
	}
	f.send(guiRequest{Type: "command", RequestID: "reply", Room: "dm:wire-bob", Body: "/reply " + strconv.FormatInt(r.Message.ID, 10)})
	if reply := f.read("action", "reply"); reply.ReplyTo != r.Message.ID {
		t.Fatal(reply)
	}
	f.send(guiRequest{Type: "send_message", RequestID: "reply-send", Room: "dm:wire-bob", Body: "wire quoted reply", ReplyTo: r.Message.ID})
	if reply := f.read("sent", "reply-send"); reply.Message == nil || reply.Message.ReplyTo != r.Message.ID || reply.Message.ReplyBody != "wire DM only" {
		t.Fatal(reply)
	}
}

func TestGUINativeWirePrivateRemovalAndReset(t *testing.T) {
	s := testStore(t)
	registerUser(t, s, "wire-owner")
	registerUser(t, s, "wire-reader")
	if err := s.CreateRoomAccess("wire-private", "wire-owner", true); err != nil {
		t.Fatal(err)
	}
	if err := s.RoomMember("wire-owner", "wire-private", "wire-reader", "invite"); err != nil {
		t.Fatal(err)
	}
	secret := mustSend(t, s, "wire-owner", "wire-private", "", "synthetic private content")
	h := newHub()
	f := newGUIWireFixture(t, s, h)
	f.send(guiRequest{Type: "login", RequestID: "auth", Username: "wire-reader", Password: "test-password-123"})
	f.read("auth", "auth")
	f.read("state", "")
	f.send(guiRequest{Type: "select_room", RequestID: "join", Room: "wire-private"})
	f.read("state", "join")
	if err := s.RoomMember("wire-owner", "wire-private", "wire-reader", "remove"); err != nil {
		t.Fatal(err)
	}
	h.RemoveRoomUser("wire-reader", "wire-private")
	if r := f.read("state", ""); r.Room != "lobby" {
		t.Fatal(r)
	}
	h.Broadcast(secret) // A delayed event cannot regain its former ACL.
	f.send(guiRequest{Type: "command", RequestID: "revoked-read", Room: "lobby", Body: "/read " + strconv.FormatInt(secret.ID, 10)})
	f.read("error", "revoked-read")
	if _, err := s.db.Exec("UPDATE user_meta SET must_reset=1 WHERE name='wire-reader'"); err != nil {
		t.Fatal(err)
	}
	f.send(guiRequest{Type: "send_message", RequestID: "reset-send", Room: "lobby", Body: "must not send"})
	f.read("error", "reset-send")
	history, err := s.History("lobby", "wire-owner", "", 100)
	if err != nil || len(history) != 0 {
		t.Fatal("reset account sent a message", history, err)
	}
}

func TestGUINativeWireSessionOwnershipAndServerContext(t *testing.T) {
	s := testStore(t)
	alice := registerUser(t, s, "session-alice")
	bob := registerUser(t, s, "session-bob")
	for _, c := range []*Client{
		{ID: "other-alice-fixture", Name: alice.Name, IP: "192.0.2.81", Version: "test-only", AuthEpoch: alice.AuthEpoch, Ctx: t.Context()},
		{ID: "foreign-bob-fixture", Name: bob.Name, IP: "192.0.2.82", Version: "test-only", AuthEpoch: bob.AuthEpoch, Ctx: t.Context()},
	} {
		if err := s.RecordSession(c); err != nil {
			t.Fatal(err)
		}
	}
	h := newHub()
	f := newGUIWireFixture(t, s, h)
	f.send(guiRequest{Type: "login", RequestID: "auth", Username: alice.Name, Password: "test-password-123"})
	f.read("auth", "auth")
	f.read("state", "")
	var current string
	if err := s.db.QueryRow("SELECT id FROM sessions WHERE user=? AND client='native-fixture/gpui'", alice.Name).Scan(&current); err != nil {
		t.Fatal(err)
	}
	// Raw client fields must never choose the preserved session or actor.
	if err := f.writer.Encode(map[string]any{"type": "command", "request_id": "others", "room": "lobby", "body": "/logoutothers", "sessionID": "foreign-bob-fixture", "session_id": "foreign-bob-fixture", "actor": bob.Name}); err != nil {
		t.Fatal(err)
	}
	f.read("action", "others")
	if !s.SessionValid(current) || s.SessionValid("other-alice-fixture") || !s.SessionValid("foreign-bob-fixture") {
		t.Fatal("logoutothers trusted client context or revoked the wrong account")
	}
	for i, command := range []string{"/sessions session-bob", "/logins session-bob", "/disconnect foreign-bob-fixture"} {
		id := strconv.Itoa(i)
		f.send(guiRequest{Type: "command", RequestID: id, Room: "lobby", Body: command})
		f.read("error", id)
	}
	f.send(guiRequest{Type: "command", RequestID: "own-sessions", Room: "lobby", Body: "/sessions"})
	if r := f.read("action", "own-sessions"); r.Panel == nil || len(r.Panel.Lines) < 2 {
		t.Fatal(r)
	}
	f.send(guiRequest{Type: "command", RequestID: "quit", Room: "lobby", Body: "/quit"})
	if r := f.read("action", "quit"); r.Status != "bye" {
		t.Fatal("explicit exit needs a correlated graceful acknowledgement", r)
	}
}
