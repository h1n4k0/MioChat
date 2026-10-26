package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func explainMessageQuery(t *testing.T, s *Store, query string, args ...any) string {
	t.Helper()
	rows, err := s.readDB().Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err = rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		details = append(details, detail)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(details, "\n")
}

func TestDirectMessagePagesUseBoundedOrderedRanges(t *testing.T) {
	s, _ := featureFixture(t)
	if _, err := s.db.Exec(`WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<10000) INSERT INTO messages(room,sender,recipient,body,kind,at) SELECT '',CASE WHEN x%2=0 THEN 'alice' ELSE 'bob' END,CASE WHEN x%2=0 THEN 'bob' ELSE 'alice' END,'双向历史私信','chat',1700000000+x FROM n`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<1000) INSERT INTO messages(room,sender,recipient,body,kind,at) SELECT '','bob','eve','不可见私信','chat',1700100000+x FROM n; WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<1000) INSERT INTO messages(room,sender,recipient,body,kind,at) SELECT '','eve','alice','另一个会话','chat',1700200000+x FROM n; WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<1000) INSERT INTO messages(room,sender,recipient,body,kind,at) SELECT 'lobby','alice','','频道消息','chat',1700300000+x FROM n`); err != nil {
		t.Fatal(err)
	}
	oldQuery := `SELECT * FROM (SELECT ` + messageColumns + ` FROM messages WHERE room='' AND ((sender=? AND recipient=?) OR (sender=? AND recipient=?)) AND id<? ORDER BY id DESC LIMIT ?) ORDER BY id`
	t.Log("former OR plan:\n" + explainMessageQuery(t, s, oldQuery, "alice", "bob", "bob", "alice", int64(1<<63-1), 100))
	for _, forward := range []bool{false, true} {
		query, args := directMessagePageQuery("alice", "bob", 5000, 100, forward)
		plan := explainMessageQuery(t, s, query, args...)
		t.Logf("ordered range plan (forward=%v):\n%s", forward, plan)
		if strings.Count(plan, "USING INDEX dm_history") != 2 || strings.Contains(plan, "SCAN messages") || strings.Contains(plan, "USING INDEX room_history") {
			t.Fatal("DM pagination did not use the two ordered participant ranges", plan)
		}
	}
	started := time.Now()
	latest, err := s.History("", "alice", "bob", 100)
	if err != nil {
		t.Fatal("10,000-message history exceeded the unchanged page budget", err)
	}
	t.Logf("latest 100 of 10,000 DMs, including decoration: %s", time.Since(started))
	assertPageIDs(t, latest, 9901, 100)
	previous, err := s.Page("", "alice", "bob", 5051, 100)
	if err != nil {
		t.Fatal(err)
	}
	assertPageIDs(t, previous, 4951, 100)
	started = time.Now()
	next, err := s.PageAfter("", "alice", "bob", 4950, 100)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("adjacent next 100 of 10,000 DMs, including decoration: %s", time.Since(started))
	assertPageIDs(t, next, 4951, 100)
	first, err := s.PageAfter("", "alice", "bob", -100, 10000)
	if err != nil {
		t.Fatal(err)
	}
	assertPageIDs(t, first, 1, 200)
	empty, err := s.PageAfter("", "alice", "bob", 10000, 100)
	if err != nil || len(empty) != 0 {
		t.Fatal("adjacent page crossed into another conversation", err, len(empty))
	}
}

func assertPageIDs(t *testing.T, messages []Message, first int64, count int) {
	t.Helper()
	if len(messages) != count {
		t.Fatalf("page length = %d, want %d", len(messages), count)
	}
	for i, message := range messages {
		if message.ID != first+int64(i) {
			t.Fatalf("page[%d].ID = %d, want %d", i, message.ID, first+int64(i))
		}
	}
}

func TestForwardPagesEnforceCurrentACLAndRecallState(t *testing.T) {
	s, _ := featureFixture(t)
	if err := s.CreateRoomAccess("forward-private", "alice", true); err != nil {
		t.Fatal(err)
	}
	if err := s.RoomMember("alice", "forward-private", "bob", "invite"); err != nil {
		t.Fatal(err)
	}
	m := mustSend(t, s, "alice", "forward-private", "", "保密消息")
	if err := s.UpdateMessage("alice", m.ID, "", true); err != nil {
		t.Fatal(err)
	}
	page, err := s.PageAfter("forward-private", "bob", "", 0, 200)
	if err != nil || len(page) != 1 || page[0].Recalled == 0 || page[0].Body != "[消息已撤回]" {
		t.Fatal("forward page did not decorate recall state", err, page)
	}
	if err := s.RoomMember("alice", "forward-private", "bob", "remove"); err != nil {
		t.Fatal(err)
	}
	if page, err = s.PageAfter("forward-private", "bob", "", 0, 200); err == nil || len(page) != 0 {
		t.Fatal("forward page retained revoked private-room access", err, page)
	}
}

func TestMessagePagesShareReaderBudgetWithoutWaitingForWriter(t *testing.T) {
	s, _ := featureFixture(t)
	m := mustSend(t, s, "alice", "lobby", "", "bounded reader page")
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, forward := range []bool{false, true} {
		var page []Message
		if forward {
			page, err = s.PageAfter("lobby", "bob", "", 0, 100)
		} else {
			page, err = s.Page("lobby", "bob", "", 0, 100)
		}
		if err != nil || len(page) != 1 || page[0].ID != m.ID {
			t.Fatal("page ACL or query still waited for the writer", err, page)
		}
	}
	first, err := s.readDB().Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := s.readDB().Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if page, err := s.PageAfter("lobby", "bob", "", 0, 100); !errors.Is(err, context.DeadlineExceeded) || len(page) != 0 {
		t.Fatal("page ACL was outside the total query deadline", err, page)
	}
}
