package main

import "testing"

// This regression deliberately uses the existing TUI entry point, so the exact
// same test can serve as an old-source negative without referencing new helpers.
func TestTUIMaintenanceSettingsAndAuditsAreOneAtomicOperation(t *testing.T) {
	for _, fault := range []string{"config-ignore", "audit-abort", "audit-ignore", "later-write-changes-state"} {
		t.Run(fault, func(t *testing.T) {
			s, h := featureFixture(t)
			m := testModel(t, s, h, "admin")
			beforeState := storageScalar(t, s, `SELECT value FROM config WHERE key='maintenance'`)
			beforeNotice := storageScalar(t, s, `SELECT value FROM config WHERE key='maintenance_notice'`)
			beforeAudit := storageScalar(t, s, `SELECT count(*) FROM audit`)
			trigger := ""
			switch fault {
			case "config-ignore":
				trigger = `CREATE TRIGGER config_fault BEFORE UPDATE ON config WHEN NEW.key='maintenance_notice' BEGIN SELECT RAISE(IGNORE); END`
			case "audit-abort":
				trigger = `CREATE TRIGGER config_fault BEFORE INSERT ON audit WHEN NEW.action='config.set' AND NEW.target='maintenance_notice' BEGIN SELECT RAISE(ABORT,'synthetic audit fault'); END`
			case "audit-ignore":
				trigger = `CREATE TRIGGER config_fault BEFORE INSERT ON audit WHEN NEW.action='config.set' AND NEW.target='maintenance_notice' BEGIN SELECT RAISE(IGNORE); END`
			case "later-write-changes-state":
				trigger = `CREATE TRIGGER config_fault AFTER UPDATE ON config WHEN NEW.key='maintenance_notice' BEGIN UPDATE config SET value='off' WHERE key='maintenance'; END`
			}
			if _, err := s.db.Exec(trigger); err != nil {
				t.Fatal(err)
			}
			handled, _ := m.extendedCommand("/maintenance on synthetic-notice")
			if !handled {
				t.Fatal("existing maintenance TUI entry was not routed")
			}
			if after := storageScalar(t, s, `SELECT value FROM config WHERE key='maintenance'`); after != beforeState {
				t.Errorf("MAINTENANCE_PARTIAL_STATE: before=%q after=%q", beforeState, after)
			}
			if after := storageScalar(t, s, `SELECT value FROM config WHERE key='maintenance_notice'`); after != beforeNotice {
				t.Errorf("MAINTENANCE_PARTIAL_NOTICE: before=%q after=%q", beforeNotice, after)
			}
			if after := storageScalar(t, s, `SELECT count(*) FROM audit`); after != beforeAudit {
				t.Errorf("MAINTENANCE_PARTIAL_AUDIT: before=%s after=%s", beforeAudit, after)
			}
		})
	}
}
