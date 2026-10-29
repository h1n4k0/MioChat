package main

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"
)

const schemaVersion = 7
const appVersion = "2.1.1"

// Additive database migrations preserve account credentials, message IDs and
// existing SQL projections.
const schemaBaseSQL = `
CREATE TABLE IF NOT EXISTS schema_migrations(version INTEGER PRIMARY KEY, applied INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS user_meta(uid INTEGER PRIMARY KEY AUTOINCREMENT,name TEXT NOT NULL UNIQUE REFERENCES users(name),last_login INTEGER NOT NULL DEFAULT 0,avatar TEXT NOT NULL DEFAULT 'camp',must_reset INTEGER NOT NULL DEFAULT 0,settings TEXT NOT NULL DEFAULT '{}');
INSERT OR IGNORE INTO user_meta(name) SELECT name FROM users ORDER BY created,name;
CREATE TABLE IF NOT EXISTS room_meta(name TEXT PRIMARY KEY REFERENCES rooms(name),private INTEGER NOT NULL DEFAULT 0,readonly INTEGER NOT NULL DEFAULT 0,archived INTEGER NOT NULL DEFAULT 0,category TEXT NOT NULL DEFAULT '',rules TEXT NOT NULL DEFAULT '',announcement TEXT NOT NULL DEFAULT '',slow INTEGER NOT NULL DEFAULT 0);
INSERT OR IGNORE INTO room_meta(name) SELECT name FROM rooms;
CREATE TABLE IF NOT EXISTS members(room TEXT NOT NULL REFERENCES rooms(name),user TEXT NOT NULL REFERENCES users(name),role TEXT NOT NULL DEFAULT 'member',joined INTEGER NOT NULL,PRIMARY KEY(room,user));
CREATE TABLE IF NOT EXISTS message_meta(id INTEGER PRIMARY KEY REFERENCES messages(id),reply_to INTEGER NOT NULL DEFAULT 0,edited INTEGER NOT NULL DEFAULT 0,recalled INTEGER NOT NULL DEFAULT 0,recaller TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS revisions(id INTEGER PRIMARY KEY AUTOINCREMENT,message_id INTEGER NOT NULL REFERENCES messages(id),actor TEXT NOT NULL,action TEXT NOT NULL,body TEXT NOT NULL,at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS revisions_message ON revisions(message_id,id);
CREATE TABLE IF NOT EXISTS reactions(message_id INTEGER NOT NULL REFERENCES messages(id),user TEXT NOT NULL REFERENCES users(name),reaction TEXT NOT NULL,at INTEGER NOT NULL,PRIMARY KEY(message_id,user,reaction));
CREATE TABLE IF NOT EXISTS bookmarks(user TEXT NOT NULL REFERENCES users(name),message_id INTEGER NOT NULL REFERENCES messages(id),at INTEGER NOT NULL,PRIMARY KEY(user,message_id));
CREATE TABLE IF NOT EXISTS notifications(id INTEGER PRIMARY KEY AUTOINCREMENT,user TEXT NOT NULL REFERENCES users(name),kind TEXT NOT NULL,actor TEXT NOT NULL,ref INTEGER NOT NULL DEFAULT 0,body TEXT NOT NULL,seen INTEGER NOT NULL DEFAULT 0,at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS notifications_user ON notifications(user,seen,id);
CREATE TABLE IF NOT EXISTS audit(id INTEGER PRIMARY KEY AUTOINCREMENT,actor TEXT NOT NULL,action TEXT NOT NULL,target TEXT NOT NULL,detail TEXT NOT NULL,room TEXT NOT NULL DEFAULT '',at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS audit_at ON audit(id);
CREATE TABLE IF NOT EXISTS logins(id INTEGER PRIMARY KEY AUTOINCREMENT,user TEXT NOT NULL,ip TEXT NOT NULL,client TEXT NOT NULL,ok INTEGER NOT NULL,at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS logins_user ON logins(user,id);
CREATE TABLE IF NOT EXISTS sessions(id TEXT PRIMARY KEY,user TEXT NOT NULL REFERENCES users(name),ip TEXT NOT NULL,client TEXT NOT NULL,started INTEGER NOT NULL,seen INTEGER NOT NULL,closed INTEGER NOT NULL DEFAULT 0,revoked INTEGER NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS sessions_user ON sessions(user,closed,revoked);
CREATE TABLE IF NOT EXISTS sanctions(id INTEGER PRIMARY KEY AUTOINCREMENT,kind TEXT NOT NULL,target TEXT NOT NULL,room TEXT NOT NULL DEFAULT '',expires INTEGER NOT NULL,reason TEXT NOT NULL,actor TEXT NOT NULL,at INTEGER NOT NULL,revoked INTEGER NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS sanctions_active ON sanctions(kind,target,revoked,expires);
CREATE TABLE IF NOT EXISTS reports(id INTEGER PRIMARY KEY AUTOINCREMENT,user TEXT NOT NULL REFERENCES users(name),message_id INTEGER NOT NULL REFERENCES messages(id),reason TEXT NOT NULL,status TEXT NOT NULL DEFAULT 'open',resolver TEXT NOT NULL DEFAULT '',at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS reports_status ON reports(status,id);
CREATE TABLE IF NOT EXISTS relations(owner TEXT NOT NULL REFERENCES users(name),target TEXT NOT NULL REFERENCES users(name),kind TEXT NOT NULL,value TEXT NOT NULL DEFAULT '',at INTEGER NOT NULL,PRIMARY KEY(owner,target,kind));
CREATE TABLE IF NOT EXISTS preferences(user TEXT NOT NULL REFERENCES users(name),scope TEXT NOT NULL,pinned INTEGER NOT NULL DEFAULT 0,subscribed INTEGER NOT NULL DEFAULT 0,muted INTEGER NOT NULL DEFAULT 0,last_read INTEGER NOT NULL DEFAULT 0,offset INTEGER NOT NULL DEFAULT 0,draft TEXT NOT NULL DEFAULT '',reply_to INTEGER NOT NULL DEFAULT 0,anchor INTEGER NOT NULL DEFAULT 0,PRIMARY KEY(user,scope));
CREATE TABLE IF NOT EXISTS invites(hash TEXT PRIMARY KEY,creator TEXT NOT NULL,expires INTEGER NOT NULL,remaining INTEGER NOT NULL,created INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS poke_limits(actor TEXT NOT NULL,target TEXT NOT NULL,at INTEGER NOT NULL,PRIMARY KEY(actor,target));
CREATE TABLE IF NOT EXISTS config(key TEXT PRIMARY KEY,value TEXT NOT NULL);
INSERT OR IGNORE INTO config VALUES('registration','open'),('registration_quota','10'),('maintenance','off'),('maintenance_notice',''),('retention','permanent');
CREATE TABLE IF NOT EXISTS objects(id INTEGER PRIMARY KEY AUTOINCREMENT,kind TEXT NOT NULL,room TEXT NOT NULL REFERENCES rooms(name),owner TEXT NOT NULL REFERENCES users(name),title TEXT NOT NULL,payload TEXT NOT NULL,created INTEGER NOT NULL,closed INTEGER NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS objects_room ON objects(room,kind,id);
CREATE TABLE IF NOT EXISTS participation(object_id INTEGER NOT NULL REFERENCES objects(id),user TEXT NOT NULL REFERENCES users(name),value TEXT NOT NULL,at INTEGER NOT NULL,PRIMARY KEY(object_id,user));
CREATE TABLE IF NOT EXISTS object_events(id INTEGER PRIMARY KEY AUTOINCREMENT,object_id INTEGER NOT NULL REFERENCES objects(id),user TEXT NOT NULL REFERENCES users(name),body TEXT NOT NULL,at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS badges(user TEXT NOT NULL REFERENCES users(name),badge TEXT NOT NULL,at INTEGER NOT NULL,PRIMARY KEY(user,badge));
CREATE TABLE IF NOT EXISTS diagnostics(id INTEGER PRIMARY KEY AUTOINCREMENT,kind TEXT NOT NULL,detail TEXT NOT NULL,at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS messages_context ON messages(room,recipient,id);
CREATE INDEX IF NOT EXISTS messages_sender ON messages(sender,id);
CREATE INDEX IF NOT EXISTS messages_at ON messages(at);
CREATE TABLE IF NOT EXISTS rate_stats(hour INTEGER PRIMARY KEY,count INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS compositions(user TEXT NOT NULL REFERENCES users(name),room TEXT NOT NULL REFERENCES rooms(name),kind TEXT NOT NULL,title TEXT NOT NULL,body TEXT NOT NULL,updated INTEGER NOT NULL,PRIMARY KEY(user,room,kind));
CREATE VIRTUAL TABLE IF NOT EXISTS message_search USING fts5(body,content='messages',content_rowid='id',tokenize='trigram');
CREATE TRIGGER IF NOT EXISTS message_search_insert AFTER INSERT ON messages BEGIN INSERT INTO message_search(rowid,body) VALUES(new.id,new.body); END;
CREATE TRIGGER IF NOT EXISTS message_search_delete AFTER DELETE ON messages BEGIN INSERT INTO message_search(message_search,rowid,body) VALUES('delete',old.id,old.body); END;
CREATE TRIGGER IF NOT EXISTS message_search_update AFTER UPDATE OF body ON messages BEGIN INSERT INTO message_search(message_search,rowid,body) VALUES('delete',old.id,old.body);INSERT INTO message_search(rowid,body) VALUES(new.id,new.body);END;
INSERT INTO message_search(message_search) VALUES('rebuild');
CREATE TRIGGER IF NOT EXISTS permanent_messages BEFORE DELETE ON messages BEGIN SELECT RAISE(ABORT,'MioChat messages are retained permanently');END;
CREATE TRIGGER IF NOT EXISTS permanent_revisions BEFORE DELETE ON revisions BEGIN SELECT RAISE(ABORT,'MioChat revisions are retained permanently');END;
CREATE TRIGGER IF NOT EXISTS immutable_revisions BEFORE UPDATE ON revisions BEGIN SELECT RAISE(ABORT,'MioChat revisions are append-only');END;
CREATE TRIGGER IF NOT EXISTS permanent_audit BEFORE DELETE ON audit BEGIN SELECT RAISE(ABORT,'MioChat audit records are retained permanently');END;
CREATE TRIGGER IF NOT EXISTS immutable_audit BEFORE UPDATE ON audit BEGIN SELECT RAISE(ABORT,'MioChat audit records are append-only');END;
`

