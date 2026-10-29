package main

import (
	"fmt"
	"testing"
	"time"
)

func TestSanctionRevokeIgnoreCannotClaimCommitted(t *testing.T) {
	for _, kind := range []string{"mute", "user"} {
		for _, partial := range []bool{false, true} {
			for _, native := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/partial=%t/native=%t", kind, partial, native), func(t *testing.T) {
					s, h, u, r, target := moderationFixture(t)
					s.db.Exec(`UPDATE users SET role='owner' WHERE name=?`, u.Name)
					room := ""
					command := "/unban"
					if kind == "mute" {
						room = "lobby"
						command = "/unmute"
					}
					// Both a live and an expired but unrevoked row must be removed; existing
					// revoked rows need not be rewritten. IGNORE must not yield a false receipt.
					for _, expiry := range []int64{0, time.Now().Unix() - 100} {
						if _, e := s.db.Exec(`INSERT INTO sanctions(kind,target,room,expires,reason,actor,at) VALUES(?,?,?,?,'fixture',?,?)`, kind, target.Name, room, expiry, u.Name, time.Now().Unix()); e != nil {
							t.Fatal(e)
						}
					}
					condition := ""
					if partial {
						condition = " AND OLD.expires<>0"
					}
					if _, e := s.db.Exec(`CREATE TRIGGER synthetic_revoke_ignore BEFORE UPDATE OF revoked ON sanctions WHEN NEW.revoked=1` + condition + ` BEGIN SELECT RAISE(IGNORE);END`); e != nil {
						t.Fatal(e)
					}
					var err error
					if native {
						var out guiActionResult
						out, err = sanctionAction(t, s, h, u, r, guiScope{room: "lobby"}, command+" target")
						if out.sanctionReceipt != nil || out.enforcePolicy {
							t.Errorf("REVOKE_FALSE_COMMITTED kind=%s partial=%t", kind, partial)
						}
					} else {
						err = s.Sanction(u.Name, kind, target.Name, room, "permanent", "fixture", true)
					}
					if err == nil {
						t.Error("REVOKE_IGNORED_ACCEPTED")
					}
					var total, revoked, audits int
					s.db.QueryRow(`SELECT count(*),sum(revoked) FROM sanctions`).Scan(&total, &revoked)
					s.db.QueryRow(`SELECT count(*) FROM audit WHERE action='sanction.remove'`).Scan(&audits)
					if total != 2 || revoked != 0 || audits != 0 {
						t.Errorf("REVOKE_PARTIAL_WRITE total=%d revoked=%d audit=%d", total, revoked, audits)
					}
				})
			}
		}
	}
}
func TestSanctionRevokeZeroMatchesAndExpiredRowsCompatibility(t *testing.T) {
	s, h, u, r, target := moderationFixture(t)
	s.db.Exec(`UPDATE users SET role='owner' WHERE name=?`, u.Name)
	for _, body := range []string{"/unmute target", "/ungmute target", "/unban target"} {
		out, e := sanctionAction(t, s, h, u, r, guiScope{room: "lobby"}, body)
		if e != nil || out.sanctionReceipt == nil || !out.sanctionReceipt.Committed {
			t.Fatal(body, out, e)
		}
	}
	if _, e := s.db.Exec(`INSERT INTO sanctions(kind,target,room,expires,reason,actor,at) VALUES('mute',?,'lobby',?,'old fixture',?,?)`, target.Name, time.Now().Unix()-100, u.Name, time.Now().Unix()); e != nil {
		t.Fatal(e)
	}
	if _, e := sanctionAction(t, s, h, u, r, guiScope{room: "lobby"}, "/unmute target"); e != nil {
		t.Fatal(e)
	}
	var revoked, audits int
	s.db.QueryRow(`SELECT revoked FROM sanctions`).Scan(&revoked)
	s.db.QueryRow(`SELECT count(*) FROM audit WHERE action='sanction.remove'`).Scan(&audits)
	if revoked != 1 || audits != 4 {
		t.Fatal(revoked, audits)
	}
}
