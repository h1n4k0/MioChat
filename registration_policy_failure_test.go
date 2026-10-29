package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func registrationPolicyFault(t *testing.T, s *Store, fault string) {
	t.Helper()
	mode := "open"
	if strings.HasPrefix(fault, "mode-") {
		mode = "closed"
		if strings.Contains(fault, "scan") {
			mode = "invite"
		}
	}
	for _, q := range []string{fmt.Sprintf(`UPDATE config SET value='%s' WHERE key='registration'`, mode), `UPDATE config SET value='1' WHERE key='registration_quota'`} {
		if _, e := s.db.Exec(q); e != nil {
			t.Fatal(e)
		}
	}
	if fault == "count-sql" {
		if _, e := s.db.Exec(`INSERT INTO logins(user,ip,client,ok,at) VALUES('prior-synthetic','192.0.2.70','registration',1,?)`, time.Now().Unix()); e != nil {
			t.Fatal(e)
		}
		statements := []string{`ALTER TABLE logins RENAME TO registration_log_backing`,
			`CREATE VIEW logins AS SELECT id,user,CASE WHEN client='registration' THEN json_extract('invalid-json','$') ELSE ip END AS ip,client,ok,at FROM registration_log_backing`,
			`CREATE TRIGGER registration_forward INSTEAD OF INSERT ON logins BEGIN INSERT INTO registration_log_backing(user,ip,client,ok,at) VALUES(NEW.user,NEW.ip,NEW.client,NEW.ok,NEW.at); END`}
		for _, q := range statements {
			if _, e := s.db.Exec(q); e != nil {
				t.Fatal(e)
			}
		}
		var n int
		if e := s.db.QueryRow(`SELECT count(*) FROM logins WHERE ip=? AND client='registration' AND at>?`, "192.0.2.70", time.Now().Unix()-3600).Scan(&n); e == nil {
			t.Fatal("count fault did not trigger")
		}
		// The view deliberately still permits INSERT, so an old-policy failure can commit.
		if _, e := s.db.Exec(`INSERT INTO logins(user,ip,client,ok,at) VALUES('probe','192.0.2.71','probe',0,1)`); e != nil {
			t.Fatal("unrelated log write unavailable", e)
		}
		return
	}
	key := "registration"
	if strings.HasPrefix(fault, "quota-") {
		key = "registration_quota"
	}
	expr := "json_extract('invalid-json','$')"
	if strings.Contains(fault, "scan") {
		expr = "NULL"
	}
	if _, e := s.db.Exec(`ALTER TABLE config RENAME TO registration_config_backing`); e != nil {
		t.Fatal(e)
	}
	if _, e := s.db.Exec(fmt.Sprintf(`CREATE VIEW config AS SELECT key,CASE WHEN key='%s' THEN %s ELSE value END AS value FROM registration_config_backing`, key, expr)); e != nil {
		t.Fatal(e)
	}
	var value string
	if e := s.db.QueryRow(`SELECT value FROM config WHERE key=?`, key).Scan(&value); e == nil {
		t.Fatal("selected policy fault did not trigger")
	}
	other := "registration"
	if key == other {
		other = "registration_quota"
	}
	if e := s.db.QueryRow(`SELECT value FROM config WHERE key=?`, other).Scan(&value); e != nil {
		t.Fatal("unrelated policy also broken", e)
	}
}
func TestRegistrationPolicyReadFailuresNeverCommit(t *testing.T) {
	for _, fault := range []string{"mode-sql", "mode-scan", "quota-sql", "quota-scan", "count-sql"} {
		t.Run(fault, func(t *testing.T) {
			s := testStore(t)
			registrationPolicyFault(t, s, fault)
			_, e := s.RegisterInvite("new-policy-user", "synthetic-password-123", "synthetic-password-123", "", "192.0.2.70", "synthetic")
			if e == nil || !strings.Contains(e.Error(), "注册策略暂不可用") {
				t.Errorf("REGISTRATION_POLICY_NOT_REJECTED_AT_READ: %v", e)
			}
			for _, q := range []string{`SELECT count(*) FROM users WHERE name='new-policy-user'`, `SELECT count(*) FROM user_meta WHERE name='new-policy-user'`, `SELECT count(*) FROM badges WHERE user='new-policy-user'`} {
				var n int
				if e := s.db.QueryRow(q).Scan(&n); e != nil {
					t.Fatal(e)
				}
				if n != 0 {
					t.Errorf("REGISTRATION_POLICY_WROTE %s rows=%d", q, n)
				}
			}
			logs := "logins"
			if fault == "count-sql" {
				logs = "registration_log_backing"
			}
			var n int
			if e := s.db.QueryRow(`SELECT count(*) FROM ` + logs + ` WHERE user='new-policy-user' AND client='registration'`).Scan(&n); e != nil {
				t.Fatal(e)
			}
			if n != 0 {
				t.Errorf("REGISTRATION_POLICY_WROTE successful count log=%d", n)
			}
		})
	}
}
func TestGUIRegistrationPolicyReadFailureCannotAuthenticate(t *testing.T) {
	s := testStore(t)
	registrationPolicyFault(t, s, "mode-sql")
	f := newGUIWireFixture(t, s, newHub())
	f.send(guiRequest{Type: "register", RequestID: "policy-register", Username: "wire-policy-user", Password: "synthetic-password-123", Confirm: "synthetic-password-123"})
	r := f.read("error", "policy-register")
	if !strings.Contains(r.Error, "注册策略暂不可用") {
		t.Fatal("policy failure not returned", r.Error)
	}
	var n int
	if e := s.db.QueryRow(`SELECT count(*) FROM users WHERE name='wire-policy-user'`).Scan(&n); e != nil || n != 0 {
		t.Fatal("GUI registered under failed closed policy", n, e)
	}
	// Outer failed-login diagnostics are allowed; no successful registration count.
	if e := s.db.QueryRow(`SELECT count(*) FROM logins WHERE user='wire-policy-user' AND client='registration'`).Scan(&n); e != nil || n != 0 {
		t.Fatal(n, e)
	}
}

