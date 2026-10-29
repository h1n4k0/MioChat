package main

import (
	"fmt"
	"testing"
)

func TestZonedIPv6PolicyColdHotAndEquivalentZones(t *testing.T) {
	s, _ := featureFixture(t)
	if e := s.Sanction("admin", "ip", "fe80::/64", "", "1h", "synthetic scoped peer", false); e != nil {
		t.Fatal(e)
	}
	for _, zone := range []string{"synthetic-a", "synthetic-b"} {
		peer := "fe80::1%" + zone
		s.invalidateIPPolicy()
		if !s.IPBannedContext(t.Context(), peer) {
			t.Errorf("ZONE_POLICY_COLD_ALLOWED %s", peer)
		}
		if !s.IPBannedContext(t.Context(), "fe80::1") {
			t.Fatal("unscoped control did not match")
		}
		if !s.IPBannedContext(t.Context(), peer) {
			t.Errorf("ZONE_POLICY_HOT_ALLOWED %s", peer)
		}
	}
	if s.IPBannedContext(t.Context(), "fe81::1%synthetic-a") {
		t.Fatal("zone removal broadened prefix")
	}
	if !s.IPBannedContext(t.Context(), "not-an-address") {
		t.Fatal("invalid trusted source must fail closed")
	}
}
func TestTUISelfIPConfirmationCannotBeSkippedByZoneOrInvalidAddress(t *testing.T) {
	for _, peer := range []string{"fe80::1%synthetic-a", "invalid-synthetic-ip"} {
		t.Run(peer, func(t *testing.T) {
			s, h := featureFixture(t)
			m := testModel(t, s, h, "admin")
			m.ip = peer
			m.command("/banip fe80::/64 1h synthetic unconfirmed self ban")
			var count int
			if e := s.db.QueryRow(`SELECT count(*) FROM sanctions WHERE kind='ip'`).Scan(&count); e != nil {
				t.Fatal(e)
			}
			if count != 0 {
				t.Error(fmt.Sprintf("UNCONFIRMED_SELF_IP_WRITE peer=%s rows=%d", peer, count))
			}
		})
	}
}
