package main

import (
	"database/sql"
	"errors"
	"strconv"
	"strings"
)

type guiManagementReceipt struct {
	Command          string `json:"command"`
	RequestedCommand string `json:"requested_command"`
	Actor            string `json:"actor"`
	OriginScope      string `json:"origin_scope"`
	RequestedTarget  string `json:"requested_target"`
	Target           string `json:"target"`
	Room             string `json:"room"`
	Value            string `json:"value"`
	Committed        bool   `json:"committed"`
}

// A kick authorizes and audits a transient disconnect request, not revocation.
func kickQ(q queryer, actor, target string) (string, error) {
	u, err := mutationActorQ(q, actor)
	if err != nil {
		return "", err
	}
	other, err := resolveUserQ(q, target)
	if err != nil {
		return "", err
	}
	if !isAdmin(u.Role) || roleRank(other.Role) >= roleRank(u.Role) {
		return "", errors.New("不能踢出此账户")
	}
	if err = auditQ(q, u.Name, "user.kick", other.Name, "", ""); err != nil {
		return "", err
	}
	return other.Name, nil
}
func (s *Store) kickAudited(actor, target string) (string, error) {
	var canonical string
	err := s.writeTransaction(func(tx *sql.Tx) error { var e error; canonical, e = kickQ(tx, actor, target); return e })
	if err != nil {
		return "", err
	}
	return canonical, nil
}
func guiManagementAction(store *Store, user User, scope guiScope, replyTo int64, request guiRequest, command, arg string) (guiActionResult, error) {
	result := guiActionResult{scope: scope, replyTo: replyTo}
	if request.ctx == nil || request.sessionID == "" {
		return result, errCredentialsChanged
	}
	args := strings.Fields(arg)
	target, value, room := "", "", ""
	switch command {
	case "/slowmode":
		if scope.peer != "" || scope.room == "" {
			return result, errors.New("慢速模式请在实际频道内设置")
		}
		if len(args) != 1 {
			return result, errors.New("用法：/slowmode 0–3600秒")
		}
		n, e := strconv.Atoi(args[0])
		if e != nil || n < 0 || n > 3600 {
			return result, errors.New("慢速模式为0–3600秒，0关闭")
		}
		room, target, value = scope.room, scope.room, strconv.Itoa(n)
	case "/kick":
		if len(args) != 1 {
			return result, errors.New("用法：/kick 用户")
		}
		target = args[0]
	case "/role":
		if len(args) != 2 {
			return result, errors.New("用法：/role 用户 admin/moderator/member")
		}
		target, value = args[0], args[1]
	default:
		return result, errors.New("未知管理操作")
	}
	if request.ExpectedRoom != "" && request.ExpectedRoom != room {
		return result, errors.New("原目标频道已变化")
	}
	canonical := target
	err := store.communityWriteContext(request.ctx, user.Name, request.sessionID, func(q queryer) error {
		if scope.peer == "" && !canReadQ(q, user.Name, scope.room) {
			return errors.New("当前频道已不可读")
		}
		var e error
		switch command {
		case "/slowmode":
			return roomSettingQ(q, user.Name, room, "slow", value, request.sessionID)
		case "/role":
			canonical, e = setRoleQ(q, user.Name, target, value)
		case "/kick":
			canonical, e = kickQ(q, user.Name, target)
		}
		return e
	})
	if err != nil {
		var uncertain communityCommitError
		if errors.As(err, &uncertain) {
			return result, errors.New("管理操作提交结果待确认；请核对审计/频道设置/账户角色，勿自动重试")
		}
		return result, err
	}
	result.managementReceipt = &guiManagementReceipt{Command: command, RequestedCommand: request.Body, Actor: user.Name, OriginScope: scope.key(), RequestedTarget: target, Target: canonical, Room: room, Value: value, Committed: true}
	if command == "/kick" {
		result.kickTarget = canonical
		result.status = "已审计并请求断开该账户当前聊天室连接；不是封禁，允许重新登录。"
	} else {
		result.refreshState = true
		result.status = "管理设置已提交并写入审计。"
	}
	return result, nil
}
func guiCompleteActionResult(writer *guiWriter, hub *Hub, response guiResponse, result guiActionResult) error {
	// Never wait for a blocked ACK before applying committed effects.
	if result.kickTarget != "" {
		hub.Kick(result.kickTarget)
	}
	if result.refreshState {
		hub.Refresh()
	}
	return guiCompleteAction(writer, hub, response, result.enforcePolicy)
}
