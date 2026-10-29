package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

type messageSegment struct {
	ID                                      int64
	User                                    string
	First, Last, NameLine, NameX, NameWidth int
}
type panelAction struct {
	Label, Command string
	Fill           bool
}
type userClickMsg struct {
	Name string
	At   time.Time
}

func (m *model) nameClick(name string, x, y int) tea.Cmd {
	now := time.Now()
	if m.lastClickName == name && m.lastClickX == x && m.lastClickY == y && now.Sub(m.lastClickAt) <= 350*time.Millisecond {
		m.lastClickName = ""
		m.closePanel()
		return m.command("/poke " + name)
	}
	m.lastClickName = name
	m.lastClickAt = now
	m.lastClickX = x
	m.lastClickY = y
	return tea.Tick(300*time.Millisecond, func(time.Time) tea.Msg { return userClickMsg{name, now} })
}
func noticeActor(s string) string {
	parts := strings.Fields(s)
	if len(parts) == 0 {
		return ""
	}
	return strings.Trim(parts[0], "@[]")
}
func (m *model) rebuild(bottom bool) {
	if len(m.feed) > maxFeedMessages {
		if bottom {
			m.feed = m.feed[len(m.feed)-maxFeedMessages:]
		} else {
			m.feed = m.feed[:maxFeedMessages]
			m.feedAtLatest = false
		}
	}
	_, width, height := m.dimensions()
	m.view.Width = width
	m.view.Height = height
	m.reflowInputWidths()
	m.segments = nil
	if m.messageCache == nil {
		m.messageCache = map[int64]cachedMessage{}
	}
	style := m.user.Theme + "/" + m.settings["contrast"]
	var lines []string
	boundary := false
	for _, message := range m.feed {
		if m.ignored[message.Sender] {
			continue
		}
		if message.Kind == "system" {
			lines = append(lines, strings.Split(m.color(ansi.Hardwrap("  · "+message.Body, width, true), "#93A6BC", false), "\n")...)
			continue
		}
		if !boundary && m.lastRead > 0 && message.ID > m.lastRead && message.Sender != m.user.Name {
			lines = append(lines, m.color("── 未读消息 ──", "#FFD166", false))
			boundary = true
		}
		block, ok := m.messageCache[message.ID]
		if !ok || block.Message != message || block.Width != width || block.Style != style {
			block = m.renderMessage(message, width, style)
			m.messageCache[message.ID] = block
		}
		first := len(lines)
		lines = append(lines, block.Lines...)
		m.segments = append(m.segments, messageSegment{ID: message.ID, User: message.Sender, First: first, Last: len(lines) - 1, NameLine: first + block.NameOffset, NameX: block.NameX, NameWidth: block.NameWidth})
	}
	if len(m.messageCache) > maxCachedMessages {
		active := map[int64]bool{}
		for _, message := range m.feed {
			active[message.ID] = true
		}
		for id := range m.messageCache {
			if !active[id] {
				delete(m.messageCache, id)
			}
		}
	}
	m.view.SetContent(strings.Join(lines, "\n"))
	if bottom {
		m.view.GotoBottom()
		m.newMessages = 0
	}
}
func (m *model) saveView() error {
	m.flushRates()
	if !m.logged || !m.contextLoaded {
		return nil
	}
	if err := m.retryEmergencyDraft(); err != nil {
		m.showEmergencyDraft()
		return err
	}
	if m.positionUnknown {
		if err := m.retryPositionLoad(); err != nil {
			m.showPositionLoadError(err)
			return err
		}
	}
	var compositionError error
	if m.composeKind != "" {
		if err := m.saveCompositionDraft(); err != nil {
			m.notice = "草稿未保存：" + err.Error()
			compositionError = err
		}
	}
	if err := m.savePositionDraft(); err != nil {
		return err
	}
	return compositionError
}
func (m *model) move(room, peer string) error {
	return m.moveContext(room, peer, false)
}

func (m *model) forceMove(room string) error {
	return m.moveContext(room, "", true)
}

