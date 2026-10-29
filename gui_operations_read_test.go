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

func TestGUIOperationsReadRolesWindowPrivacyAndNoWrites(t *testing.T) {
	s, h, u, r, _ := moderationFixture(t)
	for i := 0; i < 105; i++ {
		if _, e := s.db.Exec(`INSERT INTO diagnostics(at,kind,detail) VALUES(?,?,?)`, i+1, "synthetic", fmt.Sprintf("private-synthetic-detail-%03d", i)); e != nil {
			t.Fatal(e)
		}
	}
	if _, e := s.SavePositionVersioned(u.Name, "room:lobby", Position{Draft: "ordinary draft", Anchor: 42, Offset: 3}); e != nil {
		t.Fatal(e)
	}
	before, e := s.PositionWithError(u.Name, "room:lobby")
	if e != nil {
		t.Fatal(e)
	}
	var audits int
	if e = s.db.QueryRow(`SELECT count(*) FROM audit`).Scan(&audits); e != nil {
		t.Fatal(e)
	}
	// These original reads do not invent audit writes. A rejecting audit trigger must not block them.
	if _, e = s.db.Exec(`CREATE TRIGGER reject_read_audit BEFORE INSERT ON audit BEGIN SELECT RAISE(ABORT,'synthetic read audit'); END`); e != nil {
		t.Fatal(e)
	}
	for _, role := range []string{"member", "moderator", "admin", "owner"} {
		if _, e = s.db.Exec(`UPDATE users SET role=? WHERE name=?`, role, u.Name); e != nil {
			t.Fatal(e)
		}
		for _, command := range []string{"/errors", "/health"} {
			r.Body = command
			res, err := guiExecuteAction(s, h, u, guiScope{room: "lobby"}, 7, r)
			allowed := role == "admin" || role == "owner"
			if (err == nil) != allowed {
				t.Fatal(role, command, err)
			}
			if !allowed {
				if res.panel != nil {
					t.Fatal("denied data")
				}
				continue
			}
			p := res.panel
			v := p.Moderation
			if p.Kind != "moderation" || p.Statistics != nil || p.Community != nil || res.replyTo != 7 || v.Viewer != u.Name || v.Command != command || v.OriginScope != "lobby" || v.Audited || v.Canonical != "" || len(p.Actions) != 0 || len(p.Lines) != 0 {
				t.Fatal("mixed/private metadata", p)
			}
			if command == "/errors" {
				if v.Health != nil || v.Window != 100 || len(v.Diagnostics) != 100 || v.Diagnostics[0].Detail != "private-synthetic-detail-104" || v.Diagnostics[99].Detail != "private-synthetic-detail-005" || v.Diagnostics[0].Time != time.Unix(105, 0).UTC().Format(time.RFC3339) {
					t.Fatal(v)
				}
				for i := 1; i < len(v.Diagnostics); i++ {
					if v.Diagnostics[i].ID >= v.Diagnostics[i-1].ID {
						t.Fatal("unordered")
					}
				}
			} else {
				if v.Health == nil || len(v.Diagnostics) != 0 || v.Window != 0 {
					t.Fatal(v)
				}
				public, err := guiStatisticsSnapshot(context.Background(), s, h, time.Now())
				if err != nil {
					t.Fatal(err)
				}
				// Compare the stable database aggregate contract, not runtime
				// samples taken at different instants or free-space readings.
				counters := func(v *guiStatistics) []any {
					return []any{v.Version, v.Users, v.Messages, v.ActiveSessions, v.MinuteMessages, v.HourLimited}
				}
				if !reflect.DeepEqual(counters(v.Health), counters(public)) {
					t.Fatal("health/public aggregate mismatch", counters(v.Health), counters(public))
				}

				raw, _ := json.Marshal(v.Health)
				for _, secret := range []string{"private-synthetic-detail", u.Name, s.path, "ordinary draft", "password", "192.0.2."} {
					if secret != "" && secret != ":memory:" && strings.Contains(string(raw), secret) {
						t.Fatal("health leak", secret)
					}
				}
			}
			r.Body = command + " unexpected"
			if x, err := guiExecuteAction(s, h, u, guiScope{room: "lobby"}, 7, r); err == nil || x.panel != nil {
				t.Fatal("unexpected args accepted")
			}
		}
	}
	after, e := s.PositionWithError(u.Name, "room:lobby")
	if e != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("read mutated position", e)
	}
	var n int
	if e = s.db.QueryRow(`SELECT count(*) FROM audit`).Scan(&n); e != nil || n != audits {
		t.Fatal("read audited", e, n)
	}
	r.Body = "/admin"
	res, e := guiExecuteAction(s, h, u, guiScope{room: "lobby"}, 0, r)
	if e != nil {
		t.Fatal(e)
	}
	for _, command := range []string{"/errors", "/health"} {
		found := false
		for _, a := range res.panel.Actions {
			if strings.TrimSpace(a.Command) == command {
				found = true
				if a.Fill {
					t.Fatal("no-input action is form")
				}
			}
		}
		if !found {
			t.Fatal("missing admin read", command)
		}
	}
}

