package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"
)

func isAdmin(role string) bool { return role == "owner" || role == "admin" }
func roleRank(role string) int {
	switch role {
	case "owner":
		return 3
	case "admin":
		return 2
	case "moderator":
		return 1
	}
	return 0
}
func userQ(q queryer, name string) (User, error) {
	var u User
	err := q.QueryRow(`SELECT u.name,u.bio,u.status,u.theme,u.role,u.created,u.banned,x.uid,x.last_login,x.avatar,x.must_reset,x.settings,COALESCE(v.epoch,0) FROM users u JOIN user_meta x ON x.name=u.name LEFT JOIN credential_versions v ON v.user=u.name WHERE u.name=?`, accountKey(name)).Scan(&u.Name, &u.Bio, &u.Status, &u.Theme, &u.Role, &u.Created, &u.Banned, &u.UID, &u.LastLogin, &u.Avatar, &u.MustReset, &u.Settings, &u.AuthEpoch)
	return u, err
}
func (s *Store) Resolve(arg string) (User, error) {
	if strings.HasPrefix(arg, "#") {
		id, e := strconv.ParseInt(strings.TrimPrefix(arg, "#"), 10, 64)
		if e != nil {
			return User{}, errors.New("无效 UID")
		}
		var name string
		if e = s.db.QueryRow(`SELECT name FROM user_meta WHERE uid=?`, id).Scan(&name); e != nil {
			return User{}, errors.New("账户不存在")
		}
		return s.User(name)
	}
	u, e := s.User(arg)
	if e != nil {
		return u, errors.New("账户不存在")
	}
	return u, nil
}

type RoomInfo struct {
	Room
	Private, ReadOnly, Archived   bool
	Category, Rules, Announcement string
	Slow                          int
}

