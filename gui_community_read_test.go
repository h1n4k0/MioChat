package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func communityReader(t *testing.T, s *Store, name, id string) (User, guiRequest) {
	t.Helper()
	u, err := s.User(name)
	if err != nil {
		t.Fatal(err)
	}
	c := &Client{ID: id, Name: u.Name, AuthEpoch: u.AuthEpoch, Ctx: t.Context()}
	if err = s.RecordSession(c); err != nil {
		t.Fatal(err)
	}
	return u, guiRequest{sessionID: id, ctx: t.Context()}
}
func TestGUICommunityReadProjectionPrivacyAndNoWrites(t *testing.T) {
	s := testStore(t)
	for _, name := range []string{"community-owner", "community-reader", "community-admin"} {
		registerUser(t, s, name)
	}
	if _, err := s.db.Exec(`UPDATE users SET role='admin' WHERE name='community-admin'`); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRoomAccess("community-private", "community-owner", true); err != nil {
		t.Fatal(err)
	}
	if err := s.RoomMember("community-owner", "community-private", "community-reader", "invite"); err != nil {
		t.Fatal(err)
	}
	u, request := communityReader(t, s, "community-reader", "community-read-session")
	request.communityRoom = "community-private"
	post, err := s.Post("community-owner", "community-private", "private post", "中文正文\nsecond line", false)
	if err != nil {
		t.Fatal(err)
	}
	art, err := s.Post("community-owner", "community-private", "private art", "  /\\\n /__\\", true)
	if err != nil {
		t.Fatal(err)
	}
	poll, err := s.Poll("community-owner", "community-private", "Question|One|Two")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Vote(u.Name, poll, 2); err != nil {
		t.Fatal(err)
	}
	event, err := s.NewEvent("community-owner", "community-private", "Event|"+time.Now().Add(24*time.Hour).In(time.FixedZone("CST", 8*3600)).Format("2006-01-02 15:04")+"|description")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RSVP(u.Name, event, true); err != nil {
		t.Fatal(err)
	}
	word, err := s.NewGuess("community-owner", "community-private", "word", "never-leak-secret")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Comment(u.Name, post, "comment"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SavePositionVersioned(u.Name, "room:community-private", Position{Draft: "keep draft", Anchor: 42, Offset: 2}); err != nil {
		t.Fatal(err)
	}
	before, err := s.PositionWithError(u.Name, "room:community-private")
	if err != nil {
		t.Fatal(err)
	}
	counts := func() []int {
		var out []int
		for _, table := range []string{"messages", "notifications", "objects", "object_events", "participation", "audit"} {
			var n int
			if err := s.db.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil {
				t.Fatal(err)
			}
			out = append(out, n)
		}
		return out
	}
	beforeCounts := counts()
	call := func(command, arg string) *guiPanel {
		t.Helper()
		p, e := guiCommunityPanel(s, u, guiScope{peer: "community-owner"}, request, command, arg)
		if e != nil {
			t.Fatal(command, arg, e)
		}
		if p.Scope != "dm:community-owner" || p.Community == nil {
			t.Fatal(p)
		}
		return p
	}
	for command, kind := range map[string]string{"/board": "post", "/gallery": "art", "/events": "event", "/results": "poll"} {
		p := call(command, "")
		expectedActions := 1
		if command == "/events" || command == "/results" {
			expectedActions = 2
			if !strings.HasPrefix(p.Actions[1].Command, "local:community:") {
				t.Fatal(p.Actions)
			}
		}
		if p.Community.Room != "community-private" || len(p.Community.Objects) != 1 || p.Community.Objects[0].Kind != kind || len(p.Actions) != expectedActions {
			t.Fatal(command, p)
		}
	}
	for _, id := range []int64{post, art, poll, event, word} {
		p := call("/object", fmt.Sprint(id))
		data, e := json.Marshal(p)
		if e != nil || len(data) > 1024*1024 || strings.Contains(string(data), "never-leak-secret") {
			t.Fatal("private game secret or oversized frame", e, len(data))
		}
		if p.Community.Detail.Card.ID != id {
			t.Fatal("wrong id")
		}
	}
	if detail := call("/results", fmt.Sprint(poll)).Community.Detail; len(detail.Options) != 2 || detail.Options[1].Votes != 1 {
		t.Fatal(detail)
	}
	if detail := call("/participants", fmt.Sprint(event)).Community.Detail; detail.ParticipantCount != 1 || len(detail.Participants) != 1 || detail.Participants[0] != u.Name {
		t.Fatal(detail)
	}
	daily := call("/daily", "")
	if daily.Community.DailyTopic == "" || len(daily.Actions) != 1 || daily.Actions[0].Command != "local:insert-daily" || daily.Actions[0].Fill {
		t.Fatal("daily sends/replaces draft", daily)
	}
	after, err := s.PositionWithError(u.Name, "room:community-private")
	if err != nil || !reflect.DeepEqual(before, after) || !reflect.DeepEqual(beforeCounts, counts()) {
		t.Fatal("read modified state", before, after, err)
	}
	admin, adminRequest := communityReader(t, s, "community-admin", "community-admin-session")
	if p, e := guiCommunityPanel(s, admin, guiScope{room: "lobby"}, adminRequest, "/object", fmt.Sprint(post)); e == nil || p != nil {
		t.Fatal("admin read private object", p, e)
	}
	if _, err = s.db.Exec(`UPDATE objects SET closed=1 WHERE id=?`, word); err != nil {
		t.Fatal(err)
	}
	if secret := call("/object", fmt.Sprint(word)).Community.Detail.Game.Secret; secret != "never-leak-secret" {
		t.Fatal("closed answer absent", secret)
	}
	if _, err = s.db.Exec(`UPDATE objects SET payload=? WHERE id=?`, strings.Repeat("x", maxCommunityPayloadBytes+1), post); err != nil {
		t.Fatal(err)
	}
	if detail := call("/object", fmt.Sprint(post)).Community.Detail; !detail.Card.Unavailable || detail.Body != "" {
		t.Fatal("oversized raw content exposed", detail)
	}
	for _, cmd := range []string{"/board", "/gallery", "/events", "/results", "/participants", "/daily", "/object", "/commands"} {
		if !guiImplementedCommand(cmd) {
			t.Fatal("missing entry", cmd)
		}
	}
}
func TestGUICommunityParticipantPaginationAndReaderRevocation(t *testing.T) {
	s := testStore(t)
	registerUser(t, s, "page-owner")
	u, request := communityReader(t, s, "page-owner", "community-pages")
	id, err := s.NewObject(u.Name, "lobby", "event", "participants", `{"when":2000000000,"description":"synthetic"}`)
	if err != nil {
		t.Fatal(err)
	}
	// Synthetic fixture accounts need no authentication; reuse only this test's
	// generated credential bytes, without weakening FK checks or auth code.
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := range 205 {
		name := fmt.Sprintf("participant-%03d", i)
		if _, err = tx.Exec(`INSERT INTO users(name,hash,salt,created) SELECT ?,hash,salt,created FROM users WHERE name='page-owner'`, name); err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(`INSERT INTO participation VALUES(?,?,'yes',1)`, id, name); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	cursor := ""
	for page := 0; page < 3; page++ {
		arg := fmt.Sprint(id)
		if cursor != "" {
			arg += " " + cursor
		}
		panel, e := guiCommunityPanel(s, u, guiScope{room: "lobby"}, request, "/participants", arg)
		if e != nil {
			t.Fatal(e)
		}
		d := panel.Community.Detail
		if d.ParticipantCount != 205 || len(d.Participants) > 100 {
			t.Fatal(d)
		}
		for _, name := range d.Participants {
			if seen[name] {
				t.Fatal("duplicate page", name)
			}
			seen[name] = true
		}
		cursor = d.NextParticipant
	}
	if len(seen) != 205 || cursor != "" {
		t.Fatal("participants lost", len(seen), cursor)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	bad := request
	bad.ctx = canceled
	if panel, e := guiCommunityPanel(s, u, guiScope{room: "lobby"}, bad, "/board", ""); panel != nil || !errors.Is(e, context.Canceled) {
		t.Fatal(panel, e)
	}
	s.readDB().SetMaxOpenConns(1)
	conn, err := s.readDB().Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	panel, e := guiCommunityPanel(s, u, guiScope{room: "lobby"}, request, "/board", "")
	if panel != nil || !errors.Is(e, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatal(panel, e, time.Since(start))
	}
	conn.Close()
	if _, err = s.db.Exec(`UPDATE sessions SET revoked=1 WHERE id=?`, request.sessionID); err != nil {
		t.Fatal(err)
	}
	if panel, e := guiCommunityPanel(s, u, guiScope{room: "lobby"}, request, "/object", fmt.Sprint(id)); panel != nil || !errors.Is(e, errCredentialsChanged) {
		t.Fatal("revoked reader accepted", panel, e)
	}
}
func TestGUICommunityWireLastChannelRevocationAndArchivedRead(t *testing.T) {
	s := testStore(t)
	for _, name := range []string{"channel-owner", "channel-reader"} {
		registerUser(t, s, name)
	}
	if err := s.CreateRoomAccess("old-private", "channel-owner", true); err != nil {
		t.Fatal(err)
	}
	if err := s.RoomMember("channel-owner", "old-private", "channel-reader", "invite"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Post("channel-owner", "old-private", "remembered", "private body", false); err != nil {
		t.Fatal(err)
	}
	h := newHub()
	f := newGUIWireFixture(t, s, h)
	f.send(guiRequest{Type: "command", RequestID: "no-auth", Room: "lobby", Body: "/daily"})
	f.read("error", "no-auth")
	f.send(guiRequest{Type: "login", RequestID: "login", Username: "channel-reader", Password: "test-password-123"})
	f.read("auth", "login")
	f.read("state", "")
	f.send(guiRequest{Type: "select_room", RequestID: "private", Room: "old-private"})
	f.read("state", "private")
	f.send(guiRequest{Type: "command", RequestID: "dm", Room: "old-private", Body: "/dm channel-owner"})
	f.read("action", "dm")
	f.read("state", "")
	for _, id := range []string{"before-archive", "after-archive"} {
		if id == "after-archive" {
			if _, err := s.db.Exec(`UPDATE room_meta SET archived=1 WHERE name='old-private'`); err != nil {
				t.Fatal(err)
			}
		}
		f.send(guiRequest{Type: "command", RequestID: id, Room: "dm:channel-owner", Body: "/board"})
		r := f.read("action", id)
		if r.Room != "dm:channel-owner" || r.Panel == nil || r.Panel.Community.Room != "old-private" || len(r.Panel.Community.Objects) != 1 {
			t.Fatal("last channel lost or hidden navigation", r)
		}
	}
	if err := s.RoomMember("channel-owner", "old-private", "channel-reader", "remove"); err != nil {
		t.Fatal(err)
	}
	h.Refresh()
	f.send(guiRequest{Type: "command", RequestID: "revoked-channel", Room: "dm:channel-owner", Body: "/board"})
	r := f.read("error", "revoked-channel")
	if !strings.Contains(r.Error, "无权") {
		t.Fatal("not explicit failclosed", r)
	}
}
func TestGUIDailyPreservesLegacyUTCRotationWithCSTDate(t *testing.T) {
	zone := time.FixedZone("CST", 8*3600)
	before := time.Date(2026, 10, 27, 23, 59, 59, 0, zone)
	midnight := before.Add(time.Second)
	if dailyTopicAt(before) != dailyTopicAt(midnight) {
		t.Fatal("silently changed legacy rotation to CST midnight")
	}
	eight := time.Date(2026, 10, 28, 8, 0, 0, 0, zone)
	if dailyTopicAt(eight) == dailyTopicAt(eight.Add(-time.Second)) {
		t.Fatal("legacy UTC-day boundary lost")
	}
}

func TestGUICommunityArt160ColumnsPreservesSource(t *testing.T) {
	s := testStore(t)
	registerUser(t, s, "wide-art")
	u, request := communityReader(t, s, "wide-art", "wide-art-reader")
	body := "  " + strings.Repeat("-", 156) + "  \n\n" + strings.Repeat("*", 160)
	id, err := s.Post(u.Name, "lobby", "wide synthetic art", body, true)
	if err != nil {
		t.Fatal(err)
	}
	panel, err := guiCommunityPanel(s, u, guiScope{room: "lobby"}, request, "/object", fmt.Sprint(id))
	if err != nil {
		t.Fatal(err)
	}
	if panel.Community.Detail.Body != body {
		t.Fatal("native projection changed ASCII spaces or lines")
	}
}