func TestGUIOperationsReadRejectsFreshSessionAndQueuedRoleChanges(t *testing.T) {
	for _, command := range []string{"/errors", "/health"} {
		for _, change := range []string{"role", "revoked", "closed", "epoch", "reset", "banned", "wrong-owner", "missing", "cancel"} {
			t.Run(command+"/"+change, func(t *testing.T) {
				s, h, u, r, _ := moderationFixture(t)
				if _, e := s.db.Exec(`UPDATE users SET role='owner' WHERE name=?`, u.Name); e != nil {
					t.Fatal(e)
				}
				u.Role = "owner" // Intentionally stale authority; the reader must reload.
				if change == "missing" {
					r.sessionID = ""
				} else if change == "wrong-owner" {
					if _, e := s.db.Exec(`UPDATE sessions SET user='target' WHERE id=?`, r.sessionID); e != nil {
						t.Fatal(e)
					}
				} else if change == "cancel" {
					ctx, cancel := context.WithCancel(context.Background())
					cancel()
					r.ctx = ctx
				} else {
					s.readDB().SetMaxOpenConns(1)
					held, e := s.readDB().Conn(context.Background())
					if e != nil {
						t.Fatal(e)
					}
					defer held.Close()
					waits := s.readDB().Stats().WaitCount
					type result struct {
						p *guiPanel
						e error
					}
					done := make(chan result, 1)
					go func() {
						p, e := guiModerationRead(s, h, u, guiScope{room: "lobby"}, r, command, "")
						done <- result{p, e}
					}()
					deadline := time.Now().Add(time.Second)
					for s.readDB().Stats().WaitCount <= waits {
						if time.Now().After(deadline) {
							t.Fatal("not reader queued")
						}
						time.Sleep(time.Millisecond)
					}
					query := map[string]string{"role": `UPDATE users SET role='member' WHERE name=?`, "revoked": `UPDATE sessions SET revoked=1 WHERE user=?`, "closed": `UPDATE sessions SET closed=1 WHERE user=?`, "epoch": `INSERT INTO credential_versions(user,epoch) VALUES(?,3) ON CONFLICT(user) DO UPDATE SET epoch=3`, "reset": `UPDATE user_meta SET must_reset=1 WHERE name=?`, "banned": `UPDATE users SET banned=1 WHERE name=?`}[change]
					if _, e = s.db.Exec(query, u.Name); e != nil {
						t.Fatal(e)
					}
					if e = held.Close(); e != nil {
						t.Fatal(e)
					}
					out := <-done
					if out.e == nil || out.p != nil {
						t.Fatal("queued stale read returned data", out)
					}
					return
				}
				if p, e := guiModerationRead(s, h, u, guiScope{room: "lobby"}, r, command, ""); e == nil || p != nil {
					t.Fatal("invalid session read", p, e)
				}
			})
		}
	}
}

func TestGUIOperationsReadSQLAndScanFailureReturnNoPartial(t *testing.T) {
	for _, fault := range []string{"diagnostic-sql", "diagnostic-scan", "health-sql", "health-scan"} {
		t.Run(fault, func(t *testing.T) {
			s, h, u, r, _ := moderationFixture(t)
			if _, e := s.db.Exec(`UPDATE users SET role='admin' WHERE name=?`, u.Name); e != nil {
				t.Fatal(e)
			}
			command := "/errors"
			var statements []string
			switch fault {
			case "diagnostic-sql":
				statements = []string{`DROP TABLE diagnostics`}
			case "diagnostic-scan":
				statements = []string{`DROP TABLE diagnostics`, `CREATE VIEW diagnostics AS SELECT 2 AS id,1 AS at,'safe' AS kind,'first valid row' AS detail UNION ALL SELECT 1,'not-int','bad','private invalid'`}
			case "health-sql":
				command = "/health"
				statements = []string{`DROP TABLE rate_stats`}
			case "health-scan":
				command = "/health"
				statements = []string{`DROP TABLE rate_stats`, `CREATE VIEW rate_stats AS SELECT 1e100 AS count,CAST(strftime('%s','now') AS INTEGER)/3600 AS hour`}
			}
			for _, q := range statements {
				if _, e := s.db.Exec(q); e != nil {
					t.Fatal(e)
				}
			}
			if p, e := guiModerationRead(s, h, u, guiScope{room: "lobby"}, r, command, ""); e == nil || p != nil {
				t.Fatal("failed query leaked partial/zero data", p, e)
			}
		})
	}
}

func TestGUIOperationsReadRevokedKeyAndReaderBudget(t *testing.T) {
	for _, command := range []string{"/errors", "/health"} {
		t.Run(command, func(t *testing.T) {
			s, h, u, r, _ := moderationFixture(t)
			if _, e := s.db.Exec(`UPDATE users SET role='admin' WHERE name=?`, u.Name); e != nil {
				t.Fatal(e)
			}
			key, e := s.BindSSHKey(u.Name, "synthetic diagnostic key", testSSHKeyText(testSSHKeySigner(t).PublicKey()))
			if e != nil {
				t.Fatal(e)
			}
			if _, e = s.db.Exec(`INSERT INTO session_credentials(session_id,key_id) VALUES(?,?)`, r.sessionID, key.ID); e != nil {
				t.Fatal(e)
			}
			if _, e = s.db.Exec(`UPDATE ssh_keys SET revoked=1 WHERE id=?`, key.ID); e != nil {
				t.Fatal(e)
			}
			if p, e := guiModerationRead(s, h, u, guiScope{room: "lobby"}, r, command, ""); e == nil || p != nil {
				t.Fatal("revoked key returned data", p, e)
			}
			if _, e = s.db.Exec(`UPDATE ssh_keys SET revoked=0 WHERE id=?`, key.ID); e != nil {
				t.Fatal(e)
			}
			s.readDB().SetMaxOpenConns(1)
			held, e := s.readDB().Conn(t.Context())
			if e != nil {
				t.Fatal(e)
			}
			defer held.Close()
			start := time.Now()
			if p, e := guiModerationRead(s, h, u, guiScope{room: "lobby"}, r, command, ""); e == nil || p != nil {
				t.Fatal("reader acquisition budget ignored", p, e)
			}
			if time.Since(start) > 2*time.Second {
				t.Fatal("reader budget not bounded")
			}
		})
	}
}
