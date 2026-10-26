package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

type CommunityObject struct {
	ID                                int64
	Kind, Room, Owner, Title, Payload string
	Created                           int64
	Closed                            bool
}

const maxCommunityPayloadBytes = 32768
const maxWordGuesses = 128

var ErrObjectTooLarge = errors.New("内容状态超过安全显示限制，发起者可关闭后重新开始")

func objectQ(q queryer, id int64) (CommunityObject, error) {
	var o CommunityObject
	var payload sql.NullString
	e := q.QueryRow(`SELECT id,kind,room,owner,title,CASE WHEN length(CAST(payload AS BLOB))<=? THEN payload ELSE NULL END,created,closed FROM objects WHERE id=?`, maxCommunityPayloadBytes, id).Scan(&o.ID, &o.Kind, &o.Room, &o.Owner, &o.Title, &payload, &o.Created, &o.Closed)
	if e == nil {
		if !payload.Valid {
			return o, ErrObjectTooLarge
		}
		o.Payload = payload.String
	}
	return o, e
}

func objectMetadataQ(q queryer, id int64) (CommunityObject, error) {
	var o CommunityObject
	err := q.QueryRow(`SELECT id,kind,room,owner,title,created,closed FROM objects WHERE id=?`, id).Scan(&o.ID, &o.Kind, &o.Room, &o.Owner, &o.Title, &o.Created, &o.Closed)
	return o, err
}
func (s *Store) Object(user string, id int64) (CommunityObject, error) {
	o, e := objectQ(s.db, id)
	if e != nil && !errors.Is(e, ErrObjectTooLarge) || !s.CanRead(user, o.Room) {
		return CommunityObject{}, errors.New("内容不存在或没有权限")
	}
	if e != nil {
		return CommunityObject{}, e
	}
	return redactObject(o), nil
}
func redactObject(o CommunityObject) CommunityObject {
	if o.Kind == "guess" || o.Kind == "word" {
		var p map[string]any
		if json.Unmarshal([]byte(o.Payload), &p) == nil {
			if !o.Closed {
				delete(p, "secret")
			}
			b, _ := json.Marshal(p)
			o.Payload = string(b)
		}
	}
	return o
}
func (s *Store) Objects(user, room, kind string) ([]CommunityObject, error) {
	if !s.CanRead(user, room) {
		return nil, errors.New("无权查看该频道")
	}
	rows, e := s.db.Query(`SELECT id,kind,room,owner,title,CASE WHEN length(CAST(payload AS BLOB))<=? THEN payload ELSE NULL END,created,closed FROM objects WHERE room=? AND (?='' OR kind=?) ORDER BY id DESC LIMIT 100`, maxCommunityPayloadBytes, room, kind, kind)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var result []CommunityObject
	for rows.Next() {
		var o CommunityObject
		var payload sql.NullString
		if e = rows.Scan(&o.ID, &o.Kind, &o.Room, &o.Owner, &o.Title, &payload, &o.Created, &o.Closed); e != nil {
			return nil, e
		}
		if !payload.Valid {
			o.Title = cleanText(o.Title+" [状态过大，无法显示]", 120)
			o.Payload = `{"unavailable":true}`
			result = append(result, o)
			continue
		}
		o.Payload = payload.String
		result = append(result, redactObject(o))
	}
	return result, rows.Err()
}
func (s *Store) NewObject(user, room, kind, title, payload string) (int64, error) {
	if e := canSendQ(s.db, user, room, ""); e != nil {
		return 0, e
	}
	title = cleanText(title, 100)
	if title == "" {
		return 0, errors.New("标题不能为空")
	}
	if len(payload) > maxCommunityPayloadBytes {
		return 0, errors.New("内容过长")
	}
	switch kind {
	case "poll", "post", "event", "art", "ttt", "guess", "word":
	default:
		return 0, errors.New("无效社区内容类型")
	}
	tx, e := s.db.Begin()
	if e != nil {
		return 0, e
	}
	defer tx.Rollback()
	if e = canSendQ(tx, user, room, ""); e != nil {
		return 0, e
	}
	if kind == "guess" || kind == "word" {
		var count int
		if e = tx.QueryRow(`SELECT count(*) FROM objects WHERE room=? AND kind=? AND closed=0`, room, kind).Scan(&count); e != nil {
			return 0, e
		}
		if count > 0 {
			return 0, errors.New("此频道已有进行中的同类游戏")
		}
	}
	r, e := tx.Exec(`INSERT INTO objects(kind,room,owner,title,payload,created) VALUES(?,?,?,?,?,?)`, kind, room, user, title, payload, time.Now().Unix())
	if e != nil {
		return 0, e
	}
	id, e := r.LastInsertId()
	if e != nil {
		return 0, e
	}
	if kind == "event" {
		if _, e = tx.Exec(`INSERT OR IGNORE INTO badges VALUES(?,'组织者',?)`, user, time.Now().Unix()); e != nil {
			return 0, e
		}
	}
	e = tx.Commit()
	return id, e
}
func (s *Store) CloseObject(user string, id int64) error {
	o, e := objectMetadataQ(s.db, id)
	if e != nil || !s.CanRead(user, o.Room) {
		return errors.New("内容不存在或没有权限")
	}
	if user != o.Owner && !canManageQ(s.db, user, o.Room) {
		return errors.New("仅发起者或频道管理者可结束活动")
	}
	_, e = s.db.Exec(`UPDATE objects SET closed=1 WHERE id=?`, id)
	return e
}
func multiline(text string, asciiOnly bool) (string, error) {
	text = ansi.Strip(strings.ToValidUTF8(text, ""))
	text = strings.ReplaceAll(text, "\r\n", "\n")
	lines := strings.Split(text, "\n")
	if len(lines) > 32 {
		return "", errors.New("最多 32 行")
	}
	count := 0
	for i, line := range lines {
		var out []rune
		for _, r := range line {
			if r == '\t' {
				out = append(out, ' ', ' ', ' ', ' ')
				continue
			}
			if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
				continue
			}
			if asciiOnly && (r < 32 || r > 126) {
				return "", errors.New("ASCII 画板仅接受可打印 ASCII 字符")
			}
			out = append(out, r)
		}
		if len(out) > 160 {
			return "", errors.New("每行最多 160 个字符")
		}
		lines[i] = string(out)
		count += len(out)
	}
	if count > 4096 {
		return "", errors.New("正文最多 4096 个字符")
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n"), nil
}
func (s *Store) Post(user, room, title, text string, art bool) (int64, error) {
	body, e := multiline(text, art)
	if e != nil {
		return 0, e
	}
	if strings.TrimSpace(body) == "" {
		return 0, errors.New("正文不能为空")
	}
	kind := "post"
	if art {
		kind = "art"
	}
	payload, _ := json.Marshal(map[string]string{"body": body})
	return s.NewObject(user, room, kind, title, string(payload))
}
func (s *Store) Comment(user string, id int64, text string) error {
	o, e := s.Object(user, id)
	if e != nil {
		return e
	}
	if o.Closed {
		return errors.New("此条目已关闭")
	}
	if e = canSendQ(s.db, user, o.Room, ""); e != nil {
		return e
	}
	text = cleanText(text, 800)
	if text == "" {
		return errors.New("评论不能为空")
	}
	_, e = s.db.Exec(`INSERT INTO object_events(object_id,user,body,at) VALUES(?,?,?,?)`, id, user, text, time.Now().Unix())
	return e
}
func (s *Store) ObjectEvents(user string, id int64) ([]string, error) {
	if _, e := s.Object(user, id); e != nil {
		return nil, e
	}
	rows, e := s.db.Query(`SELECT user,body FROM object_events WHERE object_id=? ORDER BY id DESC LIMIT 100`, id)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var u, b string
		if rows.Scan(&u, &b) == nil {
			lines = append(lines, u+"："+b)
		}
	}
	return lines, rows.Err()
}

