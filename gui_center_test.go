package main

import (
	"fmt"
	"testing"
	"time"
)

func TestGUICenterOwnedContactsNotificationsAndRecipientActions(t *testing.T) {
	s := testStore(t)
	for _, name := range []string{"center-alice", "center-bob", "center-eve"} {
		registerUser(t, s, name)
	}
	if err := s.Relation("center-bob", "center-alice", "request", "", false); err != nil {
		t.Fatal(err)
	}
	if err := s.Relation("center-alice", "center-eve", "follow", "", false); err != nil {
		t.Fatal(err)
	}
	mustSend(t, s, "center-bob", "", "center-alice", "owned private message")
	mustSend(t, s, "center-eve", "", "center-bob", "foreign private message")
	p, err := guiCenterSnapshot(s, "center-alice")
	if err != nil {
		t.Fatal(err)
	}
	if p.User != "center-alice" || p.Unread < 2 {
		t.Fatal(p)
	}
	request, following, recent := false, false, false
	for _, c := range p.Contacts {
		request = request || c.Group == "requests" && c.Name == "center-bob"
		following = following || c.Group == "following" && c.Name == "center-eve"
		recent = recent || c.Group == "recent" && c.Name == "center-bob"
		if c.Group == "recent" && c.Name == "center-eve" {
			t.Fatal("foreign DM relation leaked")
		}
	}
	if !request || !following || !recent {
		t.Fatal(p.Contacts)
	}
	if err := guiCenterMutation(s, "center-eve", "", "/accept", "center-bob"); err == nil {
		t.Fatal("foreign recipient accepted request")
	}
	bob, _ := s.User("center-bob")
	if err := guiCenterMutation(s, "center-alice", "", "/accept", fmt.Sprintf("#%d", bob.UID)); err != nil {
		t.Fatal(err)
	}
	if len(s.Relations("center-alice", "friend")) != 1 || len(s.Relations("center-bob", "friend")) != 1 || len(s.PendingFriends("center-alice")) != 0 {
		t.Fatal("acceptance was not reciprocal")
	}
	if err := guiCenterMutation(s, "center-alice", "", "/decline", "center-bob"); err == nil {
		t.Fatal("stale request action accepted")
	}
	before, _ := s.Notifications("center-alice", false)
	foreignUnread := s.UnreadNotifications("center-bob")
	if err := guiCenterMutation(s, "center-alice", "", "/notifyclear", ""); err != nil {
		t.Fatal(err)
	}
	after, _ := s.Notifications("center-alice", false)
	if len(before) != len(after) || s.UnreadNotifications("center-alice") != 0 {
		t.Fatal("mark read deleted notification history")
	}
	for _, n := range after {
		if !n.Seen {
			t.Fatal("old notification not read")
		}
	}
	if s.UnreadNotifications("center-bob") != foreignUnread {
		t.Fatal("foreign history was marked read")
	}
}

func TestGUICenterFailedAcceptanceRollsBackBothDirections(t *testing.T) {
	s := testStore(t)
	registerUser(t, s, "accept-owner")
	registerUser(t, s, "accept-requester")
	if err := s.Relation("accept-requester", "accept-owner", "request", "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER reject_center_friend BEFORE INSERT ON relations WHEN NEW.kind='friend' AND NEW.owner='accept-requester' BEGIN SELECT RAISE(FAIL,'synthetic failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := guiCenterMutation(s, "accept-owner", "", "/accept", "accept-requester"); err == nil {
		t.Fatal("partial acceptance succeeded")
	}
	if len(s.PendingFriends("accept-owner")) != 1 || len(s.Relations("accept-owner", "friend")) != 0 || len(s.Relations("accept-requester", "friend")) != 0 {
		t.Fatal("failed acceptance left partial relationship")
	}
	if _, err := s.db.Exec(`UPDATE user_meta SET must_reset=1 WHERE name='accept-owner'`); err != nil {
		t.Fatal(err)
	}
	if _, err := guiCenterSnapshot(s, "accept-owner"); err == nil {
		t.Fatal("reset account read control center")
	}
	if err := guiCenterMutation(s, "accept-owner", "", "/notifyclear", ""); err == nil {
		t.Fatal("reset account changed notifications")
	}
}