func (m *model) moveContext(room, peer string, forced bool) error {
	if (room != m.room || peer != m.peer) && m.secretPanel != nil {
		m.closePanel()
	}
	if peer != "" {
		u, e := m.store.Resolve(peer)
		if e != nil {
			return e
		}
		if u.Name == m.user.Name {
			return fmt.Errorf("不能给自己发私信")
		}
		peer = u.Name
		room = ""
	} else {
		if !m.store.CanRead(m.user.Name, room) && !(forced && room == "lobby") {
			return fmt.Errorf("频道不存在或你没有访问权限")
		}
	}
	if m.contextLoaded && m.room == room && m.peer == peer {
		return m.saveView()
	}
	p, positionErr := m.store.PositionWithError(m.user.Name, scopeFor(room, peer))
	if positionErr != nil && !forced {
		return fmt.Errorf("目标对话草稿读取失败，未切换：%w", positionErr)
	}
	feed, err := m.store.History(room, m.user.Name, peer, 100)
	if err != nil && !forced {
		return err
	}
	latestID := lastMessageID(feed)
	if p.Offset > 0 && p.Anchor > 0 {
		if window, e := m.store.WindowFrom(room, m.user.Name, peer, p.Anchor); e != nil {
			if !forced {
				return e
			}
		} else if len(window) > 0 {
			feed = window
		}
	}
	originScope := scopeFor(m.room, m.peer)
	originPosition := m.currentPosition()
	if err := m.saveView(); err != nil {
		if !forced {
			return fmt.Errorf("原对话草稿尚未保存，未切换：%w", err)
		}
		m.preserveEmergencyDraft(originScope, originPosition)
	}
	if m.composeKind != "" && (room != m.composeRoom || peer != "") {
		m.freezeComposition()
	}
	archivedConflict := m.positionConflict
	// Clear the old input before changing its scope, even when the destination
	// could not be read during a mandatory permission change.
	m.input.SetValue("")
	m.replyID = 0
	m.feed = nil
	m.room, m.peer = room, peer
	m.hub.Move(m.client, room, peer)
	m.positionVersion = p.Version
	m.positionSaved = p
	m.positionUnknown = positionErr != nil
	if m.positionUnknown {
		m.positionVersion = -1
		m.positionSaved = Position{Version: -1}
	}
	m.positionConflict = false
	m.positionConflictSession = ""
	m.lastRead = p.LastRead
	m.feed = feed
	m.feedAtLatest = err == nil && lastMessageID(feed) >= latestID
	m.latestMessageID = latestID
	m.eventFloorID = latestID
	m.input.SetValue(p.Draft)
	m.input.CursorEnd()
	m.replyID = p.ReplyTo
	m.help = false
	m.closePanel()
	m.contextLoaded = true
	m.newMessages = 0
	m.refresh()
	m.rebuild(p.Offset == 0)
	if p.Offset > 0 {
		m.view.SetYOffset(p.Offset)
	}
	if peer == "" {
		r, _ := m.store.RoomInfo(room)
		m.currentAnnouncement = r.Announcement
		if r.Announcement != "" {
			m.flash = "公告：" + r.Announcement
			m.flashUntil = time.Now().Add(8 * time.Second)
		}
	}
	if archivedConflict {
		m.flash = "原对话的冲突草稿已私有保存，可用 /draft-conflicts 找回"
		m.flashUntil = time.Now().Add(8 * time.Second)
	}
	if previous, found, err := m.store.ConflictDraft(m.user.Name, scopeFor(room, peer), m.client.ID); !m.positionUnknown && err == nil && found {
		m.input.SetValue(previous.Draft)
		m.replyID = previous.ReplyTo
		m.positionConflict = true
		m.positionConflictSession = m.client.ID
		m.positionConflictSaved = previous
		m.openPositionConflict()
	}
	if m.positionUnknown {
		m.showPositionLoadError(positionErr)
	}
	if m.emergencyDraft != nil {
		m.showEmergencyDraft()
	}
	return nil
}
func (m *model) flushRates() {
	if m.rateDrops > 0 {
		m.store.Dropped(m.rateDrops)
		m.rateDrops = 0
	}
}
func (m *model) reloadVisible() {
	m.revalidateSecretPanel(time.Now(), true)
	if !m.logged {
		return
	}
	if m.composeKind != "" && !m.store.CanRead(m.user.Name, m.composeRoom) {
		m.freezeComposition()
	}
	if m.peer == "" && !m.store.CanRead(m.user.Name, m.room) {
		m.feed = nil
		m.replyID = 0
		m.closePanel()
		_ = m.forceMove("lobby")
		m.note("频道访问权限已更新，你已返回大厅")
		return
	}
	m.ignored = map[string]bool{}
	for name := range m.store.Relations(m.user.Name, "block") {
		m.ignored[name] = true
	}
	m.settings = m.store.Settings(m.user.Name)
	if m.panelTitle != "" && m.panelKind != "secret" && m.panelKind != "emergency-draft" && m.panelKind != "position-loading" {
		m.closePanel()
	}
	m.refresh()
	if m.peer == "" {
		r, e := m.store.RoomInfo(m.room)
		if e == nil && r.Announcement != m.currentAnnouncement {
			m.currentAnnouncement = r.Announcement
			if r.Announcement != "" {
				m.flash = "公告更新：" + r.Announcement
				m.flashUntil = time.Now().Add(8 * time.Second)
			}
		}
	}
}
func (m *model) refreshMessage(id int64) {
	source, e := m.store.Message(m.user.Name, id)
	if e != nil {
		return
	}
	if (m.panelMessageID == id && m.panelKind != "revisions") || m.panelKind == "messages" {
		m.closePanel()
	}
	if source.Recalled > 0 && m.replyID == id {
		m.replyID = 0
		m.flash = "回复目标已撤回，已取消回复"
		m.flashUntil = time.Now().Add(4 * time.Second)
	}
	bottom, offset := m.feedAtLatest && m.view.AtBottom(), m.view.YOffset
	for i, v := range m.feed {
		if v.ID == id {
			m.feed[i] = source
		} else if v.ReplyTo == id {
			m.feed[i].ReplyName = source.Sender
			m.feed[i].ReplyBody = cleanText(source.Body, 70)
		}
	}
	m.rebuild(bottom)
	if !bottom {
		m.view.SetYOffset(offset)
	}
}
func (s *Store) WindowFrom(room, user, peer string, anchor int64) ([]Message, error) {
	return s.PageAfter(room, user, peer, max(0, anchor-1), 200)
}
func (m *model) loadOlder() {
	if len(m.feed) == 0 {
		return
	}
	var first int64
	for _, v := range m.feed {
		if v.ID > 0 {
			first = v.ID
			break
		}
	}
	if first == 0 {
		return
	}
	older, e := m.store.Page(m.room, m.user.Name, m.peer, first, 100)
	if e != nil {
		m.timelineError(e)
		return
	}
	if len(older) == 0 {
		m.flash = "已经到最早的记录"
		m.flashUntil = time.Now().Add(3 * time.Second)
		return
	}
	anchor := m.timelineAnchor()
	m.feed = append(older, m.feed...)
	if len(m.feed) > maxFeedMessages {
		m.feed = m.feed[:maxFeedMessages]
		m.feedAtLatest = false
	}
	m.rebuild(false)
	m.restoreTimelineAnchor(anchor)
}
func (m *model) selectReply(id int64) error {
	v, e := m.store.Message(m.user.Name, id)
	if e != nil {
		return e
	}
	same := m.peer == "" && v.Recipient == "" && v.Room == m.room
	if m.peer != "" {
		same = v.Room == "" && ((v.Sender == m.user.Name && v.Recipient == m.peer) || (v.Sender == m.peer && v.Recipient == m.user.Name))
	}
	if !same {
		return fmt.Errorf("只能回复当前对话的消息")
	}
	m.replyID = id
	m.closePanel()
	m.flash = fmt.Sprintf("回复 #%d @%s：%s · Esc 取消", id, v.Sender, cleanText(v.Body, 60))
	m.flashUntil = time.Now().Add(10 * time.Second)
	m.input.Focus()
	return nil
}
func (m *model) insertMention(name string) {
	r := []rune(m.input.Value())
	p := min(m.input.Position(), len(r))
	insert := "@" + name + " "
	if p > 0 && !unicode.IsSpace(r[p-1]) {
		insert = " " + insert
	}
	updated := append(append(append([]rune{}, r[:p]...), []rune(insert)...), r[p:]...)
	m.input.SetValue(string(updated))
	m.input.SetCursor(p + len([]rune(insert)))
	m.closePanel()
	m.input.Focus()
}
func (m *model) poke(name string) {
	u, e := m.store.Resolve(name)
	if e == nil {
		e = m.store.Poke(m.user.Name, u.Name)
	}
	if e != nil {
		m.note(e.Error())
		return
	}
	m.flash = "你戳了戳 @" + u.Name
	m.flashUntil = time.Now().Add(3 * time.Second)
	m.hub.NotifyUser(u.Name, "poke", m.user.Name+" 戳了戳你")
}

