package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestReplyDecorationDoesNotWaitForTheWriterConnection(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "miochat.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.db.Close()
	for _, name := range []string{"alice", "bob"} {
		registerUser(t, s, name)
	}
	parent := mustSend(t, s, "alice", "lobby", "", "quoted content")
	reply, err := s.Send(Message{Room: "lobby", Sender: "bob", Body: "reply", Kind: "chat", ReplyTo: parent.ID})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	messages := []Message{reply}
	completed := make(chan error, 1)
	go func() { completed <- s.decorateContext(ctx, messages, "bob") }()
	select {
	case err = <-completed:
		if err != nil || messages[0].ReplyBody != parent.Body {
			t.Fatal("reply decoration could not complete using readers", err, messages)
		}
	case <-ctx.Done():
		t.Fatal("reply ACL still waited for the occupied writer connection")
	}
}

func TestReplyDecorationUsesCurrentBatchACLAndBudget(t *testing.T) {
	s, _ := featureFixture(t)
	if err := s.CreateRoomAccess("quote-private", "alice", true); err != nil {
		t.Fatal(err)
	}
	if err := s.RoomMember("alice", "quote-private", "bob", "invite"); err != nil {
		t.Fatal(err)
	}
	parent := mustSend(t, s, "alice", "quote-private", "", "restricted parent")
	reply, err := s.Send(Message{Room: "quote-private", Sender: "bob", Body: "reply", Kind: "chat", ReplyTo: parent.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RoomMember("alice", "quote-private", "bob", "remove"); err != nil {
		t.Fatal(err)
	}
	values := []Message{reply}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err = s.decorateContext(ctx, values, "bob"); err != nil {
		t.Fatal(err)
	}
	if values[0].ReplyBody != "" || values[0].ReplyName != "" {
		t.Fatal("batch ACL retained a parent after membership revocation")
	}
	expired, cancelExpired := context.WithCancel(context.Background())
	cancelExpired()
	if err = s.decorateContext(expired, values, "alice"); !errors.Is(err, context.Canceled) {
		t.Fatal("reply decoration ignored the caller's query budget", err)
	}
}

// The bounded decorator is intentionally fail-closed after Send has committed:
// it must never turn a committed write into a retryable send failure.
func TestCommittedSendDecorationTimeoutKeepsOneDurableMessage(t *testing.T) {
	s, _ := featureFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
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
	before := s.readDB().Stats().WaitCount
	started := time.Now()
	message, err := s.Send(Message{Sender: "alice", Room: "lobby", Body: "synthetic committed despite decoration timeout", Kind: "chat"})
	if err != nil || message.ID <= 0 || message.Body != "[暂时无法加载消息状态，请刷新历史]" || message.ReplyBody != "" || message.Attachment != nil {
		t.Fatal("committed send did not retain a safe placeholder", message, err)
	}
	if time.Since(started) > time.Second || s.readDB().Stats().WaitCount <= before {
		t.Fatal("did not exercise the bounded reader wait", time.Since(started))
	}
	var body string
	if err = s.db.QueryRow(`SELECT body FROM messages WHERE id=? AND sender='alice' AND room='lobby' AND recipient='' AND kind='chat'`, message.ID).Scan(&body); err != nil || body != "synthetic committed despite decoration timeout" {
		t.Fatal("committed content changed", err)
	}
	var count int
	if err = s.db.QueryRow(`SELECT count(*) FROM messages WHERE sender='alice' AND body=?`, body).Scan(&count); err != nil || count != 1 {
		t.Fatal("send duplicated or was rolled back", count, err)
	}
	if err = first.Close(); err != nil {
		t.Fatal(err)
	}
	if err = second.Close(); err != nil {
		t.Fatal(err)
	}
	restored, err := s.Message("alice", message.ID)
	if err != nil || restored.Body != body {
		t.Fatal("reader recovery did not restore committed content", restored, err)
	}
}
