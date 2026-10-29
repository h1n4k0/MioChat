package main

import (
	"errors"
	"strings"
)

type guiSanctionReceipt struct {
	SelfIPForce      *bool  `json:"self_ip_force,omitempty"`
	Command          string `json:"command"`
	RequestedCommand string `json:"requested_command"`
	Actor            string `json:"actor"`
	OriginScope      string `json:"origin_scope"`
	RequestedTarget  string `json:"requested_target"`
	Target           string `json:"target"`
	Kind             string `json:"kind"`
	Room             string `json:"room"`
	Expires          int64  `json:"expires"`
	Remove           bool   `json:"remove"`
	Committed        bool   `json:"committed"`
}

func guiSanctionAction(store *Store, user User, scope guiScope, replyTo int64, request guiRequest, command, arg string) (guiActionResult, error) {
	result := guiActionResult{scope: scope, replyTo: replyTo}
	if request.ctx == nil || request.sessionID == "" {
		return result, errCredentialsChanged
	}
	args := strings.Fields(arg)
	if len(args) == 0 {
		return result, errors.New("请提供目标用户")
	}
	kind, room := "user", ""
	switch command {
	case "/mute", "/unmute":
		if scope.peer != "" || scope.room == "" {
			return result, errors.New("频道禁言请在频道内执行")
		}
		kind, room = "mute", scope.room
	case "/gmute", "/ungmute":
		kind = "mute"
	case "/banip", "/unbanip":
		kind = "ip"
	case "/ban", "/unban":
	default:
		return result, errors.New("未知制裁操作")
	}
	if request.ExpectedRoom != "" && request.ExpectedRoom != room {
		return result, errors.New("原目标频道已变化，请重新核对")
	}
	remove := strings.HasPrefix(command, "/un")
	if request.SelfIPForce != nil && (kind != "ip" || remove) {
		return result, errors.New("此操作不接受当前IP影响确认字段")
	}
	duration, reason := "permanent", "管理员操作"
	if !remove && len(args) > 1 {
		duration = args[1]
	}
	if len(args) > 2 {
		reason = strings.Join(args[2:], " ")
	}
	p, err := prepareSanction(kind, args[0], room, duration, reason, remove)
	if err != nil {
		return result, err
	}
	if kind == "ip" && !remove {
		peer, e := trustedPeerAddress(request.connectionIP)
		if e != nil {
			return result, errors.New("无法核验当前聊天室连接IP，未提交封禁")
		}
		prefix, e := normalizePrefix(p.target)
		if e != nil {
			return result, e
		}
		force := strings.Contains(reason, "--force")
		if request.SelfIPForce != nil {
			force = *request.SelfIPForce
		}
		if prefix.Contains(peer) && !force {
			return result, errors.New("此封禁会影响当前聊天室连接；原生表单须勾选明确确认，手工命令须在原因附加 --force；管理SSH不受影响")
		}
	}
	err = store.communityWriteContext(request.ctx, user.Name, request.sessionID, func(q queryer) error {
		// Compare immutable intent, then independently authorize current scope in this writer.
		if request.ExpectedRoom != "" && request.ExpectedRoom != p.room {
			return errors.New("原目标频道已变化")
		}
		if scope.peer == "" && !canReadQ(q, user.Name, scope.room) {
			return errors.New("当前频道已不可读")
		}
		return sanctionQ(q, user.Name, &p)
	})
	if err != nil {
		var uncertain communityCommitError
		if errors.As(err, &uncertain) {
			return result, errors.New("管理操作提交结果待确认；请核对制裁列表和审计，勿直接重复操作")
		}
		return result, err
	}
	if kind == "ip" {
		store.invalidateIPPolicy()
	}
	var force *bool
	if request.SelfIPForce != nil {
		value := *request.SelfIPForce
		force = &value
	}
	result.sanctionReceipt = &guiSanctionReceipt{SelfIPForce: force, Command: command, RequestedCommand: request.Body, Actor: user.Name, OriginScope: scope.key(), RequestedTarget: args[0], Target: p.target, Kind: p.kind, Room: p.room, Expires: p.expires, Remove: p.remove, Committed: true}
	result.enforcePolicy = true
	result.status = "管理状态已更新并写入审计；当前连接策略将重新核验。"
	return result, nil
}

// Committed policy enforcement cannot be skipped when the initiating transport
// fails to deliver its acknowledgement. Never call this before commit.
func guiCompleteAction(writer *guiWriter, hub *Hub, response guiResponse, enforce bool) error {
	// Policy takes effect independently of blocked or failed ACK delivery.
	// Self-impact may close this connection before acknowledgement; never retry
	// a committed action automatically to compensate for transport uncertainty.
	if enforce {
		hub.Enforce()
	}
	return writer.send(response)
}
