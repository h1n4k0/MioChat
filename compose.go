package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

func avatarArt(name string) string {
	switch name {
	case "cat":
		return " /\\_/\\\n( o.o )\n > ^ <"
	case "robot":
		return " .----.\n |o  o|\n | -- |\n '----'"
	case "planet":
		return "    _..._\n .-'  o  '-.\n(____ O ____)\n    '---'"
	case "flower":
		return "   .-.-.\n  (  *  )\n   '-|-`\n     |"
	default:
		return "   /\\\n  /__\\\n  |[]|\n  '--'"
	}
}
func (m *model) startCompose(kind, title string) {
	if m.peer != "" {
		m.note("长帖与画板请在频道内使用")
		return
	}
	if m.composeKind != "" {
		if err := m.saveCompositionDraft(); err != nil {
			m.notice = err.Error()
			return
		}
	}
	savedTitle, body := m.store.Composition(m.user.Name, m.room, kind)
	cleanTitle := cleanText(title, 100)
	if title == "" && savedTitle != "" {
		cleanTitle = savedTitle
	}
	if cleanTitle == "" {
		cleanTitle = "未命名作品"
	}
	// Reserve this owner's draft while room access is still valid. If access is
	// later revoked, only this existing row can receive the suspended text.
	if err := m.store.SaveComposition(m.user.Name, m.room, kind, cleanTitle, body); err != nil {
		m.note(err.Error())
		return
	}
	m.closePanel()
	m.composeKind = kind
	m.composeRoom = m.room
	m.composeTitle = cleanTitle
	m.composePreview = false
	m.composeFrozen = false
	m.composeSavedTitle, m.composeSavedBody = cleanTitle, body
	m.notice = ""
	m.editor.SetValue(body)
	m.editor.SetWidth(max(10, m.w-6))
	m.editor.SetHeight(max(4, m.h-9))
	m.editor.Focus()
}
func (m model) composeView() string {
	title := "长帖编辑"
	if m.composeKind == "art" {
		title = "ASCII 画板"
	}
	mode := "编辑"
	if m.composePreview {
		mode = "预览"
	}
	if m.composeFrozen {
		mode = "发布已暂停"
	}
	lines := []string{m.fitLine(m.color(" MIOCHAT / "+title+" / "+mode+" / #"+m.composeRoom, m.accent(), true), m.w), m.fitLine(" 标题："+m.composeTitle, m.w), m.color(strings.Repeat("─", m.w), "#344359", false)}
	if m.composePreview {
		body := m.editor.Value()
		if safe, e := multiline(body, m.composeKind == "art"); e == nil {
			body = safe
		}
		rows := strings.Split(ansi.Hardwrap(body, max(8, m.w-4), true), "\n")
		for _, row := range rows[:min(len(rows), max(4, m.h-8))] {
			lines = append(lines, "  "+row)
		}
	} else {
		lines = append(lines, m.editor.View())
	}
	lines = append(lines, "", m.color(" [F2 预览/编辑]  [F4 发布]  [Esc 取消]", m.accent(), false), m.fitLine(" "+m.notice, m.w))
	return strings.Join(lines, "\n")
}
func (m *model) composeKey(x tea.KeyMsg) tea.Cmd {
	switch x.String() {
	case "esc":
		if err := m.saveCompositionDraft(); err != nil {
			m.notice = "草稿未保存，请保留当前窗口：" + err.Error()
			return nil
		}
		m.composeKind = ""
		m.composeRoom = ""
		m.composeFrozen = false
		m.editor.Blur()
		m.notice = ""
		return nil
	case "f2":
		m.composePreview = !m.composePreview
		return nil
	case "f4":
		if m.composeFrozen || m.composeRoom == "" || m.composeRoom != m.room || m.peer != "" || !m.store.CanRead(m.user.Name, m.composeRoom) {
			m.freezeComposition()
			return nil
		}
		if !m.limiter.Allow() {
			m.notice = "操作过于频繁"
			return nil
		}
		origin := m.composeRoom
		id, e := m.store.Post(m.user.Name, origin, m.composeTitle, m.editor.Value(), m.composeKind == "art")
		if e != nil {
			m.notice = e.Error()
			return nil
		}
		_ = m.store.SaveComposition(m.user.Name, origin, m.composeKind, m.composeTitle, "")
		m.composeKind = ""
		m.composeRoom = ""
		m.composeFrozen = false
		m.editor.Blur()
		m.notice = ""
		m.announce(fmt.Sprintf("发布了作品 #%d：%s", id, m.composeTitle), origin)
		m.openObject(id)
		return nil
	}
	if m.composePreview {
		return nil
	}
	var cmd tea.Cmd
	m.editor, cmd = m.editor.Update(x)
	return cmd
}

