package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

type guiObjectCard struct {
	ID          int64  `json:"id"`
	Kind        string `json:"kind"`
	Room        string `json:"room"`
	Owner       string `json:"owner"`
	Title       string `json:"title"`
	Created     int64  `json:"created"`
	Closed      bool   `json:"closed"`
	Unavailable bool   `json:"unavailable"`
}
type guiPollOption struct {
	Index int    `json:"index"`
	Text  string `json:"text"`
	Votes int64  `json:"votes"`
}
type guiObjectComment struct {
	User string `json:"user"`
	Body string `json:"body"`
}
type guiGameView struct {
	Masked    string   `json:"masked,omitempty"`
	CanGuess  bool     `json:"can_guess"`
	Revision  int64    `json:"revision"`
	CanAccept bool     `json:"can_accept"`
	CanMove   bool     `json:"can_move"`
	Max       int      `json:"max"`
	Wrong     int      `json:"wrong"`
	Guesses   []string `json:"guesses,omitempty"`
	Secret    string   `json:"secret,omitempty"`
	X         string   `json:"x,omitempty"`
	O         string   `json:"o,omitempty"`
	Turn      string   `json:"turn,omitempty"`
	Winner    string   `json:"winner,omitempty"`
	Cells     []string `json:"cells,omitempty"`
	Accepted  bool     `json:"accepted"`
}
type guiObjectDetail struct {
	Card             guiObjectCard      `json:"card"`
	Body             string             `json:"body,omitempty"`
	When             int64              `json:"when,omitempty"`
	WhenText         string             `json:"when_text,omitempty"`
	Description      string             `json:"description,omitempty"`
	Options          []guiPollOption    `json:"options,omitempty"`
	Participants     []string           `json:"participants,omitempty"`
	ParticipantCount int64              `json:"participant_count"`
	NextParticipant  string             `json:"next_participant,omitempty"`
	Comments         []guiObjectComment `json:"comments,omitempty"`
	Game             *guiGameView       `json:"game,omitempty"`
}
type guiCommunityView struct {
	Room       string           `json:"room,omitempty"`
	Objects    []guiObjectCard  `json:"objects,omitempty"`
	Detail     *guiObjectDetail `json:"detail,omitempty"`
	DailyTopic string           `json:"daily_topic,omitempty"`
	DailyDate  string           `json:"daily_date,omitempty"`
}

func guiCard(object CommunityObject) guiObjectCard {
	return guiObjectCard{ID: object.ID, Kind: object.Kind, Room: object.Room, Owner: object.Owner, Title: object.Title, Created: object.Created, Closed: object.Closed}
}

