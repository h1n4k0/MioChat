package main

import (
	"bytes"
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

func secretRequest(id, kind, target string) guiRequest {
	r := guiRequest{Type: "secret_admin", RequestID: id, Room: "lobby", SecretAdmin: &guiSecretAdminRequest{Kind: kind, Target: target}}
	if kind == "invitecode" {
		r.SecretAdmin.Duration, r.SecretAdmin.Uses = "1d", 2
	}
	return r
}
func TestGUISecretAdminCommitsOnceAndKeepsConversationPrivate(t *testing.T) {
	s, h := featureFixture(t)
	m := testModel(t, s, h, "admin")
	key, e := s.BindSSHKey("bob", "synthetic reset key", testSSHKeyText(testSSHKeySigner(t).PublicKey()))
	if e != nil {
		t.Fatal(e)
	}
	old := credentialSession(t, s, "bob", "synthetic-old-password")
	if _, e = s.SavePositionVersioned("admin", "room:lobby", Position{Draft: "retain ordinary draft", ReplyTo: 42}); e != nil {
		t.Fatal(e)
	}
	before, e := s.PositionWithError("admin", "room:lobby")
	if e != nil {
		t.Fatal(e)
	}
	r := secretRequest("reset", "resetpass", "BOB")
	out, e := guiSecretAdminRequestAction(t.Context(), s, h, m.user, guiScope{room: "lobby"}, m.client.ID, r)
	if e != nil || out.SecretAdmin == nil {
		t.Fatal("reset did not complete", e)
	}
	v := out.SecretAdmin
	if v.State != "shown" || v.Committed == nil || !*v.Committed || v.RequestedTarget != "BOB" || v.Target != "bob" || len(v.Value) != 32 || out.Panel != nil || out.Draft != nil || out.Message != nil {
		t.Fatal("invalid reset projection", out)
	}
	u, e := s.Authenticate("bob", v.Value)
	if e != nil || !u.MustReset || u.AuthEpoch != 1 {
		t.Fatal("temporary password failed", e)
	}
	if _, e = s.Authenticate("bob", "test-password-123"); e == nil {
		t.Fatal("old password still accepted")
	}
	if s.SessionValid(old.ID) || storageScalar(t, s, `SELECT revoked FROM ssh_keys WHERE id=?`, key.ID) != "1" {
		t.Fatal("old credentials not revoked")
	}
	if strings.Contains(fmt.Sprintf("%+v %#v", v, v), v.Value) {
		t.Fatal("secret appeared in debug rendering")
	}
	r = secretRequest("invite", "invitecode", "")
	out, e = guiSecretAdminRequestAction(t.Context(), s, h, m.user, guiScope{room: "lobby"}, m.client.ID, r)
	if e != nil {
		t.Fatal(e)
	}
	v = out.SecretAdmin
	if v.State != "shown" || v.Uses != 2 || v.Expires <= time.Now().Unix() || storageScalar(t, s, `SELECT remaining FROM invites WHERE hash=?`, tokenHash(v.Value)) != "2" {
		t.Fatal("invite does not match durable state", v)
	}
	if _, e = s.db.Exec(`UPDATE config SET value='invite' WHERE key='registration'`); e != nil {
		t.Fatal(e)
	}
	if _, e = s.RegisterInvite("new-synthetic", "synthetic-password-123", "synthetic-password-123", v.Value, "", "synthetic"); e != nil {
		t.Fatal("generated invite not usable", e)
	}
	if strings.Contains(storageScalar(t, s, `SELECT group_concat(detail) FROM audit`), v.Value) {
		t.Fatal("secret logged")
	}
	after, e := s.PositionWithError("admin", "room:lobby")
	if e != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("secret flow changed ordinary draft", e)
	}
}

