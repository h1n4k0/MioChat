package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
	"golang.org/x/crypto/argon2"
	_ "modernc.org/sqlite"
)

type User struct {
	Name, Bio, Status, Theme, Role string
	Created                        int64
	Banned                         bool
	UID, LastLogin                 int64
	Avatar, Settings               string
	MustReset                      bool
	AuthEpoch                      int64
}
type Room struct{ Name, Topic, Owner string }
type Message struct {
	Attachment                                      *attachmentInfo
	ID                                              int64
	Room, Sender, Recipient, Body, Kind             string
	At                                              int64
	ReplyTo, Edited, Recalled                       int64
	Recaller, ReactionSummary, ReplyName, ReplyBody string
}
type Store struct {
	attachmentSweepMu sync.Mutex
	attachmentSweepAt time.Time
	attachmentNow     func() time.Time // nil uses the server clock; injectable in synthetic tests.
	db                *appDB
	dummySalt         []byte
	path              string
	ipPolicy          ipPolicyCache
	maintenanceMu     sync.Mutex
}
type appDB struct {
	*sql.DB
	reader                 *sql.DB
	policyReader           *sql.DB
	readPlans, policyPlans *readStatements
}

func (db *appDB) Close() error {
	db.policyPlans.close()
	db.readPlans.close()
	if db.policyReader != nil {
		_ = db.policyReader.Close()
	}
	if db.reader != nil {
		_ = db.reader.Close()
	}
	return db.DB.Close()
}
func (s *Store) readDB() *sql.DB {
	if s.db.reader != nil {
		return s.db.reader
	}
	return s.db.DB
}
func (s *Store) policyDB() *sql.DB {
	if s.db.policyReader != nil {
		return s.db.policyReader
	}
	return s.readDB()
}

func openStore(path string) (*Store, error) {
	if path != ":memory:" && !strings.HasPrefix(path, "file:") {
		if err := ensureNoPendingRestore(filepath.Dir(path)); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	var existingVersion int
	if e := db.QueryRow("PRAGMA user_version").Scan(&existingVersion); e != nil {
		db.Close()
		return nil, e
	}
	if existingVersion > schemaVersion {
		db.Close()
		return nil, fmt.Errorf("数据库版本 %d 高于程序支持的 %d，拒绝打开", existingVersion, schemaVersion)
	}
	var legacy int
	_ = db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='users'`).Scan(&legacy)
	_, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000; PRAGMA foreign_keys=ON;
CREATE TABLE IF NOT EXISTS users(name TEXT PRIMARY KEY, hash BLOB NOT NULL, salt BLOB NOT NULL, bio TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT '', theme TEXT NOT NULL DEFAULT 'aurora', role TEXT NOT NULL DEFAULT 'member', created INTEGER NOT NULL, banned INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS rooms(name TEXT PRIMARY KEY, topic TEXT NOT NULL, owner TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS messages(id INTEGER PRIMARY KEY AUTOINCREMENT, room TEXT NOT NULL, sender TEXT NOT NULL, recipient TEXT NOT NULL DEFAULT '', body TEXT NOT NULL, kind TEXT NOT NULL, at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS room_history ON messages(room,recipient,id);
CREATE INDEX IF NOT EXISTS dm_history ON messages(sender,recipient,id);
CREATE TABLE IF NOT EXISTS pins(room TEXT NOT NULL, message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE, PRIMARY KEY(room,message_id));
INSERT OR IGNORE INTO rooms VALUES('lobby','欢迎来到 MioChat · 从一句你好开始',''),('dev','代码、项目与奇思妙想',''),('random','随便聊聊，分享生活',''),('arcade','/roll /coin /8ball · 休息一下','');`)
	if err != nil {
		db.Close()
		return nil, err
	}
	if err = migrate(db, path, legacy > 0); err != nil {
		db.Close()
		return nil, err
	}
	wrapped := &appDB{DB: db}
	if path != ":memory:" {
		reader, e := sql.Open("sqlite", path)
		if e != nil {
			db.Close()
			return nil, e
		}
		reader.SetMaxOpenConns(2)
		wrapped.reader = reader
		policy, e := sql.Open("sqlite", path)
		if e != nil {
			wrapped.Close()
			return nil, e
		}
		policy.SetMaxOpenConns(1)
		policy.SetMaxIdleConns(1)
		wrapped.policyReader = policy
	}
	readDB := wrapped.reader
	if readDB == nil {
		readDB = wrapped.DB
	}
	policyDB := wrapped.policyReader
	if policyDB == nil {
		policyDB = readDB
	}
	wrapped.readPlans = newReadStatements(readDB, 32)
	wrapped.policyPlans = newReadStatements(policyDB, 8)
	s := &Store{db: wrapped, dummySalt: make([]byte, 16), path: path}
	if _, err = rand.Read(s.dummySalt); err != nil {
		db.Close()
		return nil, err
	}
	if _, err = s.ExpireAttachmentUploads(context.Background(), 32); err != nil {
		wrapped.Close()
		return nil, err
	}
	return s, nil
}