func guiCommunityPanel(store *Store, user User, scope guiScope, request guiRequest, command, arg string) (*guiPanel, error) {
	view := &guiCommunityView{}
	panel := &guiPanel{Kind: "community", Scope: scope.key(), Title: "社区", Community: view}
	args := strings.Fields(arg)
	if command == "/daily" {
		if len(args) != 0 {
			return nil, errors.New("/daily 不接受参数")
		}
		now := time.Now()
		view.DailyTopic = dailyTopicAt(now)
		view.DailyDate = now.In(time.FixedZone("CST", 8*3600)).Format("2006-01-02")
		panel.Title = "今日话题"
		panel.Actions = []guiPanelAction{{Label: "插入输入框（不发送）", Command: "local:insert-daily"}}
		return panel, nil
	}
	if request.ctx == nil || request.sessionID == "" {
		return nil, errCredentialsChanged
	}
	ctx, cancel := context.WithTimeout(request.ctx, 300*time.Millisecond)
	defer cancel()
	tx, err := store.readDB().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	q := scopedQuery{ctx: ctx, db: tx}
	if valid, e := sessionCurrentQ(q, request.sessionID, user.Name); e != nil || !valid {
		return nil, errCredentialsChanged
	}
	if _, err = mutationActorQ(q, user.Name); err != nil {
		return nil, err
	}
	listKind := ""
	switch command {
	case "/board":
		listKind = "post"
	case "/gallery":
		listKind = "art"
	case "/events":
		listKind = "event"
	case "/results":
		listKind = "poll"
	}
	list := command == "/board" || command == "/gallery" || command == "/events" || len(args) == 0 && (command == "/results" || command == "/participants")
	if list {
		if len(args) != 0 {
			return nil, errors.New("列表不接受参数")
		}
		room := scope.room
		if scope.peer != "" {
			room = request.communityRoom
		}
		if !canReadQ(q, user.Name, room) {
			return nil, errors.New("无权查看原频道社区；请先打开一个有权访问的频道")
		}
		view.Room = room
		rows, e := q.Query(`SELECT id,kind,room,owner,title,created,closed,length(CAST(payload AS BLOB))>? FROM objects WHERE room=? AND (?='' OR kind=?) ORDER BY id DESC LIMIT 100`, maxCommunityPayloadBytes, room, listKind, listKind)
		if e != nil {
			return nil, e
		}
		for rows.Next() {
			var card guiObjectCard
			if e = rows.Scan(&card.ID, &card.Kind, &card.Room, &card.Owner, &card.Title, &card.Created, &card.Closed, &card.Unavailable); e != nil {
				rows.Close()
				return nil, e
			}
			view.Objects = append(view.Objects, card)
			panel.Actions = append(panel.Actions, guiPanelAction{Label: fmt.Sprintf("#%d %s", card.ID, card.Title), Command: fmt.Sprintf("/object %d", card.ID)})
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return nil, e
		}
		if scope.peer == "" && (command == "/board" || command == "/gallery") {
			command := "/post"
			if listKind == "art" {
				command = "/draw"
			}
			panel.Actions = append(panel.Actions, guiPanelAction{Label: "打开原频道编辑器", Command: command})
		}
		if canSendQ(q, user.Name, room, "") == nil {
			if command == "/participants" {
				panel.Actions = append(panel.Actions, guiPanelAction{Label: "发起井字棋挑战 · #" + room, Command: "local:ttt-challenge"})
			}
			if command == "/results" {
				panel.Actions = append(panel.Actions, guiPanelAction{Label: "创建投票 · #" + room, Command: "local:community:/poll"})
			}
			if command == "/events" {
				panel.Actions = append(panel.Actions, guiPanelAction{Label: "创建活动 · #" + room, Command: "local:community:/event"})
			}
		}
		panel.Title = map[string]string{"/board": "长帖留言板", "/gallery": "ASCII 作品", "/events": "频道活动", "/results": "选择投票查看结果", "/participants": "选择条目查看参与名单"}[command]
		return panel, nil
	}
	if len(args) < 1 || len(args) > 2 || len(args) == 2 && command != "/participants" {
		return nil, errors.New("请输入一个有效社区编号")
	}
	id, e := strconv.ParseInt(args[0], 10, 64)
	if e != nil || id <= 0 {
		return nil, errors.New("无效社区编号")
	}
	object, e := objectQ(q, id)
	if e != nil && !errors.Is(e, ErrObjectTooLarge) || !canReadQ(q, user.Name, object.Room) {
		return nil, errors.New("内容不存在或无权查看")
	}
	detail := &guiObjectDetail{Card: guiCard(object)}
	view.Detail = detail
	view.Room = object.Room
	panel.Title = fmt.Sprintf("社区 #%d", id)
	if command == "/results" && object.Kind != "poll" {
		return nil, errors.New("该编号不是投票")
	}
	if !object.Closed && (user.Name == object.Owner || canManageQ(q, user.Name, object.Room)) {
		panel.Actions = append(panel.Actions, guiPanelAction{Label: "结束此条目…", Command: fmt.Sprintf("/close %d", id)})
	}
	if errors.Is(e, ErrObjectTooLarge) {
		detail.Card.Unavailable = true
		return panel, nil
	}
	// A typed projection replaces raw payload serialization. Unknown/malformed
	// states fail closed; active guessing secrets can never enter a GUI frame.
	switch object.Kind {
	case "post", "art":
		var body struct {
			Body string `json:"body"`
		}
		if e = json.Unmarshal([]byte(object.Payload), &body); e != nil {
			return nil, errors.New("内容状态损坏")
		}
		detail.Body = body.Body
	case "poll":
		var poll Poll
		if e = json.Unmarshal([]byte(object.Payload), &poll); e != nil || len(poll.Options) > 10 {
			return nil, errors.New("投票状态损坏")
		}
		rows, e := q.Query(`SELECT value,count(*) FROM participation WHERE object_id=? GROUP BY value`, id)
		if e != nil {
			return nil, e
		}
		counts := map[string]int64{}
		for rows.Next() {
			var value string
			var count int64
			if e = rows.Scan(&value, &count); e != nil {
				rows.Close()
				return nil, e
			}
			counts[value] = count
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return nil, e
		}
		for index, text := range poll.Options {
			detail.Options = append(detail.Options, guiPollOption{Index: index + 1, Text: text, Votes: counts[strconv.Itoa(index+1)]})
		}
	case "event":
		var event EventData
		if e = json.Unmarshal([]byte(object.Payload), &event); e != nil {
			return nil, errors.New("活动状态损坏")
		}
		detail.When = event.When
		detail.WhenText = formatUnix(event.When)
		detail.Description = event.Description
	case "ttt":
		var game TicTacToe
		if e = json.Unmarshal([]byte(object.Payload), &game); e != nil {
			return nil, errors.New("对局状态损坏")
		}
		detail.Game = &guiGameView{X: game.X, O: game.O, Turn: game.Turn, Winner: game.Winner, Cells: game.Cells[:], Accepted: game.Accepted}
		detail.Game.Revision, e = ticRevisionQ(q, id)
		if e != nil {
			return nil, e
		}
		canSend := !object.Closed && canSendQ(q, user.Name, object.Room, "") == nil
		detail.Game.CanAccept = canSend && !game.Accepted && game.O == user.Name
		detail.Game.CanMove = canSend && game.Accepted && game.Turn == user.Name && (user.Name == game.X || user.Name == game.O)
		if detail.Game.CanAccept {
			panel.Actions = append(panel.Actions, guiPanelAction{Label: "接受井字棋挑战…", Command: fmt.Sprintf("/ttt accept %d", id)})
		}
		if detail.Game.CanMove {
			for i, cell := range game.Cells {
				if cell == "" {
					panel.Actions = append(panel.Actions, guiPanelAction{Label: fmt.Sprintf("落子 %d…", i+1), Command: fmt.Sprintf("/ttt %d %d", id, i+1)})
				}
			}
		}
	case "guess", "word":
		var game GuessGame
		if e = json.Unmarshal([]byte(object.Payload), &game); e != nil || len(game.Guesses) > maxWordGuesses {
			return nil, errors.New("猜谜状态损坏")
		}
		detail.Game = &guiGameView{Max: game.Max, Wrong: game.Wrong, Guesses: game.Guesses}
		if object.Kind == "guess" && !object.Closed && canSendQ(q, user.Name, object.Room, "") == nil {
			detail.Game.CanGuess = true
			panel.Actions = append(panel.Actions, guiPanelAction{Label: "猜一个数字…（频道可见）", Command: fmt.Sprintf("local:guess-input %d", id)})
		}
		if object.Kind == "word" {
			var board []string
			for _, r := range game.Secret {
				found := false
				for _, v := range game.Guesses {
					if strings.ContainsRune(v, r) {
						found = true
						break
					}
				}
				if found {
					board = append(board, string(r))
				} else {
					board = append(board, "_")
				}
			}
			detail.Game.Masked = strings.Join(board, " ")
			if !object.Closed && object.Owner != user.Name && canSendQ(q, user.Name, object.Room, "") == nil {
				detail.Game.CanGuess = true
				panel.Actions = append(panel.Actions, guiPanelAction{Label: "猜字/词…（频道可见）", Command: fmt.Sprintf("local:word-input %d", id)})
			}
		}
		if object.Closed {
			detail.Game.Secret = game.Secret
		}
	default:
		return nil, errors.New("未知社区内容类型")
	}
	if detail.Game != nil {
		panel.Actions = append(panel.Actions, guiPanelAction{Label: "刷新游戏详情", Command: fmt.Sprintf("/object %d", id)})
	}
	if command == "/results" && object.Kind != "poll" {
		return nil, errors.New("该编号不是投票")
	}
	cursor := ""
	if len(args) == 2 {
		cursor = args[1]
	}
	if e = q.QueryRow(`SELECT count(*) FROM participation WHERE object_id=?`, id).Scan(&detail.ParticipantCount); e != nil {
		return nil, e
	}
	rows, e := q.Query(`SELECT user FROM participation WHERE object_id=? AND user>? ORDER BY user LIMIT 101`, id, cursor)
	if e != nil {
		return nil, e
	}
	for rows.Next() {
		var name string
		if e = rows.Scan(&name); e != nil {
			rows.Close()
			return nil, e
		}
		detail.Participants = append(detail.Participants, name)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	if len(detail.Participants) > 100 {
		detail.Participants = detail.Participants[:100]
		detail.NextParticipant = detail.Participants[99]
		panel.Actions = append(panel.Actions, guiPanelAction{Label: "下一页参与者", Command: fmt.Sprintf("/participants %d %s", id, detail.NextParticipant)})
	}
	rows, e = q.Query(`SELECT user,body FROM object_events WHERE object_id=? ORDER BY id DESC LIMIT 100`, id)
	if e != nil {
		return nil, e
	}
	for rows.Next() {
		var comment guiObjectComment
		if e = rows.Scan(&comment.User, &comment.Body); e != nil {
			rows.Close()
			return nil, e
		}
		detail.Comments = append(detail.Comments, comment)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	if !object.Closed {
		sendAllowed := canSendQ(q, user.Name, object.Room, "") == nil
		if sendAllowed {
			panel.Actions = append(panel.Actions, guiPanelAction{Label: "添加评论…", Command: fmt.Sprintf("/comment %d", id)})
		}
		if object.Kind == "poll" && sendAllowed {
			for _, option := range detail.Options {
				panel.Actions = append(panel.Actions, guiPanelAction{Label: fmt.Sprintf("投票：%d. %s…", option.Index, option.Text), Command: fmt.Sprintf("/vote %d %d", id, option.Index)})
			}
		}
		if object.Kind == "event" && detail.When >= time.Now().Unix() {
			var joined int
			if e = q.QueryRow(`SELECT count(*) FROM participation WHERE object_id=? AND user=?`, id, user.Name).Scan(&joined); e != nil {
				return nil, e
			}
			label, cmd := "报名参加…", "/rsvp"
			if joined > 0 {
				label, cmd = "取消报名…", "/unrsvp"
			}
			panel.Actions = append(panel.Actions, guiPanelAction{Label: label, Command: fmt.Sprintf("%s %d", cmd, id)})
		}
	}
	if cursor != "" {
		panel.Actions = append(panel.Actions, guiPanelAction{Label: "返回名单首页", Command: fmt.Sprintf("/participants %d", id)})
	}
	return panel, nil
}