func TestGUICenterPrivateReferenceRevocationAndDeletedRequest(t *testing.T) {
	s := testStore(t)
	for _, name := range []string{"notice-owner", "notice-reader", "notice-admin"} {
		registerUser(t, s, name)
	}
	if _, err := s.db.Exec(`UPDATE users SET role='admin' WHERE name='notice-admin'`); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRoomAccess("notice-private", "notice-owner", true); err != nil {
		t.Fatal(err)
	}
	if err := s.RoomMember("notice-owner", "notice-private", "notice-reader", "invite"); err != nil {
		t.Fatal(err)
	}
	m := mustSend(t, s, "notice-owner", "notice-private", "", "@notice-reader private fixture")
	p, err := guiCenterSnapshot(s, "notice-reader")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, n := range p.Notices {
		if n.Ref == m.ID && n.Command != "" {
			found = true
		}
	}
	if !found {
		t.Fatal("accessible mention missing")
	}
	if err := s.RoomMember("notice-owner", "notice-private", "notice-reader", "remove"); err != nil {
		t.Fatal(err)
	}
	p, err = guiCenterSnapshot(s, "notice-reader")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range p.Notices {
		if n.Kind == "mention" && (n.Command != "" || n.Ref != 0) {
			t.Fatal("revoked notification remained actionable", n)
		}
	}
	if err := s.Notify("notice-admin", "mention", "notice-owner", m.ID, "must not reveal private reference"); err != nil {
		t.Fatal(err)
	}
	admin, err := guiCenterSnapshot(s, "notice-admin")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range admin.Notices {
		if n.Ref == m.ID || n.Command != "" {
			t.Fatal("admin bypassed private room", n)
		}
	}
	if err := s.Relation("notice-owner", "notice-reader", "request", "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM relations WHERE owner='notice-owner' AND target='notice-reader' AND kind='request'`); err != nil {
		t.Fatal(err)
	}
	if err := guiCenterMutation(s, "notice-reader", "", "/accept", "notice-owner"); err == nil {
		t.Fatal("deleted request accepted")
	}
}

func TestGUICenterMutationRechecksSessionAfterTransactionWait(t *testing.T) {
	for _, command := range []string{"/notifyclear", "/accept", "/decline"} {
		t.Run(command, func(t *testing.T) {
			s := testStore(t)
			u := registerUser(t, s, "center-race")
			registerUser(t, s, "center-requester")
			if err := s.Relation("center-requester", u.Name, "request", "", false); err != nil {
				t.Fatal(err)
			}
			c := &Client{ID: "center-race-session", Name: u.Name, IP: "192.0.2.92", AuthEpoch: u.AuthEpoch, Ctx: t.Context()}
			if err := s.RecordSession(c); err != nil {
				t.Fatal(err)
			}
			blocker, err := s.db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback()
			waits := s.db.Stats().WaitCount
			done := make(chan error, 1)
			go func() { done <- guiCenterMutation(s, u.Name, c.ID, command, "center-requester") }()
			deadline := time.After(3 * time.Second)
			tick := time.NewTicker(5 * time.Millisecond)
			defer tick.Stop()
			for s.db.Stats().WaitCount <= waits {
				select {
				case <-tick.C:
				case <-deadline:
					t.Fatal("mutation did not reach transaction wait")
				}
			}
			if _, err = blocker.Exec(`UPDATE sessions SET revoked=1 WHERE id=?`, c.ID); err != nil {
				t.Fatal(err)
			}
			if err = blocker.Commit(); err != nil {
				t.Fatal(err)
			}
			select {
			case err = <-done:
				if err == nil {
					t.Fatal("revoked session changed control center")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("mutation hung")
			}
			if s.UnreadNotifications(u.Name) == 0 || len(s.PendingFriends(u.Name)) != 1 {
				t.Fatal("denied action changed data")
			}
		})
	}
}

func TestGUICenterWireSubscriptionUpdatesBothDevicesAndIsolatesActor(t *testing.T) {
	s := testStore(t)
	registerUser(t, s, "center-wire")
	registerUser(t, s, "center-peer")
	if err := s.Notify("center-wire", "poke", "center-peer", 0, "synthetic notice"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SavePositionVersioned("center-wire", "room:lobby", Position{Draft: "control-center draft"}); err != nil {
		t.Fatal(err)
	}
	h := newHub()
	a := newGUIWireFixture(t, s, h)
	b := newGUIWireFixture(t, s, h)
	for _, f := range []guiWireFixture{a, b} {
		f.send(guiRequest{Type: "login", RequestID: "login", Username: "center-wire", Password: "test-password-123"})
		f.read("auth", "login")
		f.read("state", "")
		f.send(guiRequest{Type: "control_center", RequestID: "center", Username: "center-peer"})
		r := f.read("control_center", "center")
		if r.Center == nil || r.Center.User != "center-wire" || r.Center.Unread != 1 {
			t.Fatal(r)
		}
	}
	a.send(guiRequest{Type: "command", RequestID: "mark", Room: "lobby", Body: "/notifyclear", Username: "center-peer"})
	if r := a.read("action", "mark"); r.Center == nil || r.Center.Unread != 0 {
		t.Fatal(r)
	}
	if r := b.read("control_center", ""); r.Center == nil || r.Center.User != "center-wire" || r.Center.Unread != 0 || len(r.Center.Notices) != 1 {
		t.Fatal("second device did not update", r)
	}
	p, err := s.PositionWithError("center-wire", "room:lobby")
	if err != nil || p.Draft != "control-center draft" {
		t.Fatal("control center changed draft", p, err)
	}
}
