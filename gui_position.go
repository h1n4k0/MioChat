package main

import (
	"errors"
	"github.com/charmbracelet/x/ansi"
	"strings"
	"unicode"
)

type guiViewPosition struct {
	LastRead int64 `json:"last_read"`
	Anchor   int64 `json:"anchor"`
	Offset   int   `json:"offset"`
}

// A draft is an editable buffer, not a chat message. Keep legitimate whitespace
// and line breaks while excluding terminal control sequences and format controls.
func cleanPositionDraft(text string) string {
	text = ansi.Strip(strings.ToValidUTF8(text, ""))
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	out := make([]rune, 0, min(len(text), 1600))
	for _, r := range text {
		if r != '\n' && r != '\t' && (unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp)) {
			continue
		}
		out = append(out, r)
		if len(out) == 1600 {
			break
		}
	}
	return string(out)
}

func guiMergePosition(store *Store, user string, scope guiScope, current Position, view *guiViewPosition) (Position, error) {
	if view == nil {
		return current, nil
	}
	if view.Anchor < 0 || view.LastRead < 0 || view.Offset < 0 || view.Offset > 16000 || view.Offset > 0 && view.Anchor == 0 {
		return current, errors.New("无效阅读位置")
	}
	for _, id := range []int64{view.Anchor, view.LastRead} {
		if id == 0 {
			continue
		}
		message, err := store.Message(user, id)
		if err != nil || guiScopeForMessage(message, user) != scope {
			return current, errors.New("阅读位置不属于当前有权对话")
		}
	}
	current.LastRead = max(current.LastRead, view.LastRead)
	current.Anchor, current.Offset = view.Anchor, view.Offset
	return current, nil
}

func guiReadPositionWindow(store *Store, user User, scope guiScope, request guiRequest) (guiResponse, error) {
	response := guiResponse{Type: "window", RequestID: request.RequestID, Room: scope.key()}
	if request.Room != scope.key() {
		return response, errors.New("对话已切换")
	}
	if _, err := guiResolveScope(store, user.Name, scope.key()); err != nil {
		return response, err
	}
	var messages []Message
	var err error
	if request.Type == "view_newer" {
		if request.Before <= 0 {
			return response, errors.New("缺少消息游标")
		}
		source, readErr := store.Message(user.Name, request.Before)
		if readErr != nil || guiScopeForMessage(source, user.Name) != scope {
			return response, errors.New("消息游标不属于当前对话")
		}
		messages, err = store.PageAfter(scope.room, user.Name, scope.peer, request.Before, 100)
		response.Type = "window_append"
	} else {
		if request.Position == nil {
			return response, errors.New("缺少阅读位置")
		}
		if _, err := guiMergePosition(store, user.Name, scope, Position{}, request.Position); err != nil {
			return response, err
		}
		if request.Position.Offset == 0 {
			messages, err = store.History(scope.room, user.Name, scope.peer, 100)
		} else {
			messages, err = store.WindowFrom(scope.room, user.Name, scope.peer, request.Position.Anchor)
		}
	}
	if err != nil {
		return response, err
	}
	latest, err := store.History(scope.room, user.Name, scope.peer, 1)
	if err != nil {
		return response, err
	}
	for _, message := range messages {
		response.Messages = append(response.Messages, guiMessageForViewer(message, user.Name))
	}
	response.AtLatest = lastMessageID(messages) >= lastMessageID(latest) || len(messages) == 0 && request.Type == "view_newer"
	return response, nil
}

// Forced ACL changes may preserve only this session owner's unsent text. This
// writes a private conflict backup, never a canonical room draft or a message.
func guiSuspendDraft(store *Store, user User, sessionID string, request guiRequest) (guiResponse, error) {
	if guiReservedWordStart(request.Body) {
		return guiResponse{}, errors.New("猜词答案请使用秘密创建表单；此内容未写入聊天草稿")
	}
	response := guiResponse{Type: "draft_suspended", RequestID: request.RequestID, Room: request.Room}
	room, peer := request.Room, ""
	if strings.HasPrefix(room, "dm:") {
		peer = strings.TrimPrefix(room, "dm:")
		room = ""
	}
	name := room
	if peer != "" {
		name = peer
	}
	if !validName(name) || len([]rune(request.Body)) > 1600 {
		return response, errors.New("无效原对话草稿")
	}
	p := Position{Draft: request.Body, ReplyTo: request.ReplyTo, Version: max(int64(0), request.Version)}
	if err := store.SaveConflictDraft(user.Name, scopeFor(room, peer), sessionID, p); err != nil {
		return response, err
	}
	response.Draft = &guiDraft{Room: request.Room, Body: cleanPositionDraft(request.Body), ReplyTo: request.ReplyTo, BackupID: sessionID}
	return response, nil
}
