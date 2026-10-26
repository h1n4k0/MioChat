package main

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func (m *model) showPositionLoadError(err error) {
	if err != nil {
		m.positionLoadError = err.Error()
	}
	m.input.Blur()
	m.openPanel("草稿加载失败，已暂停输入", "position-loading", []string{
		"当前对话：" + scopeFor(m.room, m.peer),
		"尚未确认此对话已有的草稿，暂不接受新输入或保存，防止覆盖原有内容。",
		"每 5 秒自动重试；也可按 Enter 或点击重试。",
		m.positionLoadError,
	}, []panelAction{{"重新加载草稿", "", false}})
}

func (m *model) positionLoadKey(key tea.KeyMsg) tea.Cmd {
	if key.String() == "enter" {
		if err := m.retryPositionLoad(); err != nil {
			m.showPositionLoadError(err)
		}
	} else if m.panelKind != "position-loading" {
		m.showPositionLoadError(nil)
	}
	return nil
}

func (m *model) retryPositionLoad() error {
	if !m.positionUnknown {
		return nil
	}
	p, err := m.store.PositionWithError(m.user.Name, scopeFor(m.room, m.peer))
	if err != nil {
		return err
	}
	feed, err := m.store.History(m.room, m.user.Name, m.peer, 100)
	if err != nil {
		return err
	}
	latestID := lastMessageID(feed)
	if p.Offset > 0 && p.Anchor > 0 {
		window, readErr := m.store.WindowFrom(m.room, m.user.Name, m.peer, p.Anchor)
		if readErr != nil {
			return readErr
		}
		if len(window) > 0 {
			feed = window
		}
	}
	// Apply the complete baseline only after every required read succeeds. An
	// absent preference row is a valid empty position; a failed query is not.
	m.positionVersion, m.positionSaved, m.lastRead = p.Version, p, p.LastRead
	m.input.SetValue(p.Draft)
	m.input.CursorEnd()
	m.input.Focus()
	m.replyID = p.ReplyTo
	m.feed, m.latestMessageID, m.eventFloorID = feed, latestID, latestID
	m.feedAtLatest = lastMessageID(feed) >= latestID
	m.positionUnknown = false
	m.positionLoadError = ""
	m.rebuild(p.Offset == 0)
	if p.Offset > 0 {
		m.view.SetYOffset(p.Offset)
	}
	if m.panelKind == "position-loading" {
		m.closePanel()
	}
	if previous, found, readErr := m.store.ConflictDraft(m.user.Name, scopeFor(m.room, m.peer), m.client.ID); readErr == nil && found {
		m.input.SetValue(previous.Draft)
		m.replyID = previous.ReplyTo
		m.positionConflict = true
		m.positionConflictSession = m.client.ID
		m.positionConflictSaved = previous
		m.openPositionConflict()
	}
	m.flash = "草稿已恢复，可以继续输入"
	m.flashUntil = time.Now().Add(5 * time.Second)
	return nil
}
