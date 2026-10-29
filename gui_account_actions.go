package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

type guiDraft struct {
	Position guiViewPosition `json:"position"`
	Room     string          `json:"room"`
	Body     string          `json:"body"`
	Version  int64           `json:"version"`
	ReplyTo  int64           `json:"reply_to,omitempty"`
	Conflict bool            `json:"conflict,omitempty"`
	BackupID string          `json:"backup_id,omitempty"`
}

func guiDraftFrom(scope guiScope, p Position) *guiDraft {
	return &guiDraft{Room: scope.key(), Body: p.Draft, Version: p.Version, ReplyTo: p.ReplyTo, Position: guiViewPosition{LastRead: p.LastRead, Anchor: p.Anchor, Offset: p.Offset}}
}

// Only the already authenticated owner and selected, revalidated scope reach
// this function. PositionWithError failures are never treated as empty drafts.
func guiDraftRequest(store *Store, user User, scope guiScope, sessionID string, request guiRequest) (guiResponse, error) {
	if request.Type != "draft_load" && guiReservedWordStart(request.Body) {
		return guiResponse{}, errors.New("猜词答案请使用秘密创建表单；此内容未写入聊天草稿")
	}
	response := guiResponse{Type: "draft", RequestID: request.RequestID, Room: scope.key()}
	if request.Room != scope.key() {
		return response, errors.New("对话已切换，草稿未改发到当前对话")
	}
	if _, err := guiResolveScope(store, user.Name, scope.key()); err != nil {
		return response, err
	}
	dbScope := scopeFor(scope.room, scope.peer)
	current, err := store.PositionWithError(user.Name, dbScope)
	if err != nil {
		return response, err
	}
	if request.Type == "draft_load" {
		response.Draft = guiDraftFrom(scope, current)
		return response, nil
	}
	if request.Version < 0 || request.Version == 1<<63-1 {
		return response, errors.New("无效草稿版本，请重新读取")
	}
	if utf8.RuneCountInString(request.Body) > 1600 {
		return response, errors.New("草稿最多 1600 个字符")
	}
	if request.ReplyTo > 0 {
		message, err := store.Message(user.Name, request.ReplyTo)
		if err != nil || guiScopeForMessage(message, user.Name) != scope {
			return response, errors.New("草稿引用不属于当前有权对话")
		}
	}
	local, positionErr := guiMergePosition(store, user.Name, scope, current, request.Position)
	if positionErr != nil {
		return response, positionErr
	}
	local.Draft, local.ReplyTo, local.Version = request.Body, request.ReplyTo, request.Version
	version, err := store.SavePositionVersioned(user.Name, dbScope, local)
	if errors.Is(err, ErrPositionConflict) {
		if saveErr := store.SaveConflictDraft(user.Name, dbScope, sessionID, local); saveErr != nil {
			return response, errors.New("草稿冲突且备份未保存，请保留窗口并复制当前内容")
		}
		latest, readErr := store.PositionWithError(user.Name, dbScope)
		if readErr != nil {
			return response, readErr
		}
		response.Type = "draft_conflict"
		response.Draft = guiDraftFrom(scope, local)
		response.Draft.Version = latest.Version
		response.Draft.Conflict = true
		response.Draft.BackupID = sessionID
		return response, nil
	}
	if err != nil {
		return response, err
	}
	local.Version = version
	response.Type = "draft_saved"
	response.Draft = guiDraftFrom(scope, local)
	if request.Type == "draft_keep" && request.BackupID != "" {
		if err := store.RemoveConflictDraft(user.Name, dbScope, request.BackupID); err != nil {
			response.Status = "草稿已保存，私有冲突备份暂未移除"
		}
	}
	return response, nil
}