// Database changes are additive and preserve account credentials, history,
// roles, settings and the service host key.
const schemaKeysSQL = `
CREATE TABLE IF NOT EXISTS ssh_keys(id INTEGER PRIMARY KEY AUTOINCREMENT,user TEXT NOT NULL REFERENCES users(name),fingerprint TEXT NOT NULL UNIQUE,public_key TEXT NOT NULL,algorithm TEXT NOT NULL,label TEXT NOT NULL,created INTEGER NOT NULL,last_used INTEGER NOT NULL DEFAULT 0,revoked INTEGER NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS ssh_keys_user ON ssh_keys(user,revoked,id);
CREATE TABLE IF NOT EXISTS session_credentials(session_id TEXT PRIMARY KEY REFERENCES sessions(id),key_id INTEGER NOT NULL REFERENCES ssh_keys(id));
CREATE INDEX IF NOT EXISTS session_credentials_key ON session_credentials(key_id);
`

const schemaSecuritySQL = `
CREATE TABLE IF NOT EXISTS credential_versions(user TEXT PRIMARY KEY REFERENCES users(name),epoch INTEGER NOT NULL DEFAULT 0);
INSERT OR IGNORE INTO credential_versions(user) SELECT name FROM users;
CREATE TABLE IF NOT EXISTS session_versions(session_id TEXT PRIMARY KEY REFERENCES sessions(id),epoch INTEGER NOT NULL);
INSERT OR IGNORE INTO session_versions(session_id,epoch) SELECT s.id,COALESCE(c.epoch,0) FROM sessions s LEFT JOIN credential_versions c ON c.user=s.user;
CREATE TABLE IF NOT EXISTS position_versions(user TEXT NOT NULL REFERENCES users(name),scope TEXT NOT NULL,version INTEGER NOT NULL DEFAULT 0,PRIMARY KEY(user,scope));
INSERT OR IGNORE INTO position_versions(user,scope) SELECT user,scope FROM preferences;
CREATE TABLE IF NOT EXISTS draft_conflicts(user TEXT NOT NULL REFERENCES users(name),scope TEXT NOT NULL,session_id TEXT NOT NULL REFERENCES sessions(id),draft TEXT NOT NULL,reply_to INTEGER NOT NULL DEFAULT 0,version INTEGER NOT NULL DEFAULT 0,updated INTEGER NOT NULL,PRIMARY KEY(user,scope,session_id));
DROP INDEX IF EXISTS messages_context;
CREATE INDEX IF NOT EXISTS object_events_object ON object_events(object_id,id);
`