func TestRegistrationRequiredWritesRollbackInviteAndAccount(t *testing.T) {
	for _, table := range []string{"users", "user_meta", "badges", "logins"} {
		t.Run(table, func(t *testing.T) {
			s := testStore(t)
			registerUser(t, s, "registrar")
			if _, e := s.db.Exec(`UPDATE users SET role='owner' WHERE name='registrar'`); e != nil {
				t.Fatal(e)
			}
			token, e := s.InviteCode("registrar", "1h", 1)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = s.db.Exec(`UPDATE config SET value='invite' WHERE key='registration'`); e != nil {
				t.Fatal(e)
			}
			column := "name"
			if table == "badges" || table == "logins" {
				column = "user"
			}
			if _, e = s.db.Exec(fmt.Sprintf(`CREATE TRIGGER ignore_required_registration BEFORE INSERT ON %s WHEN NEW.%s='ignored-register' BEGIN SELECT RAISE(IGNORE); END`, table, column)); e != nil {
				t.Fatal(e)
			}
			_, e = s.RegisterInvite("ignored-register", "synthetic-password-123", "synthetic-password-123", token, "192.0.2.70", "synthetic")
			if e == nil {
				t.Errorf("REGISTRATION_REQUIRED_WRITE_FALSE_SUCCESS %s", table)
			}
			for _, entry := range []struct{ table, column string }{{"users", "name"}, {"user_meta", "name"}, {"badges", "user"}, {"logins", "user"}} {
				var n int
				if e := s.db.QueryRow(fmt.Sprintf(`SELECT count(*) FROM %s WHERE %s='ignored-register'`, entry.table, entry.column)).Scan(&n); e != nil {
					t.Fatal(e)
				}
				if n != 0 {
					t.Errorf("REGISTRATION_REQUIRED_WRITE_PARTIAL_COMMIT %s %s=%d", table, entry.table, n)
				}
			}
			var remaining int
			if e := s.db.QueryRow(`SELECT remaining FROM invites WHERE hash=?`, tokenHash(token)).Scan(&remaining); e != nil {
				t.Fatal(e)
			}
			if remaining != 1 {
				t.Errorf("REGISTRATION_REQUIRED_WRITE_CONSUMED_INVITE %s %d", table, remaining)
			}
		})
	}
}
