package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func maintenanceTestRequest(op, id, start string) guiRequest {
	return guiRequest{Type: "maintenance", RequestID: "request-" + op, Room: "lobby", Maintenance: &guiMaintenanceRequest{Operation: op, JobID: id, StartID: start}}
}
func maintenanceTestFinish(t *testing.T, state *guiMaintenanceState, store *Store) guiResponse {
	t.Helper()
	select {
	case result := <-state.results:
		out, ok := state.finish(t.Context(), store, result)
		if !ok {
			t.Fatal("matching maintenance result not accepted")
		}
		return out
	case <-time.After(3 * time.Second):
		t.Fatal("background maintenance did not finish")
	}
	return guiResponse{}
}
func TestGUIMaintenancePrivateResultReopenExpiresAndRechecksRole(t *testing.T) {
	s, h := featureFixture(t)
	m := testModel(t, s, h, "admin")
	g := newGUIMaintenance()
	defer g.stop()
	scope := guiScope{room: "lobby"}
	req := maintenanceTestRequest("start", "", "")
	req.Maintenance.Kind = "check"
	out, err := g.request(t.Context(), s, m.user, scope, m.client.ID, req)
	if err != nil || out.Maintenance == nil || out.Maintenance.State != "running" || out.Maintenance.Committed != nil || len(out.Maintenance.Lines) != 0 {
		t.Fatal(out, err)
	}
	id, start := out.Maintenance.JobID, out.Maintenance.StartID
	out = maintenanceTestFinish(t, g, s)
	if out.Maintenance.State != "complete" || out.Maintenance.Committed == nil || *out.Maintenance.Committed || len(out.Maintenance.Lines) != 0 || out.Maintenance.Warning != "" {
		t.Fatal("completion must be non-sensitive", out)
	}
	view := maintenanceTestRequest("view", id, start)
	out, err = g.request(t.Context(), s, m.user, scope, m.client.ID, view)
	if err != nil || len(out.Maintenance.Lines) != 4 || !strings.Contains(out.Maintenance.Lines[0], "quick_check") || out.Panel != nil || out.Preferences != nil || out.Draft != nil {
		t.Fatal(out, err)
	}
	g.expire(g.completed.expires)
	if out, err = g.request(t.Context(), s, m.user, scope, m.client.ID, view); err == nil || out.Maintenance != nil {
		t.Fatal("expired descriptor read", out, err)
	}
	req.RequestID = "new-start"
	out, err = g.request(t.Context(), s, m.user, scope, m.client.ID, req)
	if err != nil {
		t.Fatal(err)
	}
	id, start = out.Maintenance.JobID, out.Maintenance.StartID
	maintenanceTestFinish(t, g, s)
	if err = changedOne(s.db.Exec(`UPDATE users SET role='member' WHERE name='admin'`)); err != nil {
		t.Fatal(err)
	}
	if out, err = g.request(t.Context(), s, m.user, scope, m.client.ID, maintenanceTestRequest("view", id, start)); err == nil || out.Maintenance != nil || g.completed != nil {
		t.Fatal("stale role released private result", out, err)
	}
}
func TestGUIMaintenanceCancelsOnlyBoundOwnJobAndKeepsChatUsable(t *testing.T) {
	ctx, stopTest := context.WithTimeout(t.Context(), 5*time.Second)
	defer stopTest()
	started := time.Now()
	s, h := featureFixture(t)
	m := testModel(t, s, h, "admin")
	other := testModel(t, s, h, "owner")
	g := newGUIMaintenance()
	defer g.stop()
	foreign := newGUIMaintenance()
	defer foreign.stop()
	scope := guiScope{room: "lobby"}
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
	req := maintenanceTestRequest("start", "", "")
	req.Maintenance.Kind = "check"
	out, err := g.request(ctx, s, m.user, scope, m.client.ID, req)
	if err != nil {
		t.Fatal(err)
	}
	id, start := out.Maintenance.JobID, out.Maintenance.StartID
	oldJob := g.active
	if _, err = g.request(ctx, s, m.user, scope, m.client.ID, req); err == nil {
		t.Fatal("second connection-local job admitted")
	}
	cancel := maintenanceTestRequest("cancel", id, start)
	if _, err = foreign.request(ctx, s, other.user, scope, other.client.ID, cancel); err == nil {
		t.Fatal("foreign controller cancelled another job")
	}
	if _, err = g.request(ctx, s, other.user, scope, other.client.ID, cancel); err == nil {
		t.Fatal("foreign owner/session cancelled job")
	}
	// Send commits on the writer, then decorates on the deliberately blocked
	// reader. Do not synchronously wait for decoration before requesting cancel:
	// that would spend the maintenance authorization budget in this test itself.
	sendDone := make(chan error, 1)
	go func() {
		_, e := s.SendForSessionContext(ctx, Message{Sender: "admin", Room: "lobby", Kind: "chat", Body: "chat during background check"}, m.client.ID)
		sendDone <- e
		close(sendDone)
	}()
	defer func() {
		stopTest()
		first.Close()
		second.Close()
		select {
		case <-sendDone:
		case <-time.After(time.Second):
			t.Error("owned synthetic sender did not stop")
		}
	}()
	deadline := time.Now().Add(time.Second)
	for {
		var committed int
		if e := s.db.QueryRowContext(ctx, `SELECT count(*) FROM messages WHERE sender='admin' AND body='chat during background check'`).Scan(&committed); e != nil {
			t.Fatal(e)
		}
		if committed == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("chat writer did not commit while maintenance reader was queued")
		}
		time.Sleep(time.Millisecond)
	}
	out, err = g.request(ctx, s, m.user, scope, m.client.ID, cancel)
	if err != nil || out.Maintenance.State != "cancel_requested" || out.Maintenance.Committed != nil {
		t.Fatal("cancel must only acknowledge request", out, err)
	}
	first.Close()
	second.Close()
	select {
	case e := <-sendDone:
		if e != nil {
			t.Fatal("chat decoration failed after reader release", e)
		}
	case <-time.After(time.Second):
		t.Fatal("chat did not finish after reader release")
	}
	var result guiMaintenanceResult
	select {
	case result = <-g.results:
	case <-ctx.Done():
		t.Fatal("cancelled job did not finish", ctx.Err())
	}
	var matched bool
	out, matched = g.finish(ctx, s, result)
	if !matched || out.Maintenance.State != "cancelled" {
		t.Fatalf("cancel outcome=%+v error=%v elapsed=%s (no deadline is accepted as cancelled)", out.Maintenance, result.err, time.Since(started))
	}
	req.RequestID = "replacement"
	_, err = g.request(ctx, s, m.user, scope, m.client.ID, req)
	if err != nil {
		t.Fatal(err)
	}
	replacement := g.active
	if _, valid := g.finish(ctx, s, guiMaintenanceResult{job: oldJob}); valid || g.active != replacement {
		t.Fatal("late completion cleared replacement")
	}
	if _, err = g.request(ctx, s, m.user, scope, m.client.ID, cancel); err == nil || g.active != replacement {
		t.Fatal("stale cancel affected replacement")
	}
	maintenanceTestFinish(t, g, s)
}
func TestGUIMaintenanceAuthMixedRequestsAndUnavailableOutcome(t *testing.T) {
	s, h := featureFixture(t)
	m := testModel(t, s, h, "admin")
	g := newGUIMaintenance()
	defer g.stop()
	scope := guiScope{room: "lobby"}
	req := maintenanceTestRequest("start", "", "")
	req.Maintenance.Kind = "check"
	for _, field := range []string{"body", "password", "reply_to", "composition", "room"} {
		raw, _ := json.Marshal(req)
		var fields map[string]any
		json.Unmarshal(raw, &fields)
		switch field {
		case "reply_to":
			fields[field] = 1
		case "composition":
			fields[field] = map[string]any{}
		default:
			fields[field] = "synthetic-mixed"
		}
		raw, _ = json.Marshal(fields)
		var mixed guiRequest
		json.Unmarshal(raw, &mixed)
		if out, err := g.request(t.Context(), s, m.user, scope, m.client.ID, mixed); err == nil || out.Maintenance != nil || g.active != nil {
			t.Fatal(field, out, err)
		}
	}
	_, err := g.request(t.Context(), s, m.user, scope, m.client.ID, req)
	if err != nil {
		t.Fatal(err)
	}
	var result guiMaintenanceResult
	select {
	case result = <-g.results:
	case <-time.After(3 * time.Second):
		t.Fatal("job timeout")
	}
	if err = changedOne(s.db.Exec(`UPDATE sessions SET revoked=1 WHERE id=?`, m.client.ID)); err != nil {
		t.Fatal(err)
	}
	out, ok := g.finish(t.Context(), s, result)
	if !ok || out.Maintenance.State != "unavailable" || out.Maintenance.Committed != nil || len(out.Maintenance.Lines) != 0 || g.completed != nil {
		t.Fatal("revoked completion disclosed outcome", out)
	}
}
func TestMaintenanceCommittedBackupAuditWarningAndTUIMatchingJob(t *testing.T) {
	state := restoreFixture(t, "snapshot-maintenance")
	s, err := openStore(filepath.Join(state, "miochat.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.db.Close()
	if err = s.Promote("snapshot-maintenance"); err != nil {
		t.Fatal(err)
	}
	h := newHub()
	m := testModel(t, s, h, "snapshot-maintenance")
	if _, err = s.db.Exec(`CREATE TRIGGER synthetic_maintenance_audit BEFORE INSERT ON audit WHEN NEW.action='backup.create' BEGIN SELECT RAISE(IGNORE); END`); err != nil {
		t.Fatal(err)
	}
	out, err := s.RunMaintenanceForSession(t.Context(), m.user.Name, m.client.ID, "backup")
	if err != nil || !out.Committed || out.Warning == "" || len(out.Lines) != 2 {
		t.Fatal("postcommit audit failure lost durable outcome", out, err)
	}
	if _, err = ValidateSnapshot(out.Lines[0]); err != nil {
		t.Fatal("committed backup not valid", err)
	}
	cmd := m.startMaintenance("check")
	if cmd == nil {
		t.Fatal("TUI job missing")
	}
	result := cmd().(maintenanceResult)
	oldID := result.id
	m.finishMaintenance(result)
	replacement := m.startMaintenance("check")
	if replacement == nil {
		t.Fatal("new TUI job missing")
	}
	newID := m.maintenanceID
	m.finishMaintenance(result)
	if m.maintenanceID != newID || m.maintenanceCancel == nil || m.cancelMaintenance(oldID) {
		t.Fatal("late TUI result/cancel affected replacement")
	}
	m.finishMaintenance(replacement().(maintenanceResult))
	if m.maintenanceID != 0 || m.maintenanceCancel != nil {
		t.Fatal("matching TUI job retained slot")
	}
}

func TestGUIMaintenanceWireIsAsyncPrivateAndMenuComplete(t *testing.T) {
	s, h := featureFixture(t)
	admin := newGUIWireFixture(t, s, h)
	admin.send(guiRequest{Type: "login", RequestID: "maintenance-login", Username: "admin", Password: "test-password-123"})
	admin.read("auth", "maintenance-login")
	admin.read("state", "")
	admin.send(guiRequest{Type: "command", RequestID: "maintenance-menu", Room: "lobby", Body: "/menu"})
	menu := admin.read("action", "maintenance-menu").Panel
	if menu == nil {
		t.Fatal("missing menu")
	}
	for _, command := range []string{"/backup", "/check", "/canceljob"} {
		found := false
		for _, a := range menu.Actions {
			if strings.TrimSpace(a.Command) == command {
				found = true
			}
		}
		if !found {
			t.Fatal("missing native job menu", command)
		}
	}
	time.Sleep(750 * time.Millisecond)
	admin.send(guiRequest{Type: "maintenance", RequestID: "wire-check", Room: "lobby", Maintenance: &guiMaintenanceRequest{Operation: "start", Kind: "check"}})
	ack := admin.read("maintenance", "wire-check")
	if ack.Maintenance == nil || ack.Maintenance.Operation != "start" || ack.Maintenance.State != "running" || ack.Maintenance.Committed != nil {
		t.Fatal("not a start acknowledgement", ack)
	}
	done := admin.read("maintenance", "wire-check")
	if done.Maintenance.Operation != "complete" || done.Maintenance.State != "complete" || len(done.Maintenance.Lines) != 0 || done.Maintenance.Warning != "" {
		t.Fatal("sensitive async completion", done)
	}
	time.Sleep(750 * time.Millisecond)
	admin.send(guiRequest{Type: "send_message", RequestID: "after-background", Room: "lobby", Body: "synthetic maintenance leaves chat usable"})
	if out := admin.read("sent", "after-background"); out.Message == nil {
		t.Fatal("chat unavailable", out)
	}
	time.Sleep(750 * time.Millisecond)
	admin.send(guiRequest{Type: "maintenance", RequestID: "wire-view", Room: "lobby", Maintenance: &guiMaintenanceRequest{Operation: "view", JobID: done.Maintenance.JobID, StartID: "wire-check"}})
	view := admin.read("maintenance", "wire-view")
	if view.Maintenance.Operation != "view" || len(view.Maintenance.Lines) != 4 || view.Panel != nil || view.Preferences != nil {
		t.Fatal("private explicit read absent or mixed", view)
	}
	member := newGUIWireFixture(t, s, h)
	member.send(guiRequest{Type: "login", RequestID: "maintenance-member", Username: "alice", Password: "test-password-123"})
	member.read("auth", "maintenance-member")
	member.read("state", "")
	member.send(guiRequest{Type: "maintenance", RequestID: "foreign-view", Room: "lobby", Maintenance: &guiMaintenanceRequest{Operation: "view", JobID: done.Maintenance.JobID, StartID: "wire-check"}})
	if r := member.read("error", "foreign-view"); r.Maintenance != nil {
		t.Fatal("foreign result projected", r)
	}
	member.send(guiRequest{Type: "maintenance", RequestID: "foreign-start", Room: "lobby", Maintenance: &guiMaintenanceRequest{Operation: "start", Kind: "check"}})
	member.read("error", "foreign-start")
}

func TestGUIMaintenanceIdleHealthRefreshCancelsAndForgetsPrivateDescriptor(t *testing.T) {
	for _, mode := range []string{"role", "reset", "epoch"} {
		t.Run(mode, func(t *testing.T) {
			s, h := featureFixture(t)
			m := testModel(t, s, h, "admin")
			g := newGUIMaintenance()
			defer g.stop()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan struct{})
			close(done)
			job := &guiMaintenanceWork{id: "1", startID: "start", owner: m.user.Name, sessionID: m.client.ID, epoch: m.user.AuthEpoch, cancel: cancel, done: done}
			g.active = job
			g.completed = &guiMaintenanceCompleted{job: job, outcome: maintenanceOutcome{Lines: []string{"private-path"}}, expires: time.Now().Add(time.Minute)}
			var err error
			switch mode {
			case "role":
				err = changedOne(s.db.Exec(`UPDATE users SET role='user' WHERE name='admin'`))
			case "reset":
				err = changedOne(s.db.Exec(`UPDATE user_meta SET must_reset=1 WHERE name='admin'`))
			case "epoch":
				err = changedOne(s.db.Exec(`INSERT INTO credential_versions(user,epoch) VALUES('admin',1) ON CONFLICT(user) DO UPDATE SET epoch=epoch+1`))
			}
			if err != nil {
				t.Fatal(err)
			}
			// This is the exact method invoked by health.C, with the real durable policy
			// refresh, not a fabricated cached User role. No timer/goroutine-count claim.
			g.refresh(h, m.client)
			if ctx.Err() == nil || g.completed != nil {
				t.Fatal("idle policy refresh retained active work/private descriptor", mode)
			}
		})
	}
}