type Poll struct {
	Options []string `json:"options"`
}

func (s *Store) Poll(user, room, text string) (int64, error) {
	parts := strings.Split(text, "|")
	if len(parts) < 3 || len(parts) > 11 {
		return 0, errors.New("用法：/poll 问题|选项1|选项2（2–10 个选项）")
	}
	var options []string
	for _, v := range parts[1:] {
		v = cleanText(v, 80)
		if v == "" {
			return 0, errors.New("选项不能为空")
		}
		options = append(options, v)
	}
	data, _ := json.Marshal(Poll{options})
	return s.NewObject(user, room, "poll", parts[0], string(data))
}
func (s *Store) Vote(user string, id int64, choice int) error {
	tx, e := s.db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	o, e := objectQ(tx, id)
	if e != nil || !canReadQ(tx, user, o.Room) || o.Kind != "poll" {
		return errors.New("投票不存在或没有权限")
	}
	if o.Closed {
		return errors.New("投票已截止")
	}
	if e = canSendQ(tx, user, o.Room, ""); e != nil {
		return e
	}
	var p Poll
	if e = json.Unmarshal([]byte(o.Payload), &p); e != nil {
		return e
	}
	if choice < 1 || choice > len(p.Options) {
		return errors.New("选项编号超出范围")
	}
	_, e = tx.Exec(`INSERT INTO participation VALUES(?,?,?,?) ON CONFLICT(object_id,user) DO UPDATE SET value=excluded.value,at=excluded.at`, id, user, strconv.Itoa(choice), time.Now().Unix())
	if e != nil {
		return e
	}
	return tx.Commit()
}
func (s *Store) PollResults(user string, id int64) ([]string, error) {
	o, e := s.Object(user, id)
	if e != nil || o.Kind != "poll" {
		return nil, errors.New("投票不存在或无权查看")
	}
	var p Poll
	if e = json.Unmarshal([]byte(o.Payload), &p); e != nil {
		return nil, e
	}
	rows, e := s.db.Query(`SELECT value,count(*) FROM participation WHERE object_id=? GROUP BY value`, id)
	if e != nil {
		return nil, e
	}
	counts := map[string]int{}
	for rows.Next() {
		var v string
		var n int
		if rows.Scan(&v, &n) == nil {
			counts[v] = n
		}
	}
	rows.Close()
	result := []string{o.Title}
	for i, v := range p.Options {
		result = append(result, fmt.Sprintf("%d. %s — %d 票", i+1, v, counts[strconv.Itoa(i+1)]))
	}
	return result, nil
}

