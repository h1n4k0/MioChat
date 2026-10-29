package main

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func moderationFixture(t *testing.T) (*Store, *Hub, User, guiRequest, User) {
	t.Helper()
	s := testStore(t)
	registerUser(t, s, "reader")
	target := registerUser(t, s, "target")
	u, r := communityReader(t, s, "reader", "moderation-session")
	h := newHub()
	h.Attach(s)
	return s, h, u, r, target
}
func TestGUIModerationRolesSelfWindowAndProjection(t *testing.T) {
	s, h, u, r, target := moderationFixture(t)
	if _, e := s.SavePositionVersioned(u.Name, "room:lobby", Position{Draft: "retain ordinary draft", Anchor: 42, ReplyTo: 0, Offset: 3}); e != nil {
		t.Fatal(e)
	}
	before, e := s.PositionWithError(u.Name, "room:lobby")
	if e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{u.Name, target.Name} {
		for i := 0; i < 105; i++ {
			if _, e = s.db.Exec(`INSERT INTO logins(user,ip,client,ok,at) VALUES(?,?,?,?,?)`, name, "192.0.2.8", fmt.Sprintf("synthetic-%03d", i), i%2, i+1); e != nil {
				t.Fatal(e)
			}
		}
	}
	if e = s.Relation(u.Name, target.Name, "note", "my-private-note", false); e != nil {
		t.Fatal(e)
	}
	if e = s.Relation(target.Name, u.Name, "note", "other-private-note", false); e != nil {
		t.Fatal(e)
	}
	for _, role := range []string{"member", "moderator", "admin", "owner"} {
		if _, e = s.db.Exec(`UPDATE users SET role=? WHERE name=?`, role, u.Name); e != nil {
			t.Fatal(e)
		}
		admin := role == "admin" || role == "owner"
		for _, command := range []string{"/ip", "/logins"} {
			for _, arg := range []string{"", u.Name, fmt.Sprintf("#%d", target.UID)} {
				r.Body = strings.TrimSpace(command + " " + arg)
				res, err := guiExecuteAction(s, h, u, guiScope{room: "lobby"}, 7, r)
				allowed := arg == "" || arg == u.Name || admin
				if (err == nil) != allowed {
					t.Fatal(role, r.Body, err)
				}
				if !allowed {
					if res.panel != nil {
						t.Fatal("denied panel")
					}
					continue
				}
				v := res.panel.Moderation
				if v.Viewer != u.Name || v.Window != 100 || len(v.Logins) != 100 || v.Logins[0].Client != "synthetic-104" || v.Logins[99].Client != "synthetic-005" || res.replyTo != 7 {
					t.Fatal(v, res.replyTo)
				}
			}
		}
		for _, query := range []struct{ c, a string }{{"/admin", ""}, {"/whois", fmt.Sprintf("#%d", target.UID)}, {"/ipusers", "192.0.2.8"}} {
			p, err := guiModerationRead(s, h, u, guiScope{room: "lobby"}, r, query.c, query.a)
			if (err == nil) != admin {
				t.Fatal(role, query, err)
			}
			if !admin {
				if p != nil {
					t.Fatal("denied returned data")
				}
				continue
			}
			v := p.Moderation
			if v.Viewer != u.Name || v.Command != query.c || v.Requested != query.a || v.OriginScope != "lobby" {
				t.Fatal(v)
			}
			raw, _ := json.Marshal(p)
			for _, secret := range []string{"\"hash\"", "\"salt\"", "\"settings\"", "auth_epoch", "other-private-note", "retain ordinary draft", "test-password-123"} {
				if strings.Contains(string(raw), secret) {
					t.Fatal("private projection leak", secret)
				}
			}
			if query.c == "/whois" && (v.Profile.Note != "my-private-note" || v.Profile.Name != target.Name || v.Profile.UID != target.UID) {
				t.Fatal(v.Profile)
			}
			if query.c == "/admin" {
				implemented := map[string]bool{"/ban": false, "/unban": false, "/gmute": false, "/ungmute": false, "/banip": false, "/unbanip": false, "/kick": false, "/role": false, "/resetpass": false}
				for _, a := range p.Actions {
					command := strings.TrimSpace(a.Command)
					if _, ok := implemented[command]; ok {
						if !a.Fill {
							t.Fatal("sanction confirmation entry must capture input", command)
						}
						implemented[command] = true
					}
				}
				for command, found := range implemented {
					if !found {
						t.Fatal("implemented sanction form entry missing", command)
					}
				}
			}
		}
	}
	after, e := s.PositionWithError(u.Name, "room:lobby")
	if e != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("read changed draft/position", before, after, e)
	}
}
func TestGUIModerationIPDistinctCanonicalBoundedAndAuditAtomic(t *testing.T) {
	s, h, u, r, _ := moderationFixture(t)
	if _, e := s.db.Exec(`UPDATE users SET role='admin' WHERE name=?`, u.Name); e != nil {
		t.Fatal(e)
	}
	tx, e := s.db.Begin()
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback()
	for i := 0; i < 205; i++ {
		name := fmt.Sprintf("synthetic-%03d", i)
		if _, e = tx.Exec(`INSERT INTO users(name,hash,salt,created) SELECT ?,hash,salt,created FROM users WHERE name=?`, name, u.Name); e != nil {
			t.Fatal(e)
		}
		for range 2 {
			if _, e = tx.Exec(`INSERT INTO logins(user,ip,client,ok,at) VALUES(?,?,?,1,1)`, name, "192.0.2.9", "fixture"); e != nil {
				t.Fatal(e)
			}
		}
	}
	for _, entry := range []struct {
		name, ip string
		ok       int
	}{{"reader", "::ffff:192.0.2.9", 1}, {"target", "192.0.2.9", 0}, {"removed-user", "192.0.2.9", 1}, {"target", "192.0.2.10", 1}} {
		if _, e = tx.Exec(`INSERT INTO logins(user,ip,client,ok,at) VALUES(?,?,'fixture',?,1)`, entry.name, entry.ip, entry.ok); e != nil {
			t.Fatal(e)
		}
	}
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}
	p, e := guiModerationRead(s, h, u, guiScope{room: "lobby"}, r, "/ipusers", "::ffff:192.0.2.9")
	if e != nil {
		t.Fatal(e)
	}
	v := p.Moderation
	if v.Canonical != "192.0.2.9" || !v.Audited || v.Window != 200 || len(v.Users) != 200 || v.Users[0].Name != "reader" || v.Users[199].Name != "synthetic-198" {
		t.Fatal(v)
	}
	for _, row := range v.Users {
		if row.Name == "target" || row.Name == "removed-user" {
			t.Fatal("incorrect source", row)
		}
	}
	var audits int
	s.db.QueryRow(`SELECT count(*) FROM audit WHERE action='ip.lookup'`).Scan(&audits)
	if audits != 1 {
		t.Fatal(audits)
	}
	for _, bad := range []string{"", "192.0.2.9/24", "not-ip"} {
		if p, e = guiModerationRead(s, h, u, guiScope{room: "lobby"}, r, "/ipusers", bad); e == nil || p != nil {
			t.Fatal(bad, p, e)
		}
	}
	for _, kind := range []string{"ABORT,'synthetic audit fault'", "IGNORE"} {
		if _, e = s.db.Exec(`CREATE TRIGGER synthetic_ip_audit BEFORE INSERT ON audit WHEN NEW.action='ip.lookup' BEGIN SELECT RAISE(` + kind + `);END`); e != nil {
			t.Fatal(e)
		}
		if p, e = guiModerationRead(s, h, u, guiScope{room: "lobby"}, r, "/ipusers", "192.0.2.9"); e == nil || p != nil {
			t.Fatal("unaudited rows escaped", p, e)
		}
		if _, e = s.db.Exec(`DROP TRIGGER synthetic_ip_audit`); e != nil {
			t.Fatal(e)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	r.ctx = ctx
	if p, e = guiModerationRead(s, h, u, guiScope{room: "lobby"}, r, "/ipusers", "192.0.2.9"); e == nil || p != nil {
		t.Fatal("canceled audit returned rows")
	}
	s.db.QueryRow(`SELECT count(*) FROM audit WHERE action='ip.lookup'`).Scan(&audits)
	if audits != 1 {
		t.Fatal("failed query wrote audit", audits)
	}
}
func TestGUIModerationFreshRoleSessionAndQueuedAudit(t *testing.T) {
	for _, change := range []string{"role", "session"} {
		t.Run(change, func(t *testing.T) {
			s, h, u, r, _ := moderationFixture(t)
			if _, e := s.db.Exec(`UPDATE users SET role='admin' WHERE name=?`, u.Name); e != nil {
				t.Fatal(e)
			}
			blocker, e := s.db.Begin()
			if e != nil {
				t.Fatal(e)
			}
			defer blocker.Rollback()
			waits := s.db.Stats().WaitCount
			type result struct {
				p *guiPanel
				e error
			}
			done := make(chan result, 1)
			go func() {
				p, e := guiModerationRead(s, h, u, guiScope{room: "lobby"}, r, "/ipusers", "192.0.2.1")
				done <- result{p, e}
			}()
			deadline := time.Now().Add(3 * time.Second)
			for s.db.Stats().WaitCount <= waits {
				if time.Now().After(deadline) {
					t.Fatal("not queued")
				}
				time.Sleep(time.Millisecond)
			}
			if change == "role" {
				_, e = blocker.Exec(`UPDATE users SET role='member' WHERE name=?`, u.Name)
			} else {
				_, e = blocker.Exec(`UPDATE sessions SET revoked=1 WHERE id=?`, r.sessionID)
			}
			if e != nil {
				t.Fatal(e)
			}
			if e = blocker.Commit(); e != nil {
				t.Fatal(e)
			}
			out := <-done
			if out.e == nil || out.p != nil {
				t.Fatal("stale authority", out)
			}
			var n int
			s.db.QueryRow(`SELECT count(*) FROM audit WHERE action='ip.lookup'`).Scan(&n)
			if n != 0 {
				t.Fatal(n)
			}
			// A stale User value cannot restore the revoked authority on reader queries.
			u.Role = "owner"
			for _, c := range []string{"/admin", "/whois"} {
				if p, e := guiModerationRead(s, h, u, guiScope{room: "lobby"}, r, c, "target"); e == nil || p != nil {
					t.Fatal("stale read authority", c, p, e)
				}
			}
		})
	}
}
