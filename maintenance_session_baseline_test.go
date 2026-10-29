package main

import (
	"context"
	"testing"
	"time"
)

// This exact test is intended for an isolated accepted-parent negative control
// and the future session-aware implementation. It does not bypass the real TUI
// startMaintenance command or infer permission from a helper return alone.
func maintenanceQueuedCredentialCase(t *testing.T, mode string) {
	t.Helper()
	s, h := featureFixture(t)
	m := testModel(t, s, h, "admin")
	valid, err := sessionCurrentQ(s.db, m.client.ID, "admin")
	if err != nil || !valid {
		t.Fatal("fixture active admin session missing", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	m.ctx = ctx
	first, err := s.readDB().Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := s.readDB().Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	waits := s.readDB().Stats().WaitCount
	cmd := m.startMaintenance("check")
	if cmd == nil || m.maintenanceCancel == nil {
		t.Fatal("real TUI did not admit background command")
	}
	done := make(chan maintenanceResult, 1)
	go func() { done <- cmd().(maintenanceResult) }()
	deadline := time.Now().Add(time.Second)
	for s.readDB().Stats().WaitCount <= waits {
		if time.Now().After(deadline) {
			t.Fatal("maintenance authorization did not actually queue")
		}
		time.Sleep(time.Millisecond)
	}
	switch mode {
	case "session":
		err = changedOne(s.db.Exec(`UPDATE sessions SET revoked=1 WHERE id=?`, m.client.ID))
	case "epoch":
		err = changedOne(s.db.Exec(`INSERT INTO credential_versions(user,epoch) VALUES('admin',1) ON CONFLICT(user) DO UPDATE SET epoch=epoch+1`))
	default:
		t.Fatal("unknown synthetic invalidation")
	}
	if err != nil {
		t.Fatal("invalidation did not write", err)
	}
	valid, err = sessionCurrentQ(s.db, m.client.ID, "admin")
	if err != nil || valid {
		t.Fatal("invalidation did not actually revoke session authority", err)
	}
	if err = first.Close(); err != nil {
		t.Fatal(err)
	}
	if err = second.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-done:
		if result.err == nil || len(result.lines) != 0 {
			t.Fatalf("MAINTENANCE_STALE_%s_EXECUTED: err=%v lines=%d", mode, result.err, len(result.lines))
		}
	case <-ctx.Done():
		t.Fatal("maintenance did not terminate after reader release", ctx.Err())
	}
}

func TestTUIMaintenanceQueuedRevokedSessionCannotRunCheck(t *testing.T) {
	maintenanceQueuedCredentialCase(t, "session")
}

func TestTUIMaintenanceQueuedCredentialEpochCannotRunCheck(t *testing.T) {
	maintenanceQueuedCredentialCase(t, "epoch")
}

func TestTUIMaintenanceActiveSessionStillCompletesCheck(t *testing.T) {
	s, h := featureFixture(t)
	m := testModel(t, s, h, "admin")
	m.ctx = t.Context()
	cmd := m.startMaintenance("check")
	if cmd == nil {
		t.Fatal("active admin check not admitted")
	}
	result := cmd().(maintenanceResult)
	if result.err != nil || len(result.lines) == 0 {
		t.Fatal("active admin check failed", result.err)
	}
	m.finishMaintenance(result)
	if m.maintenanceCancel != nil {
		t.Fatal("completed check retained slot")
	}
}