type EventData struct {
	When        int64  `json:"when"`
	Description string `json:"description"`
}

func (s *Store) NewEvent(user, room, text string) (int64, error) {
	parts := strings.SplitN(text, "|", 3)
	if len(parts) < 2 {
		return 0, errors.New("用法：/event 标题|2026-12-01 20:00|介绍")
	}
	zone := time.FixedZone("CST", 8*3600)
	at, e := time.ParseInLocation("2006-01-02 15:04", strings.TrimSpace(parts[1]), zone)
	if e != nil {
		return 0, errors.New("时间格式为 YYYY-MM-DD HH:MM（北京时间）")
	}
	if at.Before(time.Now()) {
		return 0, errors.New("活动时间须在未来")
	}
	description := ""
	if len(parts) == 3 {
		description = cleanText(parts[2], 800)
	}
	payload, _ := json.Marshal(EventData{at.Unix(), description})
	return s.NewObject(user, room, "event", parts[0], string(payload))
}
func (s *Store) RSVP(user string, id int64, join bool) error {
	tx, e := s.db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	o, e := objectQ(tx, id)
	if e != nil || o.Kind != "event" || !canReadQ(tx, user, o.Room) {
		return errors.New("活动不存在或无权查看")
	}
	if o.Closed {
		return errors.New("活动已结束")
	}
	var data EventData
	if e = json.Unmarshal([]byte(o.Payload), &data); e != nil {
		return e
	}
	if data.When < time.Now().Unix() {
		return errors.New("报名已截止")
	}
	if join {
		_, e = tx.Exec(`INSERT OR IGNORE INTO participation VALUES(?,?,'yes',?)`, id, user, time.Now().Unix())
	} else {
		_, e = tx.Exec(`DELETE FROM participation WHERE object_id=? AND user=?`, id, user)
	}
	if e != nil {
		return e
	}
	if join {
		if _, e = tx.Exec(`INSERT OR IGNORE INTO badges VALUES(?,'参与者',?)`, user, time.Now().Unix()); e != nil {
			return e
		}
	}
	return tx.Commit()
}
func (s *Store) Participants(user string, id int64) ([]string, error) {
	if _, e := s.Object(user, id); e != nil {
		return nil, e
	}
	rows, e := s.db.Query(`SELECT user FROM participation WHERE object_id=? ORDER BY at`, id)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if rows.Scan(&n) == nil {
			names = append(names, n)
		}
	}
	return names, rows.Err()
}

type TicTacToe struct {
	X, O, Turn, Winner string
	Cells              [9]string
	Accepted           bool
}