func (m *model) freezeComposition() {
	if m.composeKind == "" {
		return
	}
	m.composeFrozen = true
	m.notice = "原频道已变更或失去权限；草稿仍保留，禁止改发到其它频道。Esc 返回。"
	if err := m.saveCompositionDraft(); err != nil {
		m.notice = "草稿保留在当前窗口，请勿断开：" + err.Error()
	}
}

func (m *model) saveCompositionDraft() error {
	if m.composeKind == "" || m.composeRoom == "" {
		return nil
	}
	body := m.editor.Value()
	if body == m.composeSavedBody && m.composeTitle == m.composeSavedTitle {
		return nil
	}
	var err error
	if m.composeFrozen || !m.store.CanRead(m.user.Name, m.composeRoom) {
		err = m.store.SaveSuspendedComposition(m.user.Name, m.composeRoom, m.composeKind, m.composeTitle, body)
	} else {
		err = m.store.SaveComposition(m.user.Name, m.composeRoom, m.composeKind, m.composeTitle, body)
	}
	if err == nil {
		m.composeSavedBody, m.composeSavedTitle = body, m.composeTitle
	}
	return err
}
func (m *model) composeMouse(x tea.MouseMsg) (bool, tea.Cmd) {
	if x.Action == tea.MouseActionPress && x.Button == tea.MouseButtonLeft && x.Y >= m.h-5 {
		switch {
		case x.X < 20:
			return true, m.composeKey(tea.KeyMsg{Type: tea.KeyF2})
		case x.X < 35:
			return true, m.composeKey(tea.KeyMsg{Type: tea.KeyF4})
		default:
			return true, m.composeKey(tea.KeyMsg{Type: tea.KeyEsc})
		}
	}
	var cmd tea.Cmd
	m.editor, cmd = m.editor.Update(x)
	return true, cmd
}
func (m model) inviteAuthView() string {
	pad := "  "
	lines := []string{m.color(" MIOCHAT / 邀请注册", m.accent(), true), ""}
	labels := []string{"用户名", "密码", "确认密码", "邀请码"}
	for i := 0; i < 4; i++ {
		lines = append(lines, pad+m.formLabel(labels[i], m.fields[i]), pad+m.formInput(m.fields[i], max(8, m.w-4)))
	}
	lines = append(lines, "", m.color("  [创建账户 / Enter]", m.accent(), true), m.fitLine("  "+m.notice, m.w), "  [返回登录] Tab 切换 · Ctrl+R 返回登录", "")
	return strings.Join(lines, "\n")
}

func (s *Store) SaveComposition(user, room, kind, title, body string) error {
	if kind != "post" && kind != "art" {
		return fmt.Errorf("无效编辑器类型")
	}
	clean, e := multiline(body, false)
	if e != nil {
		return e
	}
	tx, e := s.db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if !canReadQ(tx, user, room) {
		return fmt.Errorf("无权访问频道")
	}
	if _, e = tx.Exec(`INSERT INTO compositions VALUES(?,?,?,?,?,?) ON CONFLICT(user,room,kind) DO UPDATE SET title=excluded.title,body=excluded.body,updated=excluded.updated`, user, room, kind, cleanText(title, 100), clean, time.Now().Unix()); e != nil {
		return e
	}
	return tx.Commit()
}