func roomQ(q queryer, name string) (RoomInfo, error) {
	var r RoomInfo
	err := q.QueryRow(`SELECT r.name,r.topic,r.owner,COALESCE(x.private,0),COALESCE(x.readonly,0),COALESCE(x.archived,0),COALESCE(x.category,''),COALESCE(x.rules,''),COALESCE(x.announcement,''),COALESCE(x.slow,0) FROM rooms r LEFT JOIN room_meta x ON x.name=r.name WHERE r.name=?`, name).Scan(&r.Name, &r.Topic, &r.Owner, &r.Private, &r.ReadOnly, &r.Archived, &r.Category, &r.Rules, &r.Announcement, &r.Slow)
	return r, err
}
func canReadQ(q queryer, user, room string) bool {
	r, e := roomQ(q, room)
	if e != nil {
		return false
	}
	if !r.Private || r.Owner == user {
		return true
	}
	var n int
	_ = q.QueryRow(`SELECT count(*) FROM members WHERE room=? AND user=?`, room, user).Scan(&n)
	return n > 0
}
func canManageQ(q queryer, user, room string) bool {
	u, e := userQ(q, user)
	if e != nil {
		return false
	}
	r, e := roomQ(q, room)
	if e != nil {
		return false
	}
	if isAdmin(u.Role) || r.Owner == user {
		return true
	}
	if u.Role == "moderator" && !r.Private {
		return true
	}
	var role string
	_ = q.QueryRow(`SELECT role FROM members WHERE room=? AND user=?`, room, user).Scan(&role)
	return role == "moderator"
}
func (s *Store) CanRead(user, room string) bool         { return canReadQ(s.db, user, room) }
func (s *Store) RoomInfo(name string) (RoomInfo, error) { return roomQ(s.readDB(), name) }
func (s *Store) VisibleRooms(user string, archived bool) ([]Room, error) {
	rows, e := s.db.Query(`SELECT r.name,r.topic,r.owner FROM rooms r LEFT JOIN room_meta x ON x.name=r.name LEFT JOIN preferences p ON p.user=? AND p.scope='room:'||r.name WHERE (COALESCE(x.private,0)=0 OR r.owner=? OR EXISTS(SELECT 1 FROM members m WHERE m.room=r.name AND m.user=?)) AND (? OR COALESCE(x.archived,0)=0) ORDER BY COALESCE(p.pinned,0) DESC,CASE r.name WHEN 'lobby' THEN 0 WHEN 'dev' THEN 1 WHEN 'random' THEN 2 WHEN 'arcade' THEN 3 ELSE 4 END,COALESCE(x.category,''),r.name`, user, user, user, archived)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var result []Room
	for rows.Next() {
		var r Room
		if e = rows.Scan(&r.Name, &r.Topic, &r.Owner); e != nil {
			return nil, e
		}
		result = append(result, r)
	}
	return result, rows.Err()
}
func (s *Store) Settings(user string) map[string]string {
	u, e := s.User(user)
	values := map[string]string{"poke": "on", "motion": "on", "bell": "off", "dnd": "off", "compact": "off", "ascii": "off", "welcome": "on", "contrast": "off", "key_reminder": "on"}
	if e == nil {
		_ = json.Unmarshal([]byte(u.Settings), &values)
	}
	return values
}
func (s *Store) Setting(user, key, value string) error {
	switch key {
	case "poke", "motion", "bell", "dnd", "compact", "ascii", "welcome", "contrast", "key_reminder":
	default:
		return errors.New("偏好项：poke motion bell dnd compact ascii welcome contrast key_reminder")
	}
	if value != "on" && value != "off" {
		return errors.New("值为 on 或 off")
	}
	return changedOne(s.db.Exec(`UPDATE user_meta SET settings=json_set(CASE WHEN json_valid(settings) AND json_type(settings)='object' THEN settings ELSE '{}' END,?,?) WHERE name=?`, "$."+key, value, accountKey(user)))
}
func (s *Store) Relation(owner, target, kind, value string, remove bool) error {
	u, e := s.Resolve(target)
	if e != nil {
		return e
	}
	target = u.Name
	if target == owner {
		return errors.New("不能对自己执行此操作")
	}
	switch kind {
	case "follow", "block", "note", "request", "friend":
	default:
		return errors.New("无效关系类型")
	}
	if remove {
		_, e = s.db.Exec(`DELETE FROM relations WHERE owner=? AND target=? AND kind=?`, owner, target, kind)
		return e
	}
	if kind == "request" {
		if s.Blocked(target, owner) {
			return errors.New("对方不接受请求")
		}
		if _, e = s.db.Exec(`INSERT INTO relations VALUES(?,?,?,?,?) ON CONFLICT(owner,target,kind) DO UPDATE SET value=excluded.value`, owner, target, kind, cleanText(value, 100), time.Now().Unix()); e == nil {
			_ = s.Notify(target, "friend", owner, 0, "收到好友申请")
		}
		return e
	}
	_, e = s.db.Exec(`INSERT INTO relations VALUES(?,?,?,?,?) ON CONFLICT(owner,target,kind) DO UPDATE SET value=excluded.value`, owner, target, kind, cleanText(value, 100), time.Now().Unix())
	return e
}
func (s *Store) Blocked(owner, target string) bool {
	var n int
	_ = s.db.QueryRow(`SELECT count(*) FROM relations WHERE owner=? AND target=? AND kind='block'`, owner, target).Scan(&n)
	return n > 0
}
func (s *Store) Relations(user, kind string) map[string]string {
	result := map[string]string{}
	rows, e := s.db.Query(`SELECT target,value FROM relations WHERE owner=? AND kind=? ORDER BY target`, user, kind)
	if e != nil {
		return result
	}
	defer rows.Close()
	for rows.Next() {
		var name, value string
		if rows.Scan(&name, &value) == nil {
			result[name] = value
		}
	}
	return result
}
func (s *Store) AcceptFriend(user, target string, accept bool) error {
	u, e := s.Resolve(target)
	if e != nil {
		return e
	}
	target = u.Name
	tx, e := s.db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var n int
	if e = tx.QueryRow(`SELECT count(*) FROM relations WHERE owner=? AND target=? AND kind='request'`, target, user).Scan(&n); e != nil {
		return e
	}
	if n == 0 {
		return errors.New("没有来自该用户的好友申请")
	}
	if _, e = tx.Exec(`DELETE FROM relations WHERE owner=? AND target=? AND kind='request'`, target, user); e != nil {
		return e
	}
	if accept {
		for _, pair := range [][2]string{{user, target}, {target, user}} {
			if _, e = tx.Exec(`INSERT OR IGNORE INTO relations VALUES(?,?,'friend','',?)`, pair[0], pair[1], time.Now().Unix()); e != nil {
				return e
			}
		}
	}
	return tx.Commit()
}
func (s *Store) PendingFriends(user string) []string {
	rows, e := s.db.Query(`SELECT owner FROM relations WHERE target=? AND kind='request' ORDER BY at`, user)
	if e != nil {
		return nil
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if rows.Scan(&n) == nil {
			names = append(names, n)
		}
	}
	return names
}
func (s *Store) Recent(user string) []string {
	rows, e := s.db.Query(`SELECT CASE WHEN sender=? THEN recipient ELSE sender END peer,max(id) FROM messages WHERE recipient!='' AND (sender=? OR recipient=?) GROUP BY peer ORDER BY max(id) DESC LIMIT 30`, user, user, user)
	if e != nil {
		return nil
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		var id int64
		if rows.Scan(&n, &id) == nil {
			names = append(names, n)
		}
	}
	return names
}

