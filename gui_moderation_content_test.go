package main

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func contentRead(t *testing.T, s *Store, h *Hub, u User, r guiRequest, c, a string) *guiModerationView {
	t.Helper()
	p, e := guiModerationRead(s, h, u, guiScope{room: "lobby"}, r, c, a)
	if e != nil {
		t.Fatal(c, a, e)
	}
	return p.Moderation
}
func contentMessage(t *testing.T, s *Store, sender, room, peer, body string) Message {
	t.Helper()
	m, e := s.Send(Message{Sender: sender, Room: room, Recipient: peer, Body: body, Kind: "chat"})
	if e != nil {
		t.Fatal(e)
	}
	return m
}
func seedRevision(t *testing.T, s *Store, m Message, n int) {
	t.Helper()
	tx, e := s.db.Begin()
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback()
	for i := 0; i < n; i++ {
		if _, e = tx.Exec(`INSERT INTO revisions(message_id,actor,action,body,at) VALUES(?,?,'edit',?,?)`, m.ID, m.Sender, fmt.Sprintf("synthetic original %03d", i), i+1); e != nil {
			t.Fatal(e)
		}
	}
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}
}
func TestGUIModerationContentRevisionPermissionMatrix(t *testing.T) {
	s, h, u, r, target := moderationFixture(t)
	if e := s.CreateRoomAccess("content-private", target.Name, true); e != nil {
		t.Fatal(e)
	}
	public := contentMessage(t, s, target.Name, "lobby", "", "public")
	private := contentMessage(t, s, target.Name, "content-private", "", "private")
	received := contentMessage(t, s, target.Name, "", u.Name, "DM received")
	sent := contentMessage(t, s, u.Name, "", target.Name, "DM sent")
	own := contentMessage(t, s, u.Name, "lobby", "", "own")
	for _, m := range []Message{public, private, received, sent, own} {
		seedRevision(t, s, m, 1)
	}
	for _, role := range []string{"member", "moderator", "admin", "owner"} {
		if _, e := s.db.Exec(`UPDATE users SET role=? WHERE name=?`, role, u.Name); e != nil {
			t.Fatal(e)
		}
		for _, c := range []struct {
			m     Message
			allow bool
		}{{public, role != "member"}, {private, false}, {received, false}, {sent, true}, {own, true}} {
			p, e := guiModerationRead(s, h, u, guiScope{room: "lobby"}, r, "/revisions", fmt.Sprint(c.m.ID))
			if (e == nil) != c.allow {
				t.Fatal(role, c.m.ID, e)
			}
			if e != nil && p != nil {
				t.Fatal("denied data")
			}
		}
	}
	if e := s.RoomMember(target.Name, "content-private", u.Name, "invite"); e != nil {
		t.Fatal(e)
	}
	for _, role := range []string{"member", "moderator", "admin", "owner"} {
		s.db.Exec(`UPDATE users SET role=? WHERE name=?`, role, u.Name)
		p, e := guiModerationRead(s, h, u, guiScope{room: "lobby"}, r, "/revisions", fmt.Sprint(private.ID))
		if (e == nil) != (role == "admin" || role == "owner") {
			t.Fatal("private member", role, p, e)
		}
	}
	if _, e := s.db.Exec(`UPDATE members SET role='moderator' WHERE room='content-private' AND user=?`, u.Name); e != nil {
		t.Fatal(e)
	}
	for _, role := range []string{"member", "moderator"} {
		s.db.Exec(`UPDATE users SET role=? WHERE name=?`, role, u.Name)
		p, e := guiModerationRead(s, h, u, guiScope{room: "lobby"}, r, "/revisions", fmt.Sprint(private.ID))
		if (e == nil) != (role == "moderator") {
			t.Fatal("both required", role, p, e)
		}
	}
	ownPrivate := contentMessage(t, s, u.Name, "content-private", "", "own private")
	seedRevision(t, s, ownPrivate, 1)
	if e := s.RoomMember(target.Name, "content-private", u.Name, "remove"); e != nil {
		t.Fatal(e)
	}
	if p, e := guiModerationRead(s, h, u, guiScope{room: "lobby"}, r, "/revisions", fmt.Sprint(ownPrivate.ID)); e == nil || p != nil {
		t.Fatal("sender bypassed current room ACL")
	}
	// Fresh role also determines whether this read must be audited.
	s.db.Exec(`UPDATE users SET role='admin' WHERE name=?`, u.Name)
	u.Role = "member"
	v := contentRead(t, s, h, u, r, "/revisions", fmt.Sprint(sent.ID))
	if !v.Audited {
		t.Fatal("stale User suppressed audit")
	}
}
func TestGUIModerationContentRevisionsCompleteStablePages(t *testing.T) {
	s, h, u, r, target := moderationFixture(t)
	m := contentMessage(t, s, u.Name, "lobby", "", "own pageable")
	seedRevision(t, s, m, 205)
	if _, e := s.SavePositionVersioned(u.Name, "room:lobby", Position{Draft: "keep draft", Anchor: 77, Offset: 2}); e != nil {
		t.Fatal(e)
	}
	before, _ := s.PositionWithError(u.Name, "room:lobby")
	page := contentRead(t, s, h, u, r, "/revisions", fmt.Sprint(m.ID)).Revisions
	if len(page.Rows) != 100 || page.NextAfter <= 0 {
		t.Fatal(page)
	}
	until := page.Until
	ids := map[int64]bool{}
	for _, row := range page.Rows {
		ids[row.ID] = true
	}
	seedRevision(t, s, m, 1)
	for page.NextAfter > 0 {
		r.ReadAfter = page.NextAfter
		r.ReadUntil = &until
		page = contentRead(t, s, h, u, r, "/revisions", fmt.Sprint(m.ID)).Revisions
		if page.Until != until {
			t.Fatal("snapshot moved")
		}
		for _, row := range page.Rows {
			if ids[row.ID] || row.ID > until {
				t.Fatal("duplicate or moved highwater", row)
			}
			ids[row.ID] = true
		}
	}
	if len(ids) != 205 {
		t.Fatal("truncated revision history", len(ids))
	}
	// Revoking channel visibility between pages must not inherit first-page access.
	if _, e := s.db.Exec(`UPDATE room_meta SET private=1 WHERE name='lobby'`); e != nil {
		t.Fatal(e)
	}
	r.ReadAfter = 100
	r.ReadUntil = &until
	if p, e := guiModerationRead(s, h, u, guiScope{room: "lobby"}, r, "/revisions", fmt.Sprint(m.ID)); e == nil || p != nil {
		t.Fatal("old page retained revoked channel access")
	}
	if _, e := s.db.Exec(`UPDATE room_meta SET private=0 WHERE name='lobby'`); e != nil {
		t.Fatal(e)
	}
	zero := int64(0)
	r.ReadAfter = 0
	r.ReadUntil = &zero
	empty := contentRead(t, s, h, u, r, "/revisions", fmt.Sprint(m.ID)).Revisions
	if empty.Until != 0 || len(empty.Rows) != 0 || empty.NextAfter != 0 {
		t.Fatal("explicit zero became unbounded", empty)
	}
	r.ReadUntil = nil
	fresh := contentRead(t, s, h, u, r, "/revisions", fmt.Sprint(m.ID)).Revisions
	if fresh.Until <= until {
		t.Fatal("restart missed new revision")
	}
	// Another message's cursor never grants access; authorization is on fixed target.
	other := contentMessage(t, s, target.Name, "", u.Name, "private received")
	seedRevision(t, s, other, 1)
	r.ReadAfter = 100
	r.ReadUntil = &until
	if p, e := guiModerationRead(s, h, u, guiScope{room: "lobby"}, r, "/revisions", fmt.Sprint(other.ID)); e == nil || p != nil {
		t.Fatal("pagination bypassed ACL")
	}
	after, _ := s.PositionWithError(u.Name, "room:lobby")
	if !reflect.DeepEqual(before, after) {
		t.Fatal("paging changed chat persistence")
	}
}
func TestGUIModerationContentReportSingleMessageAuditAndCurrentBody(t *testing.T) {
	s, h, u, r, target := moderationFixture(t)
	peer := registerUser(t, s, "content-peer")
	parent := contentMessage(t, s, target.Name, "", peer.Name, "never-project-reply-parent")
	m := contentMessage(t, s, target.Name, "", peer.Name, "reported-current-body")
	if _, e := s.db.Exec(`UPDATE message_meta SET reply_to=? WHERE id=?`, parent.ID, m.ID); e != nil {
		t.Fatal(e)
	}
	reportID := int64(8001)
	if _, e := s.db.Exec(`INSERT INTO reports(id,user,message_id,reason,status,at) VALUES(?,?,?,'synthetic report','resolved',1)`, reportID, peer.Name, m.ID); e != nil {
		t.Fatal(e)
	}
	for _, role := range []string{"member", "moderator", "admin", "owner"} {
		s.db.Exec(`UPDATE users SET role=? WHERE name=?`, role, u.Name)
		p, e := guiModerationRead(s, h, u, guiScope{room: "lobby"}, r, "/reportview", fmt.Sprint(reportID))
		if (e == nil) != (role == "admin" || role == "owner") {
			t.Fatal(role, e)
		}
		if e == nil {
			raw, _ := json.Marshal(p)
			v := p.Moderation
			if !v.Audited || v.Reported.ReportID != reportID || v.Reported.MessageID != m.ID || v.Reported.Body != m.Body || strings.Contains(string(raw), parent.Body) || strings.Contains(string(raw), "reply_body") || strings.Contains(string(raw), "recipient") {
				t.Fatal("single message projection", string(raw))
			}
		}
	}
	if p, e := guiModerationRead(s, h, u, guiScope{room: "lobby"}, r, "/reportview", fmt.Sprint(m.ID)); e == nil || p != nil {
		t.Fatal("message ID mistaken for report ID")
	}
	if p, e := guiModerationRead(s, h, u, guiScope{room: "lobby"}, r, "/revisions", fmt.Sprint(m.ID)); e == nil || p != nil {
		t.Fatal("report granted revisions")
	}
	if e := s.UpdateMessage(target.Name, m.ID, "", true); e != nil {
		t.Fatal(e)
	}
	v := contentRead(t, s, h, u, r, "/reportview", fmt.Sprint(reportID))
	if v.Reported.Body != "[消息已撤回]" {
		t.Fatal("report enriched from revisions", v.Reported)
	}
	for _, command := range []string{"/reportview", "/revisions"} {
		id := reportID
		if command == "/revisions" {
			own := contentMessage(t, s, u.Name, "lobby", "", "own audit")
			seedRevision(t, s, own, 1)
			id = own.ID
		}
		for _, fault := range []string{"ABORT,'synthetic read audit fault'", "IGNORE"} {
			if _, e := s.db.Exec(`CREATE TRIGGER synthetic_content_audit BEFORE INSERT ON audit BEGIN SELECT RAISE(` + fault + `);END`); e != nil {
				t.Fatal(e)
			}
			if p, e := guiModerationRead(s, h, u, guiScope{room: "lobby"}, r, command, fmt.Sprint(id)); e == nil || p != nil {
				t.Fatal("unaudited content escaped", command)
			}
			if _, e := s.db.Exec(`DROP TRIGGER synthetic_content_audit`); e != nil {
				t.Fatal(e)
			}
		}
	}
}
func TestGUIModerationContentListsExactWindowsAndPrivacy(t *testing.T) {
	s, h, u, r, target := moderationFixture(t)
	s.db.Exec(`UPDATE users SET role='admin' WHERE name=?`, u.Name)
	public := contentMessage(t, s, target.Name, "lobby", "", "old-public-body")
	if e := s.UpdateMessage(target.Name, public.ID, "", true); e != nil {
		t.Fatal(e)
	}
	dm := contentMessage(t, s, target.Name, "", u.Name, "old-dm-body")
	if e := s.UpdateMessage(target.Name, dm.ID, "", true); e != nil {
		t.Fatal(e)
	}
	if e := s.CreateRoomAccess("recalled-private", target.Name, true); e != nil {
		t.Fatal(e)
	}
	private := contentMessage(t, s, target.Name, "recalled-private", "", "old-private-body")
	if e := s.UpdateMessage(target.Name, private.ID, "", true); e != nil {
		t.Fatal(e)
	}
	v := contentRead(t, s, h, u, r, "/recalls", "")
	if len(v.Recalls) != 1 || v.Recalls[0].ID != public.ID || v.Recalls[0].Body != "[消息已撤回]" {
		t.Fatal(v)
	}
	if _, e := s.db.Exec(`UPDATE room_meta SET private=1 WHERE name='lobby'`); e != nil {
		t.Fatal(e)
	}
	v = contentRead(t, s, h, u, r, "/recalls", "")
	if len(v.Recalls) != 0 {
		t.Fatal("newly private recall stayed visible")
	}
	if _, e := s.db.Exec(`UPDATE room_meta SET private=0 WHERE name='lobby'`); e != nil {
		t.Fatal(e)
	}
	tx, e := s.db.Begin()
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback()
	for i := 0; i < 105; i++ {
		if _, e = tx.Exec(`INSERT INTO reports(user,message_id,reason,status,at) VALUES(?,?,?, ?,?)`, u.Name, public.ID, fmt.Sprintf("reason-%03d", i), []string{"open", "resolved", "rejected"}[i%3], i+1); e != nil {
			t.Fatal(e)
		}
		if _, e = tx.Exec(`INSERT INTO audit(actor,action,target,detail,room,at) VALUES(?,'audit.message.read','1',?,'',?)`, u.Name, fmt.Sprintf("detail-%03d", i), i+1); e != nil {
			t.Fatal(e)
		}
		if _, e = tx.Exec(`INSERT INTO sanctions(kind,target,room,reason,actor,at,expires,revoked) VALUES('user',?,'',?,?,1,1,?)`, target.Name, fmt.Sprintf("reason-%03d", i), u.Name, i%2); e != nil {
			t.Fatal(e)
		}
	}
	if _, e = tx.Exec(`INSERT INTO audit(actor,action,target,detail,room,at) VALUES(?,'message.edit','1','must-exclude','',1)`, u.Name); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}
	for _, command := range []string{"/bans", "/audit", "/reports"} {
		v = contentRead(t, s, h, u, r, command, "")
		if v.Window != 100 {
			t.Fatal(v)
		}
		switch command {
		case "/bans":
			if len(v.Sanctions) != 100 || v.Sanctions[0].Reason != "reason-104" {
				t.Fatal(v)
			}
		case "/audit":
			if len(v.Audits) != 100 || v.Audits[0].Detail != "detail-104" {
				t.Fatal(v)
			}
		case "/reports":
			if len(v.Reports) != 100 || v.Reports[0].Reason != "reason-104" {
				t.Fatal(v)
			}
		}
	}
	for _, role := range []string{"member", "moderator"} {
		s.db.Exec(`UPDATE users SET role=? WHERE name=?`, role, u.Name)
		for _, command := range []string{"/bans", "/audit", "/recalls", "/reports"} {
			if p, e := guiModerationRead(s, h, u, guiScope{room: "lobby"}, r, command, ""); e == nil || p != nil {
				t.Fatal("nonadmin list", role, command)
			}
		}
	}
}

