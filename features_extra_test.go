package main

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

func TestPermanentTriggersProtectAgainstLegacyPruning(t *testing.T) {
	s, _ := featureFixture(t)
	m := mustSend(t, s, "alice", "lobby", "", "原文")
	if e := s.UpdateMessage("alice", m.ID, "修改", false); e != nil {
		t.Fatal(e)
	}
	for _, query := range []string{`DELETE FROM messages`, `DELETE FROM revisions`, `DELETE FROM audit`, `UPDATE audit SET detail='篡改'`, `UPDATE revisions SET body='篡改'`} {
		if _, e := s.db.Exec(query); e == nil {
			t.Fatalf("permanent records modified: %s", query)
		}
	}
}
func TestRecallClearsPreviewAndOpenReader(t *testing.T) {
	s, h := featureFixture(t)
	a := testModel(t, s, h, "alice")
	b := testModel(t, s, h, "bob")
	source := mustSend(t, s, "alice", "lobby", "", "ORIGINAL_PRIVATE_PREVIEW")
	a.feed = []Message{source}
	b.feed = []Message{source}
	a.rebuild(true)
	b.rebuild(true)
	if e := b.selectReply(source.ID); e != nil {
		t.Fatal(e)
	}
	if e := b.send("回复", "chat", ""); e != nil {
		t.Fatal(e)
	}
	if strings.Contains(b.flash, "ORIGINAL") {
		t.Fatal("sent reply kept automatic preview")
	}
	b.command(fmt.Sprintf("/read %d", source.ID))
	if b.panelTitle == "" {
		t.Fatal("reader not opened")
	}
	if e := s.UpdateMessage("alice", source.ID, "", true); e != nil {
		t.Fatal(e)
	}
	out, _ := b.Update(Event{ChangedID: source.ID})
	b = out.(model)
	if strings.Contains(b.View(), "ORIGINAL_PRIVATE_PREVIEW") {
		t.Fatal("open reader retained recalled content")
	}
}
func TestSearchEscapingDatesAndQuotedQueries(t *testing.T) {
	s, _ := featureFixture(t)
	mustSend(t, s, "alice", "lobby", "", "literal %_ backslash "+string(rune(92)))
	mustSend(t, s, "bob", "lobby", "", "another unrelated message")
	for _, q := range []string{"%", "_", string(rune(92))} {
		matches, e := s.SearchAll("lobby", "alice", "", q)
		if e != nil || len(matches) != 1 {
			t.Fatalf("literal search %q returned %d %v", q, len(matches), e)
		}
	}
	if _, e := s.SearchAll("lobby", "alice", "", string(rune(34))+" OR secretword"); e != nil {
		t.Fatal("quoted FTS query was not escaped", e)
	}
	if _, e := s.SearchAll("lobby", "alice", "", "after:bad-date"); e == nil {
		t.Fatal("bad date accepted")
	}
}

func TestCompositionSurvivesCancelAndCannotLeakToOthers(t *testing.T) {
	s, h := featureFixture(t)
	a := testModel(t, s, h, "alice")
	a.startCompose("post", "我的长帖")
	a.editor.SetValue("中文第一行\n第二行")
	a.composeKey(tea.KeyMsg{Type: tea.KeyEsc})
	if a.composeKind != "" {
		t.Fatal("editor did not close")
	}
	a.startCompose("post", "")
	if a.editor.Value() != "中文第一行\n第二行" {
		t.Fatal("composition draft lost")
	}
	_, body := s.Composition("bob", "lobby", "post")
	if body != "" {
		t.Fatal("composition draft leaked to another account")
	}
}
func TestExpirySilenceReportsAndConfigPermissions(t *testing.T) {
	s, _ := featureFixture(t)
	if e := s.Sanction("admin", "user", "bob", "", "1h", "测试", false); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Authenticate("bob", "test-password-123"); e == nil {
		t.Fatal("active account ban bypassed")
	}
	s.db.Exec(`UPDATE sanctions SET expires=?`, time.Now().Unix()-1)
	if _, e := s.Authenticate("bob", "test-password-123"); e != nil {
		t.Fatal("expired ban still blocks login", e)
	}
	if e := s.Sanction("admin", "mute", "bob", "lobby", "1h", "", false); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Send(Message{Room: "lobby", Sender: "bob", Kind: "chat", Body: "被禁言"}); e == nil {
		t.Fatal("mute bypassed")
	}
	if e := s.Sanction("admin", "mute", "bob", "lobby", "0", "", true); e != nil {
		t.Fatal(e)
	}
	mustSend(t, s, "bob", "lobby", "", "已解除禁言")
	if e := s.SetConfig("alice", "registration", "closed"); e == nil {
		t.Fatal("member changed registration")
	}
	if e := s.SetConfig("admin", "retention", "50000"); e == nil {
		t.Fatal("cleanup re-enabled")
	}
	s.SetConfig("admin", "registration", "closed")
	if _, e := s.Register("newuser", "test-password-123", "test-password-123"); e == nil {
		t.Fatal("closed registration bypassed")
	}
}
func TestSharedCommandRateLimitIncludesMouseActions(t *testing.T) {
	s, h := featureFixture(t)
	m := testModel(t, s, h, "alice")
	for i := 0; i < 8; i++ {
		m.runAction(panelAction{"send", fmt.Sprintf("第%d条", i), false})
	}
	var count int
	s.db.QueryRow(`SELECT count(*) FROM messages`).Scan(&count)
	if count != 6 {
		t.Fatalf("mouse actions bypassed shared command bucket: %d", count)
	}
	if m.rateDrops != 2 {
		t.Fatal("throttle not counted")
	}
	m.flushRates()
	var total int
	s.db.QueryRow(`SELECT count FROM rate_stats`).Scan(&total)
	if total != 2 {
		t.Fatal("rate metric lost")
	}
}
func TestInviteFormAndMenuMouseBounds(t *testing.T) {
	s, h := featureFixture(t)
	s.SetConfig("admin", "registration", "invite")
	m := testModel(t, s, h, "alice")
	m.logged = false
	m.register = true
	m.formInvite = true
	for _, hit := range m.hits() {
		if hit.y < 0 || hit.y >= 24 {
			t.Fatal("invite hit outside supported screen")
		}
	}
	m.logged = true
	m.openMenu(false)
	for i := 0; i < 200; i++ {
		out, _ := m.Update(tea.MouseMsg{Button: tea.MouseButtonWheelDown, Action: tea.MouseActionPress})
		m = out.(model)
	}
	if m.panelOffset > m.panelMaxOffset() {
		t.Fatal("menu wheel overflow")
	}
	if len(m.panelHits()) == 0 {
		t.Fatal("menu can't be clicked after scrolling to bottom")
	}
}