func validName(name string) bool {
	n := utf8.RuneCountInString(name)
	if n < 2 || n > 20 || !utf8.ValidString(name) {
		return false
	}
	for _, r := range name {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '-' {
			return false
		}
	}
	return true
}
func cleanText(s string, limit int) string {
	s = ansi.Strip(strings.ToValidUTF8(s, ""))
	out := make([]rune, 0, min(len(s), limit))
	for _, r := range s {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
			continue
		}
		out = append(out, r)
		if len(out) >= limit {
			break
		}
	}
	return strings.TrimSpace(string(out))
}
func accountKey(name string) string { return strings.ToLower(strings.TrimSpace(name)) }
func passwordHash(password string, salt []byte) []byte {
	return argon2.IDKey([]byte(password), salt, 2, 32*1024, 1, 32)
}
func (s *Store) Register(name, password, confirm string) (User, error) {
	return s.RegisterInvite(name, password, confirm, "", "", "")
}
func (s *Store) Authenticate(name, password string) (User, error) {
	u, _, err := s.verifyPassword(name, password)
	return u, err
}
func (s *Store) User(name string) (User, error) {
	return userQ(s.readDB(), name)
}
func (s *Store) Profile(name, field, value string) error {
	if field != "bio" && field != "status" && field != "theme" {
		return errors.New("未知资料字段")
	}
	_, err := s.db.Exec(`UPDATE users SET `+field+`=? WHERE name=?`, cleanText(value, 160), name)
	return err
}
func (s *Store) ChangePassword(name, old, next string) error {
	return s.changePassword(name, old, next, "")
}
func (s *Store) Rooms() ([]Room, error) {
	rows, err := s.db.Query(`SELECT name,topic,owner FROM rooms ORDER BY CASE name WHEN 'lobby' THEN 0 WHEN 'dev' THEN 1 WHEN 'random' THEN 2 WHEN 'arcade' THEN 3 ELSE 4 END,name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var rooms []Room
	for rows.Next() {
		var r Room
		if err = rows.Scan(&r.Name, &r.Topic, &r.Owner); err != nil {
			return nil, err
		}
		rooms = append(rooms, r)
	}
	return rooms, rows.Err()
}
func (s *Store) Room(name string) (Room, error) {
	var r Room
	err := s.db.QueryRow(`SELECT name,topic,owner FROM rooms WHERE name=?`, name).Scan(&r.Name, &r.Topic, &r.Owner)
	return r, err
}
func (s *Store) CreateRoom(name, owner string) error {
	return s.CreateRoomAccess(name, owner, false)
}
func (s *Store) CreateRoomAccess(name, owner string, private bool) error {
	if len(name) < 2 || len(name) > 24 {
		return errors.New("频道名为 2–24 个小写字母、数字或短横线")
	}
	for _, r := range name {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return errors.New("频道名只支持小写字母、数字和短横线")
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var count int
	if err := tx.QueryRow(`SELECT count(*) FROM rooms WHERE owner=?`, owner).Scan(&count); err != nil {
		return err
	}
	if count >= 5 {
		return errors.New("每人最多创建 5 个频道")
	}
	_, err = tx.Exec(`INSERT INTO rooms VALUES(?,?,?)`, name, "新频道 · 使用 /topic 设置话题", owner)
	if err != nil {
		return errors.New("频道已存在或创建失败")
	}
	if _, err = tx.Exec(`INSERT INTO room_meta(name,private) VALUES(?,?)`, name, private); err != nil {
		return err
	}
	if _, err = tx.Exec(`INSERT INTO members(room,user,role,joined) SELECT ?,name,'owner',? FROM users WHERE name=?`, name, time.Now().Unix(), owner); err != nil {
		return err
	}
	_, _ = tx.Exec(`INSERT OR IGNORE INTO badges SELECT name,'策展人',? FROM users WHERE name=?`, time.Now().Unix(), owner)
	return tx.Commit()
}
func (s *Store) canManage(user User, room string) bool {
	return canManageQ(s.db, user.Name, room)
}
func (s *Store) Topic(user User, room, text string) error {
	if !s.canManage(user, room) {
		return errors.New("仅频道创建者或管理员可设置话题")
	}
	_, err := s.db.Exec(`UPDATE rooms SET topic=? WHERE name=?`, cleanText(text, 120), room)
	return err
}
func (s *Store) Add(msg Message) (Message, error) {
	msg.Body = cleanText(msg.Body, 800)
	msg.At = time.Now().Unix()
	if msg.Body == "" {
		return msg, errors.New("消息不能为空")
	}
	r, err := s.db.Exec(`INSERT INTO messages(room,sender,recipient,body,kind,at) VALUES(?,?,?,?,?,?)`, msg.Room, msg.Sender, msg.Recipient, msg.Body, msg.Kind, msg.At)
	if err != nil {
		return msg, err
	}
	msg.ID, err = r.LastInsertId()
	// Retention is permanent. Never prune messages, revisions or audit records.
	return msg, err
}
func scanMessages(rows *sql.Rows) ([]Message, error) {
	defer rows.Close()
	var result []Message
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.Room, &m.Sender, &m.Recipient, &m.Body, &m.Kind, &m.At); err != nil {
			return nil, err
		}
		result = append(result, m)
	}
	return result, rows.Err()
}
func (s *Store) History(room, user, peer string, limit int) ([]Message, error) {
	return s.Page(room, user, peer, 0, limit)
}
func (s *Store) Search(room, user, peer, query string) ([]Message, error) {
	return s.SearchAll(room, user, peer, query)
}
func (s *Store) Pin(user User, room string, id int64, remove bool) error {
	if !s.canManage(user, room) {
		return errors.New("仅频道创建者或管理员可管理置顶")
	}
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM messages WHERE id=? AND room=? AND recipient=''`, id, room).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return errors.New("消息不属于当前频道")
	}
	if remove {
		_, err := s.db.Exec(`DELETE FROM pins WHERE room=? AND message_id=?`, room, id)
		return err
	}
	_, err := s.db.Exec(`INSERT OR IGNORE INTO pins VALUES(?,?)`, room, id)
	return err
}
func (s *Store) Pins(room string) ([]Message, error) {
	rows, err := s.db.Query(`SELECT m.* FROM messages m JOIN pins p ON p.message_id=m.id WHERE p.room=? ORDER BY m.id DESC LIMIT 20`, room)
	if err != nil {
		return nil, err
	}
	return scanMessages(rows)
}
func (s *Store) Moderate(name string, ban bool) error {
	r, err := s.db.Exec(`UPDATE users SET banned=? WHERE name=? AND role!='admin'`, ban, accountKey(name))
	if err != nil {
		return err
	}
	n, _ := r.RowsAffected()
	if n == 0 {
		return errors.New("账户不存在或为管理员")
	}
	return nil
}
func (s *Store) Promote(name string) error {
	r, err := s.db.Exec(`UPDATE users SET role='admin' WHERE name=?`, accountKey(name))
	if err != nil {
		return err
	}
	n, _ := r.RowsAffected()
	if n == 0 {
		return fmt.Errorf("账户 %q 尚未注册", name)
	}
	return nil
}
