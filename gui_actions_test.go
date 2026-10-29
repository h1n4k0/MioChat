package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestGUINativeConversationActionsAndHistory(t *testing.T) {
	s := testStore(t)
	registerUser(t, s, "native-alice")
	registerUser(t, s, "native-bob")
	h := newHub()
	h.Attach(s)
	u, _ := s.User("native-alice")
	client := &Client{ID: "native-actions-session", Name: u.Name, AuthEpoch: u.AuthEpoch, Ctx: t.Context()}
	if err := s.RecordSession(client); err != nil {
		t.Fatal(err)
	}
	scope := guiScope{room: "lobby"}
	run := func(command string, before int64) guiActionResult {
		t.Helper()
		r, err := guiExecuteAction(s, h, u, scope, 0, guiRequest{Body: command, Before: before, sessionID: client.ID, ctx: t.Context()})
		if err != nil {
			t.Fatalf("%s: %v", command, err)
		}
		return r
	}
	dm := run("/dm native-bob", 0)
	if dm.scope.key() != "dm:native-bob" {
		t.Fatal(dm.scope)
	}
	scope = dm.scope
	first := mustSend(t, s, u.Name, "", "native-bob", "first native DM")
	second := mustSend(t, s, "native-bob", "", u.Name, "second native DM")
	if got := run("/history 1", 0); !got.replace || len(got.messages) != 1 || got.messages[0].ID != second.ID {
		t.Fatal(got)
	}
	if got := run("/older", second.ID); got.replace || len(got.messages) != 1 || got.messages[0].ID != first.ID {
		t.Fatal(got)
	}
	if got := run("/search first", 0); got.panel == nil || len(got.panel.Actions) != 1 {
		t.Fatal(got)
	}
	if got := run(fmt.Sprintf("/reply %d quoted reply", first.ID), 0); got.message == nil || got.message.ReplyTo != first.ID {
		t.Fatal(got)
	}
	run(fmt.Sprintf("/edit %d edited DM", first.ID), 0)
	run(fmt.Sprintf("/react %d heart", first.ID), 0)
	run(fmt.Sprintf("/bookmark %d", first.ID), 0)
	if got := run("/bookmarks", 0); len(got.panel.Actions) != 1 {
		t.Fatal(got)
	}
	if got := run(fmt.Sprintf("/jump %d", first.ID), 0); !got.replace || got.scope != scope || len(got.messages) < 2 {
		t.Fatal(got)
	}
	run(fmt.Sprintf("/recall %d", first.ID), 0)
	message, err := s.Message(u.Name, first.ID)
	if err != nil || message.Recalled == 0 || strings.Contains(message.Body, "edited DM") {
		t.Fatal(message, err)
	}
	if guiMessageForViewer(second, u.Name).Room != "dm:native-bob" || guiMessageForViewer(first, "native-bob").Room != "dm:native-alice" {
		t.Fatal("viewer-relative DM scope is incorrect")
	}
}