type SessionInfo struct {
	ID, User, IP, Client  string
	Started, Seen, Closed int64
	Revoked               bool
}

func (s *Store) RecordSession(c *Client) error {
	ctx := c.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	q := scopedQuery{ctx: ctx, db: tx}
	u, e := currentCredentialUserQ(q, c.Name)
	if e != nil {
		return e
	}
	if u.AuthEpoch != c.AuthEpoch {
		return errCredentialsChanged
	}
	now := time.Now().Unix()
	if e = credentialWrite(q.Exec(`INSERT INTO sessions VALUES(?,?,?,?,?,?,0,0)`, c.ID, c.Name, c.IP, cleanText(c.Version, 120), now, now)); e != nil {
		return e
	}
	if e = credentialWrite(q.Exec(`INSERT INTO session_versions(session_id,epoch) VALUES(?,?)`, c.ID, c.AuthEpoch)); e != nil {
		return e
	}
	return tx.Commit()
}
func (s *Store) EndSession(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _ = s.db.ExecContext(ctx, `UPDATE sessions SET closed=? WHERE id=? AND closed=0`, time.Now().Unix(), id)
}
func (s *Store) SessionValid(id string) bool {
	valid, err := sessionCurrentQ(s.readDB(), id, "")
	return err == nil && valid
}
func (s *Store) Sessions(actor, target string) ([]SessionInfo, error) {
	u, e := s.User(actor)
	if e != nil {
		return nil, e
	}
	if actor != target && !isAdmin(u.Role) {
		return nil, errors.New("无权查看其他账户的连接")
	}
	rows, e := s.db.Query(`SELECT id,user,ip,client,started,seen,closed,revoked FROM sessions WHERE user=? ORDER BY started DESC LIMIT 100`, target)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var result []SessionInfo
	for rows.Next() {
		var v SessionInfo
		if e = rows.Scan(&v.ID, &v.User, &v.IP, &v.Client, &v.Started, &v.Seen, &v.Closed, &v.Revoked); e != nil {
			return nil, e
		}
		result = append(result, v)
	}
	return result, rows.Err()
}
func (s *Store) RevokeSession(actor, id string) error {
	return s.writeTransaction(func(tx *sql.Tx) error {
		u, e := mutationActorQ(tx, actor)
		if e != nil {
			return e
		}
		var target string
		if e = tx.QueryRow(`SELECT user FROM sessions WHERE id=?`, id).Scan(&target); e != nil {
			return errors.New("会话不存在")
		}
		if u.Name != target && !isAdmin(u.Role) {
			return errors.New("无权断开此会话")
		}
		if u.Name != target {
			other, e := userQ(tx, target)
			if e != nil {
				return e
			}
			if roleRank(other.Role) >= roleRank(u.Role) {
				return errors.New("不能断开同级或更高权限账户")
			}
		}
		if e = changedOne(tx.Exec(`UPDATE sessions SET revoked=1 WHERE id=?`, id)); e != nil {
			return e
		}
		return auditQ(tx, u.Name, "session.revoke", target, id, "")
	})
}
func (s *Store) RevokeOthers(user, current string) error {
	_, e := s.db.Exec(`UPDATE sessions SET revoked=1 WHERE user=? AND id!=? AND closed=0`, user, current)
	return e
}

type LoginInfo struct {
	User, IP, Client string
	OK               bool
	At               int64
}

