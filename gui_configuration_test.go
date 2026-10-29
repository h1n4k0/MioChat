package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestGUIConfigurationReceiptAndNoticeSemanticsPreserveConversation(t *testing.T) {
	s, h, u, r, target := moderationFixture(t)
	if _, err := s.db.Exec(`UPDATE users SET role='admin' WHERE name=?`, u.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SavePositionVersioned(u.Name, "room:lobby", Position{Draft: "keep draft", ReplyTo: 42, Anchor: 42, Offset: 3}); err != nil {
		t.Fatal(err)
	}
	before, err := s.PositionWithError(u.Name, "room:lobby")
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"/config registration invite", "/config registration_quota +001", "/config retention permanent", "/maintenance on \x1b[31msynthetic\x1b[0m notice", "/maintenance off"} {
		out, err := sanctionAction(t, s, h, u, r, guiScope{room: "lobby"}, body)
		if err != nil {
			t.Fatal(body, err)
		}
		v := out.configurationReceipt
		if v == nil || !v.Committed || v.RequestedCommand != body || v.Command != strings.Fields(body)[0] || v.Actor != u.Name || v.OriginScope != "lobby" || !out.refreshState || out.enforcePolicy || out.replyTo != 42 || out.message != nil || out.panel != nil || out.draft != nil {
			t.Fatal(body, out, v)
		}
		for _, change := range v.Changes {
			if got := storageScalar(t, s, `SELECT value FROM config WHERE key=?`, change.Key); got != change.Value {
				t.Fatal("receipt differs from committed value", got, change)
			}
		}
	}
	if got := s.Config("maintenance_notice"); got != "synthetic notice" {
		t.Fatal("omitted notice changed stored text", got)
	}
	out, err := sanctionAction(t, s, h, u, r, guiScope{peer: target.Name}, "/config maintenance_notice")
	if err != nil || out.configurationReceipt == nil || len(out.configurationReceipt.Changes) != 1 || out.configurationReceipt.OriginScope != "dm:target" || s.Config("maintenance_notice") != "" {
		t.Fatal("explicit clear failed", out, err)
	}
	after, err := s.PositionWithError(u.Name, "room:lobby")
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("configuration changed conversation", before, after, err)
	}
	for _, body := range []string{"/config unknown x", "/config registration bad", "/config registration_quota 0", "/config registration_quota 101", "/config retention 10", "/maintenance maybe"} {
		out, err := sanctionAction(t, s, h, u, r, guiScope{room: "lobby"}, body)
		if err == nil || out.configurationReceipt != nil || out.refreshState {
			t.Fatal("invalid configuration accepted", body, out, err)
		}
	}
}

func TestGUIConfigurationRequiresFreshGlobalAdmin(t *testing.T) {
	for _, body := range []string{"/config registration closed", "/maintenance on synthetic"} {
		for _, role := range []string{"member", "moderator", "admin", "owner"} {
			t.Run(body+role, func(t *testing.T) {
				s, h, u, r, _ := moderationFixture(t)
				if _, err := s.db.Exec(`UPDATE users SET role=? WHERE name=?`, role, u.Name); err != nil {
					t.Fatal(err)
				}
				out, err := sanctionAction(t, s, h, u, r, guiScope{room: "lobby"}, body)
				allow := role == "admin" || role == "owner"
				if (err == nil) != allow || !allow && (out.configurationReceipt != nil || out.refreshState) {
					t.Fatal(role, out, err)
				}
			})
		}
	}
}