// A suspended editor can preserve only its owner's already-reserved draft. It
// cannot create a new draft in an inaccessible room or read anybody else's row.
func (s *Store) SaveSuspendedComposition(user, room, kind, title, body string) error {
	if kind != "post" && kind != "art" {
		return fmt.Errorf("无效编辑器类型")
	}
	clean, err := multiline(body, false)
	if err != nil {
		return err
	}
	result, err := s.db.Exec(`UPDATE compositions SET title=?,body=?,updated=? WHERE user=? AND room=? AND kind=?`, cleanText(title, 100), clean, time.Now().Unix(), user, room, kind)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil || count != 1 {
		return fmt.Errorf("没有可保留的本人草稿")
	}
	return nil
}
func (s *Store) Composition(user, room, kind string) (string, string) {
	var title, body string
	if s.CanRead(user, room) {
		_ = s.db.QueryRow(`SELECT title,body FROM compositions WHERE user=? AND room=? AND kind=?`, user, room, kind).Scan(&title, &body)
	}
	return title, body
}
func (m *model) announce(text, room string) {
	msg, e := m.store.Add(Message{Room: room, Sender: m.user.Name, Body: text, Kind: "action"})
	if e == nil {
		m.hub.Broadcast(msg)
	}
}
func (m *model) openObject(id int64) {
	o, e := m.store.Object(m.user.Name, id)
	if e != nil {
		m.note(e.Error())
		return
	}
	lines := []string{fmt.Sprintf("#%d [%s] @%s · #%s", o.ID, o.Kind, o.Owner, o.Room), o.Title}
	actions := []panelAction{}
	switch o.Kind {
	case "poll":
		result, e := m.store.PollResults(m.user.Name, id)
		if e == nil {
			lines = append(lines, result...)
		}
		for i := 1; i < len(result); i++ {
			actions = append(actions, panelAction{fmt.Sprintf("投第 %d 项", i), fmt.Sprintf("/vote %d %d", id, i), false})
		}
	case "post", "art":
		var p map[string]string
		decodeJSON(o.Payload, &p)
		lines = append(lines, p["body"])
		actions = append(actions, panelAction{"添加评论", fmt.Sprintf("/comment %d ", id), true})
	case "event":
		var p EventData
		decodeJSON(o.Payload, &p)
		lines = append(lines, "时间："+formatUnix(p.When), p.Description)
		participants, _ := m.store.Participants(m.user.Name, id)
		lines = append(lines, "参与者："+strings.Join(participants, "、"))
		actions = append(actions, panelAction{"报名", fmt.Sprintf("/rsvp %d", id), false}, panelAction{"取消报名", fmt.Sprintf("/unrsvp %d", id), false})
	case "ttt":
		var game TicTacToe
		decodeJSON(o.Payload, &game)
		lines = append(lines, ticBoard(game))
		if !o.Closed && !game.Accepted && game.O == m.user.Name {
			actions = append(actions, panelAction{"接受挑战", fmt.Sprintf("/ttt accept %d", id), false})
		}
		for i, v := range game.Cells {
			if v == "" && !o.Closed && game.Accepted && game.Turn == m.user.Name {
				actions = append(actions, panelAction{fmt.Sprintf("落子 %d", i+1), fmt.Sprintf("/ttt %d %d", id, i+1), false})
			}
		}
	default:
		lines = append(lines, o.Payload)
	}
	if !o.Closed && (o.Owner == m.user.Name || m.store.canManage(m.user, o.Room)) {
		actions = append(actions, panelAction{"结束/关闭", fmt.Sprintf("/close %d", id), false})
	}
	comments, _ := m.store.ObjectEvents(m.user.Name, id)
	if len(comments) > 0 {
		lines = append(lines, "讨论：")
		lines = append(lines, comments...)
	}
	m.openPanel("社区 / "+strconv.FormatInt(id, 10), "object", lines, actions)
}