func (s *Store) Challenge(user, target, room string) (int64, error) {
	u, e := s.Resolve(target)
	if e != nil {
		return 0, e
	}
	if u.Name == user || !s.CanRead(u.Name, room) || s.Blocked(u.Name, user) {
		return 0, errors.New("对方无法接受此挑战")
	}
	game := TicTacToe{X: user, O: u.Name, Turn: user}
	data, _ := json.Marshal(game)
	id, e := s.NewObject(user, room, "ttt", user+" vs "+u.Name, string(data))
	if e == nil {
		_ = s.Notify(u.Name, "game", user, 0, fmt.Sprintf("井字棋邀请 #%d：/ttt accept %d", id, id))
	}
	return id, e
}
func (s *Store) TicMove(user string, id int64, cell int, accept bool) (TicTacToe, error) {
	var game TicTacToe
	tx, e := s.db.Begin()
	if e != nil {
		return game, e
	}
	defer tx.Rollback()
	o, e := objectQ(tx, id)
	if e != nil || o.Kind != "ttt" || !canReadQ(tx, user, o.Room) {
		return game, errors.New("对局不存在或没有权限")
	}
	if e = json.Unmarshal([]byte(o.Payload), &game); e != nil {
		return game, e
	}
	if o.Closed {
		return game, errors.New("对局已结束")
	}
	if user != game.X && user != game.O {
		return game, errors.New("你不是对局参与者")
	}
	if e = canSendQ(tx, user, o.Room, ""); e != nil {
		return game, e
	}
	if accept {
		if user != game.O || game.Accepted {
			return game, errors.New("仅被邀请者可接受一次")
		}
		game.Accepted = true
	} else {
		if !game.Accepted {
			return game, errors.New("等待对方接受邀请")
		}
		if game.Turn != user {
			return game, errors.New("还没轮到你")
		}
		if cell < 1 || cell > 9 || game.Cells[cell-1] != "" {
			return game, errors.New("请选择 1–9 中的空位")
		}
		symbol := "X"
		if user == game.O {
			symbol = "O"
		}
		game.Cells[cell-1] = symbol
		for _, line := range [][3]int{{0, 1, 2}, {3, 4, 5}, {6, 7, 8}, {0, 3, 6}, {1, 4, 7}, {2, 5, 8}, {0, 4, 8}, {2, 4, 6}} {
			if game.Cells[line[0]] != "" && game.Cells[line[0]] == game.Cells[line[1]] && game.Cells[line[1]] == game.Cells[line[2]] {
				game.Winner = user
			}
		}
		full := true
		for _, v := range game.Cells {
			if v == "" {
				full = false
			}
		}
		if full && game.Winner == "" {
			game.Winner = "draw"
		}
		game.Turn = game.X
		if user == game.X {
			game.Turn = game.O
		}
	}
	data, _ := json.Marshal(game)
	if _, e = tx.Exec(`UPDATE objects SET payload=?,closed=? WHERE id=?`, string(data), game.Winner != "", id); e != nil {
		return game, e
	}
	if game.Winner != "" {
		for _, n := range []string{game.X, game.O} {
			if _, e = tx.Exec(`INSERT OR IGNORE INTO badges VALUES(?,'棋手',?)`, n, time.Now().Unix()); e != nil {
				return game, e
			}
		}
	}
	if _, e = tx.Exec(`INSERT INTO object_events VALUES(NULL,?,?,?,?)`, id, user, fmt.Sprintf("move=%d accept=%v", cell, accept), time.Now().Unix()); e != nil {
		return game, e
	}
	return game, tx.Commit()
}
func ticBoard(g TicTacToe) string {
	values := make([]string, 9)
	for i, v := range g.Cells {
		if v == "" {
			v = strconv.Itoa(i + 1)
		}
		values[i] = v
	}
	return fmt.Sprintf("%s | %s | %s\n--+---+--\n%s | %s | %s\n--+---+--\n%s | %s | %s\n轮到：%s  胜者：%s", values[0], values[1], values[2], values[3], values[4], values[5], values[6], values[7], values[8], g.Turn, g.Winner)
}

type GuessGame struct {
	Secret  string   `json:"secret"`
	Max     int      `json:"max"`
	Guesses []string `json:"guesses"`
	Wrong   int      `json:"wrong"`
}