func (m *model) closePanel() {
	if m.secretPanel != nil {
		m.secretPanel.active = false
		for i := range m.panelLines {
			m.panelLines[i] = ""
		}
		m.secretPanel = nil
	}
	m.panelMessageID = 0
	m.panelTitle = ""
	m.panelKind = ""
	m.panelLines = nil
	m.panelActions = nil
	m.panelFilter.SetValue("")
	m.panelFilter.Width = max(8, m.w-4)
	m.panelFilter.Blur()
	m.panelOffset = 0
	m.panelSelected = 0
}
func (m *model) openPanel(title, kind string, lines []string, actions []panelAction) {
	m.closePanel()
	m.help = false
	m.panelTitle = title
	m.panelKind = kind
	m.panelLines = lines
	m.panelActions = actions
	m.panelOffset = 0
	m.panelSelected = 0
	m.panelFilter.SetValue("")
	m.panelFilter.Width = max(8, m.w-4)
	m.panelFilter.Focus()
}
func (m *model) userCard(name string) {
	u, e := m.store.Resolve(name)
	if e != nil {
		m.note(e.Error())
		return
	}
	lines := []string{avatarFor(u), fmt.Sprintf("@%s  UID #%d  [%s]", u.Name, u.UID, u.Role), "注册：" + formatUnix(u.Created), "状态：" + u.Status, "简介：" + u.Bio, "成就：" + strings.Join(m.store.BadgeNames(u.Name), " / ")}
	last := "尚无登录记录"
	if u.LastLogin > 0 {
		last = formatUnix(u.LastLogin)
	}
	online := "离线"
	for _, name := range m.hub.Online() {
		if name == u.Name {
			online = "在线"
			break
		}
	}
	lines = append(lines, "最近登录："+last, "当前："+online)
	actions := []panelAction{}
	if u.Name == m.user.Name {
		actions = append(actions, panelAction{"SSH 设备密钥", "/keys", false}, panelAction{"修改密码", "/passwd", false}, panelAction{"我的会话", "/sessions", false}, panelAction{"退出其他设备", "/logoutothers", false})
	} else {
		actions = append(actions, panelAction{"发送私信", "/dm " + u.Name, false}, panelAction{"@ 提及", "/mention " + u.Name, false}, panelAction{"戳一戳", "/poke " + u.Name, false}, panelAction{"关注", "/follow " + u.Name, false}, panelAction{"申请好友", "/friend " + u.Name, false}, panelAction{"私有备注", "/note " + u.Name + " ", true}, panelAction{"屏蔽", "/ignore " + u.Name, false})
	}
	if isAdmin(m.user.Role) && u.Name != m.user.Name {
		actions = append(actions, panelAction{"登录 IP", "/ip " + u.Name, false}, panelAction{"连接会话", "/sessions " + u.Name, false}, panelAction{"禁言 10 分钟", "/mute " + u.Name + " 10m ", true}, panelAction{"封号（填写期限与原因）", "/ban " + u.Name + " 1d ", true})
	}
	m.openPanel("用户卡片", "card", lines, actions)
}
func (m *model) messageMenu(id int64) {
	v, e := m.store.Message(m.user.Name, id)
	if e != nil {
		m.note(e.Error())
		return
	}
	actions := []panelAction{{"引用回复", "/reply " + strconv.FormatInt(id, 10), false}, {"收藏", "/bookmark " + strconv.FormatInt(id, 10), false}, {"点赞", "/react " + strconv.FormatInt(id, 10) + " like", false}, {"展开阅读", "/read " + strconv.FormatInt(id, 10), false}, {"举报（填写原因）", "/report " + strconv.FormatInt(id, 10) + " ", true}}
	if v.Recalled == 0 && v.Sender == m.user.Name {
		actions = append(actions, panelAction{"编辑", "/edit " + strconv.FormatInt(id, 10) + " " + v.Body, true}, panelAction{"撤回", "/recall " + strconv.FormatInt(id, 10), false})
	} else if v.Recalled == 0 && m.peer == "" && m.store.canManage(m.user, m.room) {
		actions = append(actions, panelAction{"管理员撤回", "/recall " + strconv.FormatInt(id, 10), false})
	}
	if m.peer == "" && m.store.canManage(m.user, m.room) {
		actions = append(actions, panelAction{"置顶", "/pin " + strconv.FormatInt(id, 10), false})
	}
	m.openPanel(fmt.Sprintf("消息 #%d", id), "message", []string{"@" + v.Sender, cleanText(v.Body, 800)}, actions)
	m.panelMessageID = id
}
func (m model) filteredActions() []panelAction {
	query := strings.ToLower(strings.TrimSpace(m.panelFilter.Value()))
	if query == "" {
		return m.panelActions
	}
	var result []panelAction
	for _, a := range m.panelActions {
		if strings.Contains(strings.ToLower(a.Label+" "+a.Command), query) {
			result = append(result, a)
		}
	}
	return result
}
func (m *model) runAction(a panelAction) tea.Cmd {
	m.closePanel()
	if a.Fill {
		m.input.SetValue(a.Command)
		m.input.CursorEnd()
		m.input.Focus()
		return nil
	}
	return m.command(a.Command)
}
func (m model) panelScreen() string {
	layout := m.panelLayout()
	filter := m.panelFilter
	filter.Width = max(1, m.w-4)
	filter.SetCursor(filter.Position())
	lines := []string{m.color(strings.Repeat("─", m.w), "#344359", false), m.fitLine(m.color(" "+m.panelTitle+"  [Esc 返回]", m.accent(), true), m.w), m.fitLine(" "+filter.View(), m.w), m.color(strings.Repeat("─", m.w), "#344359", false)}
	for i := 0; i < layout.height; i++ {
		index := layout.offset + i
		line := ""
		if index < len(layout.rows) {
			line = layout.rows[index].text
		}
		lines = append(lines, "  "+line)
	}
	lines = append(lines, m.fitLine(" Enter 执行 · ↑↓ 选择 · 输入筛选 · 鼠标点击/滚轮", m.w))
	return strings.Join(lines, "\n")
}
func (m model) panelHits() []hit {
	if !m.usableViewport() {
		return nil
	}
	layout := m.panelLayout()
	var hits []hit
	for i := 0; i < layout.height && layout.offset+i < len(layout.rows); i++ {
		if action := layout.rows[layout.offset+i].action; action >= 0 {
			hits = append(hits, hit{2, 4 + i, layout.width, 1, "panel", strconv.Itoa(action)})
		}
	}
	return hits
}
func (m model) panelMaxOffset() int {
	layout := m.panelLayout()
	return max(0, len(layout.rows)-layout.height)
}
func (m *model) keepPanelSelection() {
	layout := m.panelLayout()
	first, last := -1, -1
	for i, row := range layout.rows {
		if row.action == m.panelSelected {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	if first >= 0 {
		if first < layout.offset || last-first+1 > layout.height {
			m.panelOffset = first
		} else if last >= layout.offset+layout.height {
			m.panelOffset = last - layout.height + 1
		}
	}
	m.panelOffset = max(0, min(m.panelOffset, max(0, len(layout.rows)-layout.height)))
}
func (m model) toolbarActions() []panelAction {
	count := strconv.Itoa(m.noticeCount)
	if m.noticeCount > 9999 {
		count = "9999+"
	}
	if m.w < 60 {
		actions := []panelAction{{"[!" + count + "]", "/notifications", false}, {"[菜单]", "/menu", false}}
		if isAdmin(m.user.Role) {
			actions = append(actions, panelAction{"[管]", "/admin", false})
		}
		return append(actions, panelAction{"[?]", "/help", false}, panelAction{"[退]", "/quit", false})
	}
	actions := []panelAction{{"[通知 " + count + "]", "/notifications", false}, {"[菜单]", "/menu", false}}
	if isAdmin(m.user.Role) {
		actions = append(actions, panelAction{"[管理]", "/admin", false})
	}
	return append(actions, panelAction{"[帮助]", "/help", false}, panelAction{"[退出]", "/quit", false})
}
func (m model) toolbarText() string {
	var labels []string
	for _, a := range m.toolbarActions() {
		labels = append(labels, a.Label)
	}
	return strings.Join(labels, " ")
}
func (m model) toolbarHits() []hit {
	start := max(0, m.w-ansi.StringWidth(m.toolbarText()))
	var hits []hit
	for _, a := range m.toolbarActions() {
		width := ansi.StringWidth(a.Label)
		hits = append(hits, hit{start, 1, width, 1, "toolbar", a.Command})
		start += width + 1
	}
	return hits
}
func (m model) hits() []hit {
	if !m.usableViewport() || m.awaitingTerminalSize {
		return nil
	}
	if m.keyPrompt || m.keyForm != nil {
		return m.keyDialogHits()
	}
	if !m.logged && m.formInvite && m.register {
		var hits []hit
		for i := 0; i < 4; i++ {
			hits = append(hits, hit{2, 3 + i*2, m.w - 4, 1, "field", strconv.Itoa(i)})
		}
		return append(hits, hit{2, 11, m.w - 4, 1, "submit", ""}, hit{2, 13, 12, 1, "login", ""})
	}
	if !m.logged || m.focus == 10 {
		return m.hitsLegacy()
	}
	if m.composeKind != "" {
		_, hits := m.composeControls()
		return hits
	}
	if m.panelTitle != "" {
		return m.panelHits()
	}
	hits := m.toolbarHits()
	for _, h := range m.hitsLegacy() {
		if h.action != "help" && h.action != "logout" {
			hits = append(hits, h)
		}
	}
	return hits
}
func (m model) View() string {
	if m.panelKind == "secret" && !m.secretPanelLocallyAllowed(time.Now()) {
		m.closePanel()
	}
	if m.awaitingTerminalSize {
		return ""
	}
	var raw string
	if !m.usableViewport() {
		raw = m.smallViewportView()
	} else if m.emergencyDraft != nil {
		// Async refreshes or delayed panels must not hide the persistence warning.
		if m.panelKind != "emergency-draft" {
			m.showEmergencyDraft()
		}
		raw = m.panelScreen()
	} else if m.keyPrompt || m.keyForm != nil {
		raw = m.keyDialogView()
	} else if m.logged && m.composeKind != "" {
		raw = m.composeView()
	} else if m.logged && m.panelTitle != "" && m.focus != 10 {
		raw = m.panelScreen()
	} else if !m.logged && m.formInvite && m.register {
		raw = m.inviteAuthView()
	} else {
		raw = m.viewBase()
	}
	if m.settings["ascii"] == "on" {
		raw = strings.NewReplacer("─", "-", "━", "=", "│", "|", "↳", ">", "›", ">", "…", "~", "·", ".").Replace(raw)
	}
	raw = terminalFrame(raw, m.w, m.h)
	if m.user.Theme == "light" {
		raw = m.lightPalette(raw)
	}
	if m.bellPending != nil && m.bellPending.Swap(false) {
		raw = "\a" + raw
	}
	return raw
}
func (m *model) mouseInteractions(x tea.MouseMsg) (bool, tea.Cmd) {
	if !m.usableViewport() || m.awaitingTerminalSize {
		return true, nil
	}
	if m.emergencyDraft != nil {
		if m.panelKind != "emergency-draft" {
			m.showEmergencyDraft()
		}
		if x.Action == tea.MouseActionPress && x.Button == tea.MouseButtonLeft {
			for _, h := range m.panelHits() {
				if x.Y == h.y && x.X >= h.x && x.X < h.x+h.w {
					return true, m.emergencyDraftKey(tea.KeyMsg{Type: tea.KeyEnter})
				}
			}
		}
		if x.Button == tea.MouseButtonWheelUp {
			m.panelOffset = max(0, m.panelOffset-3)
		} else if x.Button == tea.MouseButtonWheelDown {
			m.panelOffset = min(m.panelMaxOffset(), m.panelOffset+3)
		}
		return true, nil
	}
	if m.positionUnknown {
		if x.Action == tea.MouseActionPress && x.Button == tea.MouseButtonLeft {
			for _, h := range m.panelHits() {
				if x.Y == h.y && x.X >= h.x && x.X < h.x+h.w {
					return true, m.positionLoadKey(tea.KeyMsg{Type: tea.KeyEnter})
				}
			}
		}
		return true, nil
	}
	if m.keyPrompt || m.keyForm != nil {
		return true, m.keyDialogMouse(x)
	}
	if m.logged && m.panelTitle != "" && x.Action == tea.MouseActionPress && x.Button == tea.MouseButtonRight && m.panelMessageID > 0 {
		if e := m.selectReply(m.panelMessageID); e != nil {
			m.note(e.Error())
		}
		return true, nil
	}
	if !m.logged {
		return false, nil
	}
	if m.busy {
		return true, nil
	}
	if m.focus == 10 {
		return false, nil
	}
	if m.composeKind != "" {
		return m.composeMouse(x)
	}
	if x.Action == tea.MouseActionPress && x.Button == tea.MouseButtonLeft && m.lastClickName != "" && m.lastClickX == x.X && m.lastClickY == x.Y && time.Since(m.lastClickAt) <= 350*time.Millisecond {
		return true, m.nameClick(m.lastClickName, x.X, x.Y)
	}
	if m.panelTitle != "" {
		if x.Button == tea.MouseButtonWheelUp {
			m.panelOffset = max(0, m.panelOffset-3)
			return true, nil
		}
		if x.Button == tea.MouseButtonWheelDown {
			m.panelOffset = min(m.panelMaxOffset(), m.panelOffset+3)
			return true, nil
		}
		if x.Action == tea.MouseActionPress && x.Button == tea.MouseButtonLeft {
			for _, h := range m.panelHits() {
				if x.Y == h.y && x.X >= h.x && x.X < h.x+h.w {
					i, _ := strconv.Atoi(h.value)
					actions := m.filteredActions()
					if i < len(actions) {
						return true, m.runAction(actions[i])
					}
				}
			}
		}
		return true, nil
	}
	if x.Action != tea.MouseActionPress {
		return false, nil
	}
	if x.Button == tea.MouseButtonLeft && x.Y == 1 {
		for _, h := range m.toolbarHits() {
			if x.X >= h.x && x.X < h.x+h.w {
				return true, m.command(h.value)
			}
		}
	}
	for _, h := range m.hitsLegacy() {
		if h.action == "peer" && x.X >= h.x && x.X < h.x+h.w && x.Y == h.y {
			if x.Button == tea.MouseButtonRight {
				m.insertMention(h.value)
				return true, nil
			}
			if x.Button == tea.MouseButtonLeft {
				return true, m.nameClick(h.value, x.X, x.Y)
			}
		}
	}
	side, width, _ := m.dimensions()
	relativeX := x.X - side
	line := x.Y - 5 + m.view.YOffset
	if x.Y >= 5 && x.Y < 5+m.view.Height && relativeX >= 0 && relativeX < width {
		for _, seg := range m.segments {
			if line < seg.First || line > seg.Last {
				continue
			}
			if seg.nameAt(line, relativeX, width) {
				if x.Button == tea.MouseButtonRight {
					m.insertMention(seg.User)
					return true, nil
				}
				if x.Button == tea.MouseButtonLeft {
					return true, m.nameClick(seg.User, x.X, x.Y)
				}
			}
			if x.Button == tea.MouseButtonRight {
				if e := m.selectReply(seg.ID); e != nil {
					m.note(e.Error())
				}
				return true, nil
			}
			if x.Button == tea.MouseButtonLeft {
				m.messageMenu(seg.ID)
				return true, nil
			}
		}
	}
	return false, nil
}
func (m *model) keyInteractions(x tea.KeyMsg) (bool, tea.Cmd) {
	if !m.usableViewport() || m.awaitingTerminalSize {
		return true, nil
	}
	if m.emergencyDraft != nil {
		return true, m.emergencyDraftKey(x)
	}
	if m.positionUnknown {
		return true, m.positionLoadKey(x)
	}
	if m.keyPrompt || m.keyForm != nil {
		return true, m.keyDialogKey(x)
	}
	if !m.logged && x.String() == "f6" {
		if m.settings["motion"] == "off" {
			m.settings["motion"] = "on"
		} else {
			m.settings["motion"] = "off"
		}
		return true, nil
	}
	if !m.logged {
		return false, nil
	}
	if m.focus == 10 {
		return false, nil
	}
	if m.composeKind != "" {
		return true, m.composeKey(x)
	}
	if m.user.MustReset {
		m.command("/passwd")
		return true, nil
	}
	if m.panelTitle != "" {
		switch x.String() {
		case "esc":
			m.closePanel()
			return true, nil
		case "up", "shift+tab":
			m.panelSelected = max(0, m.panelSelected-1)
			m.keepPanelSelection()
		case "down", "tab":
			m.panelSelected = min(max(0, len(m.filteredActions())-1), m.panelSelected+1)
			m.keepPanelSelection()
		case "pgup":
			m.panelOffset = max(0, m.panelOffset-8)
		case "pgdown":
			m.panelOffset = min(m.panelMaxOffset(), m.panelOffset+8)
		case "enter":
			actions := m.filteredActions()
			if m.panelSelected < len(actions) {
				return true, m.runAction(actions[m.panelSelected])
			}
		default:
			var cmd tea.Cmd
			m.panelFilter, cmd = m.panelFilter.Update(x)
			m.panelSelected = 0
			m.panelOffset = 0
			return true, cmd
		}
		return true, nil
	}
	switch x.String() {
	case "tab":
		m.complete()
		return true, nil
	case "f1":
		return true, m.command("/help")
	case "f2":
		return true, m.command("/notifications")
	case "f3":
		return true, m.command("/contacts")
	case "f4":
		return true, m.command("/menu")
	case "f8":
		return true, m.command("/admin")
	case "esc":
		if m.replyID > 0 {
			m.replyID = 0
			m.flash = ""
			return true, nil
		}
	case "ctrl+end":
		if err := m.loadLatest(100); err != nil {
			m.timelineError(err)
		} else {
			m.saveView()
		}
		return true, nil
	case "pgup":
		if !m.help {
			m.scrollTimeline(-max(1, m.view.Height/2))
			return true, nil
		}
	case "pgdown":
		if !m.help {
			m.scrollTimeline(max(1, m.view.Height/2))
			return true, nil
		}
	}
	return false, nil
}
func (m *model) complete() {
	text := []rune(m.input.Value())
	pos := min(m.input.Position(), len(text))
	start := pos
	for start > 0 && !unicode.IsSpace(text[start-1]) {
		start--
	}
	prefix := string(text[start:pos])
	var choices []string
	if start == 0 && strings.HasPrefix(prefix, "/") {
		for _, c := range commandCatalog {
			if strings.HasPrefix(c.Name, prefix) {
				choices = append(choices, c.Name)
			}
		}
	} else if strings.HasPrefix(prefix, "#") {
		for _, r := range m.rooms {
			if strings.HasPrefix(r.Name, strings.TrimPrefix(prefix, "#")) {
				choices = append(choices, "#"+r.Name)
			}
		}
	} else if strings.HasPrefix(prefix, "@") || strings.HasPrefix(string(text), "/") {
		raw := strings.TrimPrefix(prefix, "@")
		rows, e := m.store.db.Query(`SELECT name FROM users WHERE name LIKE ? ORDER BY name LIMIT 100`, raw+"%")
		if e == nil {
			for rows.Next() {
				var n string
				if rows.Scan(&n) == nil {
					if strings.HasPrefix(prefix, "@") {
						n = "@" + n
					}
					choices = append(choices, n)
				}
			}
			rows.Close()
		}
	}
	sort.Strings(choices)
	if len(choices) == 1 {
		replacement := []rune(choices[0] + " ")
		updated := append(append(append([]rune{}, text[:start]...), replacement...), text[pos:]...)
		m.input.SetValue(string(updated))
		m.input.SetCursor(start + len(replacement))
		return
	}
	if len(choices) > 1 {
		var actions []panelAction
		for _, v := range choices {
			updated := string(text[:start]) + v + " " + string(text[pos:])
			actions = append(actions, panelAction{v, updated, true})
		}
		m.openPanel("自动补全", "complete", nil, actions)
	}
}
