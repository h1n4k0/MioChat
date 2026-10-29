package main

import (
	"database/sql"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

type policyReadFault struct {
	queryer
	kind string
	scan bool
	hits int
}

func (q *policyReadFault) QueryRow(query string, args ...any) *sql.Row {
	match := false
	switch q.kind {
	case "user", "mute":
		match = strings.Contains(query, "FROM sanctions WHERE kind=?") && len(args) > 0 && args[0] == q.kind
	case "maintenance":
		match = query == `SELECT value FROM config WHERE key='maintenance'`
	case "block":
		match = strings.Contains(query, `FROM relations WHERE owner=? AND target=? AND kind='block'`)
	case "slow":
		match = strings.HasPrefix(query, `SELECT COALESCE(max(at),0) FROM messages WHERE room=?`)
	case "notify-ref":
		match = query == `SELECT room FROM messages WHERE id=?`
	case "notify-mute":
		match = query == `SELECT muted FROM preferences WHERE user=? AND scope=?`
	}
	if match {
		q.hits++
		if q.scan {
			return q.queryer.QueryRow(`SELECT NULL`)
		}
		return q.queryer.QueryRow(`SELECT nonexistent_policy_probe_column`)
	}
	return q.queryer.QueryRow(query, args...)
}
func policyCounts(t *testing.T, q queryer) [5]int {
	t.Helper()
	var counts [5]int
	for i, table := range []string{"messages", "objects", "object_events", "notifications", "revisions"} {
		if e := q.QueryRow(`SELECT count(*) FROM ` + table).Scan(&counts[i]); e != nil {
			t.Fatal(e)
		}
	}
	return counts
}
func TestPolicyReadFaultsDenyBeforeAnyWrite(t *testing.T) {
	s, _ := featureFixture(t)
	ref := mustSend(t, s, "bob", "lobby", "", "notification source")
	for _, kind := range []string{"user", "mute", "maintenance", "block", "slow", "notify-ref", "notify-mute"} {
		for _, scan := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/scan=%v", kind, scan), func(t *testing.T) {
				tx, e := s.db.Begin()
				if e != nil {
					t.Fatal(e)
				}
				defer tx.Rollback()
				q := &policyReadFault{queryer: tx, kind: kind, scan: scan}
				if kind == "slow" {
					if _, e := tx.Exec(`UPDATE room_meta SET slow=30 WHERE name='lobby'`); e != nil {
						t.Fatal(e)
					}
				}
				before := policyCounts(t, tx)
				recipient := ""
				room := "lobby"
				if kind == "block" {
					recipient = "alice"
					room = ""
				}
				if strings.HasPrefix(kind, "notify-") {
					if e = notifyQ(q, "alice", "mention", "bob", ref.ID, "synthetic"); e == nil {
						t.Error("POLICY_FAULT_ACCEPTED notify gate", kind)
					}
				} else if e = canSendQ(q, "bob", room, recipient); e == nil {
					t.Error("POLICY_FAULT_ACCEPTED send gate", kind)
				}
				if q.hits == 0 {
					t.Fatal("fault did not reach selected actual query")
				}
				if kind == "block" {
					if !blockedQ(q, "alice", "bob") {
						t.Error("POLICY_FAULT_ACCEPTED boolean contact policy")
					}
					if _, _, e = challengeQ(q, "bob", "alice", "lobby"); e == nil {
						t.Error("POLICY_FAULT_ACCEPTED challenge")
					}
					if e = notifyQ(q, "alice", "mention", "bob", 0, "synthetic notification"); e == nil {
						t.Error("POLICY_FAULT_ACCEPTED notification")
					}
				}
				if after := policyCounts(t, tx); after != before {
					t.Error("POLICY_FAULT_WROTE", before, after)
				}
			})
		}
	}
}

