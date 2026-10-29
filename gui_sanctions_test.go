package main

import (
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"
)

func sanctionAction(t *testing.T, s *Store, h *Hub, u User, r guiRequest, scope guiScope, body string) (guiActionResult, error) {
	t.Helper()
	r.Body = body
	return guiExecuteAction(s, h, u, scope, 42, r)
}
func TestGUISanctionsSixCommandsAndPreservedState(t *testing.T) {
	s, h, u, r, target := moderationFixture(t)
	s.db.Exec(`UPDATE users SET role='owner' WHERE name=?`, u.Name)
	s.SavePositionVersioned(u.Name, "room:lobby", Position{Draft: "untouched ordinary draft", Anchor: 42, ReplyTo: 42, Offset: 3})
	before, _ := s.PositionWithError(u.Name, "room:lobby")
	for _, cmd := range []string{"/mute", "/unmute", "/gmute", "/ungmute", "/ban", "/unban"} {
		r.ExpectedRoom = ""
		if cmd == "/mute" || cmd == "/unmute" {
			r.ExpectedRoom = "lobby"
		}
		body := cmd + " " + strings.ToUpper(target.Name)
		if !strings.HasPrefix(cmd, "/un") {
			body += " 7d synthetic reason"
		}
		out, e := sanctionAction(t, s, h, u, r, guiScope{room: "lobby"}, body)
		if e != nil {
			t.Fatal(cmd, e)
		}
		v := out.sanctionReceipt
		if v == nil || !v.Committed || v.Target != target.Name || v.RequestedTarget != strings.ToUpper(target.Name) || v.RequestedCommand != body || v.Actor != u.Name || v.OriginScope != "lobby" || !out.enforcePolicy || out.replyTo != 42 || out.message != nil || out.panel != nil || out.draft != nil {
			t.Fatal(cmd, out, v)
		}
		if v.Remove && v.Expires != 0 || !v.Remove && v.Expires <= time.Now().Unix() {
			t.Fatal(v)
		}
	}
	after, _ := s.PositionWithError(u.Name, "room:lobby")
	if !reflect.DeepEqual(before, after) {
		t.Fatal("draft/position changed")
	}
	var active, audits int
	s.db.QueryRow(`SELECT count(*) FROM sanctions WHERE revoked=0`).Scan(&active)
	s.db.QueryRow(`SELECT count(*) FROM audit WHERE action LIKE 'sanction.%'`).Scan(&audits)
	if active != 0 || audits != 6 {
		t.Fatal(active, audits)
	}
	for _, duration := range []string{"permanent", "forever", "0"} {
		out, e := sanctionAction(t, s, h, u, r, guiScope{room: "lobby"}, "/gmute target "+duration)
		if e != nil || out.sanctionReceipt.Expires != 0 {
			t.Fatal(duration, e)
		}
	}
	for _, body := range []string{"/mute", "/ban target bad-duration", "/ban missing", "/ban reader"} {
		if out, e := sanctionAction(t, s, h, u, r, guiScope{room: "lobby"}, body); e == nil || out.sanctionReceipt != nil || out.enforcePolicy {
			t.Fatal(body, out, e)
		}
	}
	r.ExpectedRoom = "wrong"
	if _, e := sanctionAction(t, s, h, u, r, guiScope{room: "lobby"}, "/mute target"); e == nil {
		t.Fatal("stale room")
	}
	r.ExpectedRoom = ""
	if _, e := sanctionAction(t, s, h, u, r, guiScope{peer: target.Name}, "/mute target"); e == nil {
		t.Fatal("DM channel mute")
	}
	if out, e := sanctionAction(t, s, h, u, r, guiScope{peer: target.Name}, "/gmute target"); e != nil || out.sanctionReceipt.Room != "" || out.sanctionReceipt.OriginScope != "dm:target" {
		t.Fatal(out, e)
	}
}
func TestGUISanctionsRolesChannelOwnerAndPrivateACL(t *testing.T) {
	s, h, u, r, target := moderationFixture(t)
	for _, role := range []string{"member", "moderator", "admin", "owner"} {
		s.db.Exec(`UPDATE users SET role=? WHERE name=?`, role, u.Name)
		_, e := sanctionAction(t, s, h, u, r, guiScope{room: "lobby"}, "/gmute target")
		if (e == nil) != (role == "admin" || role == "owner") {
			t.Fatal(role, e)
		}
	}
	for _, role := range []string{"admin", "owner"} {
		s.db.Exec(`UPDATE users SET role='admin' WHERE name=?`, u.Name)
		s.db.Exec(`UPDATE users SET role=? WHERE name=?`, role, target.Name)
		if _, e := sanctionAction(t, s, h, u, r, guiScope{room: "lobby"}, "/ban target"); e == nil {
			t.Fatal("rank", role)
		}
	}
	s.db.Exec(`UPDATE users SET role='member' WHERE name IN (?,?)`, u.Name, target.Name)
	if e := s.CreateRoomAccess("sanction-room", u.Name, true); e != nil {
		t.Fatal(e)
	}
	if _, e := sanctionAction(t, s, h, u, r, guiScope{room: "sanction-room"}, "/mute target"); e != nil {
		t.Fatal("room owner", e)
	}
	s.db.Exec(`UPDATE users SET role='admin' WHERE name=?`, target.Name)
	if _, e := sanctionAction(t, s, h, u, r, guiScope{room: "sanction-room"}, "/mute target"); e == nil {
		t.Fatal("room owner targeted global admin")
	}
	if e := s.CreateRoomAccess("private-target", target.Name, true); e != nil {
		t.Fatal(e)
	}
	s.db.Exec(`UPDATE users SET role='owner' WHERE name=?`, u.Name)
	if _, e := sanctionAction(t, s, h, u, r, guiScope{room: "private-target"}, "/mute target"); e == nil {
		t.Fatal("global bypass private scope")
	}
}
func TestGUISanctionsAuditFailureRollsBackAddAndRevoke(t *testing.T) {
	for _, fault := range []string{"ABORT,'synthetic audit'", "IGNORE"} {
		t.Run(fault, func(t *testing.T) {
			s, h, u, r, _ := moderationFixture(t)
			s.db.Exec(`UPDATE users SET role='owner' WHERE name=?`, u.Name)
			if _, e := sanctionAction(t, s, h, u, r, guiScope{room: "lobby"}, "/ban target"); e != nil {
				t.Fatal(e)
			}
			if _, e := s.db.Exec(`CREATE TRIGGER synthetic_sanction_audit BEFORE INSERT ON audit WHEN NEW.action LIKE 'sanction.%' BEGIN SELECT RAISE(` + fault + `);END`); e != nil {
				t.Fatal(e)
			}
			for _, body := range []string{"/unban target", "/mute target"} {
				out, e := sanctionAction(t, s, h, u, r, guiScope{room: "lobby"}, body)
				if e == nil || out.sanctionReceipt != nil || out.enforcePolicy {
					t.Fatal(body, out, e)
				}
			}
			var count, revoked int
			s.db.QueryRow(`SELECT count(*),sum(revoked) FROM sanctions`).Scan(&count, &revoked)
			if count != 1 || revoked != 0 {
				t.Fatal(count, revoked)
			}
		})
	}
}
func TestGUISanctionsQueuedSessionRoleACLAndPolicy(t *testing.T) {
	for _, change := range []string{"session", "role", "private", "reset", "ban", "epoch", "key", "target-role", "member"} {
		t.Run(change, func(t *testing.T) {
			s, h, u, r, _ := moderationFixture(t)
			s.db.Exec(`UPDATE users SET role='admin' WHERE name=?`, u.Name)

			scope := guiScope{room: "lobby"}
			var boundKey int64
			if change == "key" {
				key, e := s.BindSSHKey(u.Name, "synthetic queued sanction key", testSSHKeyText(testSSHKeySigner(t).PublicKey()))
				if e != nil {
					t.Fatal(e)
				}
				boundKey = key.ID
				if _, e = s.db.Exec(`INSERT INTO session_credentials(session_id,key_id) VALUES(?,?)`, r.sessionID, boundKey); e != nil {
					t.Fatal(e)
				}
			}
			if change == "member" {
				owner := registerUser(t, s, "room-owner")
				if e := s.CreateRoomAccess("queued-room", owner.Name, true); e != nil {
					t.Fatal(e)
				}
				if e := s.RoomMember(owner.Name, "queued-room", u.Name, "mod"); e != nil {
					t.Fatal(e)
				}
				s.db.Exec(`UPDATE users SET role='member' WHERE name=?`, u.Name)
				scope = guiScope{room: "queued-room"}
			}
			blocker, e := s.db.Begin()
			if e != nil {
				t.Fatal(e)
			}
			defer blocker.Rollback()
			waits := s.db.Stats().WaitCount
			done := make(chan error, 1)
			go func() { _, e := guiSanctionAction(s, u, scope, 42, r, "/mute", "target"); done <- e }()
			deadline := time.Now().Add(3 * time.Second)
			for s.db.Stats().WaitCount <= waits {
				if time.Now().After(deadline) {
					t.Fatal("not queued")
				}
				time.Sleep(time.Millisecond)
			}
			switch change {
			case "session":
				_, e = blocker.Exec(`UPDATE sessions SET revoked=1 WHERE id=?`, r.sessionID)
			case "role":
				_, e = blocker.Exec(`UPDATE users SET role='member' WHERE name=?`, u.Name)
			case "private":
				_, e = blocker.Exec(`UPDATE room_meta SET private=1 WHERE name='lobby'`)
			case "reset":
				_, e = blocker.Exec(`UPDATE user_meta SET must_reset=1 WHERE name=?`, u.Name)
			case "ban":
				_, e = blocker.Exec(`UPDATE users SET banned=1 WHERE name=?`, u.Name)
			case "epoch":
				_, e = blocker.Exec(`INSERT INTO credential_versions(user,epoch) VALUES(?,3) ON CONFLICT(user) DO UPDATE SET epoch=3`, u.Name)
			case "key":
				_, e = blocker.Exec(`UPDATE ssh_keys SET revoked=1 WHERE id=?`, boundKey)
			case "target-role":
				_, e = blocker.Exec(`UPDATE users SET role='owner' WHERE name='target'`)
			case "member":
				_, e = blocker.Exec(`DELETE FROM members WHERE room='queued-room' AND user=?`, u.Name)
			}
			if e != nil {
				t.Fatal(e)
			}
			if e = blocker.Commit(); e != nil {
				t.Fatal(e)
			}
			if e = <-done; e == nil {
				t.Fatal("queued intent committed", change)
			}
			var count int
			s.db.QueryRow(`SELECT count(*) FROM sanctions`).Scan(&count)
			if count != 0 {
				t.Fatal(count)
			}
			_ = h
		})
	}
}

