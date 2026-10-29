package main

import (
	"context"
	"errors"

	tea "github.com/charmbracelet/bubbletea"
)

type maintenanceResult struct {
	id        uint64
	kind      string
	lines     []string
	committed bool
	warning   string
	epoch     int64
	err       error
}

func (m *model) startMaintenance(kind string) tea.Cmd {
	if m.maintenanceCancel != nil {
		m.note("当前已有后台作业，可用 /canceljob 取消")
		return nil
	}
	if !isAdmin(m.user.Role) || m.client == nil || m.client.ID == "" {
		m.note("需要已登录管理员权限")
		return nil
	}
	if m.maintenanceSequence == ^uint64(0) {
		m.note("作业标识已用尽，请重新连接")
		return nil
	}
	m.maintenanceSequence++
	m.maintenanceID = m.maintenanceSequence
	ctx, cancel := context.WithCancel(m.ctx)
	m.maintenanceCancel = cancel
	store, user, sessionID, id := m.store, m.user.Name, m.client.ID, m.maintenanceID
	m.openPanel("后台维护作业", "maintenance", []string{"作业正在后台执行，聊天室仍可继续使用。", "结束后会显示结果；退出连接会取消未完成的作业。"}, []panelAction{{"取消当前作业", "/canceljob", false}})
	return func() tea.Msg {
		out, err := store.RunMaintenanceForSession(ctx, user, sessionID, kind)
		return maintenanceResult{id: id, kind: kind, lines: out.Lines, committed: out.Committed, warning: out.Warning, epoch: out.Epoch, err: err}
	}
}

// Only the captured current job may cancel its context or alter its UI state.
func (m *model) cancelMaintenance(id uint64) bool {
	if id == 0 || id != m.maintenanceID || m.maintenanceCancel == nil {
		return false
	}
	m.maintenanceCancel()
	return true
}
func (m *model) finishMaintenance(result maintenanceResult) {
	if result.id == 0 || result.id != m.maintenanceID || m.maintenanceCancel == nil {
		return
	}
	m.maintenanceCancel()
	m.maintenanceCancel = nil
	m.maintenanceID = 0
	if m.client == nil {
		return
	}
	u, err := m.store.maintenanceActor(m.ctx, m.user.Name, m.client.ID)
	if err != nil || u.AuthEpoch != result.epoch {
		m.note("后台作业已结束，当前权限无法查看维护结果")
		return
	}
	if result.err != nil {
		if errors.Is(result.err, context.Canceled) {
			m.note("后台作业已取消；已提交的备份不会撤回")
			return
		}
		lines := append([]string{}, result.lines...)
		lines = append(lines, cleanText(result.err.Error(), 300))
		m.openPanel("后台作业结果", "maintenance-error", lines, nil)
		return
	}
	title := "数据库与配置检查"
	if result.kind == "backup" && result.committed {
		title = "一致性备份已完成"
	}
	lines := append([]string{}, result.lines...)
	if result.warning != "" {
		lines = append(lines, cleanText(result.warning, 300))
	}
	m.openPanel(title, "maintenance-result", lines, nil)
}
