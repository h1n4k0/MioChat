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
)

type guiGuessReceipt struct {
	Operation   string `json:"operation"`
	ObjectID    int64  `json:"object_id"`
	Room        string `json:"room"`
	OriginScope string `json:"origin_scope"`
	Requested   string `json:"requested,omitempty"`
	Value       string `json:"value,omitempty"`
	Maximum     int    `json:"maximum"`
	Closed      bool   `json:"closed"`
	Feedback    string `json:"feedback,omitempty"`
	Committed   bool   `json:"committed"`
}
type guessMutation struct {
	start                     bool
	room, expectedRoom, value string
	expectedID                int64
}

func (s *Store) guessForSessionContext(ctx context.Context, user, sessionID string, m guessMutation) (guiGuessReceipt, error) {
	if ctx == nil || strings.TrimSpace(sessionID) == "" {
		return guiGuessReceipt{}, errCredentialsChanged
	}
	receipt := guiGuessReceipt{Room: m.room, Operation: "guess"}
	if m.start {
		receipt.Operation = "start"
	}
	err := s.communityWriteContext(ctx, user, sessionID, func(q queryer) error {
		if !m.start && m.expectedID > 0 {
			object, err := objectMetadataQ(q, m.expectedID)
			if err != nil || object.Kind != "guess" || !canReadQ(q, user, object.Room) {
				return errors.New("猜数字游戏不存在或没有权限")
			}
			receipt.Room = object.Room
		}
		if m.expectedRoom != "" && m.expectedRoom != receipt.Room {
			return errors.New("原目标频道已变化，请重新打开游戏核对；未提交")
		}
		var err error
		if m.start {
			receipt.ObjectID, err = newGuessQ(q, user, receipt.Room, "guess", m.value)
		} else {
			receipt.Requested = m.value
			receipt.Value = cleanText(strings.ToLower(m.value), 24)
			receipt.Feedback, err = guessOutcomeQ(q, user, receipt.Room, "guess", m.value, m.expectedID, &receipt.ObjectID)
		}
		if err != nil {
			return err
		}
		object, err := objectQ(q, receipt.ObjectID)
		if err != nil {
			return err
		}
		var safe struct {
			Max int `json:"max"`
		}
		if err = json.Unmarshal([]byte(object.Payload), &safe); err != nil {
			return err
		}
		receipt.Maximum = safe.Max
		receipt.Closed = object.Closed
		return nil
	})
	if err != nil {
		return guiGuessReceipt{}, err
	}
	receipt.Committed = true
	return receipt, nil
}

func guiGuessAction(store *Store, hub *Hub, user User, scope guiScope, replyTo int64, request guiRequest, arg string) (guiActionResult, error) {
	result := guiActionResult{scope: scope, replyTo: replyTo}
	if request.GameID < 0 {
		return result, errors.New("无效游戏编号")
	}
	if strings.TrimSpace(arg) == "" {
		var err error
		result.panel, err = guiGuessOverview(store, user, scope, request)
		return result, err
	}
	first, rest := splitArgument(arg)
	m := guessMutation{start: first == "start", value: arg, room: scope.room, expectedRoom: request.ExpectedRoom, expectedID: request.GameID}
	if scope.peer != "" {
		m.room = request.communityRoom
	}
	if m.start {
		m.value = rest
		if m.expectedID != 0 {
			return result, errors.New("创建游戏不接受旧游戏编号")
		}
		if m.expectedRoom == "" {
			if scope.peer != "" {
				return result, errors.New("请先打开猜数字概览，核对原频道后开始")
			}
			m.expectedRoom = scope.room
		}
	} else if m.expectedRoom != "" && m.expectedID <= 0 {
		return result, errors.New("原生游戏编号缺失，请重新打开游戏")
	}
	receipt, err := store.guessForSessionContext(request.ctx, user.Name, request.sessionID, m)
	if err != nil {
		return result, err
	}
	receipt.OriginScope = scope.key()
	result = guiCommunityAfterCommit(result, guiCommunityReceipt{Command: "/guess", ObjectID: receipt.ObjectID, Room: receipt.Room, OriginScope: scope.key(), Committed: true}, func() error {
		body := receipt.Feedback
		if m.start {
			body = fmt.Sprintf("开始了猜谜 #%d，使用 /guess 猜测", receipt.ObjectID)
		}
		message, err := store.SendForSessionContext(request.ctx, Message{Room: receipt.Room, Sender: user.Name, Kind: "action", Body: body}, request.sessionID)
		if err == nil {
			hub.Broadcast(message)
		}
		return err
	}, func() (*guiPanel, error) {
		return guiCommunityPanel(store, user, scope, request, "/object", strconv.FormatInt(receipt.ObjectID, 10))
	})
	result.communityReceipt = nil
	result.guessReceipt = &receipt
	if receipt.Feedback != "" {
		result.status += "；" + receipt.Feedback
		result.panel.Lines = append(result.panel.Lines, receipt.Feedback)
	}
	return result, nil
}

// The overview's lookup and subsequent safe projection share a parent300ms
// budget, but release the first reader transaction before opening the second.
func guiGuessOverview(store *Store, user User, scope guiScope, request guiRequest) (*guiPanel, error) {
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
	room := scope.room
	if scope.peer != "" {
		room = request.communityRoom
	}
	if !canReadQ(q, user.Name, room) {
		return nil, errors.New("无权查看原频道游戏，请先打开有权访问的频道")
	}
	var id int64
	err = q.QueryRow(`SELECT id FROM objects WHERE room=? AND kind='guess' AND closed=0 ORDER BY id DESC LIMIT 1`, room).Scan(&id)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if id == 0 {
		panel := &guiPanel{Kind: "community", Scope: scope.key(), Title: "猜数字", Community: &guiCommunityView{Room: room}, Lines: []string{"当前频道没有进行中的猜数字游戏。答案仅由服务器随机生成；猜测及结果对频道可见。"}, Actions: []guiPanelAction{{Label: "刷新游戏", Command: "/guess"}}}
		if canSendQ(q, user.Name, room, "") == nil {
			panel.Actions = append(panel.Actions, guiPanelAction{Label: "开始猜数字…", Command: "local:guess-start"})
		}
		return panel, nil
	}
	if err = tx.Rollback(); err != nil {
		return nil, err
	}
	request.ctx = ctx
	return guiCommunityPanel(store, user, scope, request, "/object", strconv.FormatInt(id, 10))
}
