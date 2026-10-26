package main

import (
	"context"
	"errors"

	tea "github.com/charmbracelet/bubbletea"
)

type maintenanceResult struct {
	kind  string
	lines []string
	err   error
}

func (m *model) startMaintenance(kind string) tea.Cmd {
	if m.maintenanceCancel != nil {
		m.note("当前已有后台作业，可用 /canceljob 取消")
		return nil
	}
	if !isAdmin(m.user.Role) {
		m.note("需要管理员权限")
		return nil
	}
	ctx, cancel := context.WithCancel(m.ctx)
	m.maintenanceCancel = cancel
	store, user := m.store, m.user.Name
	m.openPanel("后台维护作业", "maintenance", []string{"作业正在后台执行，聊天室仍可继续使用。", "结束后会显示结果；退出连接会取消未完成的作业。"}, []panelAction{{"取消当前作业", "/canceljob", false}})
	return func() tea.Msg {
		lines, err := store.RunMaintenance(ctx, user, kind)
		return maintenanceResult{kind: kind, lines: lines, err: err}
	}
}

func (m *model) finishMaintenance(result maintenanceResult) {
	if m.maintenanceCancel != nil {
		m.maintenanceCancel()
		m.maintenanceCancel = nil
	}
	u, err := m.store.User(m.user.Name)
	if err != nil || !isAdmin(u.Role) {
		m.note("后台作业已结束，当前权限无法查看维护结果")
		return
	}
	if result.err != nil {
		if errors.Is(result.err, context.Canceled) {
			m.note("后台作业已取消")
			return
		}
		lines := append([]string{}, result.lines...)
		lines = append(lines, cleanText(result.err.Error(), 300))
		m.openPanel("后台作业结果", "maintenance-error", lines, nil)
		return
	}
	title := "数据库与配置检查"
	if result.kind == "backup" {
		title = "一致性备份已完成"
	}
	m.openPanel(title, "maintenance-result", result.lines, nil)
}
