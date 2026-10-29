package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestGUIManagementThreeCommitReceiptsAndUnchangedConversation(t *testing.T) {
	s, h, u, r, target := moderationFixture(t)
	s.db.Exec(`UPDATE users SET role='owner' WHERE name=?`, u.Name)
	s.SavePositionVersioned(u.Name, "room:lobby", Position{Draft: "synthetic keep", Anchor: 42, ReplyTo: 42, Offset: 3})
	before, _ := s.PositionWithError(u.Name, "room:lobby")
	for _, body := range []string{"/slowmode 0", "/slowmode +03600", "/role TARGET admin", "/role target moderator", "/role target member", "/kick TARGET"} {
		out, e := sanctionAction(t, s, h, u, r, guiScope{room: "lobby"}, body)
		if e != nil {
			t.Fatal(body, e)
		}
		v := out.managementReceipt
		if v == nil || !v.Committed || v.Command != strings.Fields(body)[0] || v.RequestedCommand != body || v.Actor != u.Name || v.OriginScope != "lobby" || out.replyTo != 42 || out.message != nil || out.panel != nil || out.draft != nil {
			t.Fatal(body, out, v)
		}
		if v.Command == "/slowmode" {
			if v.Target != "lobby" || v.Room != "lobby" || !out.refreshState {
				t.Fatal(v)
			}
		} else if v.Target != target.Name || v.Room != "" {
			t.Fatal(v)
		}
	}
	after, _ := s.PositionWithError(u.Name, "room:lobby")
	if !reflect.DeepEqual(before, after) {
		t.Fatal("draft/position changed")
	}
	for _, body := range []string{"/slowmode -1", "/slowmode 3601", "/slowmode 1 2", "/kick", "/kick target extra", "/role target owner", "/role reader member", "/role target admin extra"} {
		if out, e := sanctionAction(t, s, h, u, r, guiScope{room: "lobby"}, body); e == nil || out.managementReceipt != nil || out.kickTarget != "" || out.refreshState {
			t.Fatal(body, out, e)
		}
	}
	r.ExpectedRoom = "wrong"
	if _, e := sanctionAction(t, s, h, u, r, guiScope{room: "lobby"}, "/slowmode 1"); e == nil {
		t.Fatal("old room accepted")
	}
	r.ExpectedRoom = ""
	if _, e := sanctionAction(t, s, h, u, r, guiScope{peer: target.Name}, "/slowmode 1"); e == nil {
		t.Fatal("DM retarget")
	}
	if out, e := sanctionAction(t, s, h, u, r, guiScope{peer: target.Name}, "/role target member"); e != nil || out.managementReceipt.OriginScope != "dm:target" {
		t.Fatal(out, e)
	}
}
func TestGUIManagementPermissionsAndSessionGates(t *testing.T) {
	for _, cmd := range []string{"/slowmode 1", "/kick target", "/role target member"} {
		for _, role := range []string{"member", "moderator", "admin", "owner"} {
			t.Run(cmd+role, func(t *testing.T) {
				s, h, u, r, _ := moderationFixture(t)
				s.db.Exec(`UPDATE users SET role=? WHERE name=?`, role, u.Name)
				_, e := sanctionAction(t, s, h, u, r, guiScope{room: "lobby"}, cmd)
				allow := role == "owner" || cmd != "/role target member" && role == "admin" || cmd == "/slowmode 1" && role == "moderator"
				if (e == nil) != allow {
					t.Fatal(role, cmd, e)
				}
			})
		}
	}
	for _, change := range []string{"missing", "wrong-owner", "revoked", "closed", "reset", "banned", "epoch", "cancel"} {
		t.Run(change, func(t *testing.T) {
			s, h, u, r, _ := moderationFixture(t)
			s.db.Exec(`UPDATE users SET role='owner' WHERE name=?`, u.Name)
			switch change {
			case "missing":
				r.sessionID = ""
			case "wrong-owner":
				s.db.Exec(`UPDATE sessions SET user='target' WHERE id=?`, r.sessionID)
			case "revoked":
				s.db.Exec(`UPDATE sessions SET revoked=1 WHERE id=?`, r.sessionID)
			case "closed":
				s.db.Exec(`UPDATE sessions SET closed=1 WHERE id=?`, r.sessionID)
			case "reset":
				s.db.Exec(`UPDATE user_meta SET must_reset=1 WHERE name=?`, u.Name)
			case "banned":
				s.db.Exec(`UPDATE users SET banned=1 WHERE name=?`, u.Name)
			case "epoch":
				s.db.Exec(`INSERT INTO credential_versions(user,epoch) VALUES(?,3) ON CONFLICT(user) DO UPDATE SET epoch=3`, u.Name)
			case "cancel":
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				r.ctx = ctx
			}
			for _, body := range []string{"/slowmode 1", "/kick target", "/role target admin"} {
				if out, e := sanctionAction(t, s, h, u, r, guiScope{room: "lobby"}, body); e == nil || out.managementReceipt != nil {
					t.Fatal(body, out, e)
				}
			}
		})
	}
}
func TestGUIManagementAuditAndIgnoredUpdatesRollback(t *testing.T) {
	for _, body := range []string{"/slowmode 42", "/role target admin", "/kick target"} {
		for _, mode := range []string{"ABORT", "IGNORE"} {
			t.Run(body+mode, func(t *testing.T) {
				s, h, u, r, _ := moderationFixture(t)
				s.db.Exec(`UPDATE users SET role='owner' WHERE name=?`, u.Name)
				raise := "RAISE(IGNORE)"
				if mode == "ABORT" {
					raise = "RAISE(ABORT,'synthetic')"
				}
				if _, e := s.db.Exec(`CREATE TRIGGER fail_management_audit BEFORE INSERT ON audit BEGIN SELECT ` + raise + `; END`); e != nil {
					t.Fatal(e)
				}
				out, e := sanctionAction(t, s, h, u, r, guiScope{room: "lobby"}, body)
				if e == nil || out.managementReceipt != nil || out.refreshState || out.kickTarget != "" {
					t.Fatal(out, e)
				}
				var role string
				var slow, audits int
				s.db.QueryRow(`SELECT role FROM users WHERE name='target'`).Scan(&role)
				s.db.QueryRow(`SELECT slow FROM room_meta WHERE name='lobby'`).Scan(&slow)
				s.db.QueryRow(`SELECT count(*) FROM audit`).Scan(&audits)
				if role != "member" && role != "user" || slow != 0 || audits != 0 {
					t.Fatal(role, slow, audits)
				}
			})
		}
	}
	for _, table := range []string{"users", "room_meta"} {
		t.Run(table, func(t *testing.T) {
			s, h, u, r, _ := moderationFixture(t)
			s.db.Exec(`UPDATE users SET role='owner' WHERE name=?`, u.Name)
			s.db.Exec(`CREATE TRIGGER ignore_management_update BEFORE UPDATE ON ` + table + ` BEGIN SELECT RAISE(IGNORE); END`)
			cmd := "/role target admin"
			if table == "room_meta" {
				cmd = "/slowmode 42"
			}
			out, e := sanctionAction(t, s, h, u, r, guiScope{room: "lobby"}, cmd)
			if e == nil || out.managementReceipt != nil {
				t.Fatal(out, e)
			}
		})
	}
}
func TestGUIManagementQueuedFreshRoleScopeAndSession(t *testing.T) {
	for _, change := range []string{"role", "target-owner", "session", "private", "member", "key", "cancel"} {
		t.Run(change, func(t *testing.T) {
			s, _, u, r, _ := moderationFixture(t)
			s.db.Exec(`UPDATE users SET role='owner' WHERE name=?`, u.Name)
			scope := guiScope{room: "lobby"}
			cmd, arg := "/role", "target admin"
			var boundKey int64
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			r.ctx = ctx
			if change == "key" {
				key, e := s.BindSSHKey(u.Name, "synthetic management key", testSSHKeyText(testSSHKeySigner(t).PublicKey()))
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
				s.CreateRoomAccess("management-private", owner.Name, true)
				s.RoomMember(owner.Name, "management-private", u.Name, "mod")
				s.db.Exec(`UPDATE users SET role='member' WHERE name=?`, u.Name)
				scope = guiScope{room: "management-private"}
				cmd, arg = "/slowmode", "12"
			}
			tx, e := s.db.Begin()
			if e != nil {
				t.Fatal(e)
			}
			defer tx.Rollback()
			waits := s.db.Stats().WaitCount
			done := make(chan error, 1)
			go func() { _, e := guiManagementAction(s, u, scope, 42, r, cmd, arg); done <- e }()
			deadline := time.Now().Add(3 * time.Second)
			for s.db.Stats().WaitCount <= waits {
				if time.Now().After(deadline) {
					t.Fatal("not queued")
				}
				time.Sleep(time.Millisecond)
			}
			switch change {
			case "role":
				_, e = tx.Exec(`UPDATE users SET role='admin' WHERE name=?`, u.Name)
			case "target-owner":
				_, e = tx.Exec(`UPDATE users SET role='owner' WHERE name='target'`)
			case "session":
				_, e = tx.Exec(`UPDATE sessions SET revoked=1 WHERE id=?`, r.sessionID)
			case "private":
				_, e = tx.Exec(`UPDATE room_meta SET private=1 WHERE name='lobby'`)
			case "member":
				_, e = tx.Exec(`DELETE FROM members WHERE room='management-private' AND user=?`, u.Name)
			case "key":
				_, e = tx.Exec(`UPDATE ssh_keys SET revoked=1 WHERE id=?`, boundKey)
			case "cancel":
				cancel()

			}
			if e != nil {
				t.Fatal(e)
			}
			if e = tx.Commit(); e != nil {
				t.Fatal(e)
			}
			if e = <-done; e == nil {
				t.Fatal("queued stale intent committed", change)
			}
		})
	}
}
func TestGUIManagementCommittedEffectsPrecedeBlockedACKAndKickDoesNotRevoke(t *testing.T) {
	for _, cmd := range []string{"/kick target", "/role target moderator", "/slowmode 2"} {
		t.Run(cmd, func(t *testing.T) {
			s, h, u, r, target := moderationFixture(t)
			s.db.Exec(`UPDATE users SET role='owner' WHERE name=?`, u.Name)
			closed := make(chan struct{}, 4)
			client := &Client{Name: target.Name, ID: "management-target", IP: "192.0.2.12", Events: make(chan Event, 32), Close: func() { closed <- struct{}{} }, Ctx: t.Context()}
			if e := h.Join(client); e != nil {
				t.Fatal(e)
			}
			defer h.Leave(client)
			for len(client.Events) > 0 {
				<-client.Events
			}
			out, e := sanctionAction(t, s, h, u, r, guiScope{room: "lobby"}, cmd)
			if e != nil {
				t.Fatal(e)
			}
			select {
			case <-closed:
				t.Fatal("effect before completion path")
			default:
			}
			w := blockedSanctionWriter{make(chan struct{}), make(chan struct{})}
			defer func() {
				select {
				case <-w.release:
				default:
					close(w.release)
				}
			}()
			done := make(chan error, 1)
			go func() {
				done <- guiCompleteActionResult(&guiWriter{e: json.NewEncoder(w)}, h, guiResponse{Type: "action", ManagementReceipt: out.managementReceipt}, out)
			}()
			select {
			case <-w.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("writer absent")
			}
			if strings.HasPrefix(cmd, "/kick") {
				select {
				case <-closed:
				case <-time.After(3 * time.Second):
					t.Fatal("kick blocked by ACK")
				}
				h.Kick(target.Name)
				select {
				case <-closed:
					t.Fatal("duplicate close")
				default:
				}
				var revoked int
				s.db.QueryRow(`SELECT revoked FROM sessions WHERE id=?`, client.ID).Scan(&revoked)
				if revoked != 0 {
					t.Fatal("kick revoked session")
				}
				if _, ok := h.RefreshClient(client); !ok {
					t.Fatal("kick invalidated credentials")
				}
				newClient := &Client{Name: target.Name, ID: "management-reconnect", IP: "192.0.2.13", Events: make(chan Event, 32), Close: func() {}, Ctx: t.Context()}
				if e := h.Join(newClient); e != nil {
					t.Fatal("cannot reconnect", e)
				}
				h.Leave(newClient)
			} else {
				found := false
				deadline := time.After(3 * time.Second)
				for !found {
					select {
					case event := <-client.Events:
						found = event.Refresh
					case <-deadline:
						t.Fatal("refresh blocked by ACK")
					}
				}
			}
			close(w.release)
			if e = <-done; !errors.Is(e, io.ErrClosedPipe) {
				t.Fatal(e)
			}
		})
	}
}
func TestGUIManagementChannelRolesAndSlowPolicy(t *testing.T) {
	s, h, u, r, target := moderationFixture(t)
	if e := s.CreateRoomAccess("managed-channel", u.Name, true); e != nil {
		t.Fatal(e)
	}
	s.RoomMember(u.Name, "managed-channel", target.Name, "invite")
	scope := guiScope{room: "managed-channel"}
	r.ExpectedRoom = scope.room
	if _, e := sanctionAction(t, s, h, u, r, scope, "/slowmode 3600"); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Send(Message{Sender: target.Name, Room: scope.room, Kind: "chat", Body: "first"}); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Send(Message{Sender: target.Name, Room: scope.room, Kind: "chat", Body: "second"}); e == nil {
		t.Fatal("slow not enforced")
	}
	for i := 0; i < 2; i++ {
		if _, e := s.Send(Message{Sender: u.Name, Room: scope.room, Kind: "chat", Body: fmt.Sprint("manager", i)}); e != nil {
			t.Fatal("manager exemption", e)
		}
	}
	if _, e := sanctionAction(t, s, h, u, r, scope, "/slowmode 0"); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Send(Message{Sender: target.Name, Room: scope.room, Kind: "chat", Body: "after off"}); e != nil {
		t.Fatal(e)
	}
}

