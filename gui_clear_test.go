package main

import (
	"reflect"
	"strconv"
	"testing"
	"time"
)

func TestGUIClearIsOnlyAViewInstruction(t *testing.T) {
	s := testStore(t)
	u := registerUser(t, s, "clear-alice")
	registerUser(t, s, "clear-bob")
	h := newHub()
	h.Attach(s)
	message := mustSend(t, s, "clear-bob", "lobby", "", "keep stored history")
	_, err := s.SavePositionVersioned(u.Name, "room:lobby", Position{
		Draft: "keep draft\nsecond line", ReplyTo: message.ID,
		LastRead: message.ID, Anchor: message.ID, Offset: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	position, err := s.PositionWithError(u.Name, "room:lobby")
	if err != nil {
		t.Fatal(err)
	}
	history, err := s.History("lobby", u.Name, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		r, err := guiExecuteAction(s, h, u, guiScope{room: "lobby"}, message.ID, guiRequest{
			Body: "/clear", Username: "clear-bob", Room: "dm:clear-bob",
		})
		if err != nil || !r.clearView || !r.at_latest || r.replace || r.scope.key() != "lobby" || r.replyTo != message.ID || len(r.messages) != 0 || r.message != nil || r.draft != nil {
			t.Fatal("clear altered more than the local view", r, err)
		}
		current, err := s.PositionWithError(u.Name, "room:lobby")
		if err != nil || !reflect.DeepEqual(current, position) {
			t.Fatal("clear mutated draft/CAS/position", current, position, err)
		}
		currentHistory, err := s.History("lobby", u.Name, "", 100)
		if err != nil || !reflect.DeepEqual(currentHistory, history) {
			t.Fatal("clear deleted or edited history", currentHistory, err)
		}
	}
	if _, err := guiExecuteAction(s, h, u, guiScope{room: "lobby"}, 0, guiRequest{Body: "/clear extra"}); err == nil {
		t.Fatal("clear accepted invalid arguments")
	}
	if _, err := guiExecuteAction(s, h, u, guiScope{room: "not-readable"}, 0, guiRequest{Body: "/clear"}); err == nil {
		t.Fatal("clear bypassed current scope validation")
	}
	if !guiImplementedCommand("/clear") {
		t.Fatal("clear is absent from native menu")
	}
}

func TestGUIClearWireScopeDraftAndSecondDevice(t *testing.T) {
	s := testStore(t)
	u := registerUser(t, s, "clear-wire")
	registerUser(t, s, "clear-peer")
	message := mustSend(t, s, "clear-peer", "lobby", "", "both devices retain this")
	if _, err := s.SavePositionVersioned(u.Name, "room:lobby", Position{Draft: "unsent text", ReplyTo: message.ID, Anchor: message.ID, Offset: 1}); err != nil {
		t.Fatal(err)
	}
	h := newHub()
	first := newGUIWireFixture(t, s, h)
	first.send(guiRequest{Type: "command", RequestID: "unauth-clear", Room: "lobby", Body: "/clear"})
	first.read("error", "unauth-clear")
	first.send(guiRequest{Type: "login", RequestID: "login", Username: u.Name, Password: "test-password-123"})
	first.read("auth", "login")
	first.read("state", "")
	second := newGUIWireFixture(t, s, h)
	second.send(guiRequest{Type: "login", RequestID: "login", Username: u.Name, Password: "test-password-123"})
	second.read("auth", "login")
	second.read("state", "")
	first.send(guiRequest{Type: "command", RequestID: "reply", Room: "lobby", Body: "/reply " + strconv.FormatInt(message.ID, 10)})
	first.read("action", "reply")
	before, err := s.PositionWithError(u.Name, "room:lobby")
	if err != nil {
		t.Fatal(err)
	}
	first.send(guiRequest{Type: "command", RequestID: "stale-clear", Room: "dm:clear-peer", Body: "/clear"})
	first.read("error", "stale-clear")
	first.send(guiRequest{Type: "command", RequestID: "clear-one", Username: "clear-peer", Room: "lobby", Body: "/clear"})
	clear := first.read("action", "clear-one")
	if !clear.ClearView || !clear.AtLatest || clear.Replace || clear.Room != "lobby" || clear.ReplyTo != message.ID || len(clear.Messages) != 0 || clear.Message != nil || clear.Draft != nil {
		t.Fatal("invalid clear acknowledgement", clear)
	}
	after, err := s.PositionWithError(u.Name, "room:lobby")
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("clear changed persisted draft/position", before, after, err)
	}
	// A later correlated command is a barrier: no clear instruction may be
	// broadcast to the second connection before its history result.
	second.send(guiRequest{Type: "command", RequestID: "other-history", Room: "lobby", Body: "/history"})
	timeout := time.After(4 * time.Second)
	for {
		var r guiResponse
		select {
		case next, ok := <-second.responses:
			if !ok {
				t.Fatal("second device ended before history barrier")
			}
			r = next
		case <-timeout:
			t.Fatal("second device history barrier timed out")
		}
		if r.ClearView {
			t.Fatal("clear leaked to another device", r)
		}
		if r.Type == "action" && r.RequestID == "other-history" {
			if len(r.Messages) != 1 || r.Messages[0].ID != message.ID {
				t.Fatal("other device lost stored history", r)
			}
			break
		}
	}
	first.send(guiRequest{Type: "command", RequestID: "restore-history", Room: "lobby", Body: "/history"})
	if r := first.read("action", "restore-history"); r.ClearView || !r.Replace || len(r.Messages) != 1 || r.Messages[0].ID != message.ID {
		t.Fatal("explicit history could not restore cleared messages", r)
	}
}
