package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const messageColumns = "id,room,sender,recipient,body,kind,at"

func roomACL(where string, args []any, room, user string) (string, []any) {
	return where + ` AND EXISTS(SELECT 1 FROM rooms ar LEFT JOIN room_meta am ON am.name=ar.name WHERE ar.name=? AND (COALESCE(am.private,0)=0 OR ar.owner=? OR EXISTS(SELECT 1 FROM members ax WHERE ax.room=ar.name AND ax.user=?)))`, append(args, room, user, user)
}

var mentionPattern = regexp.MustCompile(`@([\pL\pN_-]{2,})`)

func mentionTargets(text string) []string {
	var targets []string
	previousEnd := -1
	for _, indices := range mentionPattern.FindAllStringSubmatchIndex(text, -1) {
		if indices[0] > 0 {
			r, _ := utf8.DecodeLastRuneInString(text[:indices[0]])
			if (unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-') && indices[0] != previousEnd {
				continue
			}
		}
		name := text[indices[2]:indices[3]]
		if validName(name) {
			targets = append(targets, accountKey(name))
			previousEnd = indices[1]
		}
	}
	return targets
}

func messageQ(q queryer, id int64) (Message, error) {
	var m Message
	e := q.QueryRow(`SELECT `+messageColumns+` FROM messages WHERE id=?`, id).Scan(&m.ID, &m.Room, &m.Sender, &m.Recipient, &m.Body, &m.Kind, &m.At)
	if e == nil {
		_ = q.QueryRow(`SELECT reply_to,edited,recalled,recaller FROM message_meta WHERE id=?`, id).Scan(&m.ReplyTo, &m.Edited, &m.Recalled, &m.Recaller)
	}
	return m, e
}
func visibleMessageQ(q queryer, user string, m Message) bool {
	if m.Recipient != "" {
		return m.Sender == user || m.Recipient == user
	}
	return canReadQ(q, user, m.Room)
}
func (s *Store) Message(user string, id int64) (Message, error) {
	m, e := messageQ(s.db, id)
	if e != nil || !visibleMessageQ(s.db, user, m) {
		return Message{}, errors.New("消息不存在或没有权限")
	}
	messages := []Message{m}
	s.decorate(messages, user)
	return messages[0], nil
}
func (s *Store) decorate(messages []Message, user string) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := s.decorateContext(ctx, messages, user); err != nil {
		for i := range messages {
			messages[i].Body = "[暂时无法加载消息状态，请刷新历史]"
			messages[i].ReplyBody = ""
			messages[i].ReplyName = ""
		}
	}
}