func TestGUIManagementRankUIDAndMenuEntry(t *testing.T) {
	s, h, u, r, target := moderationFixture(t)
	for _, actor := range []string{"admin", "owner"} {
		for _, other := range []string{"admin", "owner"} {
			s.db.Exec(`UPDATE users SET role=? WHERE name=?`, actor, u.Name)
			s.db.Exec(`UPDATE users SET role=? WHERE name=?`, other, target.Name)
			_, e := sanctionAction(t, s, h, u, r, guiScope{room: "lobby"}, "/kick target")
			if (e == nil) != (actor == "owner" && other == "admin") {
				t.Fatal(actor, other, e)
			}
		}
	}
	s.db.Exec(`UPDATE users SET role='owner' WHERE name=?`, u.Name)
	s.db.Exec(`UPDATE users SET role='member' WHERE name=?`, target.Name)
	if _, e := sanctionAction(t, s, h, u, r, guiScope{room: "lobby"}, "/kick "+u.Name); e == nil {
		t.Fatal("self kick")
	}
	for _, head := range []string{"/kick", "/role"} {
		body := fmt.Sprintf("%s #%d", head, target.UID)
		if head == "/role" {
			body += " moderator"
		}
		out, e := sanctionAction(t, s, h, u, r, guiScope{room: "lobby"}, body)
		if e != nil || out.managementReceipt.Target != target.Name {
			t.Fatal(body, out, e)
		}
	}
	out, e := sanctionAction(t, s, h, u, r, guiScope{room: "lobby"}, "/menu")
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, a := range out.panel.Actions {
		if strings.TrimSpace(a.Command) == "/slowmode" {
			found = a.Fill
		}
	}
	if !found {
		t.Fatal("slowmode native main menu entry absent")
	}
}

