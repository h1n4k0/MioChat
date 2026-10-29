package main

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestGUISimpleActionMessageSemanticsAndLimits(t *testing.T) {
	for _, sides := range []int64{2, 6, 1000000} {
		for range 20 {
			body, kind, err := guiSimpleMessage("/roll", fmt.Sprint(sides))
			var value, reported int64
			if _, e := fmt.Sscanf(body, "掷出了 %d（%d 面骰）", &value, &reported); err != nil || e != nil || kind != "action" || reported != sides || value < 1 || value > sides {
				t.Fatal(body, kind, err, e)
			}
		}
	}
	for _, arg := range []string{"1", "1000001", "-1", "99999999999999999999", "2 3"} {
		if _, _, err := guiSimpleMessage("/roll", arg); err == nil {
			t.Fatal("invalid die", arg)
		}
	}
	for range 20 {
		body, kind, err := guiSimpleMessage("/coin", "")
		if err != nil || kind != "action" || (body != "抛硬币：正面" && body != "抛硬币：反面") {
			t.Fatal(body, kind, err)
		}
	}
	if body, kind, err := guiSimpleMessage("/shrug", ""); err != nil || body != "¯\\_(ツ)_/¯" || kind != "chat" {
		t.Fatal(body, kind, err)
	}
	if _, _, err := guiSimpleMessage("/8ball", ""); err == nil {
		t.Fatal("empty question accepted")
	}
	s := testStore(t)
	u := registerUser(t, s, "play-author")
	registerUser(t, s, "play-peer")
	h := newHub()
	h.Attach(s)
	client := &Client{ID: "simple-session", Name: u.Name, AuthEpoch: u.AuthEpoch, Ctx: t.Context()}
	if err := s.RecordSession(client); err != nil {
		t.Fatal(err)
	}
	quote := mustSend(t, s, "play-peer", "", u.Name, "private quote")
	if _, err := s.SavePositionVersioned(u.Name, "dm:play-peer", Position{Draft: "原聊天草稿", ReplyTo: quote.ID, Anchor: quote.ID, Offset: 2}); err != nil {
		t.Fatal(err)
	}
	before, err := s.PositionWithError(u.Name, "dm:play-peer")
	if err != nil {
		t.Fatal(err)
	}
	call := func(body string) (guiActionResult, error) {
		return guiExecuteAction(s, h, u, guiScope{peer: "play-peer"}, quote.ID, guiRequest{Body: body, sessionID: client.ID, ctx: t.Context()})
	}
	for _, command := range []string{"/me 做个伸展", "/roll", "/coin", "/8ball 今天好吗？", "/shrug"} {
		r, err := call(command)
		if err != nil || r.scope.key() != "dm:play-peer" || r.replyTo != 0 || r.message == nil || r.message.Sender != u.Name || r.message.Recipient != "play-peer" || r.message.ReplyTo != quote.ID || r.message.ReplyBody != quote.Body {
			t.Fatal(command, r, err)
		}
		if strings.HasPrefix(command, "/8ball") && !strings.HasPrefix(r.message.Body, "问：今天好吗？ / 八号球：") {
			t.Fatal(r.message)
		}
		name, _ := splitArgument(command)
		if !guiImplementedCommand(name) {
			t.Fatal("missing native action", name)
		}
	}
	for _, command := range []string{"/me", "/me " + strings.Repeat("字", 801), "/8ball " + strings.Repeat("问", 800)} {
		if _, err := call(command); err == nil {
			t.Fatal("invalid expanded message", command[:3])
		}
	}
	after, err := s.PositionWithError(u.Name, "dm:play-peer")
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("action overwrote stored draft/position", before, after, err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM messages`).Scan(&count); err != nil || count != 6 {
		t.Fatal("failed action wrote message", count, err)
	}
	// No action variant can bypass the shared durable-session send transaction.
	if _, err := s.db.Exec(`UPDATE sessions SET revoked=1 WHERE id=?`, client.ID); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"/me nope", "/roll", "/coin", "/8ball nope", "/shrug"} {
		if _, err := call(command); err == nil {
			t.Fatal("revoked session sent", command)
		}
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM messages`).Scan(&count); err != nil || count != 6 {
		t.Fatal("revoked action wrote", count, err)
	}
}

func TestGUISimpleActionWireActorScopeAndBlock(t *testing.T) {
	s := testStore(t)
	u := registerUser(t, s, "play-wire")
	registerUser(t, s, "play-other")
	h := newHub()
	f := newGUIWireFixture(t, s, h)
	f.send(guiRequest{Type: "login", RequestID: "login", Username: u.Name, Password: "test-password-123"})
	f.read("auth", "login")
	f.read("state", "")
	f.send(guiRequest{Type: "command", RequestID: "stale", Room: "dm:play-other", Body: "/coin"})
	f.read("error", "stale")
	f.send(guiRequest{Type: "command", RequestID: "action", Username: "play-other", Room: "lobby", Body: "/me synthetic action"})
	r := f.read("action", "action")
	if r.Message == nil || r.Message.Sender != u.Name || r.Message.Kind != "action" || r.Message.Body != "synthetic action" || r.Room != "lobby" {
		t.Fatal(r)
	}
	f.send(guiRequest{Type: "command", RequestID: "dm", Room: "lobby", Body: "/dm play-other"})
	f.read("action", "dm")
	f.read("state", "")
	if _, err := s.db.Exec(`INSERT INTO relations(owner,target,kind,at) VALUES('play-other','play-wire','block',1)`); err != nil {
		t.Fatal(err)
	}
	f.send(guiRequest{Type: "command", RequestID: "blocked", Room: "dm:play-other", Body: "/shrug"})
	f.read("error", "blocked")
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM messages`).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
}
