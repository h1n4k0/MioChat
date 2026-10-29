package main

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

func boolValue(v bool) *bool { return &v }
func ipGeneration(s *Store) uint64 {
	s.ipPolicy.mu.Lock()
	defer s.ipPolicy.mu.Unlock()
	return s.ipPolicy.generation
}
func TestGUIIPTrustedSelfConfirmationAndManualSyntax(t *testing.T) {
	cases := []struct {
		name, peer, target, reason string
		force                      *bool
		ok                         bool
	}{
		{"native-unchecked", "192.0.2.8", "192.0.2.0/24", "--force in reason is not checkbox", boolValue(false), false},
		{"native-confirmed", "192.0.2.8", "::ffff:192.0.2.0/120", "synthetic", boolValue(true), true},
		{"manual-unconfirmed", "192.0.2.8", "192.0.2.0/24", "synthetic", nil, false},
		{"manual-confirmed", "192.0.2.8", "192.0.2.0/24", "synthetic --force", nil, true},
		{"other-network", "192.0.2.8", "198.51.100.8", "synthetic", boolValue(false), true},
		{"zoned-unconfirmed", "fe80::1%synthetic-a", "fe80::/64", "synthetic", boolValue(false), false},
		{"zoned-confirmed", "fe80::1%synthetic-b", "fe80::/64", "synthetic", boolValue(true), true},
		{"unknown-manual", "", "198.51.100.8", "--force", nil, false},
		{"invalid-native", "not-an-ip", "198.51.100.8", "synthetic", boolValue(true), false},
		{"internet", "192.0.2.8", "0.0.0.0/0", "synthetic", boolValue(true), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, h, u, r, _ := moderationFixture(t)
			s.db.Exec(`UPDATE users SET role='owner' WHERE name=?`, u.Name)
			r.connectionIP = c.peer
			r.SelfIPForce = c.force
			body := "/banip " + c.target + " 1h " + c.reason
			before := ipGeneration(s)
			out, e := sanctionAction(t, s, h, u, r, guiScope{room: "lobby"}, body)
			if (e == nil) != c.ok {
				t.Fatal(e, out)
			}
			var count, audit int
			s.db.QueryRow(`SELECT count(*) FROM sanctions`).Scan(&count)
			s.db.QueryRow(`SELECT count(*) FROM audit WHERE action='sanction.add'`).Scan(&audit)
			if !c.ok {
				if count != 0 || audit != 0 || out.sanctionReceipt != nil || out.enforcePolicy || ipGeneration(s) != before {
					t.Fatal("rejected IP wrote", count, audit, out)
				}
				return
			}
			v := out.sanctionReceipt
			if v == nil || !v.Committed || v.Kind != "ip" || v.Room != "" || v.RequestedCommand != body || v.RequestedTarget != c.target || v.Expires <= time.Now().Unix() || ipGeneration(s) != before+1 {
				t.Fatal(v)
			}
			if (v.SelfIPForce == nil) != (c.force == nil) || v.SelfIPForce != nil && *v.SelfIPForce != *c.force {
				t.Fatal("force association", v)
			}
			canonical, _ := normalizePrefix(c.target)
			if v.Target != canonical.String() || count != 1 || audit != 1 {
				t.Fatal(v, count, audit)
			}
		})
	}
}
func TestGUIIPJSONCannotSupplyTrustedAddressAndOtherActionsRejectForce(t *testing.T) {
	var r guiRequest
	if e := json.Unmarshal([]byte(`{"type":"command","body":"/banip 192.0.2.0/24 1h synthetic","self_ip_force":false,"connectionIP":"198.51.100.8","connection_ip":"198.51.100.8","IP":"198.51.100.8"}`), &r); e != nil {
		t.Fatal(e)
	}
	if r.connectionIP != "" {
		t.Fatal("untrusted connection override")
	}
	s, h, u, auth, _ := moderationFixture(t)
	s.db.Exec(`UPDATE users SET role='owner' WHERE name=?`, u.Name)
	r.ctx = auth.ctx
	r.sessionID = auth.sessionID
	r.connectionIP = "192.0.2.8"
	if _, e := guiExecuteAction(s, h, u, guiScope{room: "lobby"}, 0, r); e == nil {
		t.Fatal("forged address bypassed ownIP checkbox")
	}
	for _, body := range []string{"/unbanip 198.51.100.8", "/ban target", "/mute target", "/stats"} {
		r.Body = body
		if _, e := guiExecuteAction(s, h, u, guiScope{room: "lobby"}, 0, r); e == nil {
			t.Fatal("inappropriate confirmation accepted", body)
		}
	}
}
func TestGUIIPAuditAtomicCacheInvalidationAndCanonicalRemoval(t *testing.T) {
	s, h, u, r, _ := moderationFixture(t)
	s.db.Exec(`UPDATE users SET role='owner' WHERE name=?`, u.Name)
	r.connectionIP = "192.0.2.8"
	r.SelfIPForce = boolValue(false)
	if s.IPBanned("198.51.100.27") {
		t.Fatal("fixture already denied")
	}
	before := ipGeneration(s)
	for _, fault := range []string{"ABORT,'synthetic IP audit'", "IGNORE"} {
		if _, e := s.db.Exec(`CREATE TRIGGER synthetic_ip_audit BEFORE INSERT ON audit WHEN NEW.action LIKE 'sanction.%' BEGIN SELECT RAISE(` + fault + `);END`); e != nil {
			t.Fatal(e)
		}
		if out, e := sanctionAction(t, s, h, u, r, guiScope{room: "lobby"}, "/banip ::ffff:198.51.100.0/120 1h synthetic"); e == nil || out.sanctionReceipt != nil || out.enforcePolicy {
			t.Fatal(out, e)
		}
		if ipGeneration(s) != before || s.IPBanned("198.51.100.27") {
			t.Fatal("failed commit invalidated policy/cache")
		}
		s.db.Exec(`DROP TRIGGER synthetic_ip_audit`)
	}
	out, e := sanctionAction(t, s, h, u, r, guiScope{room: "lobby"}, "/banip ::ffff:198.51.100.0/120 1h synthetic")
	if e != nil || out.sanctionReceipt.Target != "198.51.100.0/24" || !s.IPBanned("198.51.100.27") {
		t.Fatal(out, e)
	}
	// Preserve legacy equivalent row spelling; removal revokes rather than rewrites.
	s.db.Exec(`INSERT INTO sanctions(kind,target,room,expires,reason,actor,at) VALUES('ip','::ffff:198.51.100.0/120','',0,'legacy fixture',?,1)`, u.Name)
	r.SelfIPForce = nil
	before = ipGeneration(s)
	for _, fault := range []string{"ABORT,'synthetic revoke audit'", "IGNORE"} {
		if _, e := s.db.Exec(`CREATE TRIGGER synthetic_revoke_ip_audit BEFORE INSERT ON audit WHEN NEW.action='sanction.remove' BEGIN SELECT RAISE(` + fault + `);END`); e != nil {
			t.Fatal(e)
		}
		if out, e := sanctionAction(t, s, h, u, r, guiScope{room: "lobby"}, "/unbanip 198.51.100.0/24"); e == nil || out.sanctionReceipt != nil {
			t.Fatal(out, e)
		}
		if ipGeneration(s) != before || !s.IPBanned("198.51.100.27") {
			t.Fatal("failed revoke changed cache/policy")
		}
		s.db.Exec(`DROP TRIGGER synthetic_revoke_ip_audit`)
	}
	out, e = sanctionAction(t, s, h, u, r, guiScope{room: "lobby"}, "/unbanip 198.51.100.0/24")
	if e != nil || !out.sanctionReceipt.Remove || s.IPBanned("198.51.100.27") {
		t.Fatal(out, e)
	}
	var count int
	s.db.QueryRow(`SELECT count(*) FROM sanctions WHERE revoked=1`).Scan(&count)
	if count != 2 {
		t.Fatal(count)
	}
	var spelling string
	s.db.QueryRow(`SELECT target FROM sanctions WHERE reason='legacy fixture'`).Scan(&spelling)
	if spelling != "::ffff:198.51.100.0/120" {
		t.Fatal(spelling)
	}
}
func TestGUIIPCommittedPolicyClosesRealTargetBeforeFailedACK(t *testing.T) {
	s, h, u, r, target := moderationFixture(t)
	s.db.Exec(`UPDATE users SET role='owner' WHERE name=?`, u.Name)
	r.connectionIP = "192.0.2.8"
	r.SelfIPForce = boolValue(false)
	closed := make(chan struct{}, 1)
	c := &Client{Name: target.Name, ID: "synthetic-ip-target", IP: "198.51.100.9", Events: make(chan Event, 32), Close: func() { closed <- struct{}{} }, Ctx: t.Context()}
	if e := h.Join(c); e != nil {
		t.Fatal(e)
	}
	defer h.Leave(c)
	out, e := sanctionAction(t, s, h, u, r, guiScope{room: "lobby"}, "/banip 198.51.100.0/24 1h synthetic")
	if e != nil {
		t.Fatal(e)
	}
	if e = guiCompleteAction(&guiWriter{e: json.NewEncoder(failedSanctionWriter{})}, h, guiResponse{Type: "action", SanctionReceipt: out.sanctionReceipt}, out.enforcePolicy); e != io.ErrClosedPipe {
		t.Fatal(e)
	}
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("target IP policy not applied")
	}
}
func TestTUIZonedSelfIPExplicitForceUsesSameAddressNormalization(t *testing.T) {
	s, h := featureFixture(t)
	m := testModel(t, s, h, "admin")
	m.ip = "fe80::1%synthetic-a"
	m.command("/banip fe80::/64 1h synthetic --force")
	var target string
	if e := s.db.QueryRow(`SELECT target FROM sanctions WHERE kind='ip'`).Scan(&target); e != nil || !strings.HasPrefix(target, "fe80:") {
		t.Fatal(target, e)
	}
}