func (s *Store) decorateContext(ctx context.Context, messages []Message, user string) error {
	if len(messages) == 0 {
		return nil
	}
	var args []any
	for _, m := range messages {
		args = append(args, m.ID)
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")
	metadata := map[int64]Message{}
	parents := map[int64]bool{}
	if rows, e := s.readDB().QueryContext(ctx, `SELECT m.id,CASE WHEN COALESCE(x.recalled,0)>0 THEN '[消息已撤回]' ELSE m.body END,COALESCE(x.reply_to,0),COALESCE(x.edited,0),COALESCE(x.recalled,0),COALESCE(x.recaller,'') FROM messages m LEFT JOIN message_meta x ON x.id=m.id WHERE m.id IN (`+marks+`)`, args...); e == nil {
		for rows.Next() {
			var m Message
			if e = rows.Scan(&m.ID, &m.Body, &m.ReplyTo, &m.Edited, &m.Recalled, &m.Recaller); e != nil {
				rows.Close()
				return e
			}
			metadata[m.ID] = m
			if m.ReplyTo > 0 {
				parents[m.ReplyTo] = true
			}
		}
		err := rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	} else {
		return e
	}
	summaries := map[int64][]string{}
	if rows, e := s.readDB().QueryContext(ctx, `SELECT message_id,reaction,count(*) FROM reactions WHERE message_id IN (`+marks+`) GROUP BY message_id,reaction ORDER BY reaction`, args...); e == nil {
		for rows.Next() {
			var id, count int64
			var reaction string
			if e = rows.Scan(&id, &reaction, &count); e != nil {
				rows.Close()
				return e
			}
			summaries[id] = append(summaries[id], fmt.Sprintf("%s:%d", reaction, count))
		}
		err := rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	} else {
		return e
	}
	sources := map[int64]Message{}
	if len(parents) > 0 {
		var values []any
		for id := range parents {
			values = append(values, id)
		}
		tokens := strings.TrimSuffix(strings.Repeat("?,", len(values)), ",")
		values = append(values, user, user, user, user)
		// Resolve every parent with the current viewer's ACL in one statement,
		// rather than issuing one room/member query per quoted message.
		if rows, e := s.readDB().QueryContext(ctx, `SELECT m.id,m.room,m.sender,m.recipient,CASE WHEN COALESCE(x.recalled,0)>0 THEN '[消息已撤回]' ELSE m.body END,m.kind,m.at FROM messages m LEFT JOIN message_meta x ON x.id=m.id WHERE m.id IN (`+tokens+`) AND ((m.recipient!='' AND (m.sender=? OR m.recipient=?)) OR (m.recipient='' AND EXISTS(SELECT 1 FROM rooms r LEFT JOIN room_meta rm ON rm.name=r.name WHERE r.name=m.room AND (COALESCE(rm.private,0)=0 OR r.owner=? OR EXISTS(SELECT 1 FROM members a WHERE a.room=r.name AND a.user=?)))))`, values...); e == nil {
			items, e := scanMessages(rows)
			if e != nil {
				return e
			}
			for _, item := range items {
				sources[item.ID] = item
			}
		} else {
			return e
		}
	}
	for i := range messages {
		m := &messages[i]
		meta := metadata[m.ID]
		if meta.ID > 0 {
			m.Body = meta.Body
		}
		m.ReplyTo = meta.ReplyTo
		m.Edited = meta.Edited
		m.Recalled = meta.Recalled
		m.Recaller = meta.Recaller
		m.ReactionSummary = strings.Join(summaries[m.ID], " ")
		m.ReplyName = ""
		m.ReplyBody = ""
		if source, ok := sources[m.ReplyTo]; ok {
			m.ReplyName = source.Sender
			m.ReplyBody = cleanText(source.Body, 70)
		}
	}
	return nil
}
func blockedQ(q queryer, owner, target string) bool {
	var n int
	_ = q.QueryRow(`SELECT count(*) FROM relations WHERE owner=? AND target=? AND kind='block'`, owner, target).Scan(&n)
	return n > 0
}
func notifyQ(q queryer, user, kind, actor string, ref int64, body string) error {
	if actor == user || blockedQ(q, user, actor) {
		return nil
	}
	if ref > 0 {
		var room string
		_ = q.QueryRow(`SELECT room FROM messages WHERE id=?`, ref).Scan(&room)
		if room != "" {
			var muted bool
			_ = q.QueryRow(`SELECT muted FROM preferences WHERE user=? AND scope=?`, user, "room:"+room).Scan(&muted)
			if muted {
				return nil
			}
		}
	}
	_, e := q.Exec(`INSERT INTO notifications(user,kind,actor,ref,body,at) SELECT name,?,?,?,?,? FROM users WHERE name=?`, kind, actor, ref, cleanText(body, 180), time.Now().Unix(), user)
	return e
}
func activeSanctionQ(q queryer, kind, user, room string) bool {
	var n int
	_ = q.QueryRow(`SELECT count(*) FROM sanctions WHERE kind=? AND target=? AND (room='' OR room=?) AND revoked=0 AND (expires=0 OR expires>?)`, kind, user, room, time.Now().Unix()).Scan(&n)
	return n > 0
}
func canSendQ(q queryer, user, room, recipient string) error {
	u, e := userQ(q, user)
	if e != nil {
		return errors.New("账户不存在")
	}
	if u.Banned || activeSanctionQ(q, "user", user, "") {
		return errors.New("账户已被封禁")
	}
	if u.MustReset {
		return errors.New("请先使用 /passwd 修改临时密码")
	}
	var maintenance string
	_ = q.QueryRow(`SELECT value FROM config WHERE key='maintenance'`).Scan(&maintenance)
	if maintenance == "on" && !isAdmin(u.Role) {
		return errors.New("聊天室维护中，暂时不能发言")
	}
	if activeSanctionQ(q, "mute", user, room) {
		return errors.New("你当前处于禁言状态")
	}
	if recipient != "" {
		if recipient == user {
			return errors.New("不能给自己发私信")
		}
		other, e := userQ(q, recipient)
		if e != nil || other.Banned {
			return errors.New("对方账户不可用")
		}
		if blockedQ(q, recipient, user) {
			return errors.New("对方不接受私信")
		}
		return nil
	}
	if !canReadQ(q, user, room) {
		return errors.New("你没有频道权限")
	}
	r, e := roomQ(q, room)
	if e != nil {
		return e
	}
	if r.Archived {
		return errors.New("频道已归档，仅可阅读")
	}
	if r.ReadOnly && !canManageQ(q, user, room) {
		return errors.New("频道为只读模式")
	}
	if r.Slow > 0 && !canManageQ(q, user, room) {
		var last int64
		_ = q.QueryRow(`SELECT COALESCE(max(at),0) FROM messages WHERE room=? AND sender=? AND recipient=''`, room, user).Scan(&last)
		if time.Now().Unix()-last < int64(r.Slow) {
			return fmt.Errorf("慢速模式：每 %d 秒可发一条", r.Slow)
		}
	}
	return nil
}
func (s *Store) Send(m Message) (Message, error) {
	m.Sender = accountKey(m.Sender)
	m.Body = cleanText(m.Body, 800)
	m.At = time.Now().Unix()
	if m.Body == "" {
		return m, errors.New("消息不能为空")
	}
	if m.Kind != "chat" && m.Kind != "action" {
		return m, errors.New("未知消息类型")
	}
	tx, e := s.db.Begin()
	if e != nil {
		return m, e
	}
	defer tx.Rollback()
	if e = canSendQ(tx, m.Sender, m.Room, m.Recipient); e != nil {
		return m, e
	}
	if m.Recipient != "" {
		m.Room = ""
	}
	if m.ReplyTo > 0 {
		source, e := messageQ(tx, m.ReplyTo)
		if e != nil || !visibleMessageQ(tx, m.Sender, source) {
			return m, errors.New("回复目标不存在或无权查看")
		}
		same := source.Room == m.Room && source.Recipient == "" && m.Recipient == ""
		if m.Recipient != "" {
			same = source.Room == "" && ((source.Sender == m.Sender && source.Recipient == m.Recipient) || (source.Sender == m.Recipient && source.Recipient == m.Sender))
		}
		if !same {
			return m, errors.New("不能跨频道或跨私聊引用消息")
		}
	}
	r, e := tx.Exec(`INSERT INTO messages(room,sender,recipient,body,kind,at) VALUES(?,?,?,?,?,?)`, m.Room, m.Sender, m.Recipient, m.Body, m.Kind, m.At)
	if e != nil {
		return m, e
	}
	m.ID, e = r.LastInsertId()
	if e != nil {
		return m, e
	}
	if _, e = tx.Exec(`INSERT INTO message_meta(id,reply_to) VALUES(?,?)`, m.ID, m.ReplyTo); e != nil {
		return m, e
	}
	if m.Recipient != "" {
		if e = notifyQ(tx, m.Recipient, "dm", m.Sender, m.ID, "收到私信"); e != nil {
			return m, e
		}
	}
	seen := map[string]bool{}
	for _, target := range mentionTargets(m.Body) {
		if seen[target] || target == m.Sender {
			continue
		}
		seen[target] = true
		if m.Recipient != "" && target != m.Recipient {
			continue
		}
		if m.Recipient == "" && !canReadQ(tx, target, m.Room) {
			continue
		}
		if e = notifyQ(tx, target, "mention", m.Sender, m.ID, "有人提到了你"); e != nil {
			return m, e
		}
	}
	if m.ReplyTo > 0 {
		source, _ := messageQ(tx, m.ReplyTo)
		if e = notifyQ(tx, source.Sender, "reply", m.Sender, m.ID, "有人回复了你"); e != nil {
			return m, e
		}
	}
	if e = tx.Commit(); e != nil {
		return m, e
	}
	messages := []Message{m}
	s.decorate(messages, m.Sender)
	return messages[0], nil
}
func (s *Store) UpdateMessage(actor string, id int64, body string, recall bool) error {
	tx, e := s.db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	m, e := messageQ(tx, id)
	if e != nil || !visibleMessageQ(tx, actor, m) {
		return errors.New("消息不存在或没有权限")
	}
	u, e := userQ(tx, actor)
	if e != nil {
		return e
	}
	if u.Banned || activeSanctionQ(tx, "user", actor, "") {
		return errors.New("账户已被封禁")
	}
	if m.Recalled > 0 {
		return errors.New("消息已经撤回")
	}
	own := m.Sender == actor
	if !own {
		if !recall || m.Recipient != "" || !canManageQ(tx, actor, m.Room) {
			return errors.New("不能修改他人消息")
		}
	} else if !isAdmin(u.Role) && time.Now().Unix()-m.At > 300 {
		return errors.New("自己的消息仅可在发送后 5 分钟内修改或撤回")
	}
	action := "edit"
	if recall {
		action = "recall"
		body = "[消息已撤回]"
	} else {
		body = cleanText(body, 800)
		if body == "" {
			return errors.New("消息不能为空")
		}
	}
	if _, e = tx.Exec(`INSERT INTO revisions(message_id,actor,action,body,at) VALUES(?,?,?,?,?)`, id, actor, action, m.Body, time.Now().Unix()); e != nil {
		return e
	}
	if _, e = tx.Exec(`UPDATE messages SET body=? WHERE id=?`, body, id); e != nil {
		return e
	}
	if _, e = tx.Exec(`INSERT OR IGNORE INTO message_meta(id) VALUES(?)`, id); e != nil {
		return e
	}
	if recall {
		_, e = tx.Exec(`UPDATE message_meta SET recalled=?,recaller=? WHERE id=?`, time.Now().Unix(), actor, id)
	} else {
		_, e = tx.Exec(`UPDATE message_meta SET edited=? WHERE id=?`, time.Now().Unix(), id)
	}
	if e != nil {
		return e
	}
	if e = auditQ(tx, actor, "message."+action, strconv.FormatInt(id, 10), "", m.Room); e != nil {
		return e
	}
	return tx.Commit()
}

type Revision struct {
	Actor, Action, Body string
	At                  int64
}

func (s *Store) Revisions(actor string, id int64) ([]Revision, error) {
	m, e := s.Message(actor, id)
	if e != nil {
		return nil, e
	}
	u, e := s.User(actor)
	if e != nil {
		return nil, e
	}
	if actor != m.Sender && (m.Recipient != "" || !canManageQ(s.db, actor, m.Room) || !isAdmin(u.Role) && u.Role != "moderator") {
		return nil, errors.New("没有审计该消息的权限")
	}
	rows, e := s.db.Query(`SELECT actor,action,body,at FROM revisions WHERE message_id=? ORDER BY id`, id)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var result []Revision
	for rows.Next() {
		var v Revision
		if e = rows.Scan(&v.Actor, &v.Action, &v.Body, &v.At); e != nil {
			return nil, e
		}
		result = append(result, v)
	}
	if isAdmin(u.Role) {
		_ = auditAfterRows(rows, s.db, actor, id)
	}
	return result, rows.Err()
}

// Finish the reader before appending an audit row (the SQLite pool is bounded).
func auditAfterRows(rows interface{ Close() error }, q queryer, actor string, id int64) error {
	rows.Close()
	return auditQ(q, actor, "audit.message.read", strconv.FormatInt(id, 10), "", "")
}
func (s *Store) React(user string, id int64, reaction string) error {
	m, e := s.Message(user, id)
	if e != nil {
		return e
	}
	if m.Recalled > 0 {
		return errors.New("不能回应已撤回消息")
	}
	allowed := map[string]bool{"like": true, "laugh": true, "question": true, "heart": true, "wow": true, "sad": true}
	if !allowed[reaction] {
		return errors.New("回应：like laugh question heart wow sad")
	}
	tx, e := s.db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var n int
	_ = tx.QueryRow(`SELECT count(*) FROM reactions WHERE message_id=? AND user=? AND reaction=?`, id, user, reaction).Scan(&n)
	if n > 0 {
		_, e = tx.Exec(`DELETE FROM reactions WHERE message_id=? AND user=? AND reaction=?`, id, user, reaction)
	} else {
		_, e = tx.Exec(`INSERT INTO reactions VALUES(?,?,?,?)`, id, user, reaction, time.Now().Unix())
	}
	if e != nil {
		return e
	}
	return tx.Commit()
}
func (s *Store) Bookmark(user string, id int64, remove bool) error {
	if _, e := s.Message(user, id); e != nil {
		return e
	}
	if remove {
		_, e := s.db.Exec(`DELETE FROM bookmarks WHERE user=? AND message_id=?`, user, id)
		return e
	}
	_, e := s.db.Exec(`INSERT OR IGNORE INTO bookmarks VALUES(?,?,?)`, user, id, time.Now().Unix())
	return e
}
func (s *Store) Bookmarks(user string) ([]Message, error) {
	rows, e := s.db.Query(`SELECT m.* FROM messages m JOIN bookmarks b ON b.message_id=m.id WHERE b.user=? ORDER BY b.at DESC LIMIT 100`, user)
	if e != nil {
		return nil, e
	}
	messages, e := scanMessages(rows)
	if e != nil {
		return nil, e
	}
	var result []Message
	for _, m := range messages {
		if visibleMessageQ(s.db, user, m) {
			result = append(result, m)
		}
	}
	s.decorate(result, user)
	return result, nil
}
func (s *Store) Page(room, user, peer string, before int64, limit int) ([]Message, error) {
	if before <= 0 {
		before = 1<<63 - 1
	}
	return s.pageMessages(room, user, peer, before, limit, false)
}

// PageAfter returns the adjacent chronological page after an exclusive ID.
func (s *Store) PageAfter(room, user, peer string, after int64, limit int) ([]Message, error) {
	return s.pageMessages(room, user, peer, max(int64(0), after), limit, true)
}

func directMessagePageQuery(user, peer string, cursor int64, limit int, forward bool) (string, []any) {
	comparison, order := "<", "DESC"
	if forward {
		comparison, order = ">", "ASC"
	}
	// The former OR collected both history ranges before sorting their union.
	// Each direction now reads an ordered sender/recipient ID range and stops
	// at the page size; the final merge can contain at most twice that many rows.
	branch := `SELECT ` + messageColumns + ` FROM messages INDEXED BY dm_history WHERE sender=? AND recipient=? AND room='' AND id` + comparison + `? ORDER BY id ` + order + ` LIMIT ?`
	args := []any{user, peer, cursor, limit}
	if user == peer {
		return branch, args
	}
	query := `SELECT * FROM (SELECT * FROM (` + branch + `) UNION ALL SELECT * FROM (` + branch + `)) ORDER BY id ` + order + ` LIMIT ?`
	return query, append(args, peer, user, cursor, limit, limit)
}

func (s *Store) pageMessages(room, user, peer string, cursor int64, limit int, forward bool) ([]Message, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	limit = max(1, min(200, limit))
	var query string
	var args []any
	if peer != "" {
		query, args = directMessagePageQuery(user, peer, cursor, limit, forward)
	} else {
		var allowed bool
		if err := s.readDB().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM rooms r LEFT JOIN room_meta m ON m.name=r.name WHERE r.name=? AND (COALESCE(m.private,0)=0 OR r.owner=? OR EXISTS(SELECT 1 FROM members a WHERE a.room=r.name AND a.user=?)))`, room, user, user).Scan(&allowed); err != nil {
			return nil, err
		}
		if !allowed {
			return nil, errors.New("无权查看该频道")
		}
		where := `room=? AND recipient=''`
		args = []any{room}
		where, args = roomACL(where, args, room, user)
		comparison, order := "<", "DESC"
		if forward {
			comparison, order = ">", "ASC"
		}
		query = `SELECT ` + messageColumns + ` FROM messages WHERE ` + where + ` AND id` + comparison + `? ORDER BY id ` + order + ` LIMIT ?`
		args = append(args, cursor, limit)
	}
	rows, e := s.readDB().QueryContext(ctx, query, args...)
	if e != nil {
		return nil, e
	}
	result, e := scanMessages(rows)
	if !forward {
		for i, j := 0, len(result)-1; i < j; i, j = i+1, j-1 {
			result[i], result[j] = result[j], result[i]
		}
	}
	if e == nil {
		e = s.decorateContext(ctx, result, user)
	}
	if e != nil {
		return nil, e
	}
	return result, e
}
func (s *Store) SearchAll(room, user, peer, query string) ([]Message, error) {
	if peer == "" && !s.CanRead(user, room) {
		return nil, errors.New("无权搜索该频道")
	}
	parts := strings.Fields(query)
	var sender, after, before string
	var words []string
	for _, p := range parts {
		switch {
		case strings.HasPrefix(p, "from:"):
			sender = strings.TrimPrefix(p, "from:")
		case strings.HasPrefix(p, "after:"):
			after = strings.TrimPrefix(p, "after:")
		case strings.HasPrefix(p, "before:"):
			before = strings.TrimPrefix(p, "before:")
		default:
			words = append(words, p)
		}
	}
	var args []any
	where := `recipient='' AND room=?`
	args = []any{room}
	if peer != "" {
		where = `room='' AND ((sender=? AND recipient=?) OR (sender=? AND recipient=?))`
		args = []any{user, peer, peer, user}
	}
	if sender != "" {
		u, e := s.Resolve(sender)
		if e != nil {
			return nil, e
		}
		where += " AND sender=?"
		args = append(args, u.Name)
	}
	for i, v := range []string{after, before} {
		if v != "" {
			d, e := time.ParseInLocation("2006-01-02", v, time.FixedZone("CST", 8*3600))
			if e != nil {
				return nil, errors.New("日期格式为 YYYY-MM-DD")
			}
			if i == 0 {
				where += " AND at>=?"
			} else {
				where += " AND at<?"
			}
			args = append(args, d.Unix())
		}
	}
	text := strings.Join(words, " ")
	if text != "" {
		if utf8.RuneCountInString(text) >= 3 {
			where += ` AND id IN (SELECT rowid FROM message_search WHERE message_search MATCH ?)`
			args = append(args, `"`+strings.ReplaceAll(text, `"`, `""`)+`"`)
		} else {
			where += ` AND body LIKE ? ESCAPE '\'`
			text = strings.NewReplacer("\\", "\\\\", "%", "\\%", "_", "\\_").Replace(text)
			args = append(args, "%"+text+"%")
		}
	}
	if peer == "" {
		where, args = roomACL(where, args, room, user)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	rows, e := s.readDB().QueryContext(ctx, `SELECT `+messageColumns+` FROM messages WHERE `+where+` ORDER BY id DESC LIMIT 200`, args...)
	if e != nil {
		return nil, e
	}
	result, e := scanMessages(rows)
	if e == nil {
		e = s.decorateContext(ctx, result, user)
	}
	if e != nil {
		return nil, e
	}
	return result, e
}

