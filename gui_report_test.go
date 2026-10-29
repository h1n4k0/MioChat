package main

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func nativeReport(t *testing.T, s *Store, u User, r guiRequest, operation string, id int64, value string) guiReportReceipt {
	t.Helper()
	out, e := s.reportForSessionContext(t.Context(), u.Name, r.sessionID, "lobby", operation, id, value)
	if e != nil {
		t.Fatal(operation, id, e)
	}
	return out
}
func TestGUIReportVisibilityNotCanSendAndPreservedDraft(t *testing.T) {
	s, h, u, r, target := moderationFixture(t)
	m := contentMessage(t, s, target.Name, "lobby", "", "synthetic visible")
	if _, e := s.SavePositionVersioned(u.Name, "room:lobby", Position{Draft: "ordinary draft", Anchor: m.ID, ReplyTo: m.ID, Offset: 2}); e != nil {
		t.Fatal(e)
	}
	before, _ := s.PositionWithError(u.Name, "room:lobby")
	if _, e := s.db.Exec(`UPDATE room_meta SET readonly=1,archived=1 WHERE name='lobby'`); e != nil {
		t.Fatal(e)
	}
	if _, e := s.db.Exec(`INSERT INTO sanctions(kind,target,room,expires,reason,actor,at) VALUES('mute',?,'lobby',?,'fixture mute',?,?)`, u.Name, time.Now().Unix()+600, target.Name, time.Now().Unix()); e != nil {
		t.Fatal(e)
	}
	if e := canSendQ(s.readDB(), u.Name, "lobby", ""); e == nil {
		t.Fatal("fixture is not read-only/muted")
	}
	reason := "  synthetic reason\nwith detail  "
	out := nativeReport(t, s, u, r, "report", m.ID, reason)
	if !out.Committed || out.MessageID != m.ID || out.ReportID <= 0 || out.RequestedReason != reason || out.Reason != cleanText(reason, 240) || out.Actor != u.Name || out.Status != "open" {
		t.Fatal(out)
	}
	r.Body = fmt.Sprintf("/report %d second explicit reason", m.ID)
	result, e := guiExecuteAction(s, h, u, guiScope{room: "lobby"}, m.ID, r)
	if e != nil || result.reportReceipt == nil || result.replyTo != m.ID || result.message != nil || result.panel != nil || result.scope.key() != "lobby" {
		t.Fatal(result, e)
	}
	after, _ := s.PositionWithError(u.Name, "room:lobby")
	if !reflect.DeepEqual(before, after) {
		t.Fatal("report changed ordinary draft/reply/position")
	}
	for _, bad := range []string{"", " \n\t "} {
		if _, e = s.reportForSessionContext(t.Context(), u.Name, r.sessionID, "lobby", "report", m.ID, bad); e == nil {
			t.Fatal("empty reason")
		}
	}
	if e = s.CreateRoomAccess("report-private", target.Name, true); e != nil {
		t.Fatal(e)
	}
	private := contentMessage(t, s, target.Name, "report-private", "", "private")
	peer := registerUser(t, s, "report-peer")
	dm := contentMessage(t, s, target.Name, "", peer.Name, "nonparticipant DM")
	s.db.Exec(`UPDATE users SET role='admin' WHERE name=?`, u.Name)
	for _, id := range []int64{private.ID, dm.ID} {
		if _, e = s.reportForSessionContext(t.Context(), u.Name, r.sessionID, "lobby", "report", id, "reason"); e == nil {
			t.Fatal("admin reporter bypassed visibility", id)
		}
	}
}
func TestGUIReportAtomicHourQuotaAndInsertFailure(t *testing.T) {
	s, _, u, r, target := moderationFixture(t)
	m := contentMessage(t, s, target.Name, "lobby", "", "quota target")
	now := time.Now().Unix()
	for i := 0; i < 9; i++ {
		if _, e := s.db.Exec(`INSERT INTO reports(user,message_id,reason,at) VALUES(?,?,'earlier',?)`, u.Name, m.ID, now); e != nil {
			t.Fatal(e)
		}
	}
	if _, e := s.db.Exec(`INSERT INTO reports(user,message_id,reason,at) VALUES(?,?,'outside-hour',?)`, u.Name, m.ID, now-3601); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 2)
	for range 2 {
		go func() {
			_, e := s.reportForSessionContext(t.Context(), u.Name, r.sessionID, "lobby", "report", m.ID, "concurrent")
			done <- e
		}()
	}
	success := 0
	for range 2 {
		if e := <-done; e == nil {
			success++
		} else if !strings.Contains(e.Error(), "每小时") {
			t.Fatal(e)
		}
	}
	if success != 1 {
		t.Fatal("quota race", success)
	}
	var count int
	s.db.QueryRow(`SELECT count(*) FROM reports WHERE user=? AND at>?`, u.Name, now-3600).Scan(&count)
	if count != 10 {
		t.Fatal(count)
	}
	// Independent actor verifies INSERT IGNORE is not reported as a committed ID.
	_, tr := communityReader(t, s, target.Name, "report-insert-session")
	if _, e := s.db.Exec(`CREATE TRIGGER synthetic_report_insert BEFORE INSERT ON reports BEGIN SELECT RAISE(IGNORE);END`); e != nil {
		t.Fatal(e)
	}
	out, e := s.reportForSessionContext(t.Context(), target.Name, tr.sessionID, "lobby", "report", m.ID, "must fail")
	if e == nil || out.Committed || out.ReportID != 0 {
		t.Fatal(out, e)
	}
}
func TestGUIResolveExactStatusRolesAuditRollbackAndRepeat(t *testing.T) {
	s, _, u, r, target := moderationFixture(t)
	m := contentMessage(t, s, target.Name, "lobby", "", "report target")
	made := nativeReport(t, s, u, r, "report", m.ID, "reason")
	for _, role := range []string{"member", "moderator", "admin", "owner"} {
		s.db.Exec(`UPDATE users SET role=? WHERE name=?`, role, u.Name)
		out, e := s.reportForSessionContext(t.Context(), u.Name, r.sessionID, "lobby", "resolve", made.ReportID, "resolved")
		if (e == nil) != (role == "admin" || role == "owner") {
			t.Fatal(role, out, e)
		}
	}
	for _, status := range []string{"rejected", "resolved", "resolved"} {
		out := nativeReport(t, s, u, r, "resolve", made.ReportID, status)
		if out.Status != status || out.ReportID != made.ReportID || out.MessageID != 0 || out.Reason != "" || !out.Committed {
			t.Fatal(out)
		}
	}
	for _, bad := range []string{"open", "RESOLVED", "resolved extra"} {
		if out, e := s.reportForSessionContext(t.Context(), u.Name, r.sessionID, "lobby", "resolve", made.ReportID, bad); e == nil || out.Committed {
			t.Fatal(bad, out, e)
		}
	}
	if out, e := s.reportForSessionContext(t.Context(), u.Name, r.sessionID, "lobby", "resolve", 999999, "resolved"); e == nil || out.Committed {
		t.Fatal("nonexistent report", out, e)
	}
	for _, fault := range []string{"ABORT,'synthetic resolve audit'", "IGNORE"} {
		if _, e := s.db.Exec(`CREATE TRIGGER synthetic_resolve_audit BEFORE INSERT ON audit WHEN NEW.action LIKE 'report.%' BEGIN SELECT RAISE(` + fault + `);END`); e != nil {
			t.Fatal(e)
		}
		out, e := s.reportForSessionContext(t.Context(), u.Name, r.sessionID, "lobby", "resolve", made.ReportID, "rejected")
		if e == nil || out.Committed {
			t.Fatal("unaudited resolve", out, e)
		}
		var status string
		s.db.QueryRow(`SELECT status FROM reports WHERE id=?`, made.ReportID).Scan(&status)
		if status != "resolved" {
			t.Fatal("audit failure didn't roll back", status)
		}
		if _, e = s.db.Exec(`DROP TRIGGER synthetic_resolve_audit`); e != nil {
			t.Fatal(e)
		}
	}
}
func TestGUIReportQueuedSessionACLAndAccountPolicy(t *testing.T) {
	for _, change := range []string{"session", "private", "reset", "ban"} {
		t.Run(change, func(t *testing.T) {
			s, _, u, r, target := moderationFixture(t)
			m := contentMessage(t, s, target.Name, "lobby", "", "queued target")
			blocker, e := s.db.Begin()
			if e != nil {
				t.Fatal(e)
			}
			defer blocker.Rollback()
			waits := s.db.Stats().WaitCount
			done := make(chan error, 1)
			go func() {
				_, e := s.reportForSessionContext(t.Context(), u.Name, r.sessionID, "lobby", "report", m.ID, "queued reason")
				done <- e
			}()
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
			case "private":
				_, e = blocker.Exec(`UPDATE room_meta SET private=1 WHERE name='lobby'`)
			case "reset":
				_, e = blocker.Exec(`UPDATE user_meta SET must_reset=1 WHERE name=?`, u.Name)
			case "ban":
				_, e = blocker.Exec(`UPDATE users SET banned=1 WHERE name=?`, u.Name)
			}
			if e != nil {
				t.Fatal(e)
			}
			if e = blocker.Commit(); e != nil {
				t.Fatal(e)
			}
			if e = <-done; e == nil {
				t.Fatal("queued revoked intent committed", change)
			}
			var count int
			s.db.QueryRow(`SELECT count(*) FROM reports WHERE user=?`, u.Name).Scan(&count)
			if count != 0 {
				t.Fatal("denied wrote report", count)
			}
		})
	}
}
