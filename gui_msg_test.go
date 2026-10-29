package main

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestGUIOneOffMessageKeepsScopeDraftAndPrivateProjection(t *testing.T) {
	s := testStore(t)
	for _, name := range []string{"one-alice", "one-bob", "one-other"} {
		registerUser(t, s, name)
	}
	if _, err := s.SavePositionVersioned("one-alice", "room:lobby", Position{Draft: "keep public draft\nsecond line", Anchor: 42, Offset: 3}); err != nil {
		t.Fatal(err)
	}
	before, err := s.PositionWithError("one-alice", "room:lobby")
	if err != nil {
		t.Fatal(err)
	}
	h := newHub()
	f := newGUIWireFixture(t, s, h)
	f.send(guiRequest{Type: "login", RequestID: "login", Username: "one-alice", Password: "test-password-123"})
	f.read("auth", "login")
	f.read("state", "")
	observer := newGUIWireFixture(t, s, h)
	observer.send(guiRequest{Type: "login", RequestID: "login-other", Username: "one-other", Password: "test-password-123"})
	observer.read("auth", "login-other")
	observer.read("state", "")
	f.send(guiRequest{Type: "command", RequestID: "one-off", Room: "lobby", Username: "one-other", Body: "/msg one-bob only the recipient may read"})
	r := f.read("action", "one-off")
	if r.Room != "lobby" || r.ReplyTo != 0 || r.Replace || r.Draft != nil || r.Message == nil || r.Message.Sender != "one-alice" || r.Message.Recipient != "one-bob" || r.Message.Room != "dm:one-bob" {
		t.Fatal("one-off changed scope or actor", r)
	}
	if _, err = s.Message("one-other", r.Message.ID); err == nil {
		t.Fatal("unrelated account read private message")
	}
	after, err := s.PositionWithError("one-alice", "room:lobby")
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("one-off changed saved draft/CAS/position", before, after, err)
	}
	f.send(guiRequest{Type: "sync", RequestID: "still-lobby"})
	if state := f.read("state", "still-lobby"); state.Room != "lobby" || len(state.Messages) != 0 {
		t.Fatal("DM appeared in public state", state)
	}
	observer.send(guiRequest{Type: "command", RequestID: "barrier", Room: "lobby", Body: "/history"})
	deadline := time.After(4 * time.Second)
	for {
		select {
		case event, ok := <-observer.responses:
			if !ok {
				t.Fatal("observer ended before barrier")
			}
			if event.Message != nil || len(event.Messages) != 0 {
				t.Fatal("one-off leaked to public observer", event)
			}
			if event.Type == "action" && event.RequestID == "barrier" {
				return
			}
		case <-deadline:
			t.Fatal("observer barrier missing")
		}
	}
}

func TestGUIOneOffMessageArgumentsAndReplyBoundaries(t *testing.T) {
	s := testStore(t)
	u := registerUser(t, s, "msg-alice")
	for _, name := range []string{"msg-bob", "msg-carol"} {
		registerUser(t, s, name)
	}
	c := &Client{ID: "one-off-session", Name: u.Name, AuthEpoch: u.AuthEpoch, Ctx: t.Context()}
	if err := s.RecordSession(c); err != nil {
		t.Fatal(err)
	}
	h := newHub()
	h.Attach(s)
	call := func(body string, scope guiScope, reply int64) (guiActionResult, error) {
		return guiExecuteAction(s, h, u, scope, reply, guiRequest{Body: body, Username: "msg-carol", sessionID: c.ID, ctx: t.Context()})
	}
	for _, body := range []string{"/msg", "/msg msg-bob", "/msg msg-bob    ", "/msg missing value", "/msg msg-alice self", "/msg msg-bob " + strings.Repeat("字", 801)} {
		if _, err := call(body, guiScope{room: "lobby"}, 0); err == nil {
			t.Fatal("invalid one-off accepted", body)
		}
	}
	quote := mustSend(t, s, "msg-bob", "", u.Name, "private quote")
	active := guiScope{peer: "msg-bob"}
	if result, err := call("/msg msg-carol cross quote", active, quote.ID); err == nil || result.replyTo != quote.ID || result.scope != active {
		t.Fatal("cross-DM quote changed reply/scope", result, err)
	}
	result, err := call("/msg msg-bob same-pair reply", active, quote.ID)
	if err != nil || result.scope != active || result.replyTo != 0 || result.message == nil || result.message.ReplyTo != quote.ID || result.message.ReplyBody != quote.Body {
		t.Fatal(result, err)
	}
	result, err = call("/msg msg-carol another pair", active, 0)
	if err != nil || result.scope != active || result.message == nil || result.message.Recipient != "msg-carol" {
		t.Fatal("one-off switched DM", result, err)
	}
	if _, err = s.db.Exec(`INSERT INTO relations(owner,target,kind,at) VALUES('msg-bob','msg-alice','block',1)`); err != nil {
		t.Fatal(err)
	}
	if _, err = call("/msg msg-bob blocked", active, 0); err == nil {
		t.Fatal("recipient block ignored")
	}
	if !guiImplementedCommand("/msg") {
		t.Fatal("missing native menu entry")
	}
}