type Notification struct {
	ID, Ref, At       int64
	Kind, Actor, Body string
	Seen              bool
}

func (s *Store) Notify(user, kind, actor string, ref int64, body string) error {
	return notifyQ(s.db, user, kind, actor, ref, body)
}
func (s *Store) Notifications(user string, mark bool) ([]Notification, error) {
	rows, e := s.db.Query(`SELECT id,ref,at,kind,actor,body,seen FROM notifications WHERE user=? ORDER BY id DESC LIMIT 100`, user)
	if e != nil {
		return nil, e
	}
	var result []Notification
	for rows.Next() {
		var n Notification
		if e = rows.Scan(&n.ID, &n.Ref, &n.At, &n.Kind, &n.Actor, &n.Body, &n.Seen); e != nil {
			rows.Close()
			return nil, e
		}
		result = append(result, n)
	}
	e = rows.Close()
	if e != nil {
		return nil, e
	}
	if mark {
		_, e = s.db.Exec(`UPDATE notifications SET seen=1 WHERE user=? AND id<=?`, user, maxNotificationID(result))
	}
	return result, e
}
func maxNotificationID(items []Notification) int64 {
	var id int64
	for _, n := range items {
		id = max(id, n.ID)
	}
	return id
}
func (s *Store) UnreadNotifications(user string) int {
	var n int
	_ = s.db.QueryRow(`SELECT count(*) FROM notifications WHERE user=? AND seen=0`, user).Scan(&n)
	return n
}
func (s *Store) Poke(actor, target string) error {
	u, e := s.Resolve(target)
	if e != nil {
		return e
	}
	target = u.Name
	if target == actor {
		return errors.New("不能戳自己")
	}
	if s.Blocked(target, actor) {
		return errors.New("对方不接受戳一戳")
	}
	prefs := s.Settings(target)
	if prefs["poke"] == "off" || prefs["dnd"] == "on" {
		return errors.New("对方当前不接受戳一戳")
	}
	tx, e := s.db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var last int64
	_ = tx.QueryRow(`SELECT at FROM poke_limits WHERE actor=? AND target=?`, actor, target).Scan(&last)
	if time.Now().Unix()-last < 30 {
		return errors.New("同一用户每 30 秒只能戳一次")
	}
	if _, e = tx.Exec(`INSERT INTO poke_limits VALUES(?,?,?) ON CONFLICT(actor,target) DO UPDATE SET at=excluded.at`, actor, target, time.Now().Unix()); e != nil {
		return e
	}
	if e = notifyQ(tx, target, "poke", actor, 0, "戳了戳你"); e != nil {
		return e
	}
	return tx.Commit()
}

