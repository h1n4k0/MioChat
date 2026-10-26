package main

import (
	"bytes"
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

//go:embed testdata/compatibility.sql
var legacySchema string

func featureFixture(t *testing.T) (*Store, *Hub) {
	t.Helper()
	s := testStore(t)
	for _, name := range []string{"owner", "admin", "alice", "bob", "eve", "中文用户"} {
		registerUser(t, s, name)
	}
	if _, e := s.db.Exec(`UPDATE users SET role=CASE name WHEN 'owner' THEN 'owner' WHEN 'admin' THEN 'admin' ELSE role END`); e != nil {
		t.Fatal(e)
	}
	h := newHub()
	h.Attach(s)
	return s, h
}
func testModel(t *testing.T, s *Store, h *Hub, name string) model {
	t.Helper()
	m := newModel(s, h, context.Background(), io.Discard, "xterm-256color", 100, 30, "192.0.2.10", func() {})
	u, e := s.User(name)
	if e != nil {
		t.Fatal(e)
	}
	m.user = u
	m.logged = true
	m.client.Name = name
	m.settings = s.Settings(name)
	if e = h.Join(m.client); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { h.Leave(m.client) })
	if e = m.move("lobby", ""); e != nil {
		t.Fatal(e)
	}
	return m
}
func mustSend(t *testing.T, s *Store, sender, room, recipient, body string) Message {
	t.Helper()
	m, e := s.Send(Message{Sender: sender, Room: room, Recipient: recipient, Body: body, Kind: "chat"})
	if e != nil {
		t.Fatal(e)
	}
	return m
}