func TestGUIConfigurationSessionAndQueuedFreshPolicy(t *testing.T) {
	for _, change := range []string{"missing", "wrong-owner", "revoked", "closed", "reset", "banned", "epoch", "role", "private", "key", "cancel"} {
		t.Run(change, func(t *testing.T) {
			s, _, u, r, _ := moderationFixture(t)
			if _, e := s.db.Exec(`UPDATE users SET role='admin' WHERE name=?`, u.Name); e != nil {
				t.Fatal(e)
			}
			if change == "missing" {
				r.sessionID = ""
				if out, e := guiConfigurationAction(s, u, guiScope{room: "lobby"}, 42, r, "/maintenance", "on"); e == nil || out.configurationReceipt != nil {
					t.Fatal(out, e)
				}
				return
			}
			var keyID int64
			if change == "key" {
				key, e := s.BindSSHKey(u.Name, "synthetic configuration key", testSSHKeyText(testSSHKeySigner(t).PublicKey()))
				if e != nil {
					t.Fatal(e)
				}
				keyID = key.ID
				if _, e = s.db.Exec(`INSERT INTO session_credentials(session_id,key_id) VALUES(?,?)`, r.sessionID, keyID); e != nil {
					t.Fatal(e)
				}
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			r.ctx = ctx
			before := storageScalar(t, s, `SELECT count(*) FROM audit`)
			tx, e := s.db.Begin()
			if e != nil {
				t.Fatal(e)
			}
			defer tx.Rollback()
			waits := s.db.Stats().WaitCount
			done := make(chan error, 1)
			go func() {
				out, e := guiConfigurationAction(s, u, guiScope{room: "lobby"}, 42, r, "/maintenance", "on")
				if e == nil || out.configurationReceipt != nil || out.refreshState {
					done <- errors.New("queued configuration accepted stale authority")
					return
				}
				done <- nil
			}()
			deadline := time.Now().Add(3 * time.Second)
			for s.db.Stats().WaitCount <= waits {
				if time.Now().After(deadline) {
					t.Fatal("configuration not queued")
				}
				time.Sleep(time.Millisecond)
			}
			switch change {
			case "wrong-owner":
				_, e = tx.Exec(`UPDATE sessions SET user='target' WHERE id=?`, r.sessionID)
			case "revoked":
				_, e = tx.Exec(`UPDATE sessions SET revoked=1 WHERE id=?`, r.sessionID)
			case "closed":
				_, e = tx.Exec(`UPDATE sessions SET closed=1 WHERE id=?`, r.sessionID)
			case "reset":
				_, e = tx.Exec(`UPDATE user_meta SET must_reset=1 WHERE name=?`, u.Name)
			case "banned":
				_, e = tx.Exec(`UPDATE users SET banned=1 WHERE name=?`, u.Name)
			case "epoch":
				_, e = tx.Exec(`INSERT INTO credential_versions(user,epoch) VALUES(?,3) ON CONFLICT(user) DO UPDATE SET epoch=3`, u.Name)
			case "role":
				_, e = tx.Exec(`UPDATE users SET role='member' WHERE name=?`, u.Name)
			case "private":
				_, e = tx.Exec(`UPDATE room_meta SET private=1 WHERE name='lobby'`)
			case "key":
				_, e = tx.Exec(`UPDATE ssh_keys SET revoked=1 WHERE id=?`, keyID)
			case "cancel":
				cancel()
			}
			if e != nil {
				t.Fatal(e)
			}
			if e = tx.Commit(); e != nil {
				t.Fatal(e)
			}
			if e = <-done; e != nil {
				t.Fatal(e)
			}
			if s.Config("maintenance") != "off" || storageScalar(t, s, `SELECT count(*) FROM audit`) != before {
				t.Fatal("rejected configuration mutated settings/audit")
			}
		})
	}
}

func TestGUIConfigurationIgnoredWritesAuditAndReadbackRollback(t *testing.T) {
	for _, fault := range []string{"config-insert-ignore", "config-update-ignore", "audit-ignore", "audit-abort", "config-not-null-constraint", "later-change"} {
		t.Run(fault, func(t *testing.T) {
			s, h, u, r, _ := moderationFixture(t)
			if _, e := s.db.Exec(`UPDATE users SET role='admin' WHERE name=?`, u.Name); e != nil {
				t.Fatal(e)
			}
			before := storageScalar(t, s, `SELECT count(*) FROM audit`)
			trigger := ""
			switch fault {
			case "config-insert-ignore":
				trigger = `CREATE TRIGGER configuration_fault BEFORE INSERT ON config BEGIN SELECT RAISE(IGNORE); END`
			case "config-update-ignore":
				trigger = `CREATE TRIGGER configuration_fault BEFORE UPDATE ON config WHEN NEW.key='maintenance_notice' BEGIN SELECT RAISE(IGNORE); END`
			case "audit-ignore":
				trigger = `CREATE TRIGGER configuration_fault BEFORE INSERT ON audit WHEN NEW.target='maintenance_notice' BEGIN SELECT RAISE(IGNORE); END`
			case "audit-abort":
				trigger = `CREATE TRIGGER configuration_fault BEFORE INSERT ON audit WHEN NEW.target='maintenance_notice' BEGIN SELECT RAISE(ABORT,'synthetic configuration audit'); END`
			case "config-not-null-constraint":
				trigger = `CREATE TRIGGER configuration_fault AFTER UPDATE ON config WHEN NEW.key='maintenance_notice' BEGIN UPDATE config SET value=NULL WHERE key='maintenance'; END`
			case "later-change":
				trigger = `CREATE TRIGGER configuration_fault AFTER UPDATE ON config WHEN NEW.key='maintenance_notice' BEGIN UPDATE config SET value='off' WHERE key='maintenance'; END`
			}
			if _, e := s.db.Exec(trigger); e != nil {
				t.Fatal(e)
			}
			out, e := sanctionAction(t, s, h, u, r, guiScope{room: "lobby"}, "/maintenance on synthetic notice")
			if e == nil || out.configurationReceipt != nil || out.refreshState {
				t.Fatal(out, e)
			}
			if s.Config("maintenance") != "off" || s.Config("maintenance_notice") != "" || storageScalar(t, s, `SELECT count(*) FROM audit`) != before {
				t.Fatal("failed configuration left partial write")
			}
		})
	}
}

func TestGUIConfigurationChangesActualRegistrationAndSendPolicy(t *testing.T) {
	s, h, u, r, target := moderationFixture(t)
	if _, e := s.db.Exec(`UPDATE users SET role='admin' WHERE name=?`, u.Name); e != nil {
		t.Fatal(e)
	}
	set := func(body string) {
		t.Helper()
		if _, e := sanctionAction(t, s, h, u, r, guiScope{room: "lobby"}, body); e != nil {
			t.Fatal(body, e)
		}
	}
	register := func(name, invite, ip string) error {
		_, e := s.RegisterInvite(name, "synthetic-register-only", "synthetic-register-only", invite, ip, "synthetic")
		return e
	}
	set("/config registration closed")
	if e := register("closed-user", "", "192.0.2.171"); e == nil {
		t.Fatal("closed registration admitted")
	}
	set("/config registration invite")
	if e := register("no-invite", "", "192.0.2.172"); e == nil {
		t.Fatal("invite-less registration admitted")
	}
	code, e := s.InviteCode(u.Name, "1h", 1)
	if e != nil {
		t.Fatal(e)
	}
	if e = register("invited-user", code, "192.0.2.173"); e != nil {
		t.Fatal(e)
	}
	set("/config registration open")
	set("/config registration_quota 1")
	if e = register("open-first", "", "192.0.2.174"); e != nil {
		t.Fatal(e)
	}
	if e = register("open-second", "", "192.0.2.174"); e == nil {
		t.Fatal("quota ignored")
	}
	set("/maintenance on synthetic maintenance")
	if _, e = s.Send(Message{Sender: target.Name, Room: "lobby", Body: "synthetic denied", Kind: "chat"}); e == nil {
		t.Fatal("maintenance ignored")
	}
	if _, e = s.SendForSessionContext(t.Context(), Message{Sender: u.Name, Room: "lobby", Body: "synthetic admin exemption", Kind: "chat"}, r.sessionID); e != nil {
		t.Fatal(e)
	}
	set("/maintenance off")
	if _, e = s.Send(Message{Sender: target.Name, Room: "lobby", Body: "synthetic allowed", Kind: "chat"}); e != nil {
		t.Fatal(e)
	}
}

func TestGUIConfigurationRefreshPrecedesBlockedACK(t *testing.T) {
	s, h, u, r, target := moderationFixture(t)
	if _, e := s.db.Exec(`UPDATE users SET role='admin' WHERE name=?`, u.Name); e != nil {
		t.Fatal(e)
	}
	client := &Client{Name: target.Name, ID: "configuration-refresh", IP: "192.0.2.175", Events: make(chan Event, 32), Close: func() {}, Ctx: t.Context()}
	if e := h.Join(client); e != nil {
		t.Fatal(e)
	}
	defer h.Leave(client)
	for len(client.Events) > 0 {
		<-client.Events
	}
	out, e := sanctionAction(t, s, h, u, r, guiScope{room: "lobby"}, "/maintenance on synthetic")
	if e != nil {
		t.Fatal(e)
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
		done <- guiCompleteActionResult(&guiWriter{e: json.NewEncoder(w)}, h, guiResponse{Type: "action", ConfigurationReceipt: out.configurationReceipt}, out)
	}()
	select {
	case <-w.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("ACK writer absent")
	}
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
	close(w.release)
	if e = <-done; !errors.Is(e, io.ErrClosedPipe) {
		t.Fatal(e)
	}
	if s.Config("maintenance") != "on" {
		t.Fatal("committed configuration lost after failed ACK")
	}
}

func TestGUIConfigurationWirePolicyAndMenu(t *testing.T) {
	s, h := featureFixture(t)
	admin := newGUIWireFixture(t, s, h)
	admin.send(guiRequest{Type: "login", RequestID: "config-login", Username: "admin", Password: "test-password-123"})
	admin.read("auth", "config-login")
	admin.read("state", "")
	member := newGUIWireFixture(t, s, h)
	member.send(guiRequest{Type: "login", RequestID: "member-login", Username: "alice", Password: "test-password-123"})
	member.read("auth", "member-login")
	member.read("state", "")
	admin.send(guiRequest{Type: "command", RequestID: "config-menu", Room: "lobby", Body: "/menu"})
	menu := admin.read("action", "config-menu").Panel
	if menu == nil {
		t.Fatal("menu absent")
	}
	for _, cmd := range []string{"/config", "/maintenance"} {
		found := false
		for _, a := range menu.Actions {
			if strings.TrimSpace(a.Command) == cmd {
				found = a.Fill
			}
		}
		if !found {
			t.Fatal("native configuration entry absent", cmd)
		}
	}
	set := func(id, body string) {
		t.Helper()
		time.Sleep(750 * time.Millisecond)
		admin.send(guiRequest{Type: "command", RequestID: id, Room: "lobby", Body: body})
		r := admin.read("action", id)
		if r.ConfigurationReceipt == nil || !r.ConfigurationReceipt.Committed || r.ConfigurationReceipt.RequestedCommand != body || r.Room != "lobby" || r.Replace || r.Draft != nil {
			t.Fatal(r)
		}
	}
	set("registration-closed", "/config registration closed")
	guest := newGUIWireFixture(t, s, h)
	guest.send(guiRequest{Type: "register", RequestID: "closed-registration", Username: "config-wire-new", Password: "synthetic-register-only", Confirm: "synthetic-register-only"})
	if r := guest.read("error", "closed-registration"); !strings.Contains(r.Error, "暂停注册") {
		t.Fatal(r)
	}
	set("maintenance-on", "/maintenance on synthetic wire notice")
	member.send(guiRequest{Type: "send_message", RequestID: "maintenance-deny", Room: "lobby", Body: "synthetic denied"})
	if r := member.read("error", "maintenance-deny"); !strings.Contains(r.Error, "维护") {
		t.Fatal(r)
	}
	time.Sleep(750 * time.Millisecond)
	member.send(guiRequest{Type: "command", RequestID: "member-config", Room: "lobby", Body: "/maintenance off"})
	if r := member.read("error", "member-config"); r.ConfigurationReceipt != nil || !strings.Contains(r.Error, "需要管理员权限") {
		t.Fatal(r)
	}
	set("maintenance-off", "/maintenance off")
	time.Sleep(750 * time.Millisecond)
	member.send(guiRequest{Type: "send_message", RequestID: "maintenance-allow", Room: "lobby", Body: "synthetic allowed"})
	r := member.read("sent", "maintenance-allow")
	if r.Message == nil || r.Message.ID <= 0 || r.Message.Room != "lobby" || r.Message.Sender != "alice" || r.Message.Kind != "chat" {
		t.Fatalf("invalid committed send receipt: %+v", r.Message)
	}
	// A post-commit metadata read may fail closed to the documented placeholder.
	// Maintenance authorization is proven by the exact durable row, not by the
	// availability of optional display decoration after that transaction.
	if r.Message.Body != "synthetic allowed" && r.Message.Body != "[暂时无法加载消息状态，请刷新历史]" {
		t.Fatalf("unexpected sent body: %q", r.Message.Body)
	}
	var room, sender, recipient, body, kind string
	if err := s.db.QueryRow(`SELECT room,sender,recipient,body,kind FROM messages WHERE id=?`, r.Message.ID).Scan(&room, &sender, &recipient, &body, &kind); err != nil || room != "lobby" || sender != "alice" || recipient != "" || body != "synthetic allowed" || kind != "chat" {
		t.Fatal("maintenance-off did not commit the exact authorized message", err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM messages WHERE sender='alice' AND body IN ('synthetic allowed','synthetic denied')`).Scan(&count); err != nil || count != 1 {
		t.Fatal("maintenance policy allowed a denied/duplicate message", count, err)
	}
	set("registration-open", "/config registration open")
	if s.Config("maintenance_notice") != "synthetic wire notice" {
		t.Fatal("omitted notice not preserved")
	}
}
