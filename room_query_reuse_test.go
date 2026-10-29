package main

import (
	"database/sql"
	"strings"
	"testing"
)

// Count actual query entrypoints while retaining SQLite execution and errors.
type roomQueryProbe struct {
	queryer
	roomReads  int
	failRoom   bool
	failMember bool
}

func (p *roomQueryProbe) QueryRow(query string, args ...any) *sql.Row {
	if strings.HasPrefix(query, "SELECT r.name,r.topic,r.owner,") {
		p.roomReads++
		if p.failRoom {
			return p.queryer.QueryRow(`SELECT nonexistent_room_probe_column`)
		}
	}
	if p.failMember && strings.HasPrefix(query, "SELECT count(*) FROM members WHERE room=? AND user=?") {
		return p.queryer.QueryRow(`SELECT nonexistent_member_probe_column`)
	}
	return p.queryer.QueryRow(query, args...)
}
func TestCanSendSingleRoomLookupKeepsFreshPolicy(t *testing.T) {
	s, _ := featureFixture(t)
	check := func(user, room, recipient string, want bool, reads int, failRoom, failMember bool) {
		t.Helper()
		tx, e := s.db.Begin()
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback()
		q := &roomQueryProbe{queryer: tx, failRoom: failRoom, failMember: failMember}
		e = canSendQ(q, user, room, recipient)
		if (e == nil) != want || q.roomReads != reads {
			t.Fatalf("permission=%v room_reads=%d want_allowed=%v want_reads=%d", e, q.roomReads, want, reads)
		}
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, e := s.db.Exec(query, args...); e != nil {
			t.Fatal(e)
		}
	}
	check("bob", "arcade", "", true, 1, false, false)
	check("bob", "missing", "", false, 1, false, false)
	check("bob", "arcade", "", false, 1, true, false)
	if e := s.CreateRoomAccess("query-private", "alice", true); e != nil {
		t.Fatal(e)
	}
	check("bob", "query-private", "", false, 1, false, false)
	if e := s.RoomMember("alice", "query-private", "bob", "invite"); e != nil {
		t.Fatal(e)
	}
	check("bob", "query-private", "", true, 1, false, false)
	check("bob", "query-private", "", false, 1, false, true)
	check("alice", "query-private", "", true, 1, false, true) // owner does not need a membership row
	if e := s.RoomMember("alice", "query-private", "bob", "remove"); e != nil {
		t.Fatal(e)
	}
	check("bob", "query-private", "", false, 1, false, false) // no retained ACL across calls
	exec(`UPDATE room_meta SET readonly=1 WHERE name='arcade'`)
	check("bob", "arcade", "", false, 2, false, false) // independent management check is retained
	exec(`UPDATE room_meta SET readonly=0,archived=1 WHERE name='arcade'`)
	check("bob", "arcade", "", false, 1, false, false)
	exec(`UPDATE room_meta SET archived=0,slow=30 WHERE name='arcade'`)
	mustSend(t, s, "bob", "arcade", "", "first slow message")
	check("bob", "arcade", "", false, 2, false, false)
	exec(`UPDATE room_meta SET slow=0 WHERE name='arcade'`)
	check("bob", "arcade", "", true, 1, false, false)
	check("bob", "", "alice", true, 0, true, false) // DM route never queries an irrelevant room
	exec(`INSERT INTO sanctions(kind,target,room,expires,reason,actor,at) VALUES('mute','bob','arcade',0,'synthetic','owner',0)`)
	check("bob", "arcade", "", false, 0, false, false)
	exec(`UPDATE sanctions SET revoked=1 WHERE target='bob'`)
	exec(`UPDATE user_meta SET must_reset=1 WHERE name='bob'`)
	check("bob", "arcade", "", false, 0, false, false)
}
