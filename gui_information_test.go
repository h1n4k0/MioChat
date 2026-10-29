package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestGUIInformationPanelsAreReadOnlyAndPreciselyProjected(t *testing.T) {
	s := testStore(t)
	u := registerUser(t, s, "info-alice")
	registerUser(t, s, "info-bob")
	old := mustSend(t, s, u.Name, "lobby", "", "old synthetic message")
	recent := mustSend(t, s, "info-bob", "", u.Name, "private contents must not enter statistics")
	now := time.Now()
	if _, err := s.db.Exec(`UPDATE messages SET at=? WHERE id=?`, now.Unix()-120, old.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO rate_stats VALUES(?,7)`, now.Unix()/3600); err != nil {
		t.Fatal(err)
	}
	client := &Client{ID: "stats-session", Name: u.Name, AuthEpoch: u.AuthEpoch, Ctx: t.Context()}
	if err := s.RecordSession(client); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SavePositionVersioned(u.Name, "room:lobby", Position{Draft: "保留未发草稿", ReplyTo: old.ID, Anchor: old.ID, Offset: 3}); err != nil {
		t.Fatal(err)
	}
	before, err := s.PositionWithError(u.Name, "room:lobby")
	if err != nil {
		t.Fatal(err)
	}
	var notifications int
	if err := s.db.QueryRow(`SELECT count(*) FROM notifications`).Scan(&notifications); err != nil {
		t.Fatal(err)
	}
	h := newHub()
	h.Started = now.Add(-2 * time.Minute)
	stats, err := guiStatisticsSnapshot(t.Context(), s, h, now)
	if err != nil || stats.Users != 2 || stats.Messages != 2 || stats.ActiveSessions != 1 || stats.MinuteMessages != 1 || stats.HourLimited != 7 || stats.OnlineSessions != 0 || stats.UptimeSeconds != 120 || stats.DatabaseBytes == nil || stats.FreeDiskBytes == nil || stats.Goroutines == 0 || stats.HeapBytes == 0 {
		t.Fatal("incorrect aggregates", stats, err)
	}
	for _, command := range []string{"/stats", "/time", "/art"} {
		r, err := guiExecuteAction(s, h, u, guiScope{room: "lobby"}, old.ID, guiRequest{Body: command, ctx: t.Context(), sessionID: client.ID})
		if err != nil || r.panel == nil || r.panel.Kind != command[1:] || r.scope.key() != "lobby" || r.replyTo != old.ID || r.message != nil || len(r.messages) != 0 || r.replace || r.clearView || r.draft != nil {
			t.Fatal("read-only action mutated chat", command, r, err)
		}
		if command == "/art" && (len(r.panel.Lines) != 1 || r.panel.Lines[0] != campArt) {
			t.Fatal("ASCII spacing lost")
		}
		if command == "/time" {
			value, e := time.ParseInLocation("2006-01-02 15:04:05 MST", r.panel.ServerTime, time.FixedZone("CST", 8*3600))
			if e != nil || time.Since(value) < -time.Second || time.Since(value) > 3*time.Second {
				t.Fatal("not server CST", r.panel.ServerTime, e)
			}
		}
		if !guiImplementedCommand(command) {
			t.Fatal("missing menu entry", command)
		}
		if _, err := guiExecuteAction(s, h, u, guiScope{room: "lobby"}, 0, guiRequest{Body: command + " extra", ctx: t.Context()}); err == nil {
			t.Fatal("unexpected argument accepted", command)
		}
		if _, err := guiExecuteAction(s, h, u, guiScope{room: "unreadable"}, 0, guiRequest{Body: command, ctx: t.Context()}); err == nil {
			t.Fatal("ACL bypass", command)
		}
	}
	after, err := s.PositionWithError(u.Name, "room:lobby")
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("draft/position changed", before, after, err)
	}
	var count, newNotifications int
	if err := s.db.QueryRow(`SELECT count(*) FROM messages`).Scan(&count); err != nil || count != 2 {
		t.Fatal("message write", count, err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM notifications`).Scan(&newNotifications); err != nil || newNotifications != notifications {
		t.Fatal("notification write", newNotifications, err)
	}
	if m, err := s.Message(u.Name, recent.ID); err != nil || m.Body != recent.Body {
		t.Fatal("history altered", m, err)
	}
}

func TestGUIStatisticsReaderBudgetFailsWithoutFabricatedZeros(t *testing.T) {
	s := testStore(t)
	h := newHub()
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if value, err := guiStatisticsSnapshot(canceled, s, h, time.Now()); value != nil || !errors.Is(err, context.Canceled) {
		t.Fatal(value, err)
	}
	s.readDB().SetMaxOpenConns(1)
	conn, err := s.readDB().Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	start := time.Now()
	value, err := guiStatisticsSnapshot(t.Context(), s, h, start)
	if value != nil || !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatal("reader wait was not bounded or returned zeros", value, err, time.Since(start))
	}
}

func TestGUIInformationWireAuthenticationScopeAndReset(t *testing.T) {
	s := testStore(t)
	u := registerUser(t, s, "info-wire")
	h := newHub()
	f := newGUIWireFixture(t, s, h)
	for _, command := range []string{"/stats", "/time", "/art"} {
		f.send(guiRequest{Type: "command", RequestID: "unauth" + command, Room: "lobby", Body: command})
		if !strings.Contains(f.read("error", "unauth"+command).Error, "authentication") {
			t.Fatal("auth gate")
		}
	}
	f.send(guiRequest{Type: "login", RequestID: "login", Username: u.Name, Password: "test-password-123"})
	f.read("auth", "login")
	f.read("state", "")
	for _, command := range []string{"/stats", "/time", "/art"} {
		f.send(guiRequest{Type: "command", RequestID: "stale" + command, Room: "dm:unknown", Body: command})
		f.read("error", "stale"+command)
		f.send(guiRequest{Type: "command", RequestID: "valid" + command, Room: "lobby", Body: command})
		r := f.read("action", "valid"+command)
		if r.Panel == nil || r.Room != "lobby" || r.Panel.Kind != command[1:] || r.Message != nil || r.Replace || r.Draft != nil {
			t.Fatal(r)
		}
	}
	if _, err := s.db.Exec(`UPDATE user_meta SET must_reset=1 WHERE name=?`, u.Name); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"/stats", "/time", "/art"} {
		f.send(guiRequest{Type: "command", RequestID: "reset" + command, Room: "lobby", Body: command})
		if !strings.Contains(f.read("error", "reset"+command).Error, "请先修改临时密码") {
			t.Fatal("reset gate")
		}
	}
}