// Views inject a real SQLite execution error only into the chosen policy.
// Other policy/session queries stay executable; fixture writes are synthetic.
func installPolicySQLFault(t *testing.T, s *Store, kind, actor, target string) {
	t.Helper()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, e := s.db.Exec(query, args...); e != nil {
			t.Fatal(e)
		}
	}
	switch kind {
	case "user", "mute":
		exec(`INSERT INTO sanctions(kind,target,room,expires,reason,actor,at) VALUES(?,?,'',0,'synthetic fault','fixture',0)`, kind, actor)
		exec(`ALTER TABLE sanctions RENAME TO policy_sanctions_fixture`)
		// kind is from this fixed test switch, never user SQL.
		exec(`CREATE VIEW sanctions AS SELECT id,kind,target,room,expires,reason,actor,at,CASE WHEN kind='` + kind + `' THEN json_extract('invalid-json','$') ELSE revoked END AS revoked FROM policy_sanctions_fixture`)
	case "maintenance":
		exec(`INSERT INTO config VALUES('maintenance','off') ON CONFLICT(key) DO UPDATE SET value='off'`)
		exec(`ALTER TABLE config RENAME TO policy_config_fixture`)
		exec(`CREATE VIEW config AS SELECT key,CASE WHEN key='maintenance' THEN json_extract('invalid-json','$') ELSE value END AS value FROM policy_config_fixture`)
	case "block":
		exec(`INSERT INTO relations VALUES(?,?,'block','',0)`, target, actor)
		exec(`ALTER TABLE relations RENAME TO policy_relations_fixture`)
		exec(`CREATE VIEW relations AS SELECT owner,target,CASE WHEN kind='block' THEN json_extract('invalid-json','$') ELSE kind END AS kind,value,at FROM policy_relations_fixture`)
	case "slow":
		exec(`UPDATE room_meta SET slow=30 WHERE name='lobby'`)
		exec(`UPDATE messages SET at='malformed-timestamp' WHERE sender=?`, actor)
	case "notify-mute":
		exec(`INSERT INTO preferences(user,scope,muted) VALUES(?,'room:lobby','malformed-boolean')`, target)
	default:
		t.Fatal("bad test fault")
	}
}
func TestPolicySQLFaultsDenyRealTerminalAndNativeWrites(t *testing.T) {
	for _, kind := range []string{"user", "mute", "maintenance", "block", "slow", "notify-mute"} {
		t.Run(kind, func(t *testing.T) {
			s, _, u, r, target := moderationFixture(t)
			original := mustSend(t, s, u.Name, "lobby", "", "original editable body")
			before := policyCounts(t, s.db)
			installPolicySQLFault(t, s, kind, u.Name, target.Name)
			if kind != "user" {
				// Prove earlier native actor/session policy is not the reason this test denies.
				if _, e := mutationActorQ(s.db, u.Name); e != nil {
					t.Fatal("unrelated actor policy broken", e)
				}
				if valid, e := sessionCurrentQ(s.db, r.sessionID, u.Name); e != nil || !valid {
					t.Fatal("unrelated durable session broken", valid, e)
				}
			}
			message := Message{Sender: u.Name, Room: "lobby", Body: "must not write", Kind: "chat"}
			if kind == "notify-mute" {
				message.Body = "@" + target.Name + " synthetic mention"
			}
			if kind == "block" {
				message.Room = ""
				message.Recipient = target.Name
			}
			if _, e := s.Send(message); e == nil {
				t.Error("POLICY_SQL_FAULT_ACCEPTED terminal send", kind)
			}
			if _, e := s.SendForSessionContext(t.Context(), message, r.sessionID); e == nil {
				t.Error("POLICY_SQL_FAULT_ACCEPTED native send", kind)
			}
			if kind == "block" {
				if _, e := s.Challenge(u.Name, target.Name, "lobby"); e == nil {
					t.Error("POLICY_SQL_FAULT_ACCEPTED real challenge")
				}
				if !s.Blocked(target.Name, u.Name) {
					t.Error("POLICY_SQL_FAULT_ACCEPTED legacy contact delivery")
				}
			}
			if kind == "user" {
				if e := s.UpdateMessage(u.Name, original.ID, "changed body", false); e == nil {
					t.Error("POLICY_SQL_FAULT_ACCEPTED real edit")
				}
				current, e := messageQ(s.db, original.ID)
				if e != nil || current.Body != original.Body {
					t.Error("POLICY_SQL_FAULT_CHANGED_BODY", e)
				}
			}
			if after := policyCounts(t, s.db); !reflect.DeepEqual(before, after) {
				t.Error("POLICY_SQL_FAULT_WROTE", before, after)
			}
		})
	}
}
func TestPolicyMissingOptionalMaintenanceIsNotSQLFailure(t *testing.T) {
	s, _, u, r, target := moderationFixture(t)
	if _, e := s.db.Exec(`DELETE FROM config WHERE key='maintenance'`); e != nil {
		t.Fatal(e)
	}
	if _, e := s.SendForSessionContext(t.Context(), Message{Sender: u.Name, Room: "lobby", Body: "@" + target.Name + " optional default remains compatible", Kind: "chat"}, r.sessionID); e != nil {
		t.Fatal(e)
	}
	var notices int
	if e := s.db.QueryRow(`SELECT count(*) FROM notifications WHERE user=?`, target.Name).Scan(&notices); e != nil || notices != 1 {
		t.Fatal("absent optional preference did not preserve notification", notices, e)
	}
	if e := notifyQ(s.db, target.Name, "game", u.Name, 999999, "non-message object reference remains compatible"); e != nil {
		t.Fatal(e)
	}
	if _, e := s.db.Exec(`INSERT INTO config VALUES('maintenance','on')`); e != nil {
		t.Fatal(e)
	}
	if e := canSendQ(s.db, u.Name, "lobby", ""); e == nil {
		t.Fatal("enabled maintenance was ignored")
	}
	if _, e := s.db.Exec(`UPDATE config SET value='off' WHERE key='maintenance';`); e != nil {
		t.Fatal(e)
	}
	if _, e := s.db.Exec(`INSERT INTO relations VALUES(?,?,'block','',0)`, target.Name, u.Name); e != nil {
		t.Fatal(e)
	}
	if e := canSendQ(s.db, u.Name, "", target.Name); e == nil {
		t.Fatal("known block was ignored")
	}
}
