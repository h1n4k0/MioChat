package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

type guiTicReceipt struct {
	Operation   string `json:"operation"`
	ObjectID    int64  `json:"object_id"`
	Room        string `json:"room"`
	OriginScope string `json:"origin_scope"`
	Revision    int64  `json:"revision"`
	Cell        int    `json:"cell"`
	Requested   string `json:"requested,omitempty"`
	Canonical   string `json:"canonical,omitempty"`
	Committed   bool   `json:"committed"`
}
type ticMutation struct {
	operation, room, expectedRoom, target string
	id                                    int64
	cell                                  int
	expectedRevision                      *int64
}

func ticRevisionQ(q queryer, id int64) (int64, error) {
	var version int64
	err := q.QueryRow(`SELECT COALESCE(max(id),0) FROM object_events WHERE object_id=?`, id).Scan(&version)
	return version, err
}
func (s *Store) ticForSessionContext(ctx context.Context, user, sessionID string, m ticMutation) (guiTicReceipt, error) {
	if ctx == nil || strings.TrimSpace(sessionID) == "" {
		return guiTicReceipt{}, errCredentialsChanged
	}
	receipt := guiTicReceipt{Operation: m.operation, ObjectID: m.id, Room: m.room, Requested: m.target, Cell: m.cell}
	err := s.communityWriteContext(ctx, user, sessionID, func(q queryer) error {
		if m.operation != "challenge" {
			object, err := objectMetadataQ(q, m.id)
			if err != nil || object.Kind != "ttt" || !canReadQ(q, user, object.Room) {
				return errors.New("对局不存在或没有权限")
			}
			receipt.Room = object.Room
		}
		if m.expectedRoom != "" && m.expectedRoom != receipt.Room {
			return errors.New("原目标频道已变化，请重新打开棋盘核对；未提交")
		}
		if m.operation == "challenge" {
			id, canonical, err := challengeQ(q, user, m.target, receipt.Room)
			receipt.ObjectID = id
			receipt.Canonical = canonical
			return err
		}
		if m.operation != "accept" && m.operation != "move" {
			return errors.New("无效棋盘操作")
		}
		revision, err := ticRevisionQ(q, m.id)
		if err != nil {
			return err
		}
		if m.expectedRevision != nil && (*m.expectedRevision < 0 || *m.expectedRevision != revision) {
			return errors.New("棋盘已变化，请刷新后重新选择；未落子")
		}
		if _, err = ticMoveQ(q, user, m.id, m.cell, m.operation == "accept"); err != nil {
			return err
		}
		receipt.Revision, err = ticRevisionQ(q, m.id)
		return err
	})
	if err != nil {
		return guiTicReceipt{}, err
	}
	receipt.Committed = true
	return receipt, nil
}

func guiTicAction(store *Store, hub *Hub, user User, scope guiScope, replyTo int64, request guiRequest, arg string) (guiActionResult, error) {
	result := guiActionResult{scope: scope, replyTo: replyTo}
	args := strings.Fields(arg)
	m := ticMutation{expectedRoom: request.ExpectedRoom, expectedRevision: request.TicVersion}
	switch len(args) {
	case 1:
		m.operation = "challenge"
		m.target = args[0]
		m.room = scope.room
		if scope.peer != "" {
			m.room = request.communityRoom
		}
		if m.expectedRoom == "" {
			if scope.peer != "" {
				return result, errors.New("请先打开社区列表，核对原频道后发起挑战")
			}
			m.expectedRoom = scope.room
		}
	case 2:
		if request.ExpectedRoom != "" && request.TicVersion == nil {
			return result, errors.New("原生棋盘版本缺失，请重新打开详情")
		}
		m.operation = "move"
		idText := args[0]
		if args[0] == "accept" {
			m.operation = "accept"
			idText = args[1]
		} else {
			var err error
			m.cell, err = strconv.Atoi(args[1])
			if err != nil {
				return result, errors.New("格子编号为1–9")
			}
		}
		var err error
		m.id, err = strconv.ParseInt(idText, 10, 64)
		if err != nil || m.id <= 0 {
			return result, errors.New("无效对局编号")
		}
	default:
		return result, errors.New("用法：/ttt 用户 | /ttt accept 编号 | /ttt 编号 格子")
	}
	receipt, err := store.ticForSessionContext(request.ctx, user.Name, request.sessionID, m)
	if err != nil {
		return result, err
	}
	receipt.OriginScope = scope.key()
	notificationFailed := false
	if m.operation == "challenge" {
		notificationFailed = store.Notify(receipt.Canonical, "game", user.Name, 0, fmt.Sprintf("井字棋邀请 #%d：/ttt accept %d", receipt.ObjectID, receipt.ObjectID)) != nil
	}
	result = guiCommunityAfterCommit(result, guiCommunityReceipt{Command: "/ttt", ObjectID: receipt.ObjectID, Room: receipt.Room, OriginScope: scope.key(), Committed: true}, func() error {
		if m.operation != "challenge" {
			return nil
		}
		message, err := store.SendForSessionContext(request.ctx, Message{Room: receipt.Room, Sender: user.Name, Kind: "action", Body: fmt.Sprintf("发起井字棋挑战 #%d", receipt.ObjectID)}, request.sessionID)
		if err == nil {
			hub.Broadcast(message)
		}
		return err
	}, func() (*guiPanel, error) {
		return guiCommunityPanel(store, user, scope, request, "/object", strconv.FormatInt(receipt.ObjectID, 10))
	})
	if notificationFailed {
		result.status += "；邀请通知未写入，请勿重复创建对局"
	}
	result.communityReceipt = nil
	result.ticReceipt = &receipt
	return result, nil
}
