package main

import (
	"errors"
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
	loaded, err := m.store.CompositionForSession(m.ctx, m.user.Name, m.client.ID, m.room, kind)
	if err != nil {
		m.note(err.Error())
		return
	}
	// Reserve the exact snapshot we loaded; title prefill must not race and
	// replace another editor's newer body between these two operations.
	saved, err := m.store.ReserveCompositionForSession(m.ctx, m.user.Name, m.client.ID, m.room, kind, loaded.Version, title)
	if err != nil {
		m.note(err.Error())
		return
	}
	cleanTitle, body := saved.Title, saved.Body
	m.closePanel()
	m.composeKind = kind
	m.composeRoom = m.room
	m.composeTitle = cleanTitle
	m.composePreview = false
	m.composeFrozen = false
	m.composeVersion = saved.Version
	m.composeRemote = nil
	m.composeConflict, m.composeDiscardArmed = false, false
	m.composeSavedTitle, m.composeSavedBody = cleanTitle, body
	m.notice = ""
	m.editor.SetValue(body)
	m.editor.SetWidth(max(10, m.w-6))
	m.editor.SetHeight(m.compositionBodyHeight())
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
		for _, row := range rows[:min(len(rows), m.compositionBodyHeight())] {
			lines = append(lines, "  "+row)
		}
	} else {
		rows := strings.Split(m.editor.View(), "\n")
		lines = append(lines, rows[:min(len(rows), m.compositionBodyHeight())]...)
	}
	for len(lines) < 3+m.compositionBodyHeight() {
		lines = append(lines, "")
	}
	lines = append(lines, "")
	controls, _ := compositionControlLayout(m.w)
	for _, row := range controls {
		lines = append(lines, m.color(row, m.accent(), false))
	}
	lines = append(lines, m.fitLine(" "+m.notice, m.w))
	return strings.Join(lines, "\n")
}
func (m *model) composeKey(x tea.KeyMsg) tea.Cmd {
	if x.String() != "f9" && x.String() != "f10" {
		m.composeDiscardArmed = false
	}
	switch x.String() {
	case "esc":
		if err := m.saveCompositionDraft(); err != nil {
			m.notice = "草稿未保存，请保留当前窗口：" + err.Error()
			return nil
		}
		m.endCompositionEditor()
		return nil
	case "f9":
		m.composeDiscardArmed = true
		m.notice = "按 F10 确认仅舍弃当前窗口未保存文本；服务器草稿不删除。其它按键取消。"
		return nil
	case "f10":
		if !m.composeDiscardArmed {
			m.notice = "先按 F9 查看舍弃说明，再按 F10 明确确认"
			return nil
		}
		m.endCompositionEditor()
		return nil
	case "f6":
		m.composeDiscardArmed = false
		remote, err := m.store.CompositionForSession(m.ctx, m.user.Name, m.client.ID, m.composeRoom, m.composeKind)
		if err != nil {
			m.composeRemote = nil
			m.notice = "不能读取原频道草稿；本地文本仍保留：" + err.Error()
			return nil
		}
		m.composeRemote = &remote
		m.notice = fmt.Sprintf("远端版本 %d：F8 重载并舍弃本地；F7 以当前本地覆盖这个固定版本。输入会取消确认。", remote.Version)
		return nil
	case "f8":
		if m.composeRemote == nil {
			m.notice = "先按 F6 读取原频道版本，再按 F8 明确重载；本地文本仍保留"
			return nil
		}
		captured := *m.composeRemote
		m.composeRemote = nil
		remote, err := m.store.CompositionForSession(m.ctx, m.user.Name, m.client.ID, m.composeRoom, m.composeKind)
		if err != nil {
			m.notice = "重载确认已失效；本地文本仍保留：" + err.Error()
			return nil
		}
		if remote.Version != captured.Version {
			m.notice = "远端再次更新；未舍弃本地，请重新按 F6 版本后确认"
			return nil
		}
		m.composeVersion, m.composeTitle = remote.Version, remote.Title
		m.editor.SetValue(remote.Body)
		m.composeSavedTitle, m.composeSavedBody = remote.Title, remote.Body
		m.composeConflict = false
		m.notice = "已明确重载此前读取版本号对应的远端草稿；若另一端又更新，下次保存仍会冲突"
		return nil
	case "f7":
		m.composeDiscardArmed = false
		if m.composeRemote == nil {
			m.notice = "先按 F6 读取并确认当前可见的原频道版本，再选择保留本地"
			return nil
		}
		remote := *m.composeRemote
		m.composeRemote = nil // A conflict cannot auto-retry against a newer version.
		body, title := m.editor.Value(), m.composeTitle
		saved, err := m.store.SaveCompositionForSession(m.ctx, m.user.Name, m.client.ID, m.composeRoom, m.composeKind, remote.Version, title, body, false)
		if err != nil {
			m.composeConflict = errors.Is(err, ErrCompositionConflict)
			m.notice = "保留本地未提交；文本仍在窗口：" + err.Error()
			return nil
		}
		m.composeVersion = saved.Version
		m.composeSavedTitle, m.composeSavedBody = title, body
		m.composeConflict = false
		m.notice = "已按明确确认的版本保留本地草稿"
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
		id, saved, e := m.store.PublishCompositionForSession(m.ctx, m.user.Name, m.client.ID, origin, m.composeKind, m.composeVersion, m.composeTitle, m.editor.Value())
		if e != nil {
			m.composeConflict = errors.Is(e, ErrCompositionConflict)
			m.composeRemote = nil
			m.notice = e.Error()
			return nil
		}
		m.endCompositionEditor()
		m.announce(fmt.Sprintf("发布了作品 #%d：%s", id, saved.Title), origin)
		m.openObject(id)
		return nil
	}
	m.composeDiscardArmed = false
	if m.composePreview {
		return nil
	}
	before := m.editor.Value()
	var cmd tea.Cmd
	m.editor, cmd = m.editor.Update(x)
	if m.editor.Value() != before {
		m.composeRemote = nil
	}
	return cmd
}

