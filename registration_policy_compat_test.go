package main

import (
	"strings"
	"sync"
	"testing"
)

func TestRegistrationPolicyAbsenceCompatibilityAndInvalidValues(t *testing.T) {
	for _, missing := range []string{"registration", "registration_quota", "both"} {
		t.Run("missing-"+missing, func(t *testing.T) {
			s := testStore(t)
			for _, key := range []string{"registration", "registration_quota"} {
				if missing == key || missing == "both" {
					if _, e := s.db.Exec(`DELETE FROM config WHERE key=?`, key); e != nil {
						t.Fatal(e)
					}
				}
			}
			if _, e := s.RegisterInvite("policy-first", "synthetic-password-123", "synthetic-password-123", "", "192.0.2.90", "synthetic"); e != nil {
				t.Fatal("missing compatible default rejected", e)
			}
			_, e := s.RegisterInvite("policy-second", "synthetic-password-123", "synthetic-password-123", "", "192.0.2.90", "synthetic")
			if missing == "registration" {
				if e != nil {
					t.Fatal("normal default quota10 changed", e)
				}
			} else if e == nil || !strings.Contains(e.Error(), "配额") {
				t.Fatal("missing quota must default1", e)
			}
		})
	}
	for _, entry := range []struct{ key, value string }{{"registration", ""}, {"registration", "OPEN"}, {"registration", "unknown"}, {"registration_quota", ""}, {"registration_quota", "bad"}, {"registration_quota", "0"}, {"registration_quota", "101"}, {"registration_quota", "-1"}} {
		t.Run(entry.key+"="+entry.value, func(t *testing.T) {
			s := testStore(t)
			if _, e := s.db.Exec(`UPDATE config SET value=? WHERE key=?`, entry.value, entry.key); e != nil {
				t.Fatal(e)
			}
			if _, e := s.RegisterInvite("policy-invalid", "synthetic-password-123", "synthetic-password-123", "", "192.0.2.90", "synthetic"); e == nil || !strings.Contains(e.Error(), "注册策略暂不可用") {
				t.Fatal("invalid policy allowed", e)
			}
			var n int
			if e := s.db.QueryRow(`SELECT count(*) FROM users`).Scan(&n); e != nil || n != 0 {
				t.Fatal("invalid policy committed", n, e)
			}
		})
	}
	t.Run("internal-empty-ip-and-plus-quota", func(t *testing.T) {
		s := testStore(t)
		if _, e := s.db.Exec(`UPDATE config SET value='+1' WHERE key='registration_quota'`); e != nil {
			t.Fatal(e)
		}
		for _, name := range []string{"internal-first", "internal-second"} {
			if _, e := s.Register(name, "synthetic-password-123", "synthetic-password-123"); e != nil {
				t.Fatal("internal emptyIP compatibility changed", e)
			}
		}
	})
}
func TestRegistrationPolicyHealthyClosedInviteAndConcurrentQuota(t *testing.T) {
	t.Run("closed", func(t *testing.T) {
		s := testStore(t)
		if _, e := s.db.Exec(`UPDATE config SET value='closed' WHERE key='registration'`); e != nil {
			t.Fatal(e)
		}
		if _, e := s.RegisterInvite("policy-closed", "synthetic-password-123", "synthetic-password-123", "", "192.0.2.91", "synthetic"); e == nil || !strings.Contains(e.Error(), "暂停注册") {
			t.Fatal(e)
		}
	})
	for _, mode := range []string{"open", "invite"} {
		t.Run(mode, func(t *testing.T) {
			s := testStore(t)
			registerUser(t, s, "registrar")
			if _, e := s.db.Exec(`UPDATE users SET role='owner' WHERE name='registrar'`); e != nil {
				t.Fatal(e)
			}
			token := ""
			if mode == "invite" {
				var e error
				token, e = s.InviteCode("registrar", "1h", 1)
				if e != nil {
					t.Fatal(e)
				}
			}
			if _, e := s.db.Exec(`UPDATE config SET value=? WHERE key='registration'`, mode); e != nil {
				t.Fatal(e)
			}
			quota := "1"
			if mode == "invite" {
				quota = "100"
			} // Isolate one-use token contention from the open-mode IP quota.
			if _, e := s.db.Exec(`UPDATE config SET value=? WHERE key='registration_quota'`, quota); e != nil {
				t.Fatal(e)
			}
			if mode == "invite" {
				if _, e := s.RegisterInvite("missing-invite", "synthetic-password-123", "synthetic-password-123", "", "192.0.2.91", "synthetic"); e == nil {
					t.Fatal("invite required")
				}
			}
			start := make(chan struct{})
			results := make(chan error, 2)
			var wg sync.WaitGroup
			for _, name := range []string{"concurrent-first", "concurrent-second"} {
				wg.Add(1)
				go func(name string) {
					defer wg.Done()
					<-start
					_, e := s.RegisterInvite(name, "synthetic-password-123", "synthetic-password-123", token, "192.0.2.91", "synthetic")
					results <- e
				}(name)
			}
			close(start)
			wg.Wait()
			close(results)
			success := 0
			for e := range results {
				if e == nil {
					success++
				}
			}
			if success != 1 {
				t.Fatal("quota/token writer boundary", success)
			}
			var n int
			if e := s.db.QueryRow(`SELECT count(*) FROM logins WHERE ip='192.0.2.91' AND client='registration'`).Scan(&n); e != nil || n != 1 {
				t.Fatal(n, e)
			}
			if mode == "invite" {
				var n int
				if e := s.db.QueryRow(`SELECT remaining FROM invites WHERE hash=?`, tokenHash(token)).Scan(&n); e != nil || n != 0 {
					t.Fatal(n, e)
				}
			}
		})
	}
}
