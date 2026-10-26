package main

import (
	"context"
	"database/sql"
	"io"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func reopenPositionReader(t *testing.T, s *Store) {
	t.Helper()
	reader, err := sql.Open("sqlite", s.path)
	if err != nil {
		t.Fatal(err)
	}
	reader.SetMaxOpenConns(2)
	s.db.reader = reader
}

func insertLegacyPositionDraft(t *testing.T, s *Store, scope, draft string) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO preferences(user,scope,draft) VALUES('alice',?,?) ON CONFLICT(user,scope) DO UPDATE SET draft=excluded.draft`, scope, draft); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM position_versions WHERE user='alice' AND scope=?`, scope); err != nil {
		t.Fatal(err)
	}
}

func assertLegacyDraftUnchanged(t *testing.T, s *Store, scope, expected string) {
	t.Helper()
	var draft string
	var version int64
	if err := s.db.QueryRow(`SELECT p.draft,COALESCE(v.version,0) FROM preferences p LEFT JOIN position_versions v ON v.user=p.user AND v.scope=p.scope WHERE p.user='alice' AND p.scope=?`, scope).Scan(&draft, &version); err != nil || draft != expected || version != 0 {
		t.Fatal("an unread version-zero draft was changed", draft, version, err)
	}
}

func TestPositionReadFailureIsDistinctFromAbsentPreferenceAndStopsTrustedSave(t *testing.T) {
	s, _ := featureFixture(t)
	p, err := s.PositionWithError("alice", "room:missing-preference")
	if err != nil || p != (Position{}) {
		t.Fatal("an absent preference is a valid empty baseline", p, err)
	}
	insertLegacyPositionDraft(t, s, "room:lobby", "版本零的既有草稿")
	if err := s.db.reader.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PositionWithError("alice", "room:lobby"); err == nil {
		t.Fatal("query failure was treated as an absent preference")
	}
	if err := s.SavePosition("alice", "room:lobby", Position{Draft: "不得覆盖"}); err == nil {
		t.Fatal("trusted SavePosition ignored its baseline read failure")
	}
	assertLegacyDraftUnchanged(t, s, "room:lobby", "版本零的既有草稿")
}

func TestForcedMoveWithUnreadLegacyDraftPausesInputAndRecoversOnTick(t *testing.T) {
	s, hub := featureFixture(t)
	if err := s.CreateRoomAccess("position-source", "alice", true); err != nil {
		t.Fatal(err)
	}
	m := testModel(t, s, hub, "alice")
	if err := m.move("position-source", ""); err != nil {
		t.Fatal(err)
	}
	m.input.SetValue("原频道尚未发送的私密内容")
	insertLegacyPositionDraft(t, s, "room:lobby", "目标频道原有草稿")
	if err := s.db.reader.Close(); err != nil {
		t.Fatal(err)
	}
	if err := m.forceMove("lobby"); err != nil {
		t.Fatal(err)
	}
	if m.room != "lobby" || !m.positionUnknown || m.positionVersion != -1 || m.panelKind != "position-loading" || m.input.Value() != "" || !strings.Contains(m.View(), "已暂停输入") {
		t.Fatal("forced destination was treated as a valid empty baseline", m.room, m.positionUnknown, m.positionVersion, m.panelKind, m.input.Value())
	}
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	m = updated.(model)
	m.command("不能作为新消息输入")
	if m.input.Value() != "" || m.saveView() == nil || m.savePositionDraft() == nil || m.send("不应发送", "chat", "") == nil {
		t.Fatal("unknown position accepted input, saving, or sending")
	}
	assertLegacyDraftUnchanged(t, s, "room:lobby", "目标频道原有草稿")
	var origin string
	if err := s.db.QueryRow(`SELECT draft FROM preferences WHERE user='alice' AND scope='room:position-source'`).Scan(&origin); err != nil || origin != "原频道尚未发送的私密内容" {
		t.Fatal("moving to safety lost the source draft", origin, err)
	}
	reopenPositionReader(t, s)
	m.frame = 4
	updated, _ = m.Update(clockMsg(time.Now()))
	m = updated.(model)
	if m.positionUnknown || m.input.Value() != "目标频道原有草稿" || m.panelKind == "position-loading" || m.positionVersion != 0 {
		t.Fatal("periodic retry failed to restore the real baseline", m.positionUnknown, m.input.Value(), m.positionVersion)
	}
	assertLegacyDraftUnchanged(t, s, "room:lobby", "目标频道原有草稿")
}

