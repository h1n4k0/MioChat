package main

import (
	"fmt"
	"testing"
)

func TestGUIRoomDirectoryMembersAndMutationACL(t *testing.T) {
	s := testStore(t)
	for _, name := range []string{"room-owner", "room-admin", "room-mod", "room-member", "room-other"} {
		registerUser(t, s, name)
	}
	if _, err := s.db.Exec(`UPDATE users SET role='user'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE users SET role='admin' WHERE name='room-admin'`); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRoomAccess("native-private", "room-owner", true); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"room-mod", "room-member"} {
		if err := s.RoomMember("room-owner", "native-private", name, "invite"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RoomMember("room-owner", "native-private", "room-mod", "mod"); err != nil {
		t.Fatal(err)
	}
	h := newHub()
	h.Attach(s)
	run := func(actor, scope, cmd string) (guiActionResult, error) {
		u, _ := s.User(actor)
		return guiExecuteAction(s, h, u, guiScope{room: scope}, 0, guiRequest{Body: cmd})
	}
	for _, actor := range []string{"room-admin", "room-other"} {
		p, err := guiRoomDirectory(s, actor, true)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range p.Rooms {
			if r.Name == "native-private" {
				t.Fatal("private room leaked to ", actor)
			}
		}
		for _, cmd := range []string{"/join native-private"} {
			if _, err := run(actor, "lobby", cmd); err == nil {
				t.Fatal("uninvited entered private room")
			}
		}
		for _, cmd := range []string{"/roominfo", "/members", "/invite room-other", "/remove room-member", "/leave"} {
			if _, err := run(actor, "native-private", cmd); err == nil {
				t.Fatal("private scope accepted", actor, cmd)
			}
		}
	}
	for _, actor := range []string{"room-member", "room-mod", "room-owner"} {
		r, err := run(actor, "native-private", "/members")
		if err != nil {
			t.Fatal(err)
		}
		if r.panel.Kind != "members" || r.panel.Scope != "native-private" || len(r.panel.Members) != 3 {
			t.Fatal(r.panel)
		}
		manage := actor != "room-member"
		if r.panel.Details.CanManage != manage || r.panel.Details.CanLeave != (actor != "room-owner") {
			t.Fatal("incorrect controls", r.panel.Details)
		}
		for _, m := range r.panel.Members {
			if m.CanRemove != (manage && m.Name != "room-owner") {
				t.Fatal("incorrect remove control", m)
			}
		}
	}
	if _, err := run("room-member", "native-private", "/invite room-other"); err == nil {
		t.Fatal("ordinary member invited")
	}
	if _, err := run("room-mod", "native-private", "/remove room-owner"); err == nil {
		t.Fatal("removed owner")
	}
	if _, err := run("room-owner", "native-private", "/leave"); err == nil {
		t.Fatal("owner left without transfer")
	}
	other, _ := s.User("room-other")
	if _, err := run("room-owner", "native-private", fmt.Sprintf("/invite #%d", other.UID)); err != nil {
		t.Fatal(err)
	}
	if !s.CanRead("room-other", "native-private") {
		t.Fatal("invite did not grant membership")
	}
	// A previously displayed management panel never grants future authority.
	if err := s.RoomMember("room-owner", "native-private", "room-mod", "unmod"); err != nil {
		t.Fatal(err)
	}
	if _, err := run("room-mod", "native-private", "/remove room-member"); err == nil {
		t.Fatal("stale moderator mutated membership")
	}
	if _, err := run("room-other", "native-private", "/leave"); err != nil {
		t.Fatal(err)
	}
	if s.CanRead("room-other", "native-private") {
		t.Fatal("leave retained private access")
	}
	for _, cmd := range []string{"/invite room-other extra", "/remove", "/leave extra"} {
		if _, err := run("room-owner", "native-private", cmd); err == nil {
			t.Fatal("malformed mutation accepted", cmd)
		}
	}
	if _, err := guiExecuteAction(s, h, other, guiScope{peer: "room-owner"}, 0, guiRequest{Body: "/invite room-member"}); err == nil {
		t.Fatal("DM membership action accepted")
	}
	if err := s.CreateRoomAccess("deleted-room", "room-owner", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM members WHERE room='deleted-room'; DELETE FROM room_meta WHERE name='deleted-room'; DELETE FROM rooms WHERE name='deleted-room'`); err != nil {
		t.Fatal(err)
	}
	if _, err := run("room-owner", "deleted-room", "/members"); err == nil {
		t.Fatal("deleted room panel accepted")
	}
}

func TestGUIRoomWireUIDRemovalMovesAllDevicesAndPreservesDraft(t *testing.T) {
	s := testStore(t)
	registerUser(t, s, "wire-room-owner")
	registerUser(t, s, "wire-room-reader")
	if err := s.CreateRoomAccess("wire-room", "wire-room-owner", true); err != nil {
		t.Fatal(err)
	}
	if err := s.RoomMember("wire-room-owner", "wire-room", "wire-room-reader", "invite"); err != nil {
		t.Fatal(err)
	}
	h := newHub()
	owner := newGUIWireFixture(t, s, h)
	reader := newGUIWireFixture(t, s, h)
	otherDevice := newGUIWireFixture(t, s, h)
	for _, f := range []struct {
		fixture guiWireFixture
		user    string
	}{{owner, "wire-room-owner"}, {reader, "wire-room-reader"}, {otherDevice, "wire-room-reader"}} {
		f.fixture.send(guiRequest{Type: "login", RequestID: "login", Username: f.user, Password: "test-password-123"})
		f.fixture.read("auth", "login")
		f.fixture.read("state", "")
		f.fixture.send(guiRequest{Type: "select_room", RequestID: "room", Room: "wire-room"})
		f.fixture.read("state", "room")
	}
	reader.send(guiRequest{Type: "draft_save", RequestID: "draft", Room: "wire-room", Body: "private unsent draft", Version: 0})
	reader.read("draft_saved", "draft")
	// Login/selection can enqueue earlier roster frames. A correlated sync
	// barrier proves both devices have consumed those before revocation.
	for _, f := range []guiWireFixture{reader, otherDevice} {
		f.send(guiRequest{Type: "sync", RequestID: "before-remove"})
		f.read("state", "before-remove")
	}
	user, _ := s.User("wire-room-reader")
	owner.send(guiRequest{Type: "command", RequestID: "remove", Room: "wire-room", Body: fmt.Sprintf("/remove #%d", user.UID)})
	owner.read("action", "remove")
	for _, f := range []guiWireFixture{reader, otherDevice} {
		if r := f.read("state", ""); r.Room != "lobby" || r.ReplyTo != 0 {
			t.Fatal("device did not leave revoked scope", r)
		}
		f.send(guiRequest{Type: "command", RequestID: "stale", Room: "wire-room", Body: "/members"})
		f.read("error", "stale")
		f.send(guiRequest{Type: "select_room", RequestID: "reenter", Room: "wire-room"})
		f.read("error", "reenter")
	}
	p, err := s.PositionWithError(user.Name, "room:wire-room")
	if err != nil || p.Draft != "private unsent draft" {
		t.Fatal("revocation erased draft", p, err)
	}
	if _, err := s.PositionWithError("wire-room-owner", "room:wire-room"); err != nil {
		t.Fatal(err)
	}
}

func TestLeaveRoomRechecksResetAndOwnerInTransaction(t *testing.T) {
	s := testStore(t)
	registerUser(t, s, "leave-owner")
	registerUser(t, s, "leave-member")
	if err := s.CreateRoomAccess("leave-private", "leave-owner", true); err != nil {
		t.Fatal(err)
	}
	if err := s.RoomMember("leave-owner", "leave-private", "leave-member", "invite"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE user_meta SET must_reset=1 WHERE name='leave-member'`); err != nil {
		t.Fatal(err)
	}
	if err := s.LeaveRoom("leave-member", "leave-private"); err == nil {
		t.Fatal("reset-only actor mutated membership")
	}
	if !s.CanRead("leave-member", "leave-private") {
		t.Fatal("denied leave changed membership")
	}
	if err := s.LeaveRoom("leave-owner", "leave-private"); err == nil {
		t.Fatal("owner left")
	}
}
