package main

import (
	"fmt"
	"time"
)

const secretAdminRetention = time.Minute

// This shared flag invalidates old value-copies of Bubble Tea's model too.
// The descriptor contains no generated secret; only panelLines holds it.
type tuiSecretPanel struct {
	active     bool
	sessionID  string
	room, peer string
	expires    time.Time
	outcome    secretAdminOutcome
}

func (m *model) showSecretAdmin(out secretAdminOutcome) {
	if !m.store.secretAdminDeliveryAllowed(m.ctx, out, m.client.ID) {
		m.closePanel()
		m.note("管理凭据操作已经提交，但当前权限无法显示一次性结果；不会自动重试，请先核验服务器。")
		return
	}
	title := "注册邀请码（仅此次显示 · 60秒后清除）"
	lines := []string{out.secret, fmt.Sprintf("到期时间 %s · 使用次数 %d", formatUnix(out.expires), out.uses), "关闭、切换会话或权限变化后清除；不会自动重新生成。"}
	if out.expires == 0 {
		lines[1] = fmt.Sprintf("期限 永久 · 使用次数 %d", out.uses)
	}
	if out.kind == "resetpass" {
		title = "临时密码（仅此次显示 · 60秒后清除）"
		lines = []string{out.secret, "已提交重置；请安全交付给该用户，下次登录必须改密。", "原有设备密钥与会话已撤销。关闭后不可找回，不会自动重试。", "原密码不可查看，临时密码不会写入管理日志。"}
	}
	m.openPanel(title, "secret", lines, nil)
	out.secret = ""
	m.secretPanel = &tuiSecretPanel{active: true, sessionID: m.client.ID, room: m.room, peer: m.peer, expires: time.Now().Add(secretAdminRetention), outcome: out}
}

func (m model) secretPanelLocallyAllowed(now time.Time) bool {
	p := m.secretPanel
	return p != nil && p.active && m.logged && m.client != nil && p.sessionID == m.client.ID &&
		p.outcome.actor == m.user.Name && p.outcome.epoch == m.user.AuthEpoch &&
		m.room == p.room && m.peer == p.peer && isAdmin(m.user.Role) && !m.user.MustReset &&
		m.focus != 10 && now.Before(p.expires)
}
func (m *model) revalidateSecretPanel(now time.Time, fresh bool) {
	if m.secretPanel == nil {
		return
	}
	if !m.secretPanelLocallyAllowed(now) || fresh && !m.store.secretAdminDeliveryAllowed(m.ctx, m.secretPanel.outcome, m.secretPanel.sessionID) {
		m.closePanel()
		m.note("一次性管理结果已清除；已提交操作不会撤回，也不会自动重新生成。")
	}
}