type Position struct {
	Pinned, Subscribed, Muted bool
	LastRead                  int64
	Offset                    int
	Draft                     string
	ReplyTo                   int64
	Anchor                    int64
	Version                   int64
}

var ErrPositionConflict = errors.New("另一设备已更新草稿，当前内容已保留，请重新选择后保存")

func scopeFor(room, peer string) string {
	if peer != "" {
		return "dm:" + peer
	}
	return "room:" + room
}
func (s *Store) Position(user, scope string) Position {
	p, _ := s.PositionWithError(user, scope)
	return p
}

func (s *Store) PositionWithError(user, scope string) (Position, error) {
	var p Position
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	err := s.readDB().QueryRowContext(ctx, `SELECT p.pinned,p.subscribed,p.muted,p.last_read,p.offset,p.draft,p.reply_to,p.anchor,COALESCE(v.version,0) FROM preferences p LEFT JOIN position_versions v ON v.user=p.user AND v.scope=p.scope WHERE p.user=? AND p.scope=?`, user, scope).Scan(&p.Pinned, &p.Subscribed, &p.Muted, &p.LastRead, &p.Offset, &p.Draft, &p.ReplyTo, &p.Anchor, &p.Version)
	if errors.Is(err, sql.ErrNoRows) {
		return Position{}, nil
	}
	if err != nil {
		return Position{}, err
	}
	return p, nil
}
func (s *Store) SavePosition(user, scope string, p Position) error {
	// Trusted callers can replace a snapshot explicitly; UI callers retain their
	// loaded Version and use SavePositionVersioned to detect concurrent edits.
	loaded, err := s.PositionWithError(user, scope)
	if err != nil {
		return err
	}
	p.Version = loaded.Version
	_, err = s.SavePositionVersioned(user, scope, p)
	return err
}

