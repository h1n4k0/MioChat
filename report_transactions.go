package main

import (
	"errors"
	"strconv"
	"time"
)

// Reporting needs visibility, not canSend: mute/read-only/archive do not revoke it.
func reportQ(q queryer, actor string, id int64, reason string) (int64, string, error) {
	u, e := mutationActorQ(q, actor)
	if e != nil {
		return 0, "", e
	}
	m, e := messageQ(q, id)
	if e != nil || !visibleMessageQ(q, u.Name, m) {
		return 0, "", errors.New("消息不存在或没有权限")
	}
	reason = cleanText(reason, 240)
	if reason == "" {
		return 0, "", errors.New("请填写举报原因")
	}
	var count int
	now := time.Now().Unix()
	if e = q.QueryRow(`SELECT count(*) FROM reports WHERE user=? AND at>?`, u.Name, now-3600).Scan(&count); e != nil {
		return 0, "", e
	}
	if count >= 10 {
		return 0, "", errors.New("每小时最多提交10次举报")
	}
	result, e := q.Exec(`INSERT INTO reports(user,message_id,reason,at) VALUES(?,?,?,?)`, u.Name, id, reason, now)
	if e = changedOne(result, e); e != nil {
		return 0, "", e
	}
	reportID, e := result.LastInsertId()
	if e != nil {
		return 0, "", e
	}
	return reportID, reason, nil
}
func resolveReportQ(q queryer, actor string, id int64, status string) error {
	if status != "resolved" && status != "rejected" {
		return errors.New("状态为 resolved 或 rejected")
	}
	u, e := mutationActorQ(q, actor)
	if e != nil {
		return e
	}
	if !isAdmin(u.Role) {
		return errors.New("需要管理员权限")
	}
	if e = changedOne(q.Exec(`UPDATE reports SET status=?,resolver=? WHERE id=?`, status, u.Name, id)); e != nil {
		return e
	}
	return auditQ(q, u.Name, "report."+status, strconv.FormatInt(id, 10), "", "")
}
