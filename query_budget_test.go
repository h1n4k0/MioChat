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
