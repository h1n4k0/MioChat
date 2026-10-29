package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

func secretToken() (string, error) {
	data := make([]byte, 24)
	if _, e := rand.Read(data); e != nil {
		return "", e
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}
func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

var errRegistrationPolicyUnavailable = errors.New("注册策略暂不可用，请稍后再试")

// Policy absence keeps the historical open/one-per-IP compatibility defaults;
// a SQL/scan error or malformed stored value is never a permission to register.
// The caller owns the same writer transaction as all registration effects.
func registrationPolicyQ(q queryer, ip string, now int64) (string, error) {
	var mode, quota string
	e := q.QueryRow(`SELECT value FROM config WHERE key='registration'`).Scan(&mode)
	if errors.Is(e, sql.ErrNoRows) {
		mode = "open"
	} else if e != nil {
		return "", errRegistrationPolicyUnavailable
	}
	switch mode {
	case "open", "invite":
	case "closed":
		return "", errors.New("管理员已暂停注册")
	default:
		return "", errRegistrationPolicyUnavailable
	}
	e = q.QueryRow(`SELECT value FROM config WHERE key='registration_quota'`).Scan(&quota)
	if errors.Is(e, sql.ErrNoRows) {
		quota = "1"
	} else if e != nil {
		return "", errRegistrationPolicyUnavailable
	}
	limit, e := strconv.Atoi(quota)
	if e != nil || limit < 1 || limit > 100 {
		return "", errRegistrationPolicyUnavailable
	}
	if ip != "" {
		var count int
		if e = q.QueryRow(`SELECT count(*) FROM logins WHERE ip=? AND client='registration' AND at>?`, ip, now-3600).Scan(&count); e != nil {
			return "", errRegistrationPolicyUnavailable
		}
		if count >= limit {
			return "", errors.New("此 IP 的每小时注册配额已用完")
		}
	}
	return mode, nil
}

func (s *Store) RegisterInvite(name, password, confirm, invite, ip, client string) (User, error) {
	name = accountKey(name)
	if !validName(name) {
		return User{}, errors.New("用户名须为 2–20 个字母、中文、数字、下划线或短横线")
	}
	if utf8.RuneCountInString(password) < 10 || len(password) > 256 {
		return User{}, errors.New("密码至少 10 个字符，且不超过 256 字节")
	}
	if password != confirm {
		return User{}, errors.New("两次密码不一致")
	}
	salt := make([]byte, 16)
	if _, e := rand.Read(salt); e != nil {
		return User{}, e
	}
	hash := passwordHash(password, salt)
	tx, e := s.db.Begin()
	if e != nil {
		return User{}, e
	}
	defer tx.Rollback()
	mode, e := registrationPolicyQ(tx, ip, time.Now().Unix())
	if e != nil {
		return User{}, e
	}
	if mode == "invite" {
		r, e := tx.Exec(`UPDATE invites SET remaining=remaining-1 WHERE hash=? AND remaining>0 AND (expires=0 OR expires>?)`, tokenHash(strings.TrimSpace(invite)), time.Now().Unix())
		if e != nil {
			return User{}, e
		}
		n, e := r.RowsAffected()
		if e != nil {
			return User{}, errors.New("邀请码验证暂不可用")
		}
		if n != 1 {
			return User{}, errors.New("邀请码无效、已用完或过期")
		}
	}
	if e = changedOne(tx.Exec(`INSERT INTO users(name,hash,salt,created) VALUES(?,?,?,?)`, name, hash, salt, time.Now().Unix())); e != nil {
		return User{}, errors.New("用户名已被注册，或注册暂不可用")
	}
	if e = changedOne(tx.Exec(`INSERT INTO user_meta(name) VALUES(?)`, name)); e != nil {
		return User{}, e
	}
	if e = changedOne(tx.Exec(`INSERT INTO badges VALUES(?,'开拓者',?)`, name, time.Now().Unix())); e != nil {
		return User{}, e
	}
	if ip != "" {
		if e = changedOne(tx.Exec(`INSERT INTO logins(user,ip,client,ok,at) VALUES(?,?,'registration',1,?)`, name, ip, time.Now().Unix())); e != nil {
			return User{}, e
		}
	}
	if e = tx.Commit(); e != nil {
		return User{}, e
	}
	return s.registrationUser(name)
}

// Compatibility APIs for non-connection fixtures. Actual TUI/native routes use
// the nonempty-session methods and perform fresh one-time result authorization.
func (s *Store) InviteCode(actor, duration string, uses int) (string, error) {
	out, err := s.inviteCodeForSession(context.Background(), actor, duration, uses, "")
	return out.secret, err
}
func (s *Store) ResetPassword(actor, target string) (string, error) {
	out, err := s.resetPasswordForSession(context.Background(), actor, target, "")
	return out.secret, err
}
func (s *Store) RoomSetting(actor, room, key, value string) error {
	return s.roomSettingForSession(actor, room, key, value, "")
}
func (s *Store) roomSettingForSession(actor, room, key, value, sessionID string) error {
	return s.writeTransaction(func(tx *sql.Tx) error { return roomSettingQ(tx, actor, room, key, value, sessionID) })
}
func roomSettingQ(q queryer, actor, room, key, value, sessionID string) error {
	column := ""
	var data any
	switch key {
	case "transfer":
	case "private", "readonly", "archive":
		if value != "on" && value != "off" {
			return errors.New("值为 on 或 off")
		}
		column = key
		if key == "archive" {
			column = "archived"
		}
		data = value == "on"
	case "category", "rules", "announcement":
		column = key
		data = cleanText(value, 240)
	case "slow":
		n, e := strconv.Atoi(value)
		if e != nil || n < 0 || n > 3600 {
			return errors.New("慢速模式为 0–3600 秒，0 关闭")
		}
		column = "slow"
		data = n
	default:
		return errors.New("频道设置：private readonly archive category rules announcement slow transfer")
	}
	u, e := mutationActorQ(q, actor)
	if e != nil {
		return e
	}
	if sessionID != "" {
		if valid, err := sessionCurrentQ(q, sessionID, u.Name); err != nil || !valid {
			return errCredentialsChanged
		}
		if !canReadQ(q, u.Name, room) {
			return errors.New("room access denied")
		}
	}
	r, e := roomQ(q, room)
	if e != nil {
		return e
	}
	if !canManageQ(q, u.Name, room) {
		return errors.New("需要频道管理权限")
	}
	if key == "private" || key == "archive" || key == "transfer" {
		if u.Name != r.Owner && !isAdmin(u.Role) {
			return errors.New("此操作需要频道所有者或管理员权限")
		}
		if room == "lobby" {
			return errors.New("公共大厅不能私密化、归档或转让")
		}
	}
	if key == "transfer" {
		target, e := resolveUserQ(q, value)
		if e != nil {
			return e
		}
		if e = changedOne(q.Exec(`UPDATE rooms SET owner=? WHERE name=?`, target.Name, room)); e != nil {
			return e
		}
		if _, e = q.Exec(`UPDATE members SET role='member' WHERE room=? AND role='owner'`, room); e != nil {
			return e
		}
		if e = changedOne(q.Exec(`INSERT INTO members VALUES(?,?,'owner',?) ON CONFLICT(room,user) DO UPDATE SET role='owner'`, room, target.Name, time.Now().Unix())); e != nil {
			return e
		}
		return auditQ(q, u.Name, "room.transfer", target.Name, "", room)
	}
	if _, e = q.Exec(`INSERT OR IGNORE INTO room_meta(name) VALUES(?)`, room); e != nil {
		return e
	}
	if e = changedOne(q.Exec(`UPDATE room_meta SET `+column+`=? WHERE name=?`, data, room)); e != nil {
		return e
	}
	return auditQ(q, u.Name, "room."+key, room, value, room)
}
func (s *Store) RoomMember(actor, room, target, action string) error {
	return s.roomMemberForSession(actor, room, target, action, "")
}
func (s *Store) roomMemberForSession(actor, room, target, action, sessionID string) error {
	return s.writeTransaction(func(tx *sql.Tx) error {
		u, e := mutationActorQ(tx, actor)
		if e != nil {
			return e
		}
		if sessionID != "" {
			if valid, err := sessionCurrentQ(tx, sessionID, u.Name); err != nil || !valid {
				return errCredentialsChanged
			}
			if !canReadQ(tx, u.Name, room) {
				return errors.New("room access denied")
			}
		}
		if !canManageQ(tx, u.Name, room) {
			return errors.New("需要频道管理权限")
		}
		r, e := roomQ(tx, room)
		if e != nil {
			return e
		}
		other, e := resolveUserQ(tx, target)
		if e != nil {
			return e
		}
		if other.Name == r.Owner {
			return errors.New("不能移除或降级频道所有者")
		}
		if action == "mod" || action == "unmod" {
			if u.Name != r.Owner && !isAdmin(u.Role) {
				return errors.New("只有频道所有者或管理员可以分配版主")
			}
		}
		switch action {
		case "invite":
			if _, e = tx.Exec(`INSERT OR IGNORE INTO members VALUES(?,?,'member',?)`, room, other.Name, time.Now().Unix()); e == nil {
				e = notifyQ(tx, other.Name, "invite", u.Name, 0, "你已获准加入 #"+room)
			}
		case "remove":
			_, e = tx.Exec(`DELETE FROM members WHERE room=? AND user=?`, room, other.Name)
		case "mod", "unmod":
			role := "moderator"
			if action == "unmod" {
				role = "member"
			}
			e = changedOne(tx.Exec(`INSERT INTO members VALUES(?,?,?,?) ON CONFLICT(room,user) DO UPDATE SET role=excluded.role`, room, other.Name, role, time.Now().Unix()))
		default:
			return errors.New("成员操作：invite remove mod unmod")
		}
		if e != nil {
			return e
		}
		return auditQ(tx, u.Name, "member."+action, other.Name, "", room)
	})
}
func (s *Store) RoomMembers(actor, room string) ([]string, error) {
	if !s.CanRead(actor, room) {
		return nil, errors.New("无权查看频道成员")
	}
	rows, e := s.db.Query(`SELECT user,role FROM members WHERE room=? ORDER BY role,user`, room)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var user, role string
		if rows.Scan(&user, &role) == nil {
			names = append(names, user+" ["+role+"]")
		}
	}
	return names, rows.Err()
}
func (s *Store) LeaveRoom(user, room string) error {
	return s.leaveRoomForSession(user, room, "")
}
func (s *Store) leaveRoomForSession(user, room, sessionID string) error {
	return s.writeTransaction(func(tx *sql.Tx) error {
		u, err := mutationActorQ(tx, user)
		if err != nil {
			return err
		}
		if sessionID != "" {
			if valid, err := sessionCurrentQ(tx, sessionID, u.Name); err != nil || !valid {
				return errCredentialsChanged
			}
			if !canReadQ(tx, u.Name, room) {
				return errors.New("room access denied")
			}
		}
		r, err := roomQ(tx, room)
		if err != nil {
			return err
		}
		if r.Owner == u.Name {
			return errors.New("请先转让频道所有权")
		}
		if !canReadQ(tx, u.Name, room) {
			return errors.New("room access denied")
		}
		_, err = tx.Exec(`DELETE FROM members WHERE room=? AND user=?`, room, u.Name)
		return err
	})
}
func (s *Store) Report(user string, id int64, reason string) error {
	return s.writeTransaction(func(tx *sql.Tx) error { _, _, err := reportQ(tx, user, id, reason); return err })
}
func (s *Store) AdminList(actor, kind string) ([]string, error) {
	u, e := s.User(actor)
	if e != nil {
		return nil, e
	}
	if !isAdmin(u.Role) {
		return nil, errors.New("需要管理员权限")
	}
	var sqlText string
	switch kind {
	case "reports":
		sqlText = `SELECT printf('%d | %s | 消息#%d | %s | %s',id,user,message_id,reason,status) FROM reports ORDER BY id DESC LIMIT 100`
	case "bans":
		sqlText = `SELECT printf('%d | %s %s | #%s | expires=%d | %s | revoked=%d',id,kind,target,room,expires,reason,revoked) FROM sanctions ORDER BY id DESC LIMIT 100`
	case "audit":
		sqlText = `SELECT printf('%d | %s | %s | %s | %s | %s',id,datetime(at,'unixepoch'),actor,action,target,detail) FROM audit WHERE action NOT LIKE 'message.%' ORDER BY id DESC LIMIT 100`
	case "recalls":
		sqlText = `SELECT printf('%d | %s | #%s | %s',m.id,m.sender,m.room,m.body) FROM messages m JOIN message_meta x ON x.id=m.id LEFT JOIN room_meta r ON r.name=m.room WHERE x.recalled>0 AND m.recipient='' AND COALESCE(r.private,0)=0 ORDER BY m.id DESC LIMIT 100`
	case "errors":
		sqlText = `SELECT printf('%d | %s | %s | %s',id,datetime(at,'unixepoch'),kind,detail) FROM diagnostics ORDER BY id DESC LIMIT 100`
	default:
		return nil, errors.New("管理列表：reports bans audit recalls errors")
	}
	rows, e := s.db.Query(sqlText)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var line string
		if rows.Scan(&line) == nil {
			lines = append(lines, line)
		}
	}
	return lines, rows.Err()
}
func (s *Store) ResolveReport(actor string, id int64, status string) error {
	return s.writeTransaction(func(tx *sql.Tx) error { return resolveReportQ(tx, actor, id, status) })
}
func (s *Store) ReportMessage(actor string, id int64) (Message, error) {
	u, e := s.User(actor)
	if e != nil || !isAdmin(u.Role) {
		return Message{}, errors.New("需要管理员权限")
	}
	var messageID int64
	if e = s.db.QueryRow(`SELECT message_id FROM reports WHERE id=?`, id).Scan(&messageID); e != nil {
		return Message{}, errors.New("举报不存在")
	}
	m, e := messageQ(s.db, messageID)
	if e == nil {
		e = auditQ(s.db, actor, "report.read", strconv.FormatInt(id, 10), "查看用户主动举报的一条消息", "")
	}
	return m, e
}
func (s *Store) Diagnostic(kind, detail string) {
	_, _ = s.db.Exec(`INSERT INTO diagnostics(kind,detail,at) VALUES(?,?,?)`, cleanText(kind, 30), cleanText(detail, 400), time.Now().Unix())
}
func (s *Store) BadgeNames(user string) []string {
	rows, e := s.db.Query(`SELECT badge FROM badges WHERE user=? ORDER BY at`, user)
	if e != nil {
		return nil
	}
	defer rows.Close()
	var result []string
	for rows.Next() {
		var badge string
		if rows.Scan(&badge) == nil {
			result = append(result, badge)
		}
	}
	return result
}
func (s *Store) Award(user, badge string) {
	_, _ = s.db.Exec(`INSERT OR IGNORE INTO badges VALUES(?,?,?)`, user, badge, time.Now().Unix())
}
func (s *Store) Avatar(user, avatar string) error {
	switch avatar {
	case "camp", "cat", "robot", "planet", "flower", "ident":
	default:
		return errors.New("头像：camp cat robot planet flower ident")
	}
	_, e := s.db.Exec(`UPDATE user_meta SET avatar=? WHERE name=?`, avatar, user)
	return e
}
func (s *Store) PromoteOwner(name string) error {
	u, e := s.Resolve(name)
	if e != nil {
		return e
	}
	_, e = s.db.Exec(`UPDATE users SET role='owner' WHERE name=?`, u.Name)
	return e
}
func formatUnix(t int64) string {
	if t == 0 {
		return "永久"
	}
	return time.Unix(t, 0).In(time.FixedZone("CST", 8*3600)).Format("2006-01-02 15:04")
}
func parseInt64(s string) (int64, error) {
	n, e := strconv.ParseInt(s, 10, 64)
	if e != nil || n < 1 {
		return 0, fmt.Errorf("编号必须为正整数")
	}
	return n, nil
}