func TestGUISecretAdminQueuedAuthorityAndEmptySessionFailClosed(t *testing.T) {
	for _, kind := range []string{"invitecode", "resetpass"} {
		for _, mode := range []string{"empty", "revoked", "epoch", "role", "reset", "key", "target-role", "cancel"} {
			t.Run(kind+"/"+mode, func(t *testing.T) {
				s, h := featureFixture(t)
				m := testModel(t, s, h, "admin")
				r := secretRequest("queued", kind, "")
				if kind == "resetpass" {
					r.SecretAdmin.Target = "bob"
				}
				if mode == "empty" {
					if out, e := guiSecretAdminRequestAction(t.Context(), s, h, m.user, guiScope{room: "lobby"}, "", r); e == nil || out.SecretAdmin != nil {
						t.Fatal("empty session accepted")
					}
					return
				}
				if mode == "target-role" && kind != "resetpass" {
					return
				}
				var keyID int64
				if mode == "key" {
					key, e := s.BindSSHKey("admin", "synthetic actor", testSSHKeyText(testSSHKeySigner(t).PublicKey()))
					if e != nil {
						t.Fatal(e)
					}
					keyID = key.ID
					if _, e = s.db.Exec(`INSERT INTO session_credentials(session_id,key_id) VALUES(?,?)`, m.client.ID, keyID); e != nil {
						t.Fatal(e)
					}
				}
				before := storageScalar(t, s, `SELECT count(*) FROM audit`)
				hash, salt := credentialBytes(t, s, "bob")
				ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
				defer cancel()
				tx, e := s.db.BeginTx(ctx, nil)
				if e != nil {
					t.Fatal(e)
				}
				defer tx.Rollback()
				waits := s.db.Stats().WaitCount
				done := make(chan error, 1)
				go func() {
					defer close(done)
					out, e := guiSecretAdminRequestAction(ctx, s, h, m.user, guiScope{room: "lobby"}, m.client.ID, r)
					if e == nil || out.SecretAdmin != nil {
						done <- errors.New("stale authority committed secret action")
					} else {
						done <- nil
					}
				}()
				defer func() {
					cancel()
					_ = tx.Rollback()
					select {
					case <-done:
					case <-time.After(time.Second):
						t.Error("owned request did not stop")
					}
				}()
				deadline := time.Now().Add(3 * time.Second)
				for s.db.Stats().WaitCount <= waits {
					if time.Now().After(deadline) {
						t.Fatal("request not actually queued")
					}
					time.Sleep(time.Millisecond)
				}
				switch mode {
				case "revoked":
					e = changedOne(tx.Exec(`UPDATE sessions SET revoked=1 WHERE id=?`, m.client.ID))
				case "epoch":
					e = changedOne(tx.Exec(`INSERT INTO credential_versions(user,epoch) VALUES('admin',1) ON CONFLICT(user) DO UPDATE SET epoch=epoch+1`))
				case "role":
					e = changedOne(tx.Exec(`UPDATE users SET role='member' WHERE name='admin'`))
				case "reset":
					e = changedOne(tx.Exec(`UPDATE user_meta SET must_reset=1 WHERE name='admin'`))
				case "key":
					e = changedOne(tx.Exec(`UPDATE ssh_keys SET revoked=1 WHERE id=?`, keyID))
				case "target-role":
					e = changedOne(tx.Exec(`UPDATE users SET role='owner' WHERE name='bob'`))
				case "cancel":
					cancel()
				}
				if e != nil {
					t.Fatal(e)
				}
				if mode == "cancel" {
					_ = tx.Rollback()
				} else if e = tx.Commit(); e != nil {
					t.Fatal(e)
				}
				select {
				case e = <-done:
					if e != nil {
						t.Fatal(e)
					}
				case <-time.After(4 * time.Second):
					t.Fatal("queued request did not finish")
				}
				afterHash, afterSalt := credentialBytes(t, s, "bob")
				if before != storageScalar(t, s, `SELECT count(*) FROM audit`) || storageScalar(t, s, `SELECT count(*) FROM invites`) != "0" || !bytes.Equal(hash, afterHash) || !bytes.Equal(salt, afterSalt) {
					t.Fatal("rejected request changed durable state")
				}
			})
		}
	}
}