func guiAccountAction(store *Store, hub *Hub, user User, scope guiScope, command, arg string, r guiActionResult) (guiActionResult, bool, error) {
	var err error
	switch command {
	case "/key-add", "/key-bind":
		if strings.TrimSpace(arg) != "" {
			return r, true, errors.New("使用 /key-add 绑定本次已签名的设备密钥，不接收公钥参数")
		}
		r.form = "key_bind"
		r.status = "绑定本次 SSH 已完成签名证明的设备密钥"
	case "/keys":
		keys, readErr := store.SSHKeys(user.Name)
		if readErr != nil {
			return r, true, readErr
		}
		r.panel = &guiPanel{Title: "我的设备密钥", Lines: []string{fmt.Sprintf("已绑定 %d / 10 把有效密钥；仅本人可管理。", len(keys)), "只绑定当前 SSH 签名证明的密钥；可在原生连接页选择设备私钥。"}}
		for _, key := range keys {
			r.panel.Lines = append(r.panel.Lines, fmt.Sprintf("#%d %s · %s", key.ID, key.Label, key.Algorithm), key.Fingerprint, "最近使用："+formatUnix(key.LastUsed))
			r.panel.Actions = append(r.panel.Actions, guiPanelAction{Label: fmt.Sprintf("重命名 #%d", key.ID), Command: fmt.Sprintf("/key-rename %d ", key.ID), Fill: true}, guiPanelAction{Label: fmt.Sprintf("撤销 #%d", key.ID), Command: fmt.Sprintf("/key-remove %d", key.ID)})
		}
		r.panel.Actions = append(r.panel.Actions, guiPanelAction{Label: "绑定当前设备密钥", Command: "/key-add"})
	case "/key-guide":
		r.panel = &guiPanel{Title: "设备密钥", Lines: []string{"私钥只留在本机，不要发送或粘贴给服务器。", "在连接页明确选择自己的 Ed25519/ECDSA/RSA 私钥；加密私钥的口令只用于本次连接，不会保存。", "首次使用未绑定的密钥仍需账户密码登录，再通过 /key-add 绑定当前签名证明；下次使用同一密钥可登录。", "每个账户最多10把有效密钥，可在 /keys 重命名和两步撤销。"}}
	case "/key-rename", "/key-remove", "/key-remove-confirm":
		idText, label := splitArgument(arg)
		id, parseErr := strconv.ParseInt(idText, 10, 64)
		if parseErr != nil || id <= 0 {
			return r, true, errors.New("请输入有效密钥编号")
		}
		keys, readErr := store.SSHKeys(user.Name)
		if readErr != nil {
			return r, true, readErr
		}
		found := false
		for _, key := range keys {
			if key.ID != id {
				continue
			}
			found = true
			if command == "/key-remove" {
				r.panel = &guiPanel{Title: "确认撤销设备密钥", Lines: []string{key.Label, key.Fingerprint, "撤销后，使用这把密钥的会话将断开。"}, Actions: []guiPanelAction{{Label: "确认撤销", Command: fmt.Sprintf("/key-remove-confirm %d", id)}}}
			}
		}
		if !found {
			return r, true, errors.New("密钥不存在或不属于当前账户")
		}
		if command == "/key-rename" {
			err = store.RenameSSHKey(user.Name, id, label)
		}
		if command == "/key-remove-confirm" {
			err = store.RevokeSSHKey(user.Name, id)
			if err == nil {
				hub.Enforce()
			}
		}
	case "/draft-conflicts":
		drafts, readErr := store.ConflictDrafts(user.Name)
		if readErr != nil {
			return r, true, readErr
		}
		r.panel = &guiPanel{Title: "本人的冲突草稿", Lines: []string{"恢复会保留原对话；没有原对话权限时不会改发到大厅。"}}
		for _, draft := range drafts {
			preview := cleanText(draft.Position.Draft, 45)
			if guiReservedWordStart(draft.Position.Draft) {
				preview = "已保存的敏感创建草稿（正文不预览；旧副本未删除）"
			}
			r.panel.Actions = append(r.panel.Actions, guiPanelAction{Label: draft.Scope + " · " + formatUnix(draft.Updated) + " · " + preview, Command: "/draft-restore " + draft.Scope + " " + draft.SessionID})
		}
	case "/draft-restore":
		original, backupID := splitArgument(arg)
		if backupID == "" {
			return r, true, errors.New("缺少草稿备份编号")
		}
		key := original
		if strings.HasPrefix(original, "room:") {
			key = strings.TrimPrefix(original, "room:")
		} else if !strings.HasPrefix(original, "dm:") {
			return r, true, errors.New("无效原对话")
		}
		originalScope, resolveErr := guiResolveScope(store, user.Name, key)
		if resolveErr != nil {
			return r, true, errors.New("没有原对话权限；私有备份仍保留，不会改发到其它频道")
		}
		backup, found, readErr := store.ConflictDraft(user.Name, original, backupID)
		if readErr != nil || !found {
			return r, true, errors.New("没有找到本人的这份草稿")
		}
		latest, readErr := store.PositionWithError(user.Name, original)
		if readErr != nil {
			return r, true, readErr
		}
		r.scope = originalScope
		r.replyTo = backup.ReplyTo
		if backup.ReplyTo > 0 {
			message, readErr := store.Message(user.Name, backup.ReplyTo)
			if readErr != nil || guiScopeForMessage(message, user.Name) != originalScope {
				r.replyTo = 0
			}
		}
		r.draft = guiDraftFrom(originalScope, backup)
		r.draft.Position = guiViewPosition{LastRead: latest.LastRead, Anchor: latest.Anchor, Offset: latest.Offset}
		r.draft.Version = latest.Version
		r.draft.ReplyTo = r.replyTo
		r.draft.Conflict = true
		r.draft.BackupID = backupID
		r.status = "已恢复原对话草稿；确认保留本地或读取远端"
	default:
		return r, false, nil
	}
	return r, true, err
}