func TestGUIIPNativeRequiresFreshAdminAndDurableSession(t *testing.T) {
	for _, change := range []string{"member", "missing-session", "revoked-session", "reset"} {
		t.Run(change, func(t *testing.T) {
			s, h, u, r, _ := moderationFixture(t)
			s.db.Exec(`UPDATE users SET role='admin' WHERE name=?`, u.Name)
			r.connectionIP = "192.0.2.8"
			r.SelfIPForce = boolValue(false)
			switch change {
			case "member":
				s.db.Exec(`UPDATE users SET role='member' WHERE name=?`, u.Name)
			case "missing-session":
				r.sessionID = ""
			case "revoked-session":
				s.db.Exec(`UPDATE sessions SET revoked=1 WHERE id=?`, r.sessionID)
			case "reset":
				s.db.Exec(`UPDATE user_meta SET must_reset=1 WHERE name=?`, u.Name)
			}
			before := ipGeneration(s)
			out, e := sanctionAction(t, s, h, u, r, guiScope{room: "lobby"}, "/banip 198.51.100.0/24 1h synthetic")
			if e == nil || out.sanctionReceipt != nil || out.enforcePolicy || ipGeneration(s) != before {
				t.Fatal(change, out, e)
			}
			var count int
			s.db.QueryRow(`SELECT count(*) FROM sanctions`).Scan(&count)
			if count != 0 {
				t.Fatal(count)
			}
		})
	}
}