func (m *model) endCompositionEditor() {
	m.composeKind, m.composeRoom = "", ""
	m.composeFrozen, m.composeConflict, m.composeDiscardArmed = false, false, false
	m.composeRemote = nil
	m.composeVersion = 0
	m.composeTitle, m.composeSavedTitle, m.composeSavedBody = "", "", ""
	m.editor.SetValue("")
	m.editor.Blur()
	m.notice = ""
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
	title := m.composeTitle
	suspended := m.composeFrozen || !m.store.CanRead(m.user.Name, m.composeRoom)
	saved, err := m.store.SaveCompositionForSession(m.ctx, m.user.Name, m.client.ID, m.composeRoom, m.composeKind, m.composeVersion, title, body, suspended)
	if err == nil {
		// Canonical stored text and raw edit generation are separate. Tabs/ANSI
		// normalization must not make every tick save the same raw input again.
		m.composeVersion = saved.Version
		m.composeSavedBody, m.composeSavedTitle = body, title
		m.composeConflict = false
		m.composeRemote = nil
	} else if errors.Is(err, ErrCompositionConflict) {
		m.composeConflict = true
		m.composeRemote = nil
	}
	return err
}

type compositionControl struct {
	label   string
	compact string
	key     tea.KeyType
}
type compositionControlHit struct {
	x, y, width int
	key         tea.KeyType
}

func compositionControls() []compositionControl {
	return []compositionControl{
		{"F2 预览", "F2览", tea.KeyF2}, {"F4 发布", "F4发", tea.KeyF4},
		{"Esc 保存", "Esc存", tea.KeyEsc}, {"F6 版本", "F6版", tea.KeyF6},
		{"F7 保留", "F7留", tea.KeyF7}, {"F8 重载", "F8载", tea.KeyF8},
		{"F9 舍弃", "F9弃", tea.KeyF9}, {"F10 确认", "F10确", tea.KeyF10},
	}
}

func compositionControlLayout(width int) ([]string, []compositionControlHit) {
	rows := []string{" "}
	hits := []compositionControlHit{}
	for _, control := range compositionControls() {
		label := control.label
		if width < 60 {
			label = control.compact
		}
		text := "[" + label + "]"
		cells := ansi.StringWidth(text)
		y := len(rows) - 1
		x := ansi.StringWidth(rows[y])
		if x+cells > width {
			rows = append(rows, " ")
			y, x = len(rows)-1, 1
		}
		rows[y] += text
		hits = append(hits, compositionControlHit{x: x, y: y, width: cells, key: control.key})
		if x+cells < width {
			rows[y] += " "
		}
	}
	return rows, hits
}

func (m model) compositionBodyHeight() int {
	rows, _ := compositionControlLayout(m.w)
	return max(1, m.h-5-len(rows))
}

func (m *model) composeMouse(x tea.MouseMsg) (bool, tea.Cmd) {
	if !m.usableViewport() {
		return true, nil
	}
	// Rendering and hit testing use the same bounded rows and display cells.
	rows, hits := compositionControlLayout(m.w)
	firstY := len(strings.Split(m.composeView(), "\n")) - 1 - len(rows)
	if x.Action == tea.MouseActionPress && x.Button == tea.MouseButtonLeft {
		for _, hit := range hits {
			if x.Y == firstY+hit.y && x.X >= hit.x && x.X < hit.x+hit.width {
				return true, m.composeKey(tea.KeyMsg{Type: hit.key})
			}
		}
	}
	if x.Y < 3 || x.Y >= 3+m.compositionBodyHeight() {
		return true, nil
	}
	m.composeDiscardArmed = false
	before := m.editor.Value()
	var cmd tea.Cmd
	m.editor, cmd = m.editor.Update(x)
	if m.editor.Value() != before {
		m.composeRemote = nil
	}
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
