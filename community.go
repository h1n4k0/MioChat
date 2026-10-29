package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

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
	var id int64
	err := s.communityWriteContext(context.Background(), user, "", func(q queryer) error {
		var err error
		id, err = newObjectQ(q, user, room, kind, title, payload)
		return err
	})
	if err != nil {
		return 0, err
	}
	return id, nil
}

func (s *Store) CloseObject(user string, id int64) error {
	return s.communityWriteContext(context.Background(), user, "", func(q queryer) error { return closeObjectQ(q, user, id) })
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
	return s.communityWriteContext(context.Background(), user, "", func(q queryer) error { return commentQ(q, user, id, text) })
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
	title, payload, err := pollInput(text)
	if err != nil {
		return 0, err
	}
	return s.NewObject(user, room, "poll", title, payload)
}

func (s *Store) Vote(user string, id int64, choice int) error {
	return s.communityWriteContext(context.Background(), user, "", func(q queryer) error { return voteQ(q, user, id, choice) })
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
	title, payload, err := eventInput(text, time.Now())
	if err != nil {
		return 0, err
	}
	return s.NewObject(user, room, "event", title, payload)
}

func (s *Store) RSVP(user string, id int64, join bool) error {
	return s.communityWriteContext(context.Background(), user, "", func(q queryer) error { return rsvpQ(q, user, id, join) })
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
	var id int64
	var canonical string
	err := s.communityWriteContext(context.Background(), user, "", func(q queryer) error { var e error; id, canonical, e = challengeQ(q, user, target, room); return e })
	if err != nil {
		return 0, err
	}
	_ = s.Notify(canonical, "game", user, 0, fmt.Sprintf("井字棋邀请 #%d：/ttt accept %d", id, id))
	return id, nil
}

func (s *Store) TicMove(user string, id int64, cell int, accept bool) (TicTacToe, error) {
	var game TicTacToe
	err := s.communityWriteContext(context.Background(), user, "", func(q queryer) error { var e error; game, e = ticMoveQ(q, user, id, cell, accept); return e })
	return game, err
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
	var id int64
	err := s.communityWriteContext(context.Background(), user, "", func(q queryer) error { var e error; id, e = newGuessQ(q, user, room, kind, secret); return e })
	if err != nil {
		return 0, err
	}
	return id, nil
}

func (s *Store) Guess(user, room, kind, value string) (string, error) {
	var result string
	err := s.communityWriteContext(context.Background(), user, "", func(q queryer) error {
		var e error
		result, e = guessOutcomeQ(q, user, room, kind, value, 0, nil)
		return e
	})
	return result, err
}

func dailyTopic() string { return dailyTopicAt(time.Now()) }

// Preserve the TUI's historical UTC-day topic rotation. CST is only the displayed date.
func dailyTopicAt(now time.Time) string {
	topics := []string{"你最近做过最有意思的小项目是什么？", "如果给这个终端增加一种超能力，你会选什么？", "分享一个让你豁然开朗的小知识。", "你最喜欢的命令行工具是什么？", "有什么想和大家一起做的事情？", "今天学到的新东西是什么？", "聊聊你第一次接触 Linux 的经历。"}
	day := now.In(time.FixedZone("CST", 8*3600)).Unix() / 86400
	return topics[int(day)%len(topics)]
}