// Version rows outlive composition deletion. Six original composition columns
// are unchanged, so legacy INSERT VALUES fixtures still participate in CAS.
const schemaCompositionsSQL = `
CREATE TABLE IF NOT EXISTS composition_versions(user TEXT NOT NULL REFERENCES users(name),room TEXT NOT NULL REFERENCES rooms(name),kind TEXT NOT NULL,version INTEGER NOT NULL CHECK(typeof(version)='integer' AND version>=1),PRIMARY KEY(user,room,kind));
INSERT OR IGNORE INTO composition_versions(user,room,kind,version) SELECT user,room,kind,1 FROM compositions;
CREATE TRIGGER IF NOT EXISTS composition_version_no_delete BEFORE DELETE ON composition_versions BEGIN SELECT RAISE(ABORT,'composition version tombstones are retained');END;
CREATE TRIGGER IF NOT EXISTS composition_version_monotonic BEFORE UPDATE OF version ON composition_versions WHEN new.version<=old.version BEGIN SELECT RAISE(ABORT,'composition versions only advance');END;
CREATE TRIGGER IF NOT EXISTS composition_version_insert AFTER INSERT ON compositions BEGIN
 INSERT INTO composition_versions VALUES(new.user,new.room,new.kind,1) ON CONFLICT(user,room,kind) DO UPDATE SET version=version+1;
END;
CREATE TRIGGER IF NOT EXISTS composition_version_update AFTER UPDATE ON compositions BEGIN
 INSERT INTO composition_versions VALUES(new.user,new.room,new.kind,1) ON CONFLICT(user,room,kind) DO UPDATE SET version=version+1;
 INSERT INTO composition_versions SELECT old.user,old.room,old.kind,1 WHERE old.user!=new.user OR old.room!=new.room OR old.kind!=new.kind ON CONFLICT(user,room,kind) DO UPDATE SET version=version+1;
END;
CREATE TRIGGER IF NOT EXISTS composition_version_delete AFTER DELETE ON compositions BEGIN
 INSERT INTO composition_versions VALUES(old.user,old.room,old.kind,1) ON CONFLICT(user,room,kind) DO UPDATE SET version=version+1;
END;
`