type failedSanctionWriter struct{}

func (failedSanctionWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
func TestGUISanctionsCommittedEnforcementSurvivesResponseWriteFailure(t *testing.T) {
	s, h, u, r, target := moderationFixture(t)
	s.db.Exec(`UPDATE users SET role='owner' WHERE name=?`, u.Name)
	closed := make(chan struct{}, 1)
	client := &Client{Name: target.Name, ID: "sanction-target-live", IP: "192.0.2.8", Events: make(chan Event, 32), Close: func() { closed <- struct{}{} }, Ctx: t.Context()}
	if e := h.Join(client); e != nil {
		t.Fatal(e)
	}
	defer h.Leave(client)
	if _, ok := h.RefreshClient(client); !ok {
		t.Fatal("fixture target initially invalid")
	}
	out, e := sanctionAction(t, s, h, u, r, guiScope{room: "lobby"}, "/ban target")
	if e != nil {
		t.Fatal(e)
	}
	select {
	case <-closed:
		t.Fatal("enforcement must be explicit after commit response path")
	default:
	}
	writer := &guiWriter{e: json.NewEncoder(failedSanctionWriter{})}
	if e = guiCompleteAction(writer, h, guiResponse{Type: "action", SanctionReceipt: out.sanctionReceipt}, out.enforcePolicy); !errors.Is(e, io.ErrClosedPipe) {
		t.Fatal(e)
	}
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("committed ban not enforced after writer failure")
	}
	var active int
	s.db.QueryRow(`SELECT count(*) FROM sanctions WHERE kind='user' AND revoked=0`).Scan(&active)
	if active != 1 {
		t.Fatal(active)
	}
}

