package main

import (
	"errors"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func (m model) currentPosition() Position {
	p := m.positionSaved
	p.Version = m.positionVersion
	p.Draft, p.ReplyTo, p.Offset = m.input.Value(), m.replyID, m.view.YOffset
	if m.feedAtLatest && m.view.AtBottom() {
		// Zero means follow the latest page, not an absolute viewport line.
		p.Offset = 0
	} else {
		p.Offset = max(1, p.Offset)
	}
	p.LastRead = max(p.LastRead, m.lastRead)
	p.Anchor = 0
	for _, message := range m.feed {
		if message.ID > 0 {
			p.Anchor = message.ID
			break
		}
	}
	if p.Offset > 0 {
		// Restore around what the user is reading, rather than requiring the
		// next login to reconstruct all 500 retained messages before that row.
		if anchor := m.timelineAnchor(); anchor.id > 0 {
			p.Anchor = anchor.id
			p.Offset = max(1, anchor.offset)
		}
	}
	if m.feedAtLatest && m.view.AtBottom() {
		for _, message := range m.feed {
			p.LastRead = max(p.LastRead, message.ID)
		}
	}
	return p
}

func (m *model) savePositionDraft() error {
	if m.positionUnknown {
		return fmt.Errorf("尚未成功读取本对话草稿，已暂停保存")
	}
	p := m.currentPosition()
	scope := scopeFor(m.room, m.peer)
	if m.positionConflict {
		if p != m.positionConflictSaved {
			if err := m.store.SaveConflictDraft(m.user.Name, scope, m.positionConflictSession, p); err != nil {
				m.notice = "本机冲突草稿未保存，请保留当前窗口：" + err.Error()
				return err
			}
			m.positionConflictSaved = p
		}
		return nil
	}
	if p == m.positionSaved {
		return nil
	}
	version, err := m.store.SavePositionVersioned(m.user.Name, scope, p)
	if errors.Is(err, ErrPositionConflict) {
		if p.Draft == m.positionSaved.Draft && p.ReplyTo == m.positionSaved.ReplyTo {
			// Another device owns the draft update. This device only changed its
			// read/scroll metadata, so adopt that draft without a conflict modal.
			latest, readErr := m.store.PositionWithError(m.user.Name, scope)
			if readErr != nil {
				m.notice = "其它设备草稿读取失败，已保留当前内容：" + readErr.Error()
				return readErr
			}
			version := latest.Version
			lastRead := max(latest.LastRead, p.LastRead)
			if lastRead > latest.LastRead {
				merged := latest
				merged.LastRead = lastRead
				version, err = m.store.SavePositionVersioned(m.user.Name, scope, merged)
				if err != nil {
					return nil // Retry metadata later; never overwrite a remote draft.
				}
			}
			m.input.SetValue(latest.Draft)
			m.input.CursorEnd()
			m.replyID = latest.ReplyTo
			p.Draft, p.ReplyTo, p.Version, p.LastRead = latest.Draft, latest.ReplyTo, version, lastRead
			m.positionVersion, m.positionSaved, m.lastRead = version, p, lastRead
			return nil
		}
		if err = m.store.SaveConflictDraft(m.user.Name, scope, m.client.ID, p); err != nil {
			m.notice = "草稿有冲突且无法另存，请保留当前窗口：" + err.Error()
			return err
		}
		m.positionConflict = true
		m.positionConflictSession = m.client.ID
		m.positionConflictSaved = p
		m.openPositionConflict()
		return nil
	}
	if err != nil {
		m.notice = "草稿未保存，请稍后重试：" + err.Error()
		return err
	}
	p.Version = version
	m.positionVersion, m.positionSaved, m.lastRead = version, p, p.LastRead
	return nil
}

func (m *model) preserveEmergencyDraft(scope string, position Position) {
	if m.emergencyDraft != nil || position.Draft == "" && position.ReplyTo == 0 {
		return
	}
	if err := m.store.SaveConflictDraft(m.user.Name, scope, m.client.ID, position); err == nil {
		return
	}
	// One bounded fallback only. No content is copied into the destination input.
	m.emergencyDraft = &ConflictDraft{Scope: scope, SessionID: m.client.ID, Position: position, Updated: time.Now().Unix()}
}

func (m *model) retryEmergencyDraft() error {
	draft := m.emergencyDraft
	if draft == nil {
		return nil
	}
	if err := m.store.SaveConflictDraft(m.user.Name, draft.Scope, draft.SessionID, draft.Position); err != nil {
		return err
	}
	m.emergencyDraft = nil
	if m.panelKind == "emergency-draft" {
		m.closePanel()
	}
	m.flash = "应急草稿已私有保存，可用 /draft-conflicts 找回"
	m.flashUntil = time.Now().Add(8 * time.Second)
	return nil
}

func (m *model) showEmergencyDraft() {
	if m.emergencyDraft == nil {
		return
	}
	m.openPanel("草稿未能落盘，请保留窗口或复制", "emergency-draft", []string{
		"原对话：" + m.emergencyDraft.Scope,
		"这份内容仍只在当前会话内，尚不能保证断线后恢复。",
		"已暂停新对话输入，防止第二份内容覆盖它。请使用终端复制，或等待自动重试。",
		m.emergencyDraft.Position.Draft,
	}, []panelAction{{"重新尝试保存", "/draft-retry", false}})
}

func (m *model) emergencyDraftKey(key tea.KeyMsg) tea.Cmd {
	if m.panelKind != "emergency-draft" {
		m.showEmergencyDraft()
	}
	switch key.String() {
	case "enter":
		if err := m.retryEmergencyDraft(); err != nil {
			m.flash = "仍无法保存，请保留当前窗口并复制草稿"
			m.flashUntil = time.Now().Add(5 * time.Second)
		}
	case "pgup", "up":
		m.panelOffset = max(0, m.panelOffset-3)
	case "pgdown", "down":
		m.panelOffset = min(m.panelMaxOffset(), m.panelOffset+3)
	}
	return nil
}

func (m *model) openPositionConflict() {
	m.openPanel("草稿在其它设备有更新", "draft-conflict", []string{
		"本机内容已另存到本账号的私有冲突草稿，不会自动覆盖其它设备。",
		"当前对话：" + scopeFor(m.room, m.peer),
		"请选择保留本机内容或加载其它设备内容；Esc 可稍后处理。",
	}, []panelAction{
		{"保留本机草稿并覆盖（再次校验版本）", "/draft-keep", false},
		{"加载其它设备草稿", "/draft-reload", false},
		{"查看本人的冲突草稿", "/draft-conflicts", false},
	})
}

func (m *model) showConflictDrafts() {
	drafts, err := m.store.ConflictDrafts(m.user.Name)
	if err != nil {
		m.note(err.Error())
		return
	}
	var actions []panelAction
	for _, draft := range drafts {
		label := fmt.Sprintf("%s · %s · %s", draft.Scope, time.Unix(draft.Updated, 0).Format("01-02 15:04"), cleanText(draft.Position.Draft, 45))
		actions = append(actions, panelAction{label, "/draft-restore " + draft.Scope + " " + draft.SessionID, false})
	}
	m.openPanel("本人的冲突草稿", "draft-list", []string{"这些内容仅本人可读取；恢复时保持原对话，不会改发到当前频道。"}, actions)
}

func (m *model) resolvePositionConflict(keepLocal bool) {
	if !m.positionConflict {
		m.note("当前对话没有待处理的草稿冲突")
		return
	}
	scope := scopeFor(m.room, m.peer)
	latest, readErr := m.store.PositionWithError(m.user.Name, scope)
	if readErr != nil {
		m.flash = "无法读取其它设备草稿，当前内容和私有备份仍保留"
		m.flashUntil = time.Now().Add(8 * time.Second)
		return
	}
	if keepLocal {
		local := m.currentPosition()
		local.Version = latest.Version
		version, err := m.store.SavePositionVersioned(m.user.Name, scope, local)
		if err != nil {
			m.openPositionConflict()
			m.flash = "另一设备仍在修改，原稿已保留，请重试"
			m.flashUntil = time.Now().Add(5 * time.Second)
			return
		}
		local.Version = version
		m.positionVersion, m.positionSaved, m.lastRead = version, local, local.LastRead
	} else {
		m.input.SetValue(latest.Draft)
		m.input.CursorEnd()
		m.replyID = latest.ReplyTo
		m.positionVersion, m.positionSaved, m.lastRead = latest.Version, latest, latest.LastRead
	}
	if err := m.store.RemoveConflictDraft(m.user.Name, scope, m.positionConflictSession); err != nil {
		m.note("草稿已处理，私有备份暂未移除：" + err.Error())
	}
	m.positionConflict = false
	m.positionConflictSession = ""
	m.closePanel()
	m.input.Focus()
}

func (m *model) restoreConflictDraft(scope, sessionID string) {
	p, found, err := m.store.ConflictDraft(m.user.Name, scope, sessionID)
	if err != nil || !found {
		m.note("没有找到本人的这份草稿")
		return
	}
	room, peer := "", ""
	switch {
	case strings.HasPrefix(scope, "room:"):
		room = strings.TrimPrefix(scope, "room:")
	case strings.HasPrefix(scope, "dm:"):
		peer = strings.TrimPrefix(scope, "dm:")
	default:
		m.note("草稿对话无效")
		return
	}
	if err = m.move(room, peer); err != nil {
		m.note("不能进入原对话，私有草稿仍保留：" + err.Error())
		return
	}
	m.input.SetValue(p.Draft)
	m.input.CursorEnd()
	m.replyID = p.ReplyTo
	m.positionConflict = true
	m.positionConflictSession = sessionID
	m.positionConflictSaved = m.currentPosition()
	m.openPositionConflict()
}

func (m *model) positionCommand(command, arg string) (bool, tea.Cmd) {
	switch command {
	case "/draft-conflicts", "/draft-keep", "/draft-reload", "/draft-restore", "/draft-retry":
	default:
		return false, nil
	}
	if !m.logged {
		return true, nil
	}
	switch command {
	case "/draft-retry":
		if err := m.retryEmergencyDraft(); err != nil {
			m.showEmergencyDraft()
		}
	case "/draft-conflicts":
		m.showConflictDrafts()
	case "/draft-keep":
		m.resolvePositionConflict(true)
	case "/draft-reload":
		m.resolvePositionConflict(false)
	case "/draft-restore":
		args := strings.Fields(arg)
		if len(args) != 2 {
			m.note("请在冲突草稿列表选择原对话")
		} else {
			m.restoreConflictDraft(args[0], args[1])
		}
	}
	return true, nil
}