func TestGUINativeActionsRevocationAndPrivateBoundaries(t *testing.T) {
	s := testStore(t)
	for _, name := range []string{"private-owner", "private-reader", "private-admin", "private-other"} {
		registerUser(t, s, name)
	}
	if _, err := s.db.Exec("UPDATE users SET role='admin' WHERE name='private-admin'"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRoomAccess("gui-private", "private-owner", true); err != nil {
		t.Fatal(err)
	}
	if err := s.RoomMember("private-owner", "gui-private", "private-reader", "invite"); err != nil {
		t.Fatal(err)
	}
	m := mustSend(t, s, "private-owner", "gui-private", "", "private fixture body")
	dm := mustSend(t, s, "private-owner", "", "private-other", "unreported DM")
	h := newHub()
	h.Attach(s)
	reader, _ := s.User("private-reader")
	admin, _ := s.User("private-admin")
	scope := guiScope{room: "gui-private"}
	if _, err := guiExecuteAction(s, h, reader, scope, 0, guiRequest{Body: fmt.Sprintf("/bookmark %d", m.ID)}); err != nil {
		t.Fatal(err)
	}
	if err := s.RoomMember("private-owner", "gui-private", "private-reader", "remove"); err != nil {
		t.Fatal(err)
	}
	for _, user := range []User{reader, admin} {
		for _, command := range []string{"/history", "/search private", "/pins", fmt.Sprintf("/read %d", m.ID), fmt.Sprintf("/reply %d", m.ID), fmt.Sprintf("/bookmark %d", m.ID)} {
			if _, err := guiExecuteAction(s, h, user, scope, 0, guiRequest{Body: command}); err == nil {
				t.Fatalf("%s accessed private room with %s", user.Name, command)
			}
		}
	}
	for _, command := range []string{fmt.Sprintf("/read %d", dm.ID), fmt.Sprintf("/jump %d", dm.ID), fmt.Sprintf("/reply %d", dm.ID), fmt.Sprintf("/bookmark %d", dm.ID)} {
		if _, err := guiExecuteAction(s, h, admin, guiScope{room: "lobby"}, 0, guiRequest{Body: command}); err == nil {
			t.Fatalf("admin read unrelated DM: %s", command)
		}
	}
	bookmarks, err := s.Bookmarks(reader.Name)
	if err != nil || len(bookmarks) != 0 {
		t.Fatal("revoked bookmark leaked", bookmarks, err)
	}
}

func TestGUINativeReplyScopeAndEditOwnership(t *testing.T) {
	s := testStore(t)
	registerUser(t, s, "scope-alice")
	registerUser(t, s, "scope-bob")
	h := newHub()
	h.Attach(s)
	u, _ := s.User("scope-alice")
	dm := mustSend(t, s, "scope-bob", "", u.Name, "private reply")
	public := mustSend(t, s, "scope-bob", "lobby", "", "public reply")
	for _, command := range []string{fmt.Sprintf("/reply %d", dm.ID), fmt.Sprintf("/edit %d forbidden", public.ID)} {
		if _, err := guiExecuteAction(s, h, u, guiScope{room: "lobby"}, 0, guiRequest{Body: command}); err == nil {
			t.Fatal("accepted", command)
		}
	}
}

func TestGUINativeTopicReadAndPinOwnership(t *testing.T) {
	s := testStore(t)
	registerUser(t, s, "native-owner")
	registerUser(t, s, "native-member")
	h := newHub()
	h.Attach(s)
	if err := s.CreateRoomAccess("native-room", "native-owner", false); err != nil {
		t.Fatal(err)
	}
	owner, _ := s.User("native-owner")
	member, _ := s.User("native-member")
	if err := s.Topic(owner, "native-room", "original topic"); err != nil {
		t.Fatal(err)
	}
	scope := guiScope{room: "native-room"}
	if r, err := guiExecuteAction(s, h, member, scope, 0, guiRequest{Body: "/topic"}); err != nil || r.panel == nil || r.panel.Lines[0] != "original topic" {
		t.Fatal(r, err)
	}
	if room, _ := s.Room("native-room"); room.Topic != "original topic" {
		t.Fatal("viewing a topic mutated it")
	}
	m := mustSend(t, s, owner.Name, "native-room", "", "pin fixture")
	if _, err := guiExecuteAction(s, h, member, scope, 0, guiRequest{Body: fmt.Sprintf("/pin %d", m.ID)}); err == nil {
		t.Fatal("member pinned without permission")
	}
	if _, err := guiExecuteAction(s, h, owner, scope, 0, guiRequest{Body: fmt.Sprintf("/pin %d", m.ID)}); err != nil {
		t.Fatal(err)
	}
	if r, err := guiExecuteAction(s, h, member, scope, 0, guiRequest{Body: "/pins"}); err != nil || len(r.panel.Actions) != 1 {
		t.Fatal(r, err)
	}
}
