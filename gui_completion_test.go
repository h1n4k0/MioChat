package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func completionWire(t *testing.T, s *Store, u User, id, kind, prefix string) (guiResponse, error) {
	t.Helper()
	return guiCompletePrefix(t.Context(), s, u, guiScope{room: "lobby"}, id, guiRequest{Type: "completion", RequestID: "synthetic-complete", Room: "lobby", Completion: &guiCompletionRequest{Kind: kind, Prefix: prefix}})
}
func TestGUICompletionPrefixTypesPrivacyLiteralAndNoDraftWrites(t *testing.T) {
	s, h := featureFixture(t)
	m := testModel(t, s, h, "alice")
	before, e := s.PositionWithError("alice", "room:lobby")
	if e != nil {
		t.Fatal(e)
	}
	for _, test := range []struct{ kind, prefix, want string }{{"command", "/time", "/time"}, {"room", "#lob", "#lobby"}, {"user", "@bo", "@bob"}, {"user", "bo", "bob"}} {
		out, e := completionWire(t, s, m.user, m.client.ID, test.kind, test.prefix)
		if e != nil || out.Completion == nil || !containsString(out.Completion.Choices, test.want) {
			t.Fatal(test, out, e)
		}
		b, _ := json.Marshal(out)
		for _, bad := range []string{"password", "sessions", "profile", "draft", "message", "preferences"} {
			if strings.Contains(string(b), `"`+bad+`"`) {
				t.Fatal("sibling projection", string(b))
			}
		}
	}
	for _, prefix := range []string{"@%", "@_", "@\\"} {
		out, e := completionWire(t, s, m.user, m.client.ID, "user", prefix)
		if e != nil || len(out.Completion.Choices) != 0 {
			t.Fatal("LIKE wildcard expanded", out, e)
		}
	}
	after, e := s.PositionWithError("alice", "room:lobby")
	if e != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("completion mutated draft", before, after, e)
	}
}
func containsString(v []string, w string) bool {
	for _, s := range v {
		if s == w {
			return true
		}
	}
	return false
}
func TestGUICompletionNamesBoundedAndRoomACL(t *testing.T) {
	s, h := featureFixture(t)
	m := testModel(t, s, h, "alice")
	// Query-only fixture names need no credentials; no login using these rows.
	for i := 0; i < 105; i++ {
		if _, e := s.db.Exec(`INSERT INTO users(name,hash,salt,bio,status,theme,role,created,banned) SELECT ?,hash,salt,bio,status,theme,role,created,banned FROM users WHERE name='bob'`, fmt.Sprintf("z-prefix-%03d", i)); e != nil {
			t.Fatal(e)
		}
	}
	out, e := completionWire(t, s, m.user, m.client.ID, "user", "z-prefix-")
	if e != nil || len(out.Completion.Choices) != 100 || out.Completion.Choices[99] != "z-prefix-099" {
		t.Fatal(out, e)
	}
	if _, e = s.db.Exec(`INSERT INTO rooms(name,topic,owner) VALUES('synthetic-private','private','bob')`); e != nil {
		t.Fatal(e)
	}
	if _, e = s.db.Exec(`INSERT INTO room_meta(name,private) VALUES('synthetic-private',1)`); e != nil {
		t.Fatal(e)
	}
	out, e = completionWire(t, s, m.user, m.client.ID, "room", "#synthetic")
	if e != nil || len(out.Completion.Choices) != 0 {
		t.Fatal("private directory leak", out, e)
	}
	if _, e = s.db.Exec(`INSERT INTO members(room,user,role,joined) VALUES('synthetic-private','alice','member',1)`); e != nil {
		t.Fatal(e)
	}
	out, e = completionWire(t, s, m.user, m.client.ID, "room", "#synthetic")
	if e != nil || !reflect.DeepEqual(out.Completion.Choices, []string{"#synthetic-private"}) {
		t.Fatal(out, e)
	}
}
func TestGUICompletionRejectMixedFramesAndInvalidPrefix(t *testing.T) {
	s, h := featureFixture(t)
	m := testModel(t, s, h, "alice")
	base := guiRequest{Type: "completion", RequestID: "synthetic", Room: "lobby", Completion: &guiCompletionRequest{Kind: "command", Prefix: "/"}}
	for _, mutate := range []func(*guiRequest){func(r *guiRequest) { r.Body = "unrelated secret" }, func(r *guiRequest) { r.ReplyTo = 1 }, func(r *guiRequest) { r.SecretAdmin = &guiSecretAdminRequest{} }, func(r *guiRequest) { r.Room = "dm:bob" }, func(r *guiRequest) { r.Password = "synthetic" }, func(r *guiRequest) { r.Completion = nil }} {
		r := base
		mutate(&r)
		out, e := guiCompletePrefix(t.Context(), s, m.user, guiScope{room: "lobby"}, m.client.ID, r)
		if e == nil || out.Completion != nil {
			t.Fatal("mixed accepted", out, e)
		}
	}
	for _, prefix := range []string{"@two words", "@\u200bsecret", "@\n", strings.Repeat("a", 257)} {
		if out, e := completionWire(t, s, m.user, m.client.ID, "user", prefix); e == nil || out.Completion != nil {
			t.Fatal(out, e)
		}
	}
}
func TestGUICompletionQueuedCredentialsAndScopeRevalidated(t *testing.T) {
	for _, change := range []string{"session", "epoch", "reset", "acl"} {
		t.Run(change, func(t *testing.T) {
			s, h := featureFixture(t)
			m := testModel(t, s, h, "alice")
			s.readDB().SetMaxOpenConns(1)
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			held, e := s.readDB().Conn(ctx)
			if e != nil {
				t.Fatal(e)
			}
			defer held.Close()
			waits := s.readDB().Stats().WaitCount
			type result struct {
				out guiResponse
				err error
			}
			done := make(chan result, 1)
			go func() {
				o, e := guiCompletePrefix(ctx, s, m.user, guiScope{room: "lobby"}, m.client.ID, guiRequest{Type: "completion", RequestID: "queued", Room: "lobby", Completion: &guiCompletionRequest{Kind: "user", Prefix: "bo"}})
				done <- result{o, e}
			}()
			deadline := time.Now().Add(time.Second)
			for s.readDB().Stats().WaitCount <= waits {
				if time.Now().After(deadline) {
					t.Fatal("not queued")
				}
				time.Sleep(time.Millisecond)
			}
			var query string
			var args []any
			switch change {
			case "session":
				query = `UPDATE sessions SET revoked=1 WHERE id=?`
				args = []any{m.client.ID}
			case "epoch":
				query = `INSERT INTO credential_versions(user,epoch) VALUES('alice',9) ON CONFLICT(user) DO UPDATE SET epoch=9`
			case "reset":
				query = `UPDATE user_meta SET must_reset=1 WHERE name='alice'`
			case "acl":
				query = `INSERT INTO room_meta(name,private) VALUES('lobby',1) ON CONFLICT(name) DO UPDATE SET private=1`
			}
			if e = changedOne(s.db.Exec(query, args...)); e != nil {
				t.Fatal(e)
			}
			if change == "acl" {
				if _, e = s.db.Exec(`UPDATE rooms SET owner='bob' WHERE name='lobby'`); e != nil {
					t.Fatal(e)
				}
				if _, e = s.db.Exec(`DELETE FROM members WHERE room='lobby' AND user='alice'`); e != nil {
					t.Fatal(e)
				}
			}
			held.Close()
			select {
			case r := <-done:
				if r.err == nil || r.out.Completion != nil || errors.Is(r.err, context.DeadlineExceeded) || errors.Is(r.err, context.Canceled) {
					t.Fatal("stale completion escaped", r)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		})
	}
}

// Preserve the selected SQL/scan failure, not partial names from earlier rows.
type completionFaultQ struct {
	queryer
	scan bool
}

func (q completionFaultQ) Query(query string, args ...any) (*sql.Rows, error) {
	if strings.Contains(query, "SELECT name FROM users") {
		if !q.scan {
			return nil, errors.New("synthetic selected-query failure")
		}
		return q.queryer.Query(`SELECT name FROM users WHERE name='alice' UNION ALL SELECT NULL`)
	}
	return q.queryer.Query(query, args...)
}
func TestGUICompletionQueryAndScanErrorsNeverReturnPartialNames(t *testing.T) {
	s, h := featureFixture(t)
	_ = h
	for _, scan := range []bool{false, true} {
		v, e := completionDataQ(completionFaultQ{queryer: s.readDB(), scan: scan}, "alice", guiCompletionRequest{Kind: "user", Prefix: ""})
		if e == nil || v != nil {
			t.Fatal(v, e)
		}
	}
}

func TestGUICompletionReaderDeadlineIsBoundedAndEmptySessionRefused(t *testing.T) {
	s, h := featureFixture(t)
	m := testModel(t, s, h, "alice")
	if out, e := completionWire(t, s, m.user, "", "command", "/"); e == nil || out.Completion != nil {
		t.Fatal(out, e)
	}
	s.readDB().SetMaxOpenConns(1)
	held, e := s.readDB().Conn(t.Context())
	if e != nil {
		t.Fatal(e)
	}
	defer held.Close()
	started := time.Now()
	out, e := completionWire(t, s, m.user, m.client.ID, "command", "/")
	if !errors.Is(e, context.DeadlineExceeded) || out.Completion != nil || time.Since(started) > time.Second {
		t.Fatal("reader budget not bounded", out, e, time.Since(started))
	}
}
