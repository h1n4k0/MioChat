package main

import (
	"bytes"
	"context"
	"testing"
	"time"
)

// Run unchanged on the accepted parent and the session-bound implementation.
// This calls the real TUI command, waits for its writer acquisition, and then
// invalidates the already-admitted connection without changing its account role.
func secretAdminQueuedSessionCase(t *testing.T, line, mode string) {
	t.Helper()
	s, h := featureFixture(t)
	m := testModel(t, s, h, "admin")
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	m.ctx = ctx
	hash, salt := credentialBytes(t, s, "bob")
	invites := storageScalar(t, s, `SELECT count(*) FROM invites`)
	audits := storageScalar(t, s, `SELECT count(*) FROM audit`)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	waits := s.db.Stats().WaitCount
	done := make(chan model, 1)
	go func() {
		defer close(done)
		m.command(line)
		done <- m
	}()
	defer func() {
		cancel()
		_ = tx.Rollback()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("owned TUI command did not stop after transaction release")
		}
	}()
	deadline := time.Now().Add(3 * time.Second)
	for s.db.Stats().WaitCount <= waits {
		if time.Now().After(deadline) {
			t.Fatal("real TUI secret command did not queue on writer")
		}
		time.Sleep(time.Millisecond)
	}
	switch mode {
	case "session":
		err = changedOne(tx.Exec(`UPDATE sessions SET revoked=1 WHERE id=?`, m.client.ID))
	case "epoch":
		err = changedOne(tx.Exec(`INSERT INTO credential_versions(user,epoch) VALUES('admin',1) ON CONFLICT(user) DO UPDATE SET epoch=epoch+1`))
	default:
		t.Fatal("unknown synthetic invalidation")
	}
	if err != nil {
		t.Fatal("invalidation did not write", err)
	}
	if valid, e := sessionCurrentQ(tx, m.client.ID, "admin"); e != nil || valid {
		t.Fatal("fixture did not actually revoke authority", e)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-done:
		afterHash, afterSalt := credentialBytes(t, s, "bob")
		if !bytes.Equal(hash, afterHash) || !bytes.Equal(salt, afterSalt) ||
			storageScalar(t, s, `SELECT count(*) FROM invites`) != invites ||
			storageScalar(t, s, `SELECT count(*) FROM audit`) != audits || result.panelKind == "secret" {
			t.Fatalf("SECRET_ADMIN_STALE_%s_COMMITTED: command=%s", mode, line)
		}
	case <-ctx.Done():
		t.Fatal("TUI command did not finish after writer release", ctx.Err())
	}
}

func TestTUISecretAdminQueuedRevokedSessionCannotCommit(t *testing.T) {
	for _, line := range []string{"/invitecode 1d 1", "/resetpass bob"} {
		t.Run(line, func(t *testing.T) { secretAdminQueuedSessionCase(t, line, "session") })
	}
}

func TestTUISecretAdminQueuedCredentialEpochCannotCommit(t *testing.T) {
	for _, line := range []string{"/invitecode 1d 1", "/resetpass bob"} {
		t.Run(line, func(t *testing.T) { secretAdminQueuedSessionCase(t, line, "epoch") })
	}
}

func TestTUISecretPanelIsClearedByFreshRoleRefresh(t *testing.T) {
	s, h := featureFixture(t)
	m := testModel(t, s, h, "admin")
	m.command("/invitecode 1d 1")
	if m.panelKind != "secret" || len(m.panelLines) == 0 {
		t.Fatal("real TUI secret panel was not shown")
	}
	if err := changedOne(s.db.Exec(`UPDATE users SET role='member' WHERE name='admin'`)); err != nil {
		t.Fatal(err)
	}
	m.frame = 2 // next actual tick invokes the real Hub.RefreshClient route
	next, _ := m.Update(clockMsg(time.Now()))
	m = next.(model)
	if m.user.Role != "member" {
		t.Fatal("fixture did not receive the fresh policy")
	}
	if m.panelKind == "secret" || len(m.panelLines) != 0 {
		t.Fatal("SECRET_ADMIN_PRIVATE_PANEL_RETAINED_AFTER_ROLE_LOSS")
	}
}