func TestGUISecretAdminResetMandatoryWritesRollback(t *testing.T) {
	for _, fault := range []string{"key-ignore", "key-partial", "session-ignore", "session-partial", "audit-ignore", "epoch-ignore", "later-reset-clear"} {
		t.Run(fault, func(t *testing.T) {
			s, h := featureFixture(t)
			m := testModel(t, s, h, "admin")
			for i := 0; i < 2; i++ {
				if _, e := s.BindSSHKey("bob", fmt.Sprintf("synthetic-key-%d", i), testSSHKeyText(testSSHKeySigner(t).PublicKey())); e != nil {
					t.Fatal(e)
				}
				credentialSession(t, s, "bob", fmt.Sprintf("reset-session-%d", i))
			}
			hash, salt := credentialBytes(t, s, "bob")
			audits := storageScalar(t, s, `SELECT count(*) FROM audit`)
			trigger := ""
			switch fault {
			case "key-ignore":
				trigger = `CREATE TRIGGER secret_fault BEFORE UPDATE ON ssh_keys BEGIN SELECT RAISE(IGNORE); END`
			case "key-partial":
				trigger = `CREATE TRIGGER secret_fault BEFORE UPDATE ON ssh_keys WHEN OLD.id=(SELECT min(id) FROM ssh_keys WHERE user='bob') BEGIN SELECT RAISE(IGNORE); END`
			case "session-ignore":
				trigger = `CREATE TRIGGER secret_fault BEFORE UPDATE ON sessions WHEN OLD.user='bob' BEGIN SELECT RAISE(IGNORE); END`
			case "session-partial":
				trigger = `CREATE TRIGGER secret_fault BEFORE UPDATE ON sessions WHEN OLD.id='reset-session-0' BEGIN SELECT RAISE(IGNORE); END`
			case "audit-ignore":
				trigger = `CREATE TRIGGER secret_fault BEFORE INSERT ON audit BEGIN SELECT RAISE(IGNORE); END`
			case "epoch-ignore":
				trigger = `CREATE TRIGGER secret_fault BEFORE INSERT ON credential_versions BEGIN SELECT RAISE(IGNORE); END`
			case "later-reset-clear":
				trigger = `CREATE TRIGGER secret_fault AFTER INSERT ON audit WHEN NEW.action='password.reset' BEGIN UPDATE user_meta SET must_reset=0 WHERE name='bob'; END`
			}
			if _, e := s.db.Exec(trigger); e != nil {
				t.Fatal(e)
			}
			out, e := guiSecretAdminRequestAction(t.Context(), s, h, m.user, guiScope{room: "lobby"}, m.client.ID, secretRequest("fault", "resetpass", "bob"))
			if e == nil || out.SecretAdmin != nil {
				t.Fatal("mandatory write failed but reset reported success")
			}
			afterHash, afterSalt := credentialBytes(t, s, "bob")
			if !bytes.Equal(hash, afterHash) || !bytes.Equal(salt, afterSalt) || audits != storageScalar(t, s, `SELECT count(*) FROM audit`) || storageScalar(t, s, `SELECT count(*) FROM ssh_keys WHERE user='bob' AND revoked=0`) != "2" || storageScalar(t, s, `SELECT count(*) FROM sessions WHERE user='bob' AND revoked=0`) != "2" {
				t.Fatal("failed reset left partial changes")
			}
		})
	}
}

func TestGUISecretAdminInviteWriteFaultsAndDeliveryLoss(t *testing.T) {
	for _, fault := range []string{"insert-ignore", "audit-ignore", "later-change", "delivery-role"} {
		t.Run(fault, func(t *testing.T) {
			s, h := featureFixture(t)
			m := testModel(t, s, h, "admin")
			before := storageScalar(t, s, `SELECT count(*) FROM audit`)
			trigger := map[string]string{
				"insert-ignore": `CREATE TRIGGER secret_fault BEFORE INSERT ON invites BEGIN SELECT RAISE(IGNORE); END`,
				"audit-ignore":  `CREATE TRIGGER secret_fault BEFORE INSERT ON audit BEGIN SELECT RAISE(IGNORE); END`,
				"later-change":  `CREATE TRIGGER secret_fault AFTER INSERT ON audit WHEN NEW.action='invite.create' BEGIN UPDATE invites SET remaining=99; END`,
				"delivery-role": `CREATE TRIGGER secret_fault AFTER INSERT ON audit WHEN NEW.action='invite.create' BEGIN UPDATE users SET role='member' WHERE name='admin'; END`,
			}[fault]
			if _, e := s.db.Exec(trigger); e != nil {
				t.Fatal(e)
			}
			out, e := guiSecretAdminRequestAction(t.Context(), s, h, m.user, guiScope{room: "lobby"}, m.client.ID, secretRequest("fault", "invitecode", ""))
			if fault == "delivery-role" {
				if e != nil || out.SecretAdmin == nil || out.SecretAdmin.State != "committed_hidden" || out.SecretAdmin.Value != "" || storageScalar(t, s, `SELECT count(*) FROM invites`) != "1" {
					t.Fatal("committed result not safely suppressed", e)
				}
				return
			}
			if e == nil || out.SecretAdmin != nil || storageScalar(t, s, `SELECT count(*) FROM invites`) != "0" || before != storageScalar(t, s, `SELECT count(*) FROM audit`) {
				t.Fatal("failed invite not rolled back", e)
			}
		})
	}
}

