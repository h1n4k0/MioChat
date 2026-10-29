package main

import (
	"errors"
	"strconv"
)

type guiSanctionRow struct {
	ID      int64  `json:"id"`
	Kind    string `json:"kind"`
	Target  string `json:"target"`
	Room    string `json:"room"`
	Expires int64  `json:"expires"`
	Reason  string `json:"reason"`
	Revoked bool   `json:"revoked"`
}
type guiAuditRow struct {
	ID     int64  `json:"id"`
	At     int64  `json:"at"`
	Time   string `json:"time"`
	Actor  string `json:"actor"`
	Action string `json:"action"`
	Target string `json:"target"`
	Detail string `json:"detail"`
}
type guiRecallRow struct {
	ID     int64  `json:"id"`
	Sender string `json:"sender"`
	Room   string `json:"room"`
	Body   string `json:"body"`
}
type guiReportRow struct {
	ID        int64  `json:"id"`
	User      string `json:"user"`
	MessageID int64  `json:"message_id"`
	Reason    string `json:"reason"`
	Status    string `json:"status"`
}
type guiReportedMessage struct {
	ReportID  int64  `json:"report_id"`
	MessageID int64  `json:"message_id"`
	Sender    string `json:"sender"`
	Body      string `json:"body"`
}
type guiRevisionRow struct {
	ID     int64  `json:"id"`
	Actor  string `json:"actor"`
	Action string `json:"action"`
	Body   string `json:"body"`
	At     int64  `json:"at"`
	Time   string `json:"time"`
}
type guiRevisionPage struct {
	MessageID int64            `json:"message_id"`
	After     int64            `json:"after"`
	Until     int64            `json:"until"`
	NextAfter int64            `json:"next_after"`
	Rows      []guiRevisionRow `json:"rows"`
}