func TestFutureSchemaRefusedWithoutWritingLegacyTables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future.db")
	db, e := sql.Open("sqlite", path)
	if e != nil {
		t.Fatal(e)
	}
	db.Exec(`CREATE TABLE future_state(value TEXT);PRAGMA user_version=99`)
	db.Close()
	if store, e := openStore(path); e == nil {
		store.db.Close()
		t.Fatal("future schema accepted")
	}
	db, _ = sql.Open("sqlite", path)
	defer db.Close()
	var count int
	db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name IN ('users','rooms','messages')`).Scan(&count)
	if count != 0 {
		t.Fatal("refusing older version changed future database")
	}
}
func TestCJKNameMouseHitAcrossWrappedDMHeader(t *testing.T) {
	segment := messageSegment{User: "中文名字很长用于测试换行", NameLine: 1, NameX: 13}
	if !segment.nameAt(2, 0, 36) || !segment.nameAt(1, 34, 36) {
		t.Fatal("CJK username hit missing")
	}
	if segment.nameAt(1, 35, 36) {
		t.Fatal("odd leftover column counted as username")
	}
}
func TestIPLookupPrivateAndGeneratedAvatarStable(t *testing.T) {
	s, _ := featureFixture(t)
	s.RecordLogin("alice", "192.0.2.10", "fixture", true)
	if _, e := s.IPUsers("bob", "192.0.2.10"); e == nil {
		t.Fatal("member looked up IP accounts")
	}
	names, e := s.IPUsers("admin", "192.0.2.10")
	if e != nil || len(names) != 1 || names[0] != "alice" {
		t.Fatal("IP lookup incorrect", e)
	}
	s.Avatar("alice", "ident")
	u, _ := s.User("alice")
	before := avatarFor(u)
	s.db.Close()
	again, e := openStore(s.path)
	if e != nil {
		t.Fatal(e)
	}
	defer again.db.Close()
	restored, _ := again.User("alice")
	if avatarFor(restored) != before || len(strings.Split(before, "\n")) != 5 {
		t.Fatal("generated avatar not stable across reopen")
	}
}
func TestUnreadCounterDoesNotOverflowNarrowToolbar(t *testing.T) {
	s, h := featureFixture(t)
	m := testModel(t, s, h, "owner")
	m.w = 40
	m.noticeCount = 1000000
	if ansi.StringWidth(m.toolbarText()) > 40 {
		t.Fatal("narrow toolbar overflow")
	}
}
func TestSubscriptionAndChannelMuteHaveSeparateEffects(t *testing.T) {
	s, _ := featureFixture(t)
	mustSend(t, s, "alice", "dev", "", "第一条")
	if s.UnreadIn("bob", "dev", "") != 0 {
		t.Fatal("unsubscribed room advertises unread")
	}
	s.Preference("bob", "room:dev", "subscribed", true)
	if s.UnreadIn("bob", "dev", "") != 1 {
		t.Fatal("subscription does not expose unread")
	}
	s.Preference("bob", "room:dev", "muted", true)
	mustSend(t, s, "alice", "dev", "", "@bob 静音测试")
	items, _ := s.Notifications("bob", false)
	if len(items) != 0 {
		t.Fatal("muted room emitted mention notification")
	}
	if s.UnreadIn("bob", "dev", "") != 2 {
		t.Fatal("muting deleted unread state")
	}
}
