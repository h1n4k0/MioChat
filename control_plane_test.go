package main

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestRegistrationSourceBudgetCannotExhaustLoginBudget(t *testing.T) {
	h := newHub()
	for i := 0; i < maxAuthSources; i++ {
		if !h.authAllowed(fmt.Sprintf("2001:db8::%x", i+1), true) {
			t.Fatal("registration budget filled prematurely")
		}
	}
	if h.authAllowed("2001:db8:1::1", true) {
		t.Fatal("registration source budget was not bounded")
	}
	if !h.authAllowed("2001:db8:2::1", false) {
		t.Fatal("hourly registration identities starved ordinary login")
	}
}

func TestSearchReadersCannotStarveSessionHealth(t *testing.T) {
	s, h := featureFixture(t)
	m := testModel(t, s, h, "alice")
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
	started := time.Now()
	u, valid := s.SessionHealth(m.client)
	if !valid || u.Name != "alice" {
		t.Fatal("occupied search readers incorrectly invalidated a healthy session")
	}
	if time.Since(started) > time.Second {
		t.Fatal("health was queued behind search connections")
	}
}

func TestEquivalentMappedIPPrefixMatchesPeer(t *testing.T) {
	s, _ := featureFixture(t)
	if err := s.Sanction("admin", "ip", "::ffff:198.51.100.0/120", "", "1h", "test", false); err != nil {
		t.Fatal(err)
	}
	if !s.IPBanned("198.51.100.27") || !s.IPBanned("::ffff:198.51.100.27") {
		t.Fatal("mapped deny prefix did not match equivalent peer")
	}
	if s.IPBanned("198.51.101.27") {
		t.Fatal("mapped deny prefix became too broad")
	}
}

func TestLegacyWideMappedRuleDoesNotDenyUnrelatedPeers(t *testing.T) {
	s, _ := featureFixture(t)
	_, err := s.db.Exec(`INSERT INTO sanctions(kind,target,room,expires,reason,actor,at) VALUES('ip','::ffff:198.51.100.0/80','',0,'legacy','admin',1)`)
	if err != nil {
		t.Fatal(err)
	}
	s.invalidateIPPolicy()
	if s.IPBanned("198.51.100.27") || s.IPBanned("203.0.113.9") {
		t.Fatal("legacy IPv6 rule became a global/IPv4 deny")
	}
	if !s.IPBanned("::1") {
		t.Fatal("legacy wide IPv6 rule was silently discarded")
	}
}

func TestMaintenanceCommandDoesNotRunSQLOnUIThread(t *testing.T) {
	s, h := featureFixture(t)
	m := testModel(t, s, h, "admin")
	writer, err := s.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	cmd := m.startMaintenance("check")
	if cmd == nil || m.maintenanceCancel == nil {
		writer.Close()
		t.Fatal("maintenance was not queued")
	}
	if time.Since(started) > time.Second {
		writer.Close()
		t.Fatal("UI waited for maintenance SQL")
	}
	m.maintenanceCancel()
	writer.Close()
	result := cmd().(maintenanceResult)
	if result.err == nil {
		t.Fatal("cancelled queued maintenance succeeded")
	}
	m.finishMaintenance(result)
	if m.maintenanceCancel != nil {
		t.Fatal("completed maintenance retained its UI cancellation state")
	}
}
