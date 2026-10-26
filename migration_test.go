package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	gossh "golang.org/x/crypto/ssh"
)

func migrationRows(t *testing.T, db queryer) map[string][]string {
	return migrationRowsExcept(t, db, "schema_migrations", "ssh_keys", "session_credentials", "credential_versions", "session_versions", "position_versions", "draft_conflicts")
}

func migrationRowsExcept(t *testing.T, db queryer, excluded ...string) map[string][]string {
	t.Helper()
	args := make([]any, len(excluded))
	for i, name := range excluded {
		args[i] = name
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' AND name NOT IN (`+marks+`) ORDER BY name`, args...)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var table string
		if err = rows.Scan(&table); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, table)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	rows.Close()
	result := map[string][]string{}
	for _, table := range tables {
		rows, err = db.Query(`SELECT * FROM "` + strings.ReplaceAll(table, `"`, `""`) + `"`)
		if err != nil {
			t.Fatal(table, err)
		}
		columns, _ := rows.Columns()
		result[table] = []string{}
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range pointers {
				pointers[i] = &values[i]
			}
			if err = rows.Scan(pointers...); err != nil {
				t.Fatal(table, err)
			}
			encoded, err := json.Marshal(values)
			if err != nil {
				t.Fatal(err)
			}
			result[table] = append(result[table], string(encoded))
		}
		if err = rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		sort.Strings(result[table])
	}
	return result
}