func TestNativeMessageTransactionRechecksAuthorityAfterWait(t *testing.T) {
	for _, kind := range []string{"revoked", "reset", "block", "epoch", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			s := testStore(t)
			u := registerUser(t, s, "tx-sender")
			registerUser(t, s, "tx-recipient")
			c := &Client{ID: "queued-send", Name: u.Name, AuthEpoch: u.AuthEpoch, Ctx: t.Context()}
			if err := s.RecordSession(c); err != nil {
				t.Fatal(err)
			}
			blocker, err := s.db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			waits := s.db.Stats().WaitCount
			done := make(chan error, 1)
			go func() {
				_, err := s.SendForSessionContext(ctx, Message{Sender: u.Name, Recipient: "tx-recipient", Body: "must never commit", Kind: "chat"}, c.ID)
				done <- err
			}()
			deadline := time.Now().Add(3 * time.Second)
			for s.db.Stats().WaitCount <= waits {
				if time.Now().After(deadline) {
					t.Fatal("send did not queue")
				}
				time.Sleep(time.Millisecond)
			}
			switch kind {
			case "revoked":
				_, err = blocker.Exec(`UPDATE sessions SET revoked=1 WHERE id=?`, c.ID)
			case "reset":
				_, err = blocker.Exec(`UPDATE user_meta SET must_reset=1 WHERE name=?`, u.Name)
			case "block":
				_, err = blocker.Exec(`INSERT INTO relations(owner,target,kind,at) VALUES('tx-recipient','tx-sender','block',1)`)
			case "epoch":
				_, err = blocker.Exec(`INSERT INTO credential_versions(user,epoch) VALUES(?,1) ON CONFLICT(user) DO UPDATE SET epoch=epoch+1`, u.Name)
			case "cancel":
				cancel()
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = blocker.Commit(); err != nil {
				t.Fatal(err)
			}
			select {
			case err = <-done:
				if err == nil {
					t.Fatal("stale send succeeded")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("send did not stop")
			}
			for _, table := range []string{"messages", "notifications"} {
				var count int
				if err := s.db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 0 {
					t.Fatal("denied send left effects", table, count, err)
				}
			}
		})
	}
}

func TestNativeMessageRequiresOwnedSessionAndAtomicNotification(t *testing.T) {
	s := testStore(t)
	u := registerUser(t, s, "atomic-sender")
	other := registerUser(t, s, "atomic-recipient")
	for _, c := range []*Client{{ID: "sender-session", Name: u.Name, AuthEpoch: u.AuthEpoch}, {ID: "foreign-session", Name: other.Name, AuthEpoch: other.AuthEpoch}} {
		if err := s.RecordSession(c); err != nil {
			t.Fatal(err)
		}
	}
	m := Message{Sender: u.Name, Recipient: other.Name, Body: "atomic direct message", Kind: "chat"}
	for _, id := range []string{"", "missing", "foreign-session"} {
		if _, err := s.SendForSessionContext(t.Context(), m, id); err == nil {
			t.Fatal("unowned session accepted", id)
		}
	}
	if _, err := s.db.Exec(`CREATE TRIGGER deny_notification BEFORE INSERT ON notifications BEGIN SELECT RAISE(ABORT,'synthetic notification failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SendForSessionContext(t.Context(), m, "sender-session"); err == nil {
		t.Fatal("notification failure partially committed")
	}
	var count int
	if err := s.db.QueryRow("SELECT count(*) FROM messages").Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
}

func TestNativeReplyBodyUsesTheSessionBoundSendPath(t *testing.T) {
	s := testStore(t)
	u := registerUser(t, s, "reply-session-user")
	registerUser(t, s, "reply-session-peer")
	parent := mustSend(t, s, "reply-session-peer", "lobby", "", "parent retained")
	c := &Client{ID: "reply-native-session", Name: u.Name, AuthEpoch: u.AuthEpoch, Ctx: t.Context()}
	if err := s.RecordSession(c); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeSession(u.Name, c.ID); err != nil {
		t.Fatal(err)
	}
	h := newHub()
	h.Attach(s)
	if _, err := guiExecuteAction(s, h, u, guiScope{room: "lobby"}, 0, guiRequest{
		Body: fmt.Sprintf("/reply %d forbidden after revoke", parent.ID), sessionID: c.ID, ctx: t.Context(),
	}); err == nil {
		t.Fatal("native reply bypassed the durable-session transaction")
	}
	var count int
	if err := s.db.QueryRow("SELECT count(*) FROM messages").Scan(&count); err != nil || count != 1 {
		t.Fatal("revoked native reply committed", count, err)
	}
}

func TestGUIOneOffUnicodeCanonicalTarget(t *testing.T) {
	s := testStore(t)
	u := registerUser(t, s, "unicode-sender")
	other := registerUser(t, s, "İuser")
	c := &Client{ID: "unicode-session", Name: u.Name, AuthEpoch: u.AuthEpoch, Ctx: t.Context()}
	if err := s.RecordSession(c); err != nil {
		t.Fatal(err)
	}
	h := newHub()
	h.Attach(s)
	r, err := guiExecuteAction(s, h, u, guiScope{room: "lobby"}, 0, guiRequest{Body: "/msg İuser canonical account test", sessionID: c.ID, ctx: t.Context()})
	if err != nil || r.directTarget == nil || r.directTarget.Requested != "İuser" || r.directTarget.Canonical != other.Name || r.message == nil || r.message.Recipient != other.Name {
		t.Fatal("canonical projection mismatch", r, err)
	}
	if other.Name != "iuser" {
		t.Fatal("Go account normalization contract changed", other.Name)
	}
}
