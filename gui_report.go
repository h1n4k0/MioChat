package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

type guiReportReceipt struct {
	Operation       string `json:"operation"`
	Actor           string `json:"actor"`
	OriginScope     string `json:"origin_scope"`
	ReportID        int64  `json:"report_id"`
	MessageID       int64  `json:"message_id"`
	RequestedReason string `json:"requested_reason"`
	Reason          string `json:"reason"`
	Status          string `json:"status"`
	Committed       bool   `json:"committed"`
}

func (s *Store) reportForSessionContext(ctx context.Context, user, sessionID, origin, operation string, id int64, value string) (guiReportReceipt, error) {
	var receipt guiReportReceipt
	if ctx == nil || sessionID == "" || id <= 0 {
		return receipt, errCredentialsChanged
	}
	receipt = guiReportReceipt{Operation: operation, Actor: user, OriginScope: origin}
	err := s.communityWriteContext(ctx, user, sessionID, func(q queryer) error {
		if operation == "report" {
			reportID, reason, e := reportQ(q, user, id, value)
			if e != nil {
				return e
			}
			receipt.ReportID = reportID
			receipt.MessageID = id
			receipt.RequestedReason = value
			receipt.Reason = reason
			receipt.Status = "open"
			return nil
		}
		if operation == "resolve" {
			if e := resolveReportQ(q, user, id, value); e != nil {
				return e
			}
			receipt.ReportID = id
			receipt.Status = value
			return nil
		}
		return errors.New("未知举报操作")
	})
	if err != nil {
		return guiReportReceipt{}, err
	}
	receipt.Committed = true
	return receipt, nil
}
func guiReportAction(store *Store, user User, scope guiScope, replyTo int64, request guiRequest, command, arg string) (guiActionResult, error) {
	r := guiActionResult{scope: scope, replyTo: replyTo}
	idText, value := splitArgument(arg)
	id, e := parseInt64(idText)
	if e != nil {
		return r, e
	}
	operation := strings.TrimPrefix(command, "/")
	if operation == "resolve" && len(strings.Fields(arg)) != 2 {
		return r, errors.New("用法：/resolve 举报编号 resolved/rejected")
	}
	receipt, e := store.reportForSessionContext(request.ctx, user.Name, request.sessionID, scope.key(), operation, id, value)
	if e != nil {
		var uncertain communityCommitError
		if errors.As(e, &uncertain) {
			return r, errors.New("举报提交结果待确认；请核对举报记录或联系管理员，不要直接重复操作")
		}
		return r, e
	}
	r.reportReceipt = &receipt
	if operation == "report" {
		r.status = fmt.Sprintf("举报 #%d 已提交：消息 #%d；授权管理员仅可审计读取这一条当前消息，不含私聊上下文。", receipt.ReportID, id)
	} else {
		r.status = fmt.Sprintf("举报 #%d 已设为 %s，处理操作已审计。", id, receipt.Status)
	}
	return r, nil // No postcommit read can reinterpret the committed write as failure.
}