func TestSchemaThreeToFourPreservesKeysSessionsRowsAndHostIdentity(t *testing.T) {
	state := t.TempDir()
	path := filepath.Join(state, "miochat.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	for _, statement := range []string{
		legacySchema,
		`INSERT INTO rooms VALUES('random','保留随机频道',''),('arcade','保留游戏频道','');
INSERT INTO users(name,hash,salt,bio,theme,role,created) VALUES('compat_alice',x'01',x'02','旧版简介','sakura','admin',1700000000),('compat_bob',x'03',x'04','','aurora','member',1700000001);
INSERT INTO messages VALUES(701,'lobby','compat_alice','','旧版中文消息','chat',1700000000),(702,'','compat_alice','compat_bob','旧版私信','chat',1700000001);
INSERT INTO pins VALUES('lobby',701);`,
		schemaBaseSQL,
		schemaKeysSQL,
		`INSERT INTO schema_migrations VALUES(2,1700000002),(3,1700000003); PRAGMA user_version=3;
UPDATE user_meta SET last_login=1700000010,avatar='star',settings='{"motion":"off","custom":"preserve-me"}' WHERE name='compat_alice';
INSERT INTO preferences(user,scope,draft,last_read,reply_to,anchor,offset) VALUES('compat_alice','room:lobby','旧版未发送草稿',701,701,701,12);
INSERT INTO relations VALUES('compat_alice','compat_bob','note','旧版备注',1700000000);
INSERT INTO audit(actor,action,target,detail,at) VALUES('compat_alice','test.baseline','compat_bob','旧版审计',1700000000);
INSERT INTO message_meta(id,reply_to) VALUES(701,0),(702,0);
INSERT INTO sessions(id,user,ip,client,started,seen,closed,revoked) VALUES('v3-active','compat_alice','192.0.2.1','旧版客户端',1700000000,1700000001,0,0),('v3-closed','compat_alice','192.0.2.2','closed',1700000000,1700000001,1,0),('v3-revoked','compat_alice','192.0.2.3','revoked',1700000000,1700000001,0,1);
PRAGMA journal_mode=WAL;`,
	} {
		if _, err = db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	salt := []byte("schema-three-compatibility-salt")
	if _, err = db.Exec(`UPDATE users SET hash=?,salt=? WHERE name='compat_alice'`, passwordHash("compatibility-password-123", salt), salt); err != nil {
		t.Fatal(err)
	}
	activeKey, revokedKey := keyUITestSigner(t).PublicKey(), keyUITestSigner(t).PublicKey()
	for i, public := range []gossh.PublicKey{activeKey, revokedKey} {
		if _, err = db.Exec(`INSERT INTO ssh_keys(id,user,fingerprint,public_key,algorithm,label,created,last_used,revoked) VALUES(?,'compat_alice',?,?,'Ed25519',?,1700000004,1700000005,?)`, i+1, gossh.FingerprintSHA256(public), strings.TrimSpace(string(gossh.MarshalAuthorizedKey(public))), fmt.Sprintf("旧版设备%d", i+1), i); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = db.Exec(`INSERT INTO session_credentials VALUES('v3-active',1),('v3-closed',1),('v3-revoked',2)`); err != nil {
		t.Fatal(err)
	}
	excluded := []string{"schema_migrations", "credential_versions", "session_versions", "position_versions", "draft_conflicts"}
	before := migrationRowsExcept(t, db, excluded...)
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	hostPath := filepath.Join(state, "host_ed25519")
	if _, err = hostSigner(hostPath); err != nil {
		t.Fatal(err)
	}
	hostBefore, err := digestFile(hostPath)
	if err != nil {
		t.Fatal(err)
	}
	s, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.db.Close()
	if after := migrationRowsExcept(t, s.db, excluded...); !reflect.DeepEqual(before, after) {
		t.Error("schema 3 to 4 changed the original table set or rows")
		for name := range before {
			if !reflect.DeepEqual(before[name], after[name]) {
				t.Error("schema 3 to 4 changed original rows", name)
			}
		}
	}
	var version, oldSteps, credentialRows, sessionRows, positionRows int
	if err = s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 4 {
		t.Fatal("schema 4 was not installed", err, version)
	}
	if err = s.db.QueryRow(`SELECT count(*) FROM schema_migrations WHERE (version=2 AND applied=1700000002) OR (version=3 AND applied=1700000003)`).Scan(&oldSteps); err != nil || oldSteps != 2 {
		t.Fatal("old migration records changed", err, oldSteps)
	}
	if err = s.db.QueryRow(`SELECT count(*) FROM credential_versions WHERE epoch=0`).Scan(&credentialRows); err != nil || credentialRows != 2 {
		t.Fatal("credential epochs were not backfilled", err, credentialRows)
	}
	if err = s.db.QueryRow(`SELECT count(*) FROM session_versions WHERE epoch=0`).Scan(&sessionRows); err != nil || sessionRows != 3 {
		t.Fatal("session epochs were not backfilled", err, sessionRows)
	}
	if err = s.db.QueryRow(`SELECT count(*) FROM position_versions WHERE version=0`).Scan(&positionRows); err != nil || positionRows != 1 {
		t.Fatal("draft versions were not backfilled", err, positionRows)
	}
	if !s.SessionValid("v3-active") || s.SessionValid("v3-closed") || s.SessionValid("v3-revoked") {
		t.Fatal("migration changed the meaning of old session state")
	}
	if err = s.db.Close(); err != nil {
		t.Fatal(err)
	}
	backups, _ := filepath.Glob(path + ".pre-v4-*.bak")
	if len(backups) != 1 {
		t.Fatal("missing single schema-3 backup", backups)
	}
	backup, err := sql.Open("sqlite", backups[0])
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	if err = backup.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 3 {
		t.Fatal("backup is not the original schema 3", err, version)
	}
	if !reflect.DeepEqual(before, migrationRowsExcept(t, backup, excluded...)) {
		t.Fatal("upgrade backup did not preserve all schema-3 rows including keys/session credentials")
	}
	backup.Close()
	s, err = openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.db.Close()
	if !reflect.DeepEqual(before, migrationRowsExcept(t, s.db, excluded...)) {
		t.Fatal("reopening schema 4 changed original rows")
	}
	again, _ := filepath.Glob(path + ".pre-v4-*.bak")
	hostAfter, err := digestFile(hostPath)
	if err != nil || hostAfter != hostBefore || len(again) != 1 {
		t.Fatal("reopen replaced host identity or repeated the upgrade", err, hostBefore, hostAfter, len(again))
	}
	if user, err := s.Authenticate("compat_alice", "compatibility-password-123"); err != nil || user.Role != "admin" {
		t.Fatal("old password or role no longer works", err)
	}
	if user, err := s.AuthenticateSSHKey(activeKey); err != nil || user.Name != "compat_alice" {
		t.Fatal("old active SSH key no longer works", err)
	}
	if _, err = s.AuthenticateSSHKey(revokedKey); err == nil {
		t.Fatal("migration revived an old revoked SSH key")
	}
}

func TestStorageMigrationPreservesEveryOriginalTableAndHostKey(t *testing.T) {
	state := t.TempDir()
	path := filepath.Join(state, "miochat.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	salt := []byte("synthetic-storage-salt")
	hash := passwordHash("compatibility-password-123", salt)
	for _, statement := range []string{legacySchema, `INSERT INTO rooms VALUES('random','保留随机频道',''),('arcade','保留游戏频道','');INSERT INTO users(name,hash,salt,bio,role,created) VALUES('compat_alice',x'01',x'02','原有简介','admin',1700000000),('compat_bob',x'03',x'04','','member',1700000001);INSERT INTO messages VALUES(900,'lobby','compat_alice','','原有中文聊天','chat',1700000000),(901,'','compat_alice','compat_bob','原有私信','chat',1700000001);INSERT INTO pins VALUES('lobby',900)`, schemaBaseSQL, `INSERT INTO schema_migrations VALUES(2,1700000000);PRAGMA user_version=2;UPDATE user_meta SET settings='{"motion":"off","custom":"preserve-me"}' WHERE name='compat_alice';INSERT INTO preferences(user,scope,draft,last_read) VALUES('compat_alice','room:lobby','原有草稿',900);INSERT INTO relations VALUES('compat_alice','compat_bob','note','原有备注',1700000000);INSERT INTO audit(actor,action,target,detail,at) VALUES('compat_alice','test.baseline','compat_bob','旧审计',1700000000);PRAGMA journal_mode=WAL`} {
		if _, err = db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = db.Exec(`UPDATE users SET hash=?,salt=? WHERE name='compat_alice'`, hash, salt); err != nil {
		t.Fatal(err)
	}
	before := migrationRows(t, db)
	db.Close()
	if _, err = hostSigner(filepath.Join(state, "host_ed25519")); err != nil {
		t.Fatal(err)
	}
	hostBefore, _ := digestFile(filepath.Join(state, "host_ed25519"))
	s, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if after := migrationRows(t, s.db); !reflect.DeepEqual(before, after) {
		for name := range before {
			if !reflect.DeepEqual(before[name], after[name]) {
				t.Error("migration changed original table", name)
			}
		}
	}
	if user, err := s.Authenticate("compat_alice", "compatibility-password-123"); err != nil || user.Role != "admin" {
		t.Fatal("stored password or role changed", err)
	}
	var version, applied int
	s.db.QueryRow(`PRAGMA user_version`).Scan(&version)
	s.db.QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&applied)
	if version != schemaVersion || applied != schemaVersion-1 {
		t.Fatal("missing additive migration", version, applied)
	}
	s.db.Close()
	backups, _ := filepath.Glob(fmt.Sprintf("%s.pre-v%d-*.bak", path, schemaVersion))
	if len(backups) != 1 {
		t.Fatal("missing single complete pre-v3 snapshot", backups)
	}
	old, err := sql.Open("sqlite", backups[0])
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, migrationRows(t, old)) {
		t.Fatal("migration backup lost stored rows")
	}
	old.Close()
	s, err = openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.db.Close()
	if !reflect.DeepEqual(before, migrationRows(t, s.db)) {
		t.Fatal("repeat migration changed old data")
	}
	again, _ := filepath.Glob(fmt.Sprintf("%s.pre-v%d-*.bak", path, schemaVersion))
	hostAfter, _ := digestFile(filepath.Join(state, "host_ed25519"))
	if len(again) != 1 || hostBefore != hostAfter {
		t.Fatal("repeat migration or host-key replacement")
	}
}