func (s *Store) SavePositionVersioned(user, scope string, p Position) (int64, error) {
	if p.Version < 0 || p.Version == 1<<63-1 {
		return p.Version, ErrPositionConflict
	}
	err := s.writeTransaction(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO position_versions(user,scope,version) VALUES(?,?,0)`, user, scope); err != nil {
			return err
		}
		result, err := tx.Exec(`UPDATE position_versions SET version=version+1 WHERE user=? AND scope=? AND version=?`, user, scope, p.Version)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return ErrPositionConflict
		}
		return changedOne(tx.Exec(`INSERT INTO preferences(user,scope,last_read,offset,draft,reply_to,anchor) VALUES(?,?,?,?,?,?,?) ON CONFLICT(user,scope) DO UPDATE SET last_read=max(preferences.last_read,excluded.last_read),offset=excluded.offset,draft=excluded.draft,reply_to=excluded.reply_to,anchor=excluded.anchor`, user, scope, p.LastRead, max(0, p.Offset), cleanText(p.Draft, 1600), p.ReplyTo, p.Anchor))
	})
	if err != nil {
		return p.Version, err
	}
	return p.Version + 1, nil
}
func (s *Store) Preference(user, scope, key string, on bool) error {
	if key != "pinned" && key != "subscribed" && key != "muted" {
		return errors.New("无效频道偏好")
	}
	if strings.HasPrefix(scope, "room:") && !s.CanRead(user, strings.TrimPrefix(scope, "room:")) {
		return errors.New("无权访问频道")
	}
	_, e := s.db.Exec(`INSERT INTO preferences(user,scope,`+key+`) VALUES(?,?,?) ON CONFLICT(user,scope) DO UPDATE SET `+key+`=excluded.`+key, user, scope, on)
	return e
}
func (s *Store) UnreadIn(user, room, peer string) int {
	scope := scopeFor(room, peer)
	p := s.Position(user, scope)
	if peer == "" && !p.Subscribed {
		return 0
	}
	var n int
	if peer != "" {
		_ = s.db.QueryRow(`SELECT count(*) FROM messages WHERE room='' AND sender=? AND recipient=? AND id>?`, peer, user, p.LastRead).Scan(&n)
	} else if s.CanRead(user, room) {
		_ = s.db.QueryRow(`SELECT count(*) FROM messages WHERE room=? AND recipient='' AND sender!=? AND id>?`, room, user, p.LastRead).Scan(&n)
	}
	return n
}
func validMessageBody(s string) bool { return utf8.ValidString(s) && utf8.RuneCountInString(s) <= 800 }