func migrate(db *sql.DB, path string, legacy bool) error {
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version > schemaVersion {
		return fmt.Errorf("数据库版本 %d 高于程序支持的 %d；拒绝降级打开", version, schemaVersion)
	}
	if version == schemaVersion {
		return nil
	}
	if legacy && path != ":memory:" && !strings.HasPrefix(path, "file:") {
		backupVersion := schemaVersion
		if version < 2 {
			backupVersion = 2
		}
		snapshot := fmt.Sprintf("%s.pre-v%d-%s.bak", path, backupVersion, time.Now().UTC().Format("20060102T150405.000000000"))
		if _, err := db.Exec("VACUUM INTO ?", snapshot); err != nil {
			return fmt.Errorf("升级前备份失败：%w", err)
		}
		if err := os.Chmod(snapshot, 0600); err != nil {
			return err
		}
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, step := range []struct {
		version int
		sql     string
	}{{2, schemaBaseSQL}, {3, schemaKeysSQL}, {4, schemaSecuritySQL}, {5, schemaCompositionsSQL}, {6, schemaAttachmentsSQL}, {7, schemaDotSQL}} {
		if version >= step.version {
			continue
		}
		if _, err = tx.Exec(step.sql); err != nil {
			return fmt.Errorf("增量迁移 v%d 失败：%w", step.version, err)
		}
		if _, err = tx.Exec("INSERT OR IGNORE INTO schema_migrations VALUES(?,?)", step.version, time.Now().Unix()); err != nil {
			return err
		}
		if _, err = tx.Exec(fmt.Sprintf("PRAGMA user_version=%d", step.version)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type queryer interface {
	QueryRow(string, ...any) *sql.Row
	Query(string, ...any) (*sql.Rows, error)
	Exec(string, ...any) (sql.Result, error)
}

func auditQ(q queryer, actor, action, target, detail, room string) error {
	result, err := q.Exec(`INSERT INTO audit(actor,action,target,detail,room,at) VALUES(?,?,?,?,?,?)`, actor, action, target, cleanText(detail, 1200), room, time.Now().Unix())
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return fmt.Errorf("审计记录未写入")
	}
	return nil
}