type blockedSanctionWriter struct{ entered, release chan struct{} }

func (w blockedSanctionWriter) Write([]byte) (int, error) {
	close(w.entered)
	<-w.release
	return 0, io.ErrClosedPipe
}
func TestGUISanctionsEnforcementDoesNotWaitForBlockedAcknowledgement(t *testing.T) {
	s, h, u, r, target := moderationFixture(t)
	s.db.Exec(`UPDATE users SET role='owner' WHERE name=?`, u.Name)
	closed := make(chan struct{}, 1)
	client := &Client{Name: target.Name, ID: "sanction-blocked-target", IP: "192.0.2.9", Events: make(chan Event, 32), Close: func() { closed <- struct{}{} }, Ctx: t.Context()}
	if e := h.Join(client); e != nil {
		t.Fatal(e)
	}
	defer h.Leave(client)
	out, e := sanctionAction(t, s, h, u, r, guiScope{room: "lobby"}, "/ban target")
	if e != nil {
		t.Fatal(e)
	}
	w := blockedSanctionWriter{make(chan struct{}), make(chan struct{})}
	defer close(w.release)
	done := make(chan error, 1)
	go func() {
		done <- guiCompleteAction(&guiWriter{e: json.NewEncoder(w)}, h, guiResponse{Type: "action", SanctionReceipt: out.sanctionReceipt}, out.enforcePolicy)
	}()
	select {
	case <-w.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("ACK writer not reached")
	}
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("committed policy waiting for blocked ACK")
	}
	select {
	case e := <-done:
		t.Fatal("writer unexpectedly completed", e)
	default:
	}
}