func TestConflictBaselineReadFailureRetainsLocalAndRemoteDrafts(t *testing.T) {
	s, hub := featureFixture(t)
	a := testModel(t, s, hub, "alice")
	b := testModel(t, s, hub, "alice")
	c := testModel(t, s, hub, "alice")
	a.input.SetValue("设备A原稿")
	if err := a.saveView(); err != nil {
		t.Fatal(err)
	}
	b.input.SetValue("设备B私有冲突稿")
	if err := b.saveView(); err != nil || !b.positionConflict {
		t.Fatal("fixture did not retain a real draft conflict", err)
	}
	if err := s.db.reader.Close(); err != nil {
		t.Fatal(err)
	}
	b.resolvePositionConflict(false)
	b.resolvePositionConflict(true)
	if !b.positionConflict || b.input.Value() != "设备B私有冲突稿" || b.positionVersion != 0 {
		t.Fatal("a failed explicit reload adopted an empty draft", b.positionConflict, b.input.Value(), b.positionVersion)
	}
	var remote, backup string
	if err := s.db.QueryRow(`SELECT draft FROM preferences WHERE user='alice' AND scope='room:lobby'`).Scan(&remote); err != nil || remote != "设备A原稿" {
		t.Fatal("a failed explicit keep overwrote the remote draft", remote, err)
	}
	if err := s.db.QueryRow(`SELECT draft FROM draft_conflicts WHERE user='alice' AND scope='room:lobby' AND session_id=?`, b.client.ID).Scan(&backup); err != nil || backup != "设备B私有冲突稿" {
		t.Fatal("failed resolution removed its private backup", backup, err)
	}
	c.lastRead = 1 // A metadata-only collision must also reject a failed reread.
	if err := c.savePositionDraft(); err == nil || c.input.Value() != "" || c.positionVersion != 0 {
		t.Fatal("metadata collision adopted a fabricated empty baseline", err, c.input.Value(), c.positionVersion)
	}
}

func TestInitialLoginReadFailurePausesUntilManualRetry(t *testing.T) {
	s, hub := featureFixture(t)
	insertLegacyPositionDraft(t, s, "room:lobby", "初次登录已有草稿")
	u, err := s.User("alice")
	if err != nil {
		t.Fatal(err)
	}
	m := newModel(s, hub, context.Background(), io.Discard, "xterm-256color", 100, 30, "192.0.2.10", func() {})
	// A corrupt SQLite value makes only the position scan fail, after the
	// independent authentication/user queries have succeeded normally.
	if _, err := s.db.Exec(`UPDATE preferences SET offset='invalid-offset' WHERE user='alice' AND scope='room:lobby'`); err != nil {
		t.Fatal(err)
	}
	m.completeLogin(u, false)
	t.Cleanup(func() { hub.Leave(m.client) })
	if !m.logged || !m.contextLoaded || !m.positionUnknown || m.panelKind != "position-loading" {
		t.Fatal("initial login ignored failure to establish its draft baseline", m.logged, m.contextLoaded, m.positionUnknown, m.panelKind)
	}
	if _, err := s.db.Exec(`UPDATE preferences SET offset=0 WHERE user='alice' AND scope='room:lobby'`); err != nil {
		t.Fatal(err)
	}
	m.positionLoadKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.positionUnknown || m.input.Value() != "初次登录已有草稿" {
		t.Fatal("manual retry did not recover the initial draft")
	}
	assertLegacyDraftUnchanged(t, s, "room:lobby", "初次登录已有草稿")
}
