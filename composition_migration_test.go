package main

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSchemaFourCompositionMigrationPreservesRowsAndBackfillsCAS(t *testing.T) {
	// Synthetic rollback of the composition-v5 layer, preserving later additive
	// tables. Reopen verifies the v4 marker upgrade and original rows, not a
	// historical pure-v4 image of the entire database.
	s, _ := featureFixture(t)
	for _, kind := range []string{"post", "art"} {
		if err := s.SaveComposition("alice", "lobby", kind, "old title", "旧草稿\n保留"); err != nil {
			t.Fatal(err)
		}
	}
	_, err := s.db.Exec(`DROP TRIGGER composition_version_insert;DROP TRIGGER composition_version_update;DROP TRIGGER composition_version_delete;DROP TRIGGER composition_version_no_delete;DROP TRIGGER composition_version_monotonic;DROP TABLE composition_versions;DELETE FROM schema_migrations WHERE version=5;PRAGMA user_version=4;`)
	if err != nil {
		t.Fatal(err)
	}
	before := migrationRowsExcept(t, s.db, "schema_migrations", "composition_versions")
	if err = s.db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openStore(s.path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.db.Close()
	if after := migrationRowsExcept(t, reopened.db, "schema_migrations", "composition_versions"); !reflect.DeepEqual(before, after) {
		t.Fatal("v4 original rows changed")
	}
	if got := storageScalar(t, reopened, `PRAGMA user_version`); got != fmt.Sprint(schemaVersion) {
		t.Fatal("not current schema after v4 migration", got)
	}
	if got := storageScalar(t, reopened, `SELECT count(*) FROM composition_versions WHERE user='alice' AND room='lobby' AND version=1`); got != "2" {
		t.Fatal("existing drafts not backfilled", got)
	}
	if got := storageScalar(t, reopened, `SELECT count(*) FROM pragma_table_info('compositions')`); got != "6" {
		t.Fatal("old six-column shape changed", got)
	}
	backups, err := filepath.Glob(fmt.Sprintf("%s.pre-v%d-*.bak", s.path, schemaVersion))
	if err != nil || len(backups) != 1 {
		t.Fatal("missing schema4 upgrade backup", backups, err)
	}
	backup, err := sql.Open("sqlite", backups[0])
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	var version int
	if err = backup.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 4 {
		t.Fatal("backup is not original4", version, err)
	}
	if !reflect.DeepEqual(before, migrationRowsExcept(t, backup, "schema_migrations", "composition_versions")) {
		t.Fatal("schema4 backup rows changed")
	}
	if _, err = reopened.db.Exec(`DELETE FROM compositions WHERE user='alice' AND room='lobby' AND kind='post'`); err != nil {
		t.Fatal(err)
	}
	if got := storageScalar(t, reopened, `SELECT version FROM composition_versions WHERE user='alice' AND room='lobby' AND kind='post'`); got != "2" {
		t.Fatal("delete failed to retain tombstone", got)
	}
	if _, err = reopened.db.Exec(`DELETE FROM composition_versions WHERE user='alice'`); err == nil {
		t.Fatal("durable tombstones deletable")
	}
	if _, err = reopened.db.Exec(`UPDATE composition_versions SET version=0 WHERE user='alice'`); err == nil {
		t.Fatal("durable versions decrementable")
	}
}

func TestCompositionReadFailuresAndBoundsNeverCreateZeroBaseline(t *testing.T) {
	for _, fault := range []string{"reader", "version-table"} {
		t.Run(fault, func(t *testing.T) {
			s, h := featureFixture(t)
			m := testModel(t, s, h, "alice")
			m.startCompose("post", "read-fault")
			if fault == "reader" {
				if err := s.db.reader.Close(); err != nil {
					t.Fatal(err)
				}
				out, err := s.CompositionForSession(t.Context(), "alice", m.client.ID, "lobby", "post")
				if err == nil || out.Exists || out.Body != "" {
					t.Fatal("read failure became an absent snapshot", out, err)
				}
			} else {
				if _, err := s.db.Exec(`DROP TABLE composition_versions`); err != nil {
					t.Fatal(err)
				}
				out, err := s.SaveCompositionForSession(t.Context(), "alice", m.client.ID, "lobby", "post", 0, "false baseline", "unexpected", false)
				if err == nil || out.Exists {
					t.Fatal("version read failure became zero", out, err)
				}
				if got := storageScalar(t, s, `SELECT body FROM compositions WHERE user='alice' AND room='lobby' AND kind='post'`); got != "" {
					t.Fatal("version fault wrote body", got)
				}
			}
		})
	}
	s, h := featureFixture(t)
	m := testModel(t, s, h, "alice")
	m.startCompose("art", "bounds")
	for _, body := range []string{strings.Repeat("x\n", 32) + "x", strings.Repeat("中", 161), strings.Repeat(strings.Repeat("x", 160)+"\n", 25) + strings.Repeat("x", 97)} {
		if _, err := s.SaveCompositionForSession(t.Context(), "alice", m.client.ID, "lobby", "art", m.composeVersion, "bounds", body, false); err == nil {
			t.Fatal("composition bounds bypassed")
		}
		if got := storageScalar(t, s, `SELECT body FROM compositions WHERE user='alice' AND room='lobby' AND kind='art'`); got != "" {
			t.Fatal("invalid save changed draft", got)
		}
	}
}

func TestCompositionLegacySQLOverflowCannotResetOrPartiallyWrite(t *testing.T) {
	for _, action := range []string{"update", "delete", "insert"} {
		t.Run(action, func(t *testing.T) {
			s, _ := featureFixture(t)
			if err := s.SaveComposition("alice", "lobby", "post", "original", "retain me"); err != nil {
				t.Fatal(err)
			}
			if action == "insert" {
				if _, err := s.db.Exec(`DELETE FROM compositions WHERE user='alice' AND room='lobby' AND kind='post'`); err != nil {
					t.Fatal(err)
				}
			}
			if err := changedOne(s.db.Exec(`UPDATE composition_versions SET version=9223372036854775807 WHERE user='alice' AND room='lobby' AND kind='post'`)); err != nil {
				t.Fatal(err)
			}
			before := migrationRowsExcept(t, s.db, "schema_migrations")
			query := map[string]string{
				"update": `UPDATE compositions SET body='must not write' WHERE user='alice' AND room='lobby' AND kind='post'`,
				"delete": `DELETE FROM compositions WHERE user='alice' AND room='lobby' AND kind='post'`,
				"insert": `INSERT INTO compositions VALUES('alice','lobby','post','must not write','must not write',1)`,
			}[action]
			if _, err := s.db.Exec(query); err == nil {
				t.Fatal("legacy SQL overflow did not fail")
			}
			if after := migrationRowsExcept(t, s.db, "schema_migrations"); !reflect.DeepEqual(before, after) {
				t.Fatal("legacy overflow partially wrote or reset version")
			}
		})
	}
}

func TestCompositionBackupRestorePreservesDraftTombstoneAndTriggers(t *testing.T) {
	state := t.TempDir()
	s, err := openStore(filepath.Join(state, "miochat.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.db.Close()
	registerUser(t, s, "alice")
	if _, err = hostSigner(filepath.Join(state, "host_ed25519")); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"post", "art"} {
		if err = s.SaveComposition("alice", "lobby", kind, "snapshot title", "snapshot body"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.db.Exec(`DELETE FROM compositions WHERE user='alice' AND room='lobby' AND kind='art'`); err != nil {
		t.Fatal(err)
	}
	beforeDraft := storageScalar(t, s, `SELECT body FROM compositions WHERE kind='post'`)
	beforePost := storageScalar(t, s, `SELECT version FROM composition_versions WHERE kind='post'`)
	beforeTombstone := storageScalar(t, s, `SELECT version FROM composition_versions WHERE kind='art'`)
	backup := filepath.Join(t.TempDir(), "snapshot")
	if _, err = MakeSnapshot(state, backup); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveComposition("alice", "lobby", "post", "newer title", "newer body"); err != nil {
		t.Fatal(err)
	}
	if err = s.db.Close(); err != nil {
		t.Fatal(err)
	}
	if err = RestoreSnapshot(state, backup); err != nil {
		t.Fatal(err)
	}
	restored, err := openStore(filepath.Join(state, "miochat.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer restored.db.Close()
	if storageScalar(t, restored, `SELECT body FROM compositions WHERE kind='post'`) != beforeDraft || storageScalar(t, restored, `SELECT version FROM composition_versions WHERE kind='post'`) != beforePost || storageScalar(t, restored, `SELECT version FROM composition_versions WHERE kind='art'`) != beforeTombstone || storageScalar(t, restored, `SELECT count(*) FROM compositions WHERE kind='art'`) != "0" {
		t.Fatal("restore lost draft/tombstone identity")
	}
	if err = restored.SaveComposition("alice", "lobby", "post", "after restore", "after restore"); err != nil {
		t.Fatal(err)
	}
	if err = restored.SaveComposition("alice", "lobby", "art", "recreated", "recreated"); err != nil {
		t.Fatal(err)
	}
	if storageScalar(t, restored, `SELECT version FROM composition_versions WHERE kind='post'`) != "2" || storageScalar(t, restored, `SELECT version FROM composition_versions WHERE kind='art'`) != "3" {
		t.Fatal("restored triggers did not advance existing/tombstone versions")
	}
	rescues, err := filepath.Glob(filepath.Join(state, "restore-rescue-*"))
	if err != nil || len(rescues) != 1 {
		t.Fatal("newer source not rescued", rescues, err)
	}
}