func TestGUIManagementKickAllCurrentTargetConnectionsOnly(t *testing.T) {
	s, h, u, r, target := moderationFixture(t)
	s.db.Exec(`UPDATE users SET role='owner' WHERE name=?`, u.Name)
	other := registerUser(t, s, "unrelated")
	closed := make(chan string, 4)
	for index, name := range []string{target.Name, target.Name, other.Name} {
		name := name
		client := &Client{Name: name, ID: fmt.Sprintf("multi-kick-%d", index), IP: "192.0.2.14", Events: make(chan Event, 32), Close: func() { closed <- name }, Ctx: t.Context()}
		if e := h.Join(client); e != nil {
			t.Fatal(e)
		}
		defer h.Leave(client)
	}
	out, e := sanctionAction(t, s, h, u, r, guiScope{room: "lobby"}, "/kick target")
	if e != nil {
		t.Fatal(e)
	}
	if e = guiCompleteActionResult(&guiWriter{e: json.NewEncoder(failedSanctionWriter{})}, h, guiResponse{Type: "action", ManagementReceipt: out.managementReceipt}, out); !errors.Is(e, io.ErrClosedPipe) {
		t.Fatal(e)
	}
	for i := 0; i < 2; i++ {
		select {
		case name := <-closed:
			if name != target.Name {
				t.Fatal("wrong target closed", name)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("not all current target clients closed")
		}
	}
	select {
	case name := <-closed:
		t.Fatal("unrelated close", name)
	case <-time.After(30 * time.Millisecond):
	}
}