func guiModerationContentQ(q queryer, viewer User, view *guiModerationView, request guiRequest) error {
	command, arg := view.Command, view.Requested
	if command != "/revisions" && !isAdmin(viewer.Role) {
		return errors.New("需要管理员权限")
	}
	if command != "/revisions" && (request.ReadAfter != 0 || request.ReadUntil != nil) {
		return errors.New("此查询不接受修订分页")
	}
	switch command {
	case "/bans":
		view.Window = 100
		view.Explanation = "最近最多100条封禁与禁言记录（包含已撤销、已过期），不是全部历史。"
		rows, e := q.Query(`SELECT id,kind,target,room,expires,reason,revoked FROM sanctions ORDER BY id DESC LIMIT 100`)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var r guiSanctionRow
			if e = rows.Scan(&r.ID, &r.Kind, &r.Target, &r.Room, &r.Expires, &r.Reason, &r.Revoked); e != nil {
				return e
			}
			view.Sanctions = append(view.Sanctions, r)
		}
		return rows.Err()
	case "/audit":
		view.Window = 100
		view.Explanation = "最近最多100条管理员操作记录；按原规则不含message.*，包含授权读取审计。详情为原记录，未声称自动识别全部敏感文本。"
		rows, e := q.Query(`SELECT id,at,actor,action,target,detail FROM audit WHERE action NOT LIKE 'message.%' ORDER BY id DESC LIMIT 100`)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var r guiAuditRow
			if e = rows.Scan(&r.ID, &r.At, &r.Actor, &r.Action, &r.Target, &r.Detail); e != nil {
				return e
			}
			r.Time = formatUnix(r.At)
			view.Audits = append(view.Audits, r)
		}
		return rows.Err()
	case "/recalls":
		view.Window = 100
		view.Explanation = "公开频道最近最多100条撤回目录；显示当前消息正文（正常撤回为占位），不补出原文。修订原文须另行授权。"
		rows, e := q.Query(`SELECT m.id,m.sender,m.room,m.body FROM messages m JOIN message_meta x ON x.id=m.id LEFT JOIN room_meta r ON r.name=m.room WHERE x.recalled>0 AND m.recipient='' AND COALESCE(r.private,0)=0 ORDER BY m.id DESC LIMIT 100`)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var r guiRecallRow
			if e = rows.Scan(&r.ID, &r.Sender, &r.Room, &r.Body); e != nil {
				return e
			}
			view.Recalls = append(view.Recalls, r)
		}
		return rows.Err()
	case "/reports":
		view.Window = 100
		view.Explanation = "最近最多100条举报，包含全部处理状态；列表不含消息正文。详情按举报编号单独审计读取。"
		rows, e := q.Query(`SELECT id,user,message_id,reason,status FROM reports ORDER BY id DESC LIMIT 100`)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var r guiReportRow
			if e = rows.Scan(&r.ID, &r.User, &r.MessageID, &r.Reason, &r.Status); e != nil {
				return e
			}
			view.Reports = append(view.Reports, r)
		}
		return rows.Err()
	case "/reportview":
		id, e := parseInt64(arg)
		if e != nil || id <= 0 {
			return errors.New("请输入有效举报编号")
		}
		var result guiReportedMessage
		result.ReportID = id
		e = q.QueryRow(`SELECT m.id,m.sender,m.body FROM reports r JOIN messages m ON m.id=r.message_id WHERE r.id=?`, id).Scan(&result.MessageID, &result.Sender, &result.Body)
		if e != nil {
			return e
		}
		if e = auditQ(q, viewer.Name, "report.read", strconv.FormatInt(id, 10), "查看用户主动举报的一条消息", ""); e != nil {
			return e
		}
		view.Canonical = strconv.FormatInt(id, 10)
		view.Reported = &result
		view.Audited = true
		view.Explanation = "访问已审计：仅此举报关联的一条当前消息，不是举报时快照；无私聊上下文、引用或修订权限。处理状态按钮会另行明确确认。"
		return nil
	case "/revisions":
		id, e := parseInt64(arg)
		if e != nil || id <= 0 {
			return errors.New("请输入有效消息编号")
		}
		if request.ReadAfter < 0 || request.ReadUntil != nil && (*request.ReadUntil < 0 || request.ReadAfter > *request.ReadUntil) || request.ReadUntil == nil && request.ReadAfter != 0 {
			return errors.New("无效修订分页")
		}
		// No decorate call: permission is on this message only; no parent is projected.
		m, e := messageQ(q, id)
		if e != nil || !visibleMessageQ(q, viewer.Name, m) {
			return errors.New("消息不存在或没有权限")
		}
		if viewer.Name != m.Sender && (m.Recipient != "" || !canManageQ(q, viewer.Name, m.Room) || !isAdmin(viewer.Role) && viewer.Role != "moderator") {
			return errors.New("没有审计该消息的权限")
		}
		page := &guiRevisionPage{MessageID: id, After: request.ReadAfter, Rows: []guiRevisionRow{}}
		if request.ReadUntil == nil {
			if e = q.QueryRow(`SELECT COALESCE(max(id),0) FROM revisions WHERE message_id=?`, id).Scan(&page.Until); e != nil {
				return e
			}
		} else {
			page.Until = *request.ReadUntil
		}
		rows, e := q.Query(`SELECT id,actor,action,body,at FROM revisions WHERE message_id=? AND id>? AND id<=? ORDER BY id LIMIT 101`, id, page.After, page.Until)
		if e != nil {
			return e
		}
		for rows.Next() {
			var r guiRevisionRow
			if e = rows.Scan(&r.ID, &r.Actor, &r.Action, &r.Body, &r.At); e != nil {
				rows.Close()
				return e
			}
			r.Time = formatUnix(r.At)
			page.Rows = append(page.Rows, r)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
		if len(page.Rows) > 100 {
			page.Rows = page.Rows[:100]
			page.NextAfter = page.Rows[99].ID
		}
		// Role is the fresh actor from this same writer transaction, not request User.
		if isAdmin(viewer.Role) {
			if e = auditQ(q, viewer.Name, "audit.message.read", strconv.FormatInt(id, 10), "", ""); e != nil {
				return e
			}
			view.Audited = true
		}
		view.Canonical = strconv.FormatInt(id, 10)
		view.Revisions = page
		view.Window = 100
		view.Explanation = "按修订编号升序，每页最多100条；下一页沿初始上界完整浏览。每页重新授权，刷新从头可见新修订；原文不放入聊天或草稿。"
		return nil
	}
	return errors.New("未知管理内容查询")
}