func TestGUISecretAdminEnforcesBeforeBlockedWriterAndRejectsMixedInput(t *testing.T) {
	s, h := featureFixture(t)
	m := testModel(t, s, h, "admin")
	closed := make(chan struct{}, 1)
	client := &Client{Name: "bob", ID: "secret-target-live", IP: "192.0.2.22", Events: make(chan Event, 32), Close: func() { closed <- struct{}{} }, Ctx: t.Context()}
	if e := h.Join(client); e != nil {
		t.Fatal(e)
	}
	defer h.Leave(client)
	r := secretRequest("reset", "resetpass", "bob")
	r.Body = "must not share chat body"
	if out, e := guiSecretAdminRequestAction(t.Context(), s, h, m.user, guiScope{room: "lobby"}, m.client.ID, r); e == nil || out.SecretAdmin != nil {
		t.Fatal("mixed request accepted")
	}
	r.Body = ""
	out, e := guiSecretAdminRequestAction(t.Context(), s, h, m.user, guiScope{room: "lobby"}, m.client.ID, r)
	if e != nil {
		t.Fatal(e)
	}
	w := blockedSanctionWriter{make(chan struct{}), make(chan struct{})}
	done := make(chan error, 1)
	go func() { defer close(done); done <- (&guiWriter{e: json.NewEncoder(w)}).send(out) }()
	defer func() {
		close(w.release)
		select {
		case e := <-done:
			if !errors.Is(e, io.ErrClosedPipe) {
				t.Error("unexpected writer result", e)
			}
		case <-time.After(time.Second):
			t.Error("owned writer did not stop")
		}
	}()
	select {
	case <-w.entered:
	case <-time.After(time.Second):
		t.Fatal("writer not reached")
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("reset enforcement waited for ACK")
	}
	if s.SessionValid(client.ID) {
		t.Fatal("target session still valid")
	}
}

func TestTUISecretPanelCloseExpiryCredentialAndSnapshotsClear(t *testing.T) {
	for _, mode := range []string{"close", "expiry", "credentials", "epoch", "navigation"} {
		t.Run(mode, func(t *testing.T) {
			s, h := featureFixture(t)
			m := testModel(t, s, h, "admin")
			m.command("/invitecode 1d 1")
			if m.secretPanel == nil || len(m.panelLines) == 0 {
				t.Fatal("no real secret panel")
			}
			old := m
			var e error
			switch mode {
			case "close":
				m.closePanel()
			case "expiry":
				next, _ := m.Update(clockMsg(m.secretPanel.expires))
				m = next.(model)
			case "credentials":
				m.command("/passwd")
			case "epoch":
				e = changedOne(s.db.Exec(`INSERT INTO credential_versions(user,epoch) VALUES('admin',1)`))
				m.frame = 2
				next, _ := m.Update(clockMsg(time.Now()))
				m = next.(model)
			case "navigation":
				e = m.move("", "bob")
			}
			if e != nil {
				t.Fatal(e)
			}
			if m.secretPanel != nil || len(m.panelLines) != 0 {
				t.Fatal("actual panel retained secret")
			}
			if old.secretPanel.active {
				t.Fatal("old model copy can reopen secret")
			}
			for _, line := range old.panelLines {
				if line != "" {
					t.Fatal("shared panel snapshot retained cleartext")
				}
			}
		})
	}
}

func TestSecretAdminSupersededResetSuppressesStaleOneTimePassword(t *testing.T) {
	s, h := featureFixture(t)
	m := testModel(t, s, h, "admin")
	first, err := s.ResetPasswordForSession(t.Context(), "admin", "bob", m.client.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.ResetPasswordForSession(t.Context(), "admin", "bob", m.client.ID)
	if err != nil {
		t.Fatal(err)
	}
	if s.secretAdminDeliveryAllowed(t.Context(), first, m.client.ID) || !s.secretAdminDeliveryAllowed(t.Context(), second, m.client.ID) {
		t.Fatal("delivery did not bind committed target credential generation")
	}
	if _, err = s.Authenticate("bob", first.secret); err == nil {
		t.Fatal("superseded password unexpectedly accepted")
	}
	if _, err = s.Authenticate("bob", second.secret); err != nil {
		t.Fatal("latest password failed", err)
	}
	m.showSecretAdmin(first)
	if m.secretPanel != nil || m.panelKind == "secret" {
		t.Fatal("actual TUI displayed superseded secret")
	}
	m.showSecretAdmin(second)
	if m.secretPanel == nil || m.panelKind != "secret" {
		t.Fatal("actual TUI suppressed current authorized secret")
	}
}