func TestSchemaMigrationIsLosslessRepeatableAndBackedUp(t *testing.T) {
	state := t.TempDir()
	path := filepath.Join(state, "miochat.db")
	db, e := sql.Open("sqlite", path)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = db.Exec(legacySchema); e != nil {
		t.Fatal(e)
	}
	salt := bytes.Repeat([]byte{0x42}, 16)
	hash := passwordHash("legacy-password-123", salt)
	for _, pair := range [][2]string{{"legacyuser", "member"}, {"legacyadmin", "admin"}} {
		if _, e = db.Exec(`INSERT INTO users(name,hash,salt,bio,theme,role,created) VALUES(?,?,?,'原有简介','sakura',?,1700000000)`, pair[0], hash, salt, pair[1]); e != nil {
			t.Fatal(e)
		}
	}
	if _, e = db.Exec(`INSERT INTO messages VALUES(12345,'lobby','legacyuser','','原有中文聊天','chat',1700000000),(12346,'','legacyuser','legacyadmin','原有私信','chat',1700000001);INSERT INTO pins VALUES('lobby',12345)`); e != nil {
		t.Fatal(e)
	}
	db.Close()
	if _, e = hostSigner(filepath.Join(state, "host_ed25519")); e != nil {
		t.Fatal(e)
	}
	before, _ := digestFile(filepath.Join(state, "host_ed25519"))
	s, e := openStore(path)
	if e != nil {
		t.Fatal(e)
	}
	u, e := s.Authenticate("legacyuser", "legacy-password-123")
	if e != nil || u.Bio != "原有简介" || u.Theme != "sakura" || u.UID < 1 {
		t.Fatalf("legacy user changed: %#v %v", u, e)
	}
	uid := u.UID
	admin, _ := s.User("legacyadmin")
	if admin.Role != "admin" {
		t.Fatal("existing admin demoted")
	}
	var actualHash, actualSalt []byte
	s.db.QueryRow(`SELECT hash,salt FROM users WHERE name='legacyuser'`).Scan(&actualHash, &actualSalt)
	if !bytes.Equal(hash, actualHash) || !bytes.Equal(salt, actualSalt) {
		t.Fatal("legacy password bytes changed")
	}
	messages, e := s.History("lobby", "legacyuser", "", 100)
	if e != nil || len(messages) != 1 || messages[0].ID != 12345 || messages[0].Body != "原有中文聊天" {
		t.Fatal("legacy messages changed", messages, e)
	}
	dm, _ := s.History("", "legacyuser", "legacyadmin", 100)
	if len(dm) != 1 || dm[0].ID != 12346 {
		t.Fatal("legacy DM changed")
	}
	newMessage := mustSend(t, s, "legacyuser", "lobby", "", "升级后消息")
	if newMessage.ID != 12347 {
		t.Fatal("message sequence changed")
	}
	s.db.Close()
	files, _ := filepath.Glob(path + ".pre-v2-*.bak")
	if len(files) != 1 {
		t.Fatalf("expected one pre-migration snapshot, got %v", files)
	}
	snapshot, _ := sql.Open("sqlite", files[0])
	defer snapshot.Close()
	var n int
	if e = snapshot.QueryRow(`SELECT count(*) FROM messages`).Scan(&n); e != nil || n != 2 {
		t.Fatal("backup not a complete legacy snapshot", n, e)
	}
	s, e = openStore(path)
	if e != nil {
		t.Fatal(e)
	}
	defer s.db.Close()
	again, _ := s.User("legacyuser")
	if again.UID != uid {
		t.Fatal("UID changed after reopen")
	}
	files, _ = filepath.Glob(path + ".pre-v2-*.bak")
	if len(files) != 1 {
		t.Fatal("migration repeated")
	}
	after, _ := digestFile(filepath.Join(state, "host_ed25519"))
	if before != after {
		t.Fatal("host key changed")
	}
}
func TestPermanentRetentionBeyondFiftyThousand(t *testing.T) {
	s := testStore(t)
	registerUser(t, s, "alice")
	old := mustSend(t, s, "alice", "lobby", "", "不可清理的原文")
	if e := s.UpdateMessage("alice", old.ID, "", true); e != nil {
		t.Fatal(e)
	}
	if _, e := s.db.Exec(`WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<50200) INSERT INTO messages(room,sender,recipient,body,kind,at) SELECT 'lobby','alice','','永久记录'||x,'chat',1700000000 FROM n`); e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 128; i++ {
		if _, e := s.Add(Message{Room: "lobby", Sender: "alice", Body: "继续写入", Kind: "chat"}); e != nil {
			t.Fatal(e)
		}
	}
	var count int
	s.db.QueryRow(`SELECT count(*) FROM messages`).Scan(&count)
	if count != 50329 {
		t.Fatalf("records pruned: %d", count)
	}
	v, e := s.Message("alice", old.ID)
	if e != nil || v.Recalled == 0 {
		t.Fatal("old message lost")
	}
	versions, e := s.Revisions("alice", old.ID)
	if e != nil || len(versions) != 1 || versions[0].Body != "不可清理的原文" {
		t.Fatal("old revision lost")
	}
	var audits int
	s.db.QueryRow(`SELECT count(*) FROM audit WHERE action='message.recall'`).Scan(&audits)
	if audits != 1 {
		t.Fatal("audit lost")
	}
}
func TestPrivateChannelAndDMIsolationAcrossFeatures(t *testing.T) {
	s, h := featureFixture(t)
	if e := s.CreateRoomAccess("secret", "alice", true); e != nil {
		t.Fatal(e)
	}
	if e := s.RoomMember("alice", "secret", "bob", "invite"); e != nil {
		t.Fatal(e)
	}
	private := mustSend(t, s, "alice", "secret", "", "私密内容 @eve @bob")
	dm := mustSend(t, s, "alice", "", "bob", "私信秘密")
	for _, outsider := range []string{"eve", "admin"} {
		if _, e := s.Message(outsider, private.ID); e == nil {
			t.Fatal("private message leaked")
		}
		if _, e := s.History("secret", outsider, "", 100); e == nil {
			t.Fatal("private history leaked")
		}
		if _, e := s.SearchAll("secret", outsider, "", "私密"); e == nil {
			t.Fatal("private search leaked")
		}
		if e := s.Bookmark(outsider, private.ID, false); e == nil {
			t.Fatal("private bookmark leaked")
		}
		if e := s.React(outsider, private.ID, "like"); e == nil {
			t.Fatal("private reaction allowed")
		}
		if e := s.Report(outsider, private.ID, "test"); e == nil {
			t.Fatal("private report bypass")
		}
		if _, e := s.Message(outsider, dm.ID); e == nil {
			t.Fatal("DM leaked")
		}
	}
	if _, e := s.Send(Message{Sender: "alice", Room: "lobby", Body: "不能跨区引用", Kind: "chat", ReplyTo: private.ID}); e == nil {
		t.Fatal("cross-context reply leaked")
	}
	events, _ := s.Notifications("eve", false)
	for _, v := range events {
		if v.Ref == private.ID {
			t.Fatal("private mention reached outsider")
		}
	}
	if e := s.Bookmark("bob", private.ID, false); e != nil {
		t.Fatal(e)
	}
	client := &Client{Name: "bob", ID: "bob-stale", IP: "192.0.2.2", Events: make(chan Event, 64), Close: func() {}}
	if e := h.Join(client); e != nil {
		t.Fatal(e)
	}
	defer h.Leave(client)
	h.Move(client, "secret", "")
	for len(client.Events) > 0 {
		<-client.Events
	}
	if e := s.RoomMember("alice", "secret", "bob", "remove"); e != nil {
		t.Fatal(e)
	}
	msg := mustSend(t, s, "alice", "secret", "", "撤销权限后不再送达")
	h.Broadcast(msg)
	if len(client.Events) != 0 {
		t.Fatal("revoked membership still receives live content")
	}
	bookmarks, _ := s.Bookmarks("bob")
	if len(bookmarks) != 0 {
		t.Fatal("revoked membership retained bookmark access")
	}
}
func TestReplyEditRecallAuditAndSearch(t *testing.T) {
	s, _ := featureFixture(t)
	msg := mustSend(t, s, "alice", "lobby", "", "原文 secretword")
	reply, e := s.Send(Message{Sender: "bob", Room: "lobby", Body: "回复", Kind: "chat", ReplyTo: msg.ID})
	if e != nil || reply.ReplyBody != "原文 secretword" {
		t.Fatal("reply broken", reply, e)
	}
	if e = s.UpdateMessage("bob", msg.ID, "篡改", false); e == nil {
		t.Fatal("edited someone else's message")
	}
	if e = s.UpdateMessage("alice", msg.ID, "编辑后的内容", false); e != nil {
		t.Fatal(e)
	}
	if e = s.UpdateMessage("admin", msg.ID, "", true); e != nil {
		t.Fatal(e)
	}
	refreshed, e := s.Message("bob", reply.ID)
	if e != nil || refreshed.ReplyBody != "[消息已撤回]" {
		t.Fatal("recall leaked through quote")
	}
	found, e := s.SearchAll("lobby", "bob", "", "secretword")
	if e != nil || len(found) != 0 {
		t.Fatal("recall leaked through search", e)
	}
	if _, e = s.Revisions("eve", msg.ID); e == nil {
		t.Fatal("ordinary user sees recall originals")
	}
	history, e := s.Revisions("admin", msg.ID)
	if e != nil || len(history) != 2 || history[0].Body != "原文 secretword" {
		t.Fatal("audit revisions lost", e)
	}
	dm := mustSend(t, s, "alice", "", "bob", "DM原文")
	s.UpdateMessage("alice", dm.ID, "", true)
	if _, e = s.Revisions("admin", dm.ID); e == nil {
		t.Fatal("global admin implicitly browsed unreported DM")
	}
	if e = s.Report("bob", dm.ID, "举报我参与的这一条"); e != nil {
		t.Fatal(e)
	}
	var reportID int64
	s.db.QueryRow(`SELECT max(id) FROM reports`).Scan(&reportID)
	if _, e = s.ReportMessage("admin", reportID); e != nil {
		t.Fatal("admin can't review explicitly reported message", e)
	}
}
func TestMentionsPokesBlockingAndPreferences(t *testing.T) {
	s, _ := featureFixture(t)
	msg := mustSend(t, s, "alice", "lobby", "", "@bob@中文用户 欢迎！")
	for _, name := range []string{"bob", "中文用户"} {
		n, _ := s.Notifications(name, false)
		found := false
		for _, v := range n {
			if v.Ref == msg.ID && v.Kind == "mention" {
				found = true
			}
		}
		if !found {
			t.Fatalf("mention missing for %s", name)
		}
	}
	if e := s.Poke("alice", "bob"); e != nil {
		t.Fatal(e)
	}
	if e := s.Poke("alice", "bob"); e == nil {
		t.Fatal("poke cooldown bypassed")
	}
	if e := s.Setting("bob", "dnd", "on"); e != nil {
		t.Fatal(e)
	}
	if e := s.Poke("eve", "bob"); e == nil {
		t.Fatal("DND ignored")
	}
	s.Setting("bob", "dnd", "off")
	s.Relation("bob", "alice", "block", "", false)
	if _, e := s.Send(Message{Sender: "alice", Recipient: "bob", Body: "越过屏蔽", Kind: "chat"}); e == nil {
		t.Fatal("blocked DM accepted")
	}
	if e := s.Poke("alice", "bob"); e == nil {
		t.Fatal("blocked poke accepted")
	}
	if !s.Blocked("bob", "alice") {
		t.Fatal("block not persisted")
	}
	p := Position{Draft: "中文草稿", ReplyTo: msg.ID, Offset: 3, LastRead: msg.ID, Anchor: msg.ID}
	if e := s.SavePosition("bob", "room:lobby", p); e != nil {
		t.Fatal(e)
	}
	read := s.Position("bob", "room:lobby")
	if read.Draft != p.Draft || read.ReplyTo != p.ReplyTo || read.Offset != 3 {
		t.Fatal("position/draft lost")
	}
}
func TestRolesIPBansSessionRevocationAndTemporaryPassword(t *testing.T) {
	s, h := featureFixture(t)
	s.RecordLogin("alice", "203.0.113.7", "test-client", true)
	if _, e := s.LoginHistory("eve", "alice"); e == nil {
		t.Fatal("IP disclosure to member")
	}
	rows, e := s.LoginHistory("admin", "alice")
	if e != nil || len(rows) != 1 || rows[0].IP != "203.0.113.7" {
		t.Fatal("IP view broken")
	}
	var closed atomic.Int32
	c := &Client{Name: "alice", ID: "alice-session", IP: "203.0.113.7", Events: make(chan Event, 64), Close: func() { closed.Add(1) }}
	if e = h.Join(c); e != nil {
		t.Fatal(e)
	}
	defer h.Leave(c)
	if e = s.RevokeSession("eve", c.ID); e == nil {
		t.Fatal("session ownership bypass")
	}
	if e = s.Sanction("eve", "ip", "203.0.113.0/24", "", "1d", "test", false); e == nil {
		t.Fatal("member IP ban allowed")
	}
	if e = s.Sanction("admin", "ip", "203.0.113.0/24", "", "1h", "test", false); e != nil {
		t.Fatal(e)
	}
	if !s.IPBanned("203.0.113.7") || s.IPBanned("192.0.2.2") {
		t.Fatal("CIDR ban incorrect")
	}
	h.Enforce()
	deadline := time.Now().Add(time.Second)
	for closed.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if closed.Load() != 1 {
		t.Fatal("live IP ban did not close connection")
	}
	if e = s.Sanction("admin", "ip", "203.0.113.0/24", "", "0", "", true); e != nil {
		t.Fatal(e)
	}
	if s.IPBanned("203.0.113.7") {
		t.Fatal("unban failed")
	}
	if e = s.Sanction("admin", "user", "owner", "", "1d", "", false); e == nil {
		t.Fatal("owner protection bypassed")
	}
	if e = s.SetRole("admin", "alice", "admin"); e == nil {
		t.Fatal("non-owner assigned role")
	}
	if e = s.SetRole("owner", "eve", "moderator"); e != nil {
		t.Fatal(e)
	}
	password, e := s.ResetPassword("admin", "bob")
	if e != nil {
		t.Fatal(e)
	}
	u, e := s.Authenticate("bob", password)
	if e != nil || !u.MustReset {
		t.Fatal("reset flow broken")
	}
	if _, e = s.Send(Message{Sender: "bob", Room: "lobby", Kind: "chat", Body: "跳过改密"}); e == nil {
		t.Fatal("temporary password bypass")
	}
	if e = s.ChangePassword("bob", password, "changed-password-12345"); e != nil {
		t.Fatal(e)
	}
	u, _ = s.User("bob")
	if u.MustReset {
		t.Fatal("reset flag stuck")
	}
}
func TestAtomicInvitesFriendshipAndPollVotes(t *testing.T) {
	s, _ := featureFixture(t)
	if e := s.SetConfig("admin", "registration", "invite"); e != nil {
		t.Fatal(e)
	}
	code, e := s.InviteCode("admin", "1h", 1)
	if e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	var success atomic.Int32
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, e := s.RegisterInvite(fmt.Sprintf("invited%d", i), "test-password-123", "test-password-123", code, "192.0.2.50", "test"); e == nil {
				success.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if success.Load() != 1 {
		t.Fatalf("invite race: %d accounts", success.Load())
	}
	if e = s.Relation("alice", "bob", "request", "", false); e != nil {
		t.Fatal(e)
	}
	if e = s.AcceptFriend("bob", "alice", true); e != nil {
		t.Fatal(e)
	}
	if len(s.Relations("alice", "friend")) != 1 || len(s.Relations("bob", "friend")) != 1 {
		t.Fatal("friendship not mutual")
	}
	if e = s.Unfriend("alice", "bob"); e != nil {
		t.Fatal(e)
	}
	if len(s.Relations("bob", "friend")) != 0 {
		t.Fatal("unfriend not mutual")
	}
	id, e := s.Poll("alice", "lobby", "午餐|面条|米饭")
	if e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if e := s.Vote("bob", id, i%2+1); e != nil {
				t.Error(e)
			}
		}(i)
	}
	wg.Wait()
	var n int
	s.db.QueryRow(`SELECT count(*) FROM participation WHERE object_id=?`, id).Scan(&n)
	if n != 1 {
		t.Fatal("duplicate vote")
	}
	if e = s.CloseObject("alice", id); e != nil {
		t.Fatal(e)
	}
	if e = s.Vote("bob", id, 1); e == nil {
		t.Fatal("closed poll accepted vote")
	}
}
func TestRoomModesReadOnlySlowAndOwnerTransfer(t *testing.T) {
	s, _ := featureFixture(t)
	if e := s.CreateRoom("my-room", "alice"); e != nil {
		t.Fatal(e)
	}
	if e := s.RoomSetting("bob", "my-room", "readonly", "on"); e == nil {
		t.Fatal("unauthorized room settings")
	}
	s.RoomSetting("alice", "my-room", "readonly", "on")
	if _, e := s.Send(Message{Sender: "bob", Room: "my-room", Body: "只读绕过", Kind: "chat"}); e == nil {
		t.Fatal("read-only bypass")
	}
	s.RoomSetting("alice", "my-room", "readonly", "off")
	s.RoomSetting("alice", "my-room", "slow", "30")
	mustSend(t, s, "bob", "my-room", "", "第一条")
	if _, e := s.Send(Message{Sender: "bob", Room: "my-room", Body: "第二条", Kind: "chat"}); e == nil {
		t.Fatal("slow mode bypass")
	}
	if e := s.RoomSetting("alice", "my-room", "transfer", "bob"); e != nil {
		t.Fatal(e)
	}
	r, _ := s.RoomInfo("my-room")
	if r.Owner != "bob" {
		t.Fatal("transfer failed")
	}
	if e := s.RoomSetting("bob", "my-room", "archive", "on"); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Send(Message{Sender: "bob", Room: "my-room", Body: "归档后发言", Kind: "chat"}); e == nil {
		t.Fatal("archive write bypass")
	}
}
func TestCommunityGamesEventsPostsAndAsciiSafety(t *testing.T) {
	s, _ := featureFixture(t)
	id, e := s.Challenge("alice", "bob", "arcade")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.TicMove("alice", id, 1, false); e == nil {
		t.Fatal("unaccepted game allowed move")
	}
	if _, e = s.TicMove("bob", id, 0, true); e != nil {
		t.Fatal(e)
	}
	for _, move := range []struct {
		user string
		cell int
	}{{"alice", 1}, {"bob", 4}, {"alice", 2}, {"bob", 5}, {"alice", 3}} {
		g, e := s.TicMove(move.user, id, move.cell, false)
		if e != nil {
			t.Fatal(e)
		}
		if move.cell == 3 && g.Winner != "alice" {
			t.Fatal("winner not detected")
		}
	}
	if _, e = s.TicMove("bob", id, 6, false); e == nil {
		t.Fatal("finished game mutated")
	}
	guess, e := s.NewGuess("alice", "arcade", "guess", "10")
	if e != nil {
		t.Fatal(e)
	}
	public, e := s.Object("bob", guess)
	if e != nil || strings.Contains(public.Payload, "secret") {
		t.Fatal("game secret leaked")
	}
	var raw string
	s.db.QueryRow(`SELECT payload FROM objects WHERE id=?`, guess).Scan(&raw)
	var game GuessGame
	decodeJSON(raw, &game)
	if _, e = s.Guess("bob", "arcade", "guess", game.Secret); e != nil {
		t.Fatal(e)
	}
	post, e := s.Post("alice", "lobby", "长帖", "中文第一行\n第二行\x1b]52;c;ZXZpbA==\a", false)
	if e != nil {
		t.Fatal(e)
	}
	p, _ := s.Object("bob", post)
	if strings.Contains(p.Payload, "ZXZpbA") {
		t.Fatal("post terminal injection")
	}
	if _, e = s.Post("alice", "lobby", "画板", "不是ASCII", true); e == nil {
		t.Fatal("non-ASCII art accepted")
	}
	event, e := s.NewEvent("alice", "lobby", "周末聚会|2099-01-01 20:00|大家来聊天")
	if e != nil {
		t.Fatal(e)
	}
	if e = s.RSVP("bob", event, true); e != nil {
		t.Fatal(e)
	}
	names, _ := s.Participants("alice", event)
	if len(names) != 1 || names[0] != "bob" {
		t.Fatal("event RSVP failed")
	}
	if len(s.BadgeNames("bob")) < 2 {
		t.Fatal("participation badge missing")
	}
}
func TestBackupRestorePreservesNewerDataInRescue(t *testing.T) {
	state := t.TempDir()
	s, e := openStore(filepath.Join(state, "miochat.db"))
	if e != nil {
		t.Fatal(e)
	}
	registerUser(t, s, "alice")
	hostSigner(filepath.Join(state, "host_ed25519"))
	mustSend(t, s, "alice", "lobby", "", "备份前")
	backup := filepath.Join(t.TempDir(), "snapshot")
	if _, e = MakeSnapshot(state, backup); e != nil {
		t.Fatal(e)
	}
	mustSend(t, s, "alice", "lobby", "", "备份后新增，恢复前也必须保留")
	s.db.Close()
	release, e := lockState(state)
	if e != nil {
		t.Fatal(e)
	}
	if e = RestoreSnapshot(state, backup); e == nil {
		t.Fatal("restored into locked state")
	}
	release()
	if e = RestoreSnapshot(state, backup); e != nil {
		t.Fatal(e)
	}
	s, e = openStore(filepath.Join(state, "miochat.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer s.db.Close()
	messages, _ := s.History("lobby", "alice", "", 100)
	if len(messages) != 1 {
		t.Fatal("restore failed")
	}
	rescues, _ := filepath.Glob(filepath.Join(state, "restore-rescue-*"))
	if len(rescues) != 1 {
		t.Fatal("missing rescue of newer state")
	}
	db, _ := sql.Open("sqlite", filepath.Join(rescues[0], "miochat.db"))
	defer db.Close()
	var count int
	db.QueryRow(`SELECT count(*) FROM messages`).Scan(&count)
	if count != 2 {
		t.Fatal("newer messages lost during restore")
	}
	file := filepath.Join(backup, "manifest.json")
	data, _ := os.ReadFile(file)
	data = bytes.ReplaceAll(data, []byte(`"DatabaseSHA256": "`), []byte(`"DatabaseSHA256": "bad`))
	os.WriteFile(file, data, 0600)
	if _, e = ValidateSnapshot(backup); e == nil {
		t.Fatal("corrupt snapshot accepted")
	}
}
func TestMouseRightClickMentionReplyDoubleClickAndDraft(t *testing.T) {
	s, h := featureFixture(t)
	m := testModel(t, s, h, "alice")
	v := mustSend(t, s, "bob", "lobby", "", "被回复的中文消息")
	m.feed = []Message{v}
	m.rebuild(true)
	m.input.SetValue("你好世界")
	m.input.SetCursor(2)
	seg := m.segments[0]
	side, _, _ := m.dimensions()
	nameX := side + seg.NameX
	nameY := 5 + seg.NameLine - m.view.YOffset
	out, _ := m.Update(tea.MouseMsg{X: nameX, Y: nameY, Button: tea.MouseButtonRight, Action: tea.MouseActionPress})
	m = out.(model)
	if m.input.Value() != "你好 @bob 世界" {
		t.Fatalf("mention not at rune cursor: %q", m.input.Value())
	}
	out, _ = m.Update(tea.MouseMsg{X: nameX + seg.NameWidth + 3, Y: nameY, Button: tea.MouseButtonRight, Action: tea.MouseActionPress})
	m = out.(model)
	if m.replyID != v.ID {
		t.Fatal("right-click did not select correct message")
	}
	m.input.SetValue("待发送草稿")
	m.saveView()
	if e := m.move("dev", ""); e != nil {
		t.Fatal(e)
	}
	if e := m.move("lobby", ""); e != nil {
		t.Fatal(e)
	}
	if m.input.Value() != "待发送草稿" || m.replyID != v.ID {
		t.Fatal("draft/reply lost on room switch")
	}
	m.feed = []Message{v}
	m.rebuild(true)
	out, _ = m.Update(tea.MouseMsg{X: nameX, Y: nameY, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	m = out.(model)
	out, _ = m.Update(tea.MouseMsg{X: nameX, Y: nameY, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	m = out.(model)
	notifications, _ := s.Notifications("bob", false)
	found := false
	for _, n := range notifications {
		found = found || n.Kind == "poke"
	}
	if !found {
		t.Fatal("double click did not poke")
	}
	for _, width := range []int{40, 60, 80, 100} {
		for _, theme := range []string{"aurora", "light", "contrast"} {
			m.user.Theme = theme
			out, _ = m.Update(tea.WindowSizeMsg{Width: width, Height: 24})
			m = out.(model)
			for _, line := range strings.Split(m.View(), "\n") {
				if ansi.StringWidth(line) > width {
					t.Fatalf("layout overflow %d theme %s: %q", width, theme, ansi.Strip(line))
				}
			}
		}
	}
}
func TestCommandCatalogRoutesToRealHandlers(t *testing.T) {
	s, h := featureFixture(t)
	m := testModel(t, s, h, "owner")
	for _, cmd := range commandCatalog {
		m.limiter = bucket{}
		if _, ok := map[string]bool{"/quit": true, "/logout": true}[cmd.Name]; ok {
			continue
		}
		line := cmd.Name
		if cmd.Args != "" {
			continue
		}
		before := len(m.feed)
		m.command(line)
		if len(m.feed) > before && strings.Contains(m.feed[len(m.feed)-1].Body, "未知命令") {
			t.Fatalf("catalog has placeholder %s", cmd.Name)
		}
		m.closePanel()
	}
}
