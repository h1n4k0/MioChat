package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// A successful write is not reclassified as failure when a subsequent read or
// announcement fails. IDs/room originate inside the committed transaction.
type guiCommunityReceipt struct {
	Command     string `json:"command"`
	ObjectID    int64  `json:"object_id"`
	Room        string `json:"room"`
	OriginScope string `json:"origin_scope"`
	Committed   bool   `json:"committed"`
}

func guiCommunityMutationAction(store *Store, hub *Hub, user User, scope guiScope, replyTo int64, request guiRequest, command, arg string) (guiActionResult, error) {
	result := guiActionResult{scope: scope, replyTo: replyTo}
	mutation := communityMutation{command: command, text: arg, expectedRoom: request.ExpectedRoom}
	switch command {
	case "/poll", "/event":
		mutation.room = scope.room
		if scope.peer != "" {
			mutation.room = request.communityRoom
		}
		if mutation.expectedRoom == "" {
			if scope.peer != "" {
				return result, errors.New("请先打开投票或活动列表，核对原频道后创建")
			}
			mutation.expectedRoom = scope.room // Explicit typed command in the currently visible channel.
		}
	default:
		first, rest := splitArgument(arg)
		id, err := strconv.ParseInt(first, 10, 64)
		if err != nil || id <= 0 {
			return result, errors.New("请输入有效社区编号")
		}
		mutation.objectID = id
		switch command {
		case "/vote":
			if len(strings.Fields(rest)) != 1 {
				return result, errors.New("用法：/vote 社区编号 选项编号")
			}
			mutation.choice, err = strconv.Atoi(strings.TrimSpace(rest))
			if err != nil {
				return result, errors.New("无效选项编号")
			}
		case "/comment":
			mutation.text = rest
		default:
			if strings.TrimSpace(rest) != "" {
				return result, errors.New("此操作只接受社区编号")
			}
		}
	}
	receipt, err := store.communityCommitForSessionContext(request.ctx, user.Name, request.sessionID, mutation)
	if err != nil {
		return result, err
	}
	receipt.OriginScope = scope.key()
	// These two optional effects execute only once, strictly after commit.
	return guiCommunityAfterCommit(result, receipt, func() error {
		if command != "/poll" && command != "/event" {
			return nil
		}
		noun := "投票"
		if command == "/event" {
			noun = "活动"
		}
		message, err := store.SendForSessionContext(request.ctx, Message{Room: receipt.Room, Sender: user.Name, Body: fmt.Sprintf("创建了%s #%d", noun, receipt.ObjectID), Kind: "action"}, request.sessionID)
		if err == nil {
			hub.Broadcast(message)
		}
		return err
	}, func() (*guiPanel, error) {
		return guiCommunityPanel(store, user, scope, request, "/object", strconv.FormatInt(receipt.ObjectID, 10))
	}), nil
}

func guiCommunityAfterCommit(result guiActionResult, receipt guiCommunityReceipt, announce func() error, project func() (*guiPanel, error)) guiActionResult {
	result.communityReceipt = &receipt
	result.status = fmt.Sprintf("已提交 %s · #%d · #%s", receipt.Command, receipt.ObjectID, receipt.Room)
	if err := announce(); err != nil {
		result.status += "；公告未发送，请勿重复提交条目"
	}
	panel, err := project()
	if err != nil {
		result.status += "；详情暂不可读，请刷新而非重复提交"
		panel = &guiPanel{Kind: "community", Scope: receipt.OriginScope, Title: "操作已提交", Lines: []string{result.status}, Actions: []guiPanelAction{{Label: "重新读取详情", Command: fmt.Sprintf("/object %d", receipt.ObjectID)}}}
	}
	result.panel = panel
	return result
}
