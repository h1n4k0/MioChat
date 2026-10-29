package main

import (
	"context"
	"errors"
	"fmt"
	"unicode/utf8"
)

// Dedicated wire data never shares the chat draft/body/reply-to projection.
// Version is optional because zero is a genuine never-written draft version.
type guiCompositionRequest struct {
	Operation       string `json:"operation"`
	Room            string `json:"room"`
	Kind            string `json:"kind"`
	ExpectedVersion *int64 `json:"expected_version,omitempty"`
	EditGeneration  uint64 `json:"edit_generation"`
	Title           string `json:"title"`
	Body            string `json:"body"`
}
type guiCompositionReceipt struct {
	Operation       string              `json:"operation"`
	EditGeneration  uint64              `json:"edit_generation"`
	ExpectedVersion *int64              `json:"expected_version,omitempty"`
	Snapshot        CompositionSnapshot `json:"snapshot"`
	ObjectID        int64               `json:"object_id,omitempty"`
	Committed       bool                `json:"committed"`
}

func guiCompositionRequestAction(ctx context.Context, store *Store, hub *Hub, user User, scope guiScope, sessionID string, request guiRequest) (guiResponse, error) {
	var empty guiResponse
	in := request.Composition
	if in == nil || ctx == nil || sessionID == "" {
		return empty, errors.New("缺少原频道编辑请求")
	}
	if request.SecretAdmin != nil || request.Maintenance != nil || request.Room != scope.key() || request.Body != "" || request.ReplyTo != 0 || request.Position != nil || request.Version != 0 || request.Username != "" || request.Password != "" || request.NewPassword != "" || request.Confirm != "" || request.Invite != "" || request.Before != 0 || request.BackupID != "" || request.SelfIPForce != nil || request.ReadAfter != 0 || request.ReadUntil != nil || request.WordMode != "" || request.GameID != 0 || request.TicVersion != nil || request.ExpectedRoom != "" {
		return empty, errors.New("编辑器请求与当前会话不匹配")
	}
	if err := compositionKey(user.Name, in.Room, in.Kind); err != nil {
		return empty, err
	}
	if !utf8.ValidString(in.Title) || !utf8.ValidString(in.Body) || len(in.Title) > 4096 || len(in.Body) > 32*1024 {
		return empty, errors.New("编辑内容无效或过长")
	}
	if in.Operation != "suspend" && (scope.peer != "" || scope.room != in.Room) {
		return empty, errors.New("原频道已切换，编辑器已暂停；不会改发到当前会话")
	}
	var snapshot CompositionSnapshot
	var id int64
	var err error
	switch in.Operation {
	case "load", "remote", "reload":
		if (in.Operation != "reload" && in.ExpectedVersion != nil) || (in.Operation == "reload" && (in.ExpectedVersion == nil || *in.ExpectedVersion < 0)) || in.Title != "" || in.Body != "" {
			return empty, errors.New("读取草稿不能附带写入内容")
		}
		snapshot, err = store.CompositionForSession(ctx, user.Name, sessionID, in.Room, in.Kind)
		if err == nil && in.Operation == "reload" && snapshot.Version != *in.ExpectedVersion {
			err = ErrCompositionConflict
		}
	case "reserve", "save", "suspend", "publish":
		if in.ExpectedVersion == nil || *in.ExpectedVersion < 0 {
			return empty, errors.New("必须明确原草稿版本，零不是缺省值")
		}
		switch in.Operation {
		case "reserve":
			if in.Body != "" {
				return empty, errors.New("预留编辑器不能覆盖正文")
			}
			snapshot, err = store.ReserveCompositionForSession(ctx, user.Name, sessionID, in.Room, in.Kind, *in.ExpectedVersion, in.Title)
		case "save", "suspend":
			snapshot, err = store.SaveCompositionForSession(ctx, user.Name, sessionID, in.Room, in.Kind, *in.ExpectedVersion, in.Title, in.Body, in.Operation == "suspend")
		case "publish":
			id, snapshot, err = store.PublishCompositionForSession(ctx, user.Name, sessionID, in.Room, in.Kind, *in.ExpectedVersion, in.Title, in.Body)
		}
	default:
		return empty, errors.New("无效编辑器操作")
	}
	if err != nil {
		return empty, err
	}
	receipt := &guiCompositionReceipt{Operation: in.Operation, EditGeneration: in.EditGeneration, ExpectedVersion: in.ExpectedVersion, Snapshot: snapshot, ObjectID: id, Committed: in.Operation != "load" && in.Operation != "remote" && in.Operation != "reload"}
	out := guiResponse{Type: "composition", RequestID: request.RequestID, Room: in.Room, Composition: receipt}
	if id > 0 {
		out.Status = fmt.Sprintf("已发布 #%d 到 #%s；草稿已原子清空", id, in.Room)
		// An optional announcement failing after commit never becomes a failed
		// publication that would invite a duplicate publish. No object panel sibling.
		message, e := store.SendForSessionContext(ctx, Message{Room: in.Room, Sender: user.Name, Body: fmt.Sprintf("发布了作品 #%d：%s", id, snapshot.Title), Kind: "action"}, sessionID)
		if e != nil {
			out.Status += "；公告未发送，请勿重复发布"
		} else {
			hub.Broadcast(message)
		}
	}
	return out, nil
}