func (s *Store) NewGuess(user, room, kind, secret string) (int64, error) {
	var count int
	_ = s.db.QueryRow(`SELECT count(*) FROM objects WHERE room=? AND kind=? AND closed=0`, room, kind).Scan(&count)
	if count >= 1 {
		return 0, errors.New("此频道已有未结束的同类游戏")
	}
	g := GuessGame{Max: 100}
	if kind == "guess" {
		if secret != "" {
			n, e := strconv.Atoi(secret)
			if e != nil || n < 2 || n > 10000 {
				return 0, errors.New("数字上限 2–10000")
			}
			g.Max = n
		}
		g.Secret = strconv.FormatInt(randomNumber(int64(g.Max))+1, 10)
	} else {
		secret = cleanText(strings.ToLower(secret), 24)
		if utf8.RuneCountInString(secret) < 2 {
			return 0, errors.New("猜词答案至少两个字符")
		}
		g.Secret = secret
	}
	data, _ := json.Marshal(g)
	return s.NewObject(user, room, kind, "猜谜游戏", string(data))
}
func (s *Store) Guess(user, room, kind, value string) (string, error) {
	tx, e := s.db.Begin()
	if e != nil {
		return "", e
	}
	defer tx.Rollback()
	if e = canSendQ(tx, user, room, ""); e != nil {
		return "", e
	}
	var id int64
	var payloadBytes int
	if e = tx.QueryRow(`SELECT id,length(CAST(payload AS BLOB)) FROM objects WHERE room=? AND kind=? AND closed=0 ORDER BY id DESC LIMIT 1`, room, kind).Scan(&id, &payloadBytes); e != nil {
		return "", errors.New("此频道没有进行中的游戏")
	}
	if payloadBytes > maxCommunityPayloadBytes {
		return "", errors.New("此游戏状态过大，请发起者结束后重新开始")
	}
	o, e := objectQ(tx, id)
	if e != nil {
		return "", e
	}
	if kind == "word" && o.Owner == user {
		return "", errors.New("发起者已知道答案，请让其他人参与猜词")
	}
	var g GuessGame
	if e = json.Unmarshal([]byte(o.Payload), &g); e != nil {
		return "", e
	}
	value = cleanText(strings.ToLower(value), 24)
	won := value == g.Secret
	closed := won
	result := ""
	if kind == "guess" {
		n, e := strconv.Atoi(value)
		if e != nil || n < 1 || n > g.Max {
			return "", errors.New("请输入游戏范围内的整数")
		}
		target, _ := strconv.Atoi(g.Secret)
		result = "偏小"
		if n > target {
			result = "偏大"
		}
		if won {
			result = "猜对了！答案是 " + g.Secret
		}
	} else {
		if value == "" {
			return "", errors.New("请输入一个字母、汉字或完整答案")
		}
		if len(g.Guesses) >= maxWordGuesses {
			return "", errors.New("本局猜测已达上限，请发起者结束后重新开始")
		}
		for _, previous := range g.Guesses {
			if previous == value {
				return "", errors.New("这个内容已经猜过，请尝试其它答案")
			}
		}
		g.Guesses = append(g.Guesses, value)
		if !strings.Contains(g.Secret, value) {
			g.Wrong++
		}
		covered := true
		var board []string
		for _, r := range g.Secret {
			found := false
			for _, v := range g.Guesses {
				if strings.ContainsRune(v, r) {
					found = true
					break
				}
			}
			if found {
				board = append(board, string(r))
			} else {
				board = append(board, "_")
				covered = false
			}
		}
		won = won || covered
		closed = won || g.Wrong >= 6
		result = strings.Join(board, " ") + fmt.Sprintf(" · 失误 %d/6", g.Wrong)
		if closed {
			result += " · 答案：" + g.Secret
		}
	}
	data, _ := json.Marshal(g)
	if len(data) > maxCommunityPayloadBytes {
		return "", errors.New("游戏状态超过大小限制")
	}
	if _, e = tx.Exec(`UPDATE objects SET payload=?,closed=? WHERE id=?`, string(data), closed, id); e != nil {
		return "", e
	}
	if _, e = tx.Exec(`INSERT INTO object_events VALUES(NULL,?,?,?,?)`, id, user, "guess: "+value, time.Now().Unix()); e != nil {
		return "", e
	}
	if won {
		if _, e = tx.Exec(`INSERT OR IGNORE INTO badges VALUES(?,'解谜者',?)`, user, time.Now().Unix()); e != nil {
			return "", e
		}
	}
	return result, tx.Commit()
}
func dailyTopic() string {
	topics := []string{"你最近做过最有意思的小项目是什么？", "如果给这个终端增加一种超能力，你会选什么？", "分享一个让你豁然开朗的小知识。", "你最喜欢的命令行工具是什么？", "有什么想和大家一起做的事情？", "今天学到的新东西是什么？", "聊聊你第一次接触 Linux 的经历。"}
	day := time.Now().In(time.FixedZone("CST", 8*3600)).Unix() / 86400
	return topics[int(day)%len(topics)]
}
