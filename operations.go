package main

import (
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
	var mode, quota string
	_ = tx.QueryRow(`SELECT value FROM config WHERE key='registration'`).Scan(&mode)
	_ = tx.QueryRow(`SELECT value FROM config WHERE key='registration_quota'`).Scan(&quota)
	if mode == "closed" {
		return User{}, errors.New("管理员已暂停注册")
	}
	if ip != "" {
		maxRegistrations, _ := strconv.Atoi(quota)
		var count int
		_ = tx.QueryRow(`SELECT count(*) FROM logins WHERE ip=? AND client='registration' AND at>?`, ip, time.Now().Unix()-3600).Scan(&count)
		if count >= max(1, maxRegistrations) {
			return User{}, errors.New("此 IP 的每小时注册配额已用完")
		}
	}
	if mode == "invite" {
		r, e := tx.Exec(`UPDATE invites SET remaining=remaining-1 WHERE hash=? AND remaining>0 AND (expires=0 OR expires>?)`, tokenHash(strings.TrimSpace(invite)), time.Now().Unix())
		if e != nil {
			return User{}, e
		}
		n, _ := r.RowsAffected()
		if n != 1 {
			return User{}, errors.New("邀请码无效、已用完或过期")
		}
	}
	if _, e = tx.Exec(`INSERT INTO users(name,hash,salt,created) VALUES(?,?,?,?)`, name, hash, salt, time.Now().Unix()); e != nil {
		return User{}, errors.New("用户名已被注册，或注册暂不可用")
	}
	if _, e = tx.Exec(`INSERT INTO user_meta(name) VALUES(?)`, name); e != nil {
		return User{}, e
	}
	if _, e = tx.Exec(`INSERT INTO badges VALUES(?,'开拓者',?)`, name, time.Now().Unix()); e != nil {
		return User{}, e
	}
	if ip != "" {
		if _, e = tx.Exec(`INSERT INTO logins(user,ip,client,ok,at) VALUES(?,?,'registration',1,?)`, name, ip, time.Now().Unix()); e != nil {
			return User{}, e
		}
	}
	if e = tx.Commit(); e != nil {
		return User{}, e
	}
	return s.registrationUser(name)
}
func (s *Store) InviteCode(actor, duration string, uses int) (string, error) {
	if uses < 1 || uses > 100 {
		return "", errors.New("使用次数范围 1–100")
	}
	seconds, e := durationSeconds(duration)
	if e != nil {
		return "", e
	}
	token, e := secretToken()
	if e != nil {
		return "", e
	}
	expires := int64(0)
	if seconds > 0 {
		expires = time.Now().Unix() + seconds
	}
	e = s.writeTransaction(func(tx *sql.Tx) error {
		u, err := mutationActorQ(tx, actor)
		if err != nil {
			return err
		}
		if !isAdmin(u.Role) {
			return errors.New("需要管理员权限")
		}
		if err = changedOne(tx.Exec(`INSERT INTO invites VALUES(?,?,?,?,?)`, tokenHash(token), u.Name, expires, uses, time.Now().Unix())); err != nil {
			return err
		}
		return auditQ(tx, u.Name, "invite.create", "", "次数 "+strconv.Itoa(uses), "")
	})
	if e != nil {
		return "", e
	}
	return token, e
}
func (s *Store) ResetPassword(actor, target string) (string, error) {
	u, e := s.User(actor)
	if e != nil {
		return "", e
	}
	other, e := s.Resolve(target)
	if e != nil {
		return "", e
	}
	if !isAdmin(u.Role) || other.Name == actor || roleRank(other.Role) >= roleRank(u.Role) {
		return "", errors.New("不能重置此账户的密码")
	}
	password, e := secretToken()
	if e != nil {
		return "", e
	}
	salt := make([]byte, 16)
	if _, e = rand.Read(salt); e != nil {
		return "", e
	}
	hash := passwordHash(password, salt)
	tx, e := s.db.Begin()
	if e != nil {
		return "", e
	}
	defer tx.Rollback()
	currentActor, e := currentCredentialUserQ(tx, u.Name)
	if e != nil {
		return "", e
	}
	currentTarget, e := userQ(tx, other.Name)
	if e != nil {
		return "", e
	}
	if currentActor.MustReset || !isAdmin(currentActor.Role) || currentTarget.Name == currentActor.Name || roleRank(currentTarget.Role) >= roleRank(currentActor.Role) {
		return "", errors.New("不能重置此账户的密码")
	}
	if currentTarget.AuthEpoch < 0 || currentTarget.AuthEpoch == 1<<63-1 {
		return "", errCredentialsChanged
	}
	if e = credentialWrite(tx.Exec(`UPDATE users SET hash=?,salt=? WHERE name=?`, hash, salt, other.Name)); e != nil {
		return "", e
	}
	if e = setCredentialEpochQ(tx, other.Name, currentTarget.AuthEpoch+1); e != nil {
		return "", e
	}
	if e = credentialWrite(tx.Exec(`UPDATE user_meta SET must_reset=1 WHERE name=?`, other.Name)); e != nil {
		return "", e
	}
	if _, e = tx.Exec(`UPDATE ssh_keys SET revoked=1 WHERE user=? AND revoked=0`, other.Name); e != nil {
		return "", e
	}
	if _, e = tx.Exec(`UPDATE sessions SET revoked=1 WHERE user=? AND closed=0`, other.Name); e != nil {
		return "", e
	}
	if e = auditQ(tx, actor, "password.reset", other.Name, "临时密码需首次登录修改", ""); e != nil {
		return "", e
	}
	return password, tx.Commit()
}
func (s *Store) RoomSetting(actor, room, key, value string) error {
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
	return s.writeTransaction(func(tx *sql.Tx) error {
		u, e := mutationActorQ(tx, actor)
		if e != nil {
			return e
		}
		r, e := roomQ(tx, room)
		if e != nil {
			return e
		}
		if !canManageQ(tx, u.Name, room) {
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
			target, e := resolveUserQ(tx, value)
			if e != nil {
				return e
			}
			if e = changedOne(tx.Exec(`UPDATE rooms SET owner=? WHERE name=?`, target.Name, room)); e != nil {
				return e
			}
			if _, e = tx.Exec(`UPDATE members SET role='member' WHERE room=? AND role='owner'`, room); e != nil {
				return e
			}
			if e = changedOne(tx.Exec(`INSERT INTO members VALUES(?,?,'owner',?) ON CONFLICT(room,user) DO UPDATE SET role='owner'`, room, target.Name, time.Now().Unix())); e != nil {
				return e
			}
			return auditQ(tx, u.Name, "room.transfer", target.Name, "", room)
		}
		if _, e = tx.Exec(`INSERT OR IGNORE INTO room_meta(name) VALUES(?)`, room); e != nil {
			return e
		}
		if e = changedOne(tx.Exec(`UPDATE room_meta SET `+column+`=? WHERE name=?`, data, room)); e != nil {
			return e
		}
		return auditQ(tx, u.Name, "room."+key, room, value, room)
	})
}
func (s *Store) RoomMember(actor, room, target, action string) error {
	return s.writeTransaction(func(tx *sql.Tx) error {
		u, e := mutationActorQ(tx, actor)
		if e != nil {
			return e
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
	r, e := s.RoomInfo(room)
	if e != nil {
		return e
	}
	if r.Owner == user {
		return errors.New("请先转让频道所有权")
	}
	_, e = s.db.Exec(`DELETE FROM members WHERE room=? AND user=?`, room, user)
	return e
}
func (s *Store) Report(user string, id int64, reason string) error {
	if _, e := s.Message(user, id); e != nil {
		return e
	}
	if cleanText(reason, 240) == "" {
		return errors.New("请填写举报原因")
	}
	var count int
	_ = s.db.QueryRow(`SELECT count(*) FROM reports WHERE user=? AND at>?`, user, time.Now().Unix()-3600).Scan(&count)
	if count >= 10 {
		return errors.New("每小时最多提交 10 次举报")
	}
	_, e := s.db.Exec(`INSERT INTO reports(user,message_id,reason,at) VALUES(?,?,?,?)`, user, id, cleanText(reason, 240), time.Now().Unix())
	return e
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
	if status != "resolved" && status != "rejected" {
		return errors.New("状态为 resolved 或 rejected")
	}
	return s.writeTransaction(func(tx *sql.Tx) error {
		u, e := mutationActorQ(tx, actor)
		if e != nil {
			return e
		}
		if !isAdmin(u.Role) {
			return errors.New("需要管理员权限")
		}
		if e = changedOne(tx.Exec(`UPDATE reports SET status=?,resolver=? WHERE id=?`, status, u.Name, id)); e != nil {
			return e
		}
		return auditQ(tx, u.Name, "report."+status, strconv.FormatInt(id, 10), "", "")
	})
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