func TestGUIModerationContentQueuedFreshAuditAndCanceledData(t *testing.T) {
	s, h, u, r, target := moderationFixture(t)
	m := contentMessage(t, s, u.Name, "lobby", "", "own revision")
	seedRevision(t, s, m, 1)
	if _, e := s.db.Exec(`CREATE TRIGGER synthetic_upgrade_audit BEFORE INSERT ON audit WHEN NEW.action='audit.message.read' BEGIN SELECT RAISE(IGNORE);END`); e != nil {
		t.Fatal(e)
	}
	blocker, e := s.db.Begin()
	if e != nil {
		t.Fatal(e)
	}
	defer blocker.Rollback()
	waits := s.db.Stats().WaitCount
	type result struct {
		p *guiPanel
		e error
	}
	done := make(chan result, 1)
	go func() {
		p, e := guiModerationRead(s, h, u, guiScope{room: "lobby"}, r, "/revisions", fmt.Sprint(m.ID))
		done <- result{p, e}
	}()
	deadline := time.Now().Add(3 * time.Second)
	for s.db.Stats().WaitCount <= waits {
		if time.Now().After(deadline) {
			t.Fatal("revision not queued")
		}
		time.Sleep(time.Millisecond)
	}
	if _, e = blocker.Exec(`UPDATE users SET role='admin' WHERE name=?`, u.Name); e != nil {
		t.Fatal(e)
	}
	if e = blocker.Commit(); e != nil {
		t.Fatal(e)
	}
	out := <-done
	if out.p != nil || out.e == nil || !strings.Contains(out.e.Error(), "审计") {
		t.Fatal("queued upgrade skipped required audit or inconclusive timeout", out.e)
	}
	if _, e = s.db.Exec(`DROP TRIGGER synthetic_upgrade_audit`); e != nil {
		t.Fatal(e)
	}
	if _, e = s.db.Exec(`INSERT INTO reports(user,message_id,reason,at) VALUES(?,?,'synthetic',1)`, target.Name, m.ID); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	r.ctx = ctx
	for _, c := range []struct{ command, arg string }{{"/reportview", "1"}, {"/revisions", fmt.Sprint(m.ID)}} {
		if p, e := guiModerationRead(s, h, u, guiScope{room: "lobby"}, r, c.command, c.arg); e == nil || p != nil {
			t.Fatal("canceled read returned content", c)
		}
	}
	var n int
	if e = s.db.QueryRow(`SELECT count(*) FROM audit WHERE action IN ('audit.message.read','report.read')`).Scan(&n); e != nil || n != 0 {
		t.Fatal("failed/canceled read audited", n, e)
	}
}
