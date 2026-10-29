package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"testing"
	"time"
)

// Explicit, disposable integration harness. Normal test runs skip this entry.
// It has no listener, files, production identity or access to SSH credentials.
// The only account/credential below is created anew in an in-memory test DB.
func TestGUIFixtureDriver(t *testing.T) {
	if os.Getenv("MIOCHAT_LOCAL_FIXTURE") != "synthetic-pipe-v1" {
		t.Skip("manual native-client fixture only")
	}
	store, err := openStore(":memory:")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	user, err := store.Register("preview", "fixture-local-only", "fixture-local-only")
	if err == nil {
		var peer User
		peer, err = store.Register("fixture-peer", "fixture-peer-only", "fixture-peer-only")
		if err == nil {
			_, err = store.db.Exec(`UPDATE users SET role='user'`)
		}
		if err == nil {
			err = store.CreateRoomAccess("fixture-room", peer.Name, true)
		}
		if err == nil {
			err = store.RoomMember(peer.Name, "fixture-room", user.Name, "invite")
		}
		if err == nil {
			err = store.Relation(peer.Name, user.Name, "request", "", false)
		}
	}
	if err == nil {
		_, err = store.Register("fixture-admin", "fixture-admin-only", "fixture-admin-only")
		if err == nil {
			_, err = store.db.Exec(`UPDATE users SET role='admin' WHERE name='fixture-admin'`)
		}
	}
	if err == nil {
		_, err = store.Register("fixture-owner", "fixture-owner-only", "fixture-owner-only")
		if err == nil {
			_, err = store.db.Exec(`UPDATE users SET role='owner' WHERE name='fixture-owner'`)
		}
	}
	if err == nil {
		store.Award(user.Name, "synthetic-achievement")
	}
	if err == nil {
		_, err = store.Post(user.Name, "lobby", "Synthetic community post", "fixture community body", false)
	}
	if err == nil {
		_, err = store.Post(user.Name, "lobby", "Synthetic ASCII", `  /\
 /__\`, true)
	}
	if err == nil {
		_, err = store.Poll(user.Name, "lobby", "Synthetic poll|First|Second")
	}
	if err == nil {
		_, err = store.NewEvent(user.Name, "lobby", "Synthetic event|"+time.Now().Add(24*time.Hour).In(time.FixedZone("CST", 8*3600)).Format("2006-01-02 15:04")+"|fixture event")
	}
	if err == nil {
		_, err = store.NewGuess(user.Name, "lobby", "word", "fixture-hidden-word")
	}
	var anchor int64
	for i := 1; err == nil && i <= 320; i++ {
		message, sendErr := store.Send(Message{Sender: user.Name, Room: "lobby", Kind: "chat", Body: fmt.Sprintf("合成消息 %03d · 仅内存联调，不连接真实服务器。", i)})
		err = sendErr
		if i == 12 {
			anchor = message.ID
		}
	}
	// Explicit synthetic moderation history: no extra chat messages or real data.
	if err == nil {
		for i := 0; i < 105 && err == nil; i++ {
			_, err = store.db.Exec(`INSERT INTO revisions(message_id,actor,action,body,at) VALUES(?,?,'edit',?,?)`, anchor, user.Name, fmt.Sprintf("synthetic historical body %03d", i), i+1)
		}
	}
	if err == nil {
		_, err = store.db.Exec(`INSERT INTO reports(id,user,message_id,reason,status,at) VALUES(7001,'fixture-peer',?,'synthetic report','open',1)`, anchor)
	}
	if err == nil {
		_, err = store.SavePositionVersioned(user.Name, "room:lobby", Position{Draft: "合成草稿第一行\n第二行", Anchor: anchor, LastRead: anchor, Offset: 1})
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	ctx, cancel := context.WithCancel(context.Background())
	err = runGUIProtocol(ctx, os.Stdin, os.Stdout, "192.0.2.222", "synthetic-pipe-fixture", cancel, store, newHub())
	cancel()
	_ = store.db.Close()
	if err != nil && err != io.EOF {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0) // Keep the fixture stream JSON-only, without the test runner footer.
}
