package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

func newGuessQ(q queryer, user, room, kind, secret string) (int64, error) {
	if kind != "guess" && kind != "word" {
		return 0, errors.New("未知猜谜类型")
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
	return newObjectQ(q, user, room, kind, "猜谜游戏", string(data))
}
func guessOutcomeQ(q queryer, user, room, kind, value string, expectedID int64, actedID *int64) (string, error) {
	var e error
	if e = canSendQ(q, user, room, ""); e != nil {
		return "", e
	}
	var id int64
	var payloadBytes int
	if e = q.QueryRow(`SELECT id,length(CAST(payload AS BLOB)) FROM objects WHERE room=? AND kind=? AND closed=0 ORDER BY id DESC LIMIT 1`, room, kind).Scan(&id, &payloadBytes); e != nil {
		return "", errors.New("此频道没有进行中的游戏")
	}
	if expectedID > 0 && id != expectedID {
		return "", errors.New("游戏已变化，请刷新后重新猜测；未提交")
	}
	if actedID != nil {
		*actedID = id
	}
	if payloadBytes > maxCommunityPayloadBytes {
		return "", errors.New("此游戏状态过大，请发起者结束后重新开始")
	}
	o, e := objectQ(q, id)
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
	if _, e = q.Exec(`UPDATE objects SET payload=?,closed=? WHERE id=?`, string(data), closed, id); e != nil {
		return "", e
	}
	if _, e = q.Exec(`INSERT INTO object_events VALUES(NULL,?,?,?,?)`, id, user, "guess: "+value, time.Now().Unix()); e != nil {
		return "", e
	}
	if won {
		if _, e = q.Exec(`INSERT OR IGNORE INTO badges VALUES(?,'解谜者',?)`, user, time.Now().Unix()); e != nil {
			return "", e
		}
	}
	return result, nil
}
