package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestGUIMessageContextRoleAndPreloadedEditActions(t *testing.T) {
	s, h := featureFixture(t)
	alice := testModel(t, s, h, "alice")
	bob := testModel(t, s, h, "bob")
	m := mustSend(t, s, "bob", "lobby", "", "original 中文")
	p := guiMessageContext(s, bob.user, guiScope{room: "lobby"}, m)
	found := false
	for _, a := range p.Actions {
		if a.Command == fmt.Sprintf("/edit %d", m.ID) {
			found = true
			if a.Fill {
				t.Fatal("edit must be isolated native form")
			}
		}
	}
	if !found || p.Lines[1] != m.Body {
		t.Fatal(p)
	}
	p = guiMessageContext(s, alice.user, guiScope{room: "lobby"}, m)
	for _, a := range p.Actions {
		if strings.HasPrefix(a.Command, "/edit ") {
			t.Fatal("cannot edit another author")
		}
	}
	profile, e := guiProfileSnapshot(s, h, alice.user.Name, "bob")
	if e != nil || profile.CanAdmin != isAdmin(alice.user.Role) {
		t.Fatal(profile, e)
	}
}
func TestGUIMessageContextEditSessionScopeAndAtomicWrites(t *testing.T) {
	s, h := featureFixture(t)
	m := testModel(t, s, h, "alice")
	message := mustSend(t, s, "alice", "lobby", "", "original")
	run := func(scope, body string) error {
		return s.UpdateMessageForSessionContext(t.Context(), "alice", m.client.ID, scope, message.ID, body, false)
	}
	if run("elsewhere", "wrong") == nil {
		t.Fatal("cross scope edit")
	}
	if e := run("lobby", "正常修改"); e != nil {
		t.Fatal(e)
	}
	for _, table := range []string{"revisions", "messages", "message_meta", "audit"} {
		t.Run(table, func(t *testing.T) {
			trigger := fmt.Sprintf("CREATE TRIGGER context_ignore BEFORE %s ON %s BEGIN SELECT RAISE(IGNORE);END", map[bool]string{true: "INSERT", false: "UPDATE"}[table == "revisions" || table == "audit"], table)
			if _, e := s.db.Exec(trigger); e != nil {
				t.Fatal(e)
			}
			defer s.db.Exec(`DROP TRIGGER context_ignore`)
			var before int
			if e := s.db.QueryRow(`SELECT count(*) FROM revisions WHERE message_id=?`, message.ID).Scan(&before); e != nil {
				t.Fatal(e)
			}
			if run("lobby", "should rollback") == nil {
				t.Fatal("ignored required write accepted")
			}
			got, e := s.Message("alice", message.ID)
			if e != nil || got.Body != "正常修改" {
				t.Fatal(got, e)
			}
			var after int
			s.db.QueryRow(`SELECT count(*) FROM revisions WHERE message_id=?`, message.ID).Scan(&after)
			if before != after {
				t.Fatal("partial revision")
			}
		})
	}
	// Occupy the actual writer, revoke this session within the held transaction,
	// then release. The queued edit must recheck that exact session in its writer.
	tx, e := s.db.Begin()
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	before := s.db.Stats().WaitCount
	go func() {
		done <- s.UpdateMessageForSessionContext(ctx, "alice", m.client.ID, "lobby", message.ID, "revoked", false)
	}()
	deadline := time.Now().Add(time.Second)
	for s.db.Stats().WaitCount == before && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if s.db.Stats().WaitCount == before {
		t.Fatal("writer not queued")
	}
	if e = changedOne(tx.Exec(`UPDATE sessions SET revoked=1 WHERE id=?`, m.client.ID)); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}
	if e = <-done; e == nil {
		t.Fatal("queued revoked edit committed")
	}
}
