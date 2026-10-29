package main

import (
	"errors"
	"strings"
)

type guiConfigurationReceipt struct {
	Command          string                `json:"command"`
	RequestedCommand string                `json:"requested_command"`
	Actor            string                `json:"actor"`
	OriginScope      string                `json:"origin_scope"`
	Changes          []configurationChange `json:"changes"`
	Committed        bool                  `json:"committed"`
}

func guiConfigurationAction(store *Store, user User, scope guiScope, replyTo int64, request guiRequest, command, arg string) (guiActionResult, error) {
	result := guiActionResult{scope: scope, replyTo: replyTo}
	if request.ctx == nil || request.sessionID == "" {
		return result, errCredentialsChanged
	}
	key, value := splitArgument(arg)
	var changes []configurationChange
	switch command {
	case "/config":
		changes = []configurationChange{{Key: key, Value: value}}
	case "/maintenance":
		changes = maintenanceChanges(key, value)
	default:
		return result, errors.New("未知配置操作")
	}
	var stored []configurationChange
	err := store.communityWriteContext(request.ctx, user.Name, request.sessionID, func(q queryer) error {
		if scope.peer == "" && !canReadQ(q, user.Name, scope.room) {
			return errors.New("当前频道已不可读")
		}
		var err error
		stored, err = configurationChangesQ(q, user.Name, changes)
		return err
	})
	if err != nil {
		var uncertain communityCommitError
		if errors.As(err, &uncertain) {
			return result, errors.New("配置提交结果待确认；请核对审计和当前策略，勿自动重试")
		}
		return result, err
	}
	result.configurationReceipt = &guiConfigurationReceipt{Command: command, RequestedCommand: request.Body, Actor: user.Name, OriginScope: scope.key(), Changes: stored, Committed: true}
	result.refreshState = true
	var summary []string
	for _, change := range stored {
		summary = append(summary, change.Key+"="+change.Value)
	}
	result.status = "配置已原子提交并写入审计：" + strings.Join(summary, "；")
	return result, nil
}