func (s *Store) RecordLogin(user, ip, client string, ok bool) {
	_, _ = s.db.Exec(`INSERT INTO logins(user,ip,client,ok,at) VALUES(?,?,?,?,?)`, cleanText(accountKey(user), 20), ip, cleanText(client, 120), ok, time.Now().Unix())
	if ok {
		_, _ = s.db.Exec(`UPDATE user_meta SET last_login=? WHERE name=?`, time.Now().Unix(), accountKey(user))
	}
}
func (s *Store) LoginHistory(actor, target string) ([]LoginInfo, error) {
	u, e := s.User(actor)
	if e != nil {
		return nil, e
	}
	if actor != target && !isAdmin(u.Role) {
		return nil, errors.New("只有管理员可查看他人登录 IP")
	}
	rows, e := s.db.Query(`SELECT user,ip,client,ok,at FROM logins WHERE user=? ORDER BY id DESC LIMIT 100`, target)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var result []LoginInfo
	for rows.Next() {
		var v LoginInfo
		if e = rows.Scan(&v.User, &v.IP, &v.Client, &v.OK, &v.At); e != nil {
			return nil, e
		}
		result = append(result, v)
	}
	return result, rows.Err()
}
func (s *Store) Config(key string) string {
	var value string
	_ = s.readDB().QueryRow(`SELECT value FROM config WHERE key=?`, key).Scan(&value)
	return value
}
func (s *Store) SetConfig(actor, key, value string) error {
	switch key {
	case "registration":
		if value != "open" && value != "closed" && value != "invite" {
			return errors.New("registration: open / closed / invite")
		}
	case "registration_quota":
		n, e := strconv.Atoi(value)
		if e != nil || n < 1 || n > 100 {
			return errors.New("注册配额范围 1–100")
		}
	case "maintenance":
		if value != "on" && value != "off" {
			return errors.New("maintenance: on / off")
		}
	case "maintenance_notice":
		value = cleanText(value, 240)
	case "retention":
		if value != "permanent" {
			return errors.New("消息必须永久保留，禁止启用自动清理")
		}
	default:
		return errors.New("未知配置项")
	}
	return s.writeTransaction(func(tx *sql.Tx) error {
		u, e := mutationActorQ(tx, actor)
		if e != nil {
			return e
		}
		if !isAdmin(u.Role) {
			return errors.New("需要管理员权限")
		}
		if e = changedOne(tx.Exec(`INSERT INTO config VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)); e != nil {
			return e
		}
		return auditQ(tx, u.Name, "config.set", key, value, "")
	})
}
func normalizePrefix(raw string) (netip.Prefix, error) {
	if p, e := netip.ParsePrefix(raw); e == nil {
		if p.Addr().Is4In6() && p.Bits() >= 96 {
			return netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96).Masked(), nil
		}
		return p.Masked(), nil
	}
	ip, e := netip.ParseAddr(raw)
	if e != nil {
		return netip.Prefix{}, errors.New("无效 IPv4/IPv6 地址或 CIDR")
	}
	ip = ip.Unmap()
	return netip.PrefixFrom(ip, ip.BitLen()), nil
}
func (s *Store) IPBanned(ip string) bool {
	return s.IPBannedContext(context.Background(), ip)
}
func (s *Store) accountBanned(name string) bool {
	var n int
	_ = s.db.QueryRow(`SELECT count(*) FROM sanctions WHERE kind='user' AND target=? AND revoked=0 AND (expires=0 OR expires>?)`, name, time.Now().Unix()).Scan(&n)
	return n > 0
}
func durationSeconds(raw string) (int64, error) {
	if raw == "permanent" || raw == "forever" || raw == "0" {
		return 0, nil
	}
	if strings.HasSuffix(raw, "d") {
		n, e := strconv.Atoi(strings.TrimSuffix(raw, "d"))
		if e != nil || n < 1 || n > 3650 {
			return 0, errors.New("天数范围 1–3650")
		}
		return int64(n) * 86400, nil
	}
	d, e := time.ParseDuration(raw)
	if e != nil || d < time.Second || d > 3650*24*time.Hour {
		return 0, errors.New("期限示例：10m、24h、7d、permanent")
	}
	return int64(d.Seconds()), nil
}
func namesSorted(values map[string]string) []string {
	names := make([]string, 0, len(values))
	for n := range values {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func (s *Store) Unfriend(user, target string) error {
	u, e := s.Resolve(target)
	if e != nil {
		return e
	}
	tx, e := s.db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if _, e = tx.Exec(`DELETE FROM relations WHERE kind='friend' AND ((owner=? AND target=?) OR (owner=? AND target=?))`, user, u.Name, u.Name, user); e != nil {
		return e
	}
	return tx.Commit()
}
func noRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }

func (s *Store) Sanction(actor, kind, target, room, duration, reason string, remove bool) error {
	if kind != "ip" && kind != "user" && kind != "mute" {
		return errors.New("未知封禁类型")
	}
	if kind == "ip" {
		p, e := normalizePrefix(target)
		if e != nil {
			return e
		}
		if p.Bits() == 0 {
			return errors.New("不能封禁整个互联网地址段")
		}
		target = p.String()
	}
	expires := int64(0)
	if !remove {
		seconds, e := durationSeconds(duration)
		if e != nil {
			return e
		}
		if seconds > 0 {
			expires = time.Now().Unix() + seconds
		}
	}
	err := s.writeTransaction(func(tx *sql.Tx) error {
		u, e := mutationActorQ(tx, actor)
		if e != nil {
			return e
		}
		global := isAdmin(u.Role)
		if !global && (kind != "mute" || room == "" || !canManageQ(tx, u.Name, room)) {
			return errors.New("没有此管理权限")
		}
		if kind != "ip" {
			other, e := resolveUserQ(tx, target)
			if e != nil {
				return e
			}
			target = other.Name
			if target == u.Name || roleRank(other.Role) >= roleRank(u.Role) && global {
				return errors.New("不能管理自己、同级或更高角色")
			}
			if !global {
				r, e := roomQ(tx, room)
				if e != nil {
					return e
				}
				if other.Name == r.Owner || isAdmin(other.Role) {
					return errors.New("不能禁言频道所有者或管理员")
				}
			}
		}
		if remove {
			if kind == "ip" {
				desired, e := normalizePrefix(target)
				if e != nil {
					return e
				}
				rows, e := tx.Query(`SELECT id,target FROM sanctions WHERE kind='ip' AND room=? AND revoked=0`, room)
				if e != nil {
					return e
				}
				var matches []int64
				for rows.Next() {
					var id int64
					var stored string
					if e = rows.Scan(&id, &stored); e != nil {
						rows.Close()
						return e
					}
					prefix, err := normalizePrefix(stored)
					if err != nil {
						// Legacy wide mapped prefixes remain IPv6 policies; they
						// must never be promoted into a blanket IPv4 /0 policy.
						if legacy, parseErr := netip.ParsePrefix(stored); parseErr == nil && legacy.Addr().Is4In6() && legacy.Bits() < 96 {
							prefix, err = legacy.Masked(), nil
						}
					}
					if err == nil && prefix == desired {
						matches = append(matches, id)
					}
				}
				e = rows.Err()
				rows.Close()
				if e != nil {
					return e
				}
				for _, id := range matches {
					if e = changedOne(tx.Exec(`UPDATE sanctions SET revoked=1 WHERE id=? AND kind='ip'`, id)); e != nil {
						return e
					}
				}
			} else if _, e = tx.Exec(`UPDATE sanctions SET revoked=1 WHERE kind=? AND target=? AND room=?`, kind, target, room); e != nil {
				return e
			}
			if kind == "user" {
				if e = changedOne(tx.Exec(`UPDATE users SET banned=0 WHERE name=?`, target)); e != nil {
					return e
				}
			}
		} else if e = changedOne(tx.Exec(`INSERT INTO sanctions(kind,target,room,expires,reason,actor,at) VALUES(?,?,?,?,?,?,?)`, kind, target, room, expires, cleanText(reason, 240), u.Name, time.Now().Unix())); e != nil {
			return e
		}
		action := "sanction.add"
		if remove {
			action = "sanction.remove"
		}
		return auditQ(tx, u.Name, action, kind+":"+target, reason, room)
	})
	if err == nil && kind == "ip" {
		s.invalidateIPPolicy()
	}
	return err
}
func (s *Store) SetRole(actor, target, role string) error {
	if role != "admin" && role != "moderator" && role != "member" {
		return errors.New("角色：admin / moderator / member")
	}
	return s.writeTransaction(func(tx *sql.Tx) error {
		u, e := mutationActorQ(tx, actor)
		if e != nil {
			return e
		}
		if u.Role != "owner" {
			return errors.New("仅站长可分配全站角色")
		}
		other, e := resolveUserQ(tx, target)
		if e != nil {
			return e
		}
		if other.Name == u.Name || other.Role == "owner" {
			return errors.New("站长身份只能通过服务器命令行设置")
		}
		if e = changedOne(tx.Exec(`UPDATE users SET role=? WHERE name=?`, role, other.Name)); e != nil {
			return e
		}
		return auditQ(tx, u.Name, "role.set", other.Name, role, "")
	})
}
