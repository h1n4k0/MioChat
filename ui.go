package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
	gossh "golang.org/x/crypto/ssh"
)

const logo = ` __  __ _        ____ _           _
|  \/  (_) ___  / ___| |__   __ _| |_
| |\/| | |/ _ \| |   | '_ \ / _` + "`" + ` | __|
| |  | | | (_) | |___| | | | (_| | |_
|_|  |_|_|\___/ \____|_| |_|\__,_|\__|`

type authResult struct {
	user User
	err  error
}
type clockMsg time.Time
type hit struct {
	x, y, w, h    int
	action, value string
}
type model struct {
	store                                     *Store
	hub                                       *Hub
	ctx                                       context.Context
	client                                    *Client
	renderer                                  *lipgloss.Renderer
	ip                                        string
	w, h                                      int
	register, busy, logged, help, showSidebar bool
	passFocus, helpOffset                     int
	user                                      User
	focus                                     int
	notice                                    string
	frame                                     int
	fields                                    [4]textinput.Model
	input                                     textinput.Model
	view                                      viewport.Model
	rooms                                     []Room
	online                                    []string
	room, peer                                string
	feed                                      []Message
	feedAtLatest                              bool
	latestMessageID                           int64
	eventFloorID                              int64
	unread                                    map[string]int
	ignored                                   map[string]bool
	limiter                                   bucket
	drafts                                    []string
	draftIndex                                int
	close                                     func()
	settings                                  map[string]string
	replyID, lastRead                         int64
	segments                                  []messageSegment
	contextLoaded, formInvite                 bool
	currentAnnouncement                       string
	newMessages, noticeCount                  int
	panelTitle, panelKind                     string
	panelMessageID                            int64
	panelLines                                []string
	panelActions                              []panelAction
	panelFilter                               textinput.Model
	panelOffset, panelSelected                int
	lastClickName                             string
	lastClickAt                               time.Time
	lastClickX, lastClickY                    int
	flash                                     string
	flashKind                                 string
	flashUntil                                time.Time
	editor                                    textarea.Model
	composeKind, composeTitle, composeRoom    string
	composePreview, composeFrozen             bool
	composeSavedTitle, composeSavedBody       string
	positionVersion                           int64
	positionSaved                             Position
	positionUnknown                           bool
	positionLoadError                         string
	positionConflict                          bool
	positionConflictSession                   string
	positionConflictSaved                     Position
	emergencyDraft                            *ConflictDraft
	bellPending                               *atomic.Bool
	rateDrops                                 int64
	messageCache                              map[int64]cachedMessage
	sshPublicKey                              gossh.PublicKey
	sshAutoUser                               *User
	authenticatedSignal                       *atomic.Bool
	passwordVerifiedAt                        time.Time
	keyPrompt                                 bool
	keyPromptFocus                            int
	keyForm                                   *sshKeyForm
	maintenanceCancel                         context.CancelFunc
}

func newModel(store *Store, hub *Hub, ctx context.Context, out io.Writer, term string, w, h int, ip string, closeFn func()) model {
	hub.Attach(store)
	w, h = normalizeTerminalSize(w, h)
	r := lipgloss.NewRenderer(out)
	profile := termenv.ANSI256
	if strings.Contains(term, "truecolor") || strings.Contains(term, "direct") {
		profile = termenv.TrueColor
	} else if term == "dumb" || term == "" {
		profile = termenv.Ascii
	} else if !strings.Contains(term, "256") {
		profile = termenv.ANSI
	}
	r.SetColorProfile(profile)
	r.SetHasDarkBackground(true)
	m := model{store: store, hub: hub, ctx: ctx, renderer: r, w: w, h: h, ip: ip, room: "lobby", showSidebar: true, unread: map[string]int{}, ignored: map[string]bool{}, close: closeFn, draftIndex: -1}
	id, err := secretToken()
	if err != nil {
		panic(err)
	}
	m.client = &Client{Events: make(chan Event, 256), Close: closeFn, ID: id, IP: ip, Ctx: ctx}
	m.settings = map[string]string{"motion": "on", "poke": "on", "dnd": "off"}
	m.formInvite = store.Config("registration") == "invite"
	m.bellPending = &atomic.Bool{}
	for i := range m.fields {
		f := textinput.New()
		f.CharLimit = 256
		f.Prompt = ""
		f.KeyMap.Paste.SetEnabled(false)
		f.Width = 30
		if i > 0 {
			f.EchoMode = textinput.EchoPassword
			f.EchoCharacter = '*'
		}
		m.fields[i] = f
	}
	m.fields[0].CharLimit = 20
	// Bubbles v0.21's placeholder padding counts CJK columns as rune indices.
	// Keep placeholders ASCII; labels, values and all chat content are UTF-8.
	m.fields[0].Placeholder = "username"
	m.fields[1].Placeholder = "password"
	m.fields[2].Placeholder = "repeat password"
	m.fields[3].Placeholder = "invite code"
	m.fields[0].Focus()
	m.input = textinput.New()
	m.input.Prompt = ""
	m.input.KeyMap.Paste.SetEnabled(false)
	m.input.CharLimit = 1600
	m.input.Placeholder = "Type a message or /help..."
	m.view = viewport.New(60, 12)
	m.panelFilter = textinput.New()
	m.panelFilter.Prompt = ""
	m.panelFilter.Placeholder = "Filter..."
	m.panelFilter.CharLimit = 80
	m.panelFilter.KeyMap.Paste.SetEnabled(false)
	m.editor = textarea.New()
	m.editor.CharLimit = 4096
	m.editor.ShowLineNumbers = true
	m.editor.Placeholder = "Write here..."
	return m
}
func (m model) Init() tea.Cmd {
	commands := []tea.Cmd{textinput.Blink, tick()}
	if m.sshAutoUser != nil && m.sshPublicKey != nil {
		store, key, ip, version := m.store, m.sshPublicKey, m.ip, m.client.Version
		commands = append(commands, func() tea.Msg {
			user, err := store.AuthenticateSSHKey(key)
			store.RecordLogin(user.Name, ip, version, err == nil)
			return sshAuthResult{user, err}
		})
	}
	return tea.Batch(commands...)
}
func tick() tea.Cmd { return tea.Tick(time.Second, func(t time.Time) tea.Msg { return clockMsg(t) }) }
func (m model) waitEvent() tea.Cmd {
	return func() tea.Msg {
		select {
		case e := <-m.client.Events:
			return e
		case <-m.ctx.Done():
			return tea.Quit()
		}
	}
}
func (m model) color(text, color string, bold bool) string {
	if m.user.Theme == "light" {
		palette := map[string]string{"#70839B": "#57606A", "#93A6BC": "#424A53", "#526078": "#57606A", "#344359": "#8C959F", "#B8C7D9": "#24292F", "#73E2C4": "#096B4B", "#FF9A9A": "#B42318", "#D6A8FF": "#6639BA"}
		if v, ok := palette[color]; ok {
			color = v
		}
	}
	if m.user.Theme == "contrast" || m.settings["contrast"] == "on" {
		if color == "#70839B" || color == "#93A6BC" || color == "#526078" {
			color = "#FFFFFF"
		}
	}
	return m.renderer.NewStyle().Foreground(lipgloss.Color(color)).Bold(bold).Render(text)
}
func (m model) accent() string {
	switch m.user.Theme {
	case "sakura":
		return "#F5A9D0"
	case "amber":
		return "#FFD166"
	case "mono":
		return "#D8DEE9"
	case "light":
		return "#096B4B"
	case "contrast":
		return "#FFFF00"
	default:
		return "#73E2C4"
	}
}
func (m *model) note(s string) {
	m.feed = append(m.feed, Message{Kind: "system", Body: s, At: time.Now().Unix()})
	if len(m.feed) > 500 {
		m.feed = m.feed[len(m.feed)-500:]
	}
	m.rebuild(true)
}
func (m *model) focusField(i int) {
	m.focus = i
	for j := range m.fields {
		m.fields[j].Blur()
	}
	if i < len(m.fields) {
		m.fields[i].Focus()
	}
}
func (m *model) auth() tea.Cmd {
	if m.busy {
		return nil
	}
	name, password, confirm, invite := m.fields[0].Value(), m.fields[1].Value(), m.fields[2].Value(), m.fields[3].Value()
	if !m.hub.authAllowed(m.ip, m.register) {
		m.rateDrops++
		m.notice = "尝试过于频繁，请稍后再试"
		return nil
	}
	m.busy = true
	m.notice = "正在验证…"
	register := m.register
	store, hub, ctx := m.store, m.hub, m.ctx
	ip, client := m.ip, m.client.Version
	return func() tea.Msg {
		select {
		case hub.AuthSlots <- struct{}{}:
		case <-ctx.Done():
			return authResult{err: context.Canceled}
		}
		defer func() { <-hub.AuthSlots }()
		if register {
			u, e := store.RegisterInvite(name, password, confirm, invite, ip, client)
			store.RecordLogin(name, ip, client, e == nil)
			return authResult{u, e}
		}
		u, e := store.Authenticate(name, password)
		store.RecordLogin(name, ip, client, e == nil)
		return authResult{u, e}
	}
}
func (m *model) dimensions() (side, width, height int) {
	if m.w >= 85 && m.showSidebar && m.settings["compact"] != "on" {
		side = 22
	}
	return side, max(8, m.w-side-4), max(3, m.h-10)
}
func (m *model) rebuildLegacy(bottom bool) {
	_, width, height := m.dimensions()
	m.view.Width = width
	m.view.Height = height
	m.input.Width = max(5, m.w-8)
	lines := []string{}
	for _, msg := range m.feed {
		if m.ignored[msg.Sender] {
			continue
		}
		head := m.color(time.Unix(msg.At, 0).In(time.FixedZone("CST", 8*3600)).Format("15:04"), "#70839B", false) + " "
		if msg.Kind == "system" {
			lines = append(lines, m.color(ansi.Hardwrap("  · "+msg.Body, width, true), "#93A6BC", false))
			continue
		}
		name := m.color(msg.Sender, m.accent(), true)
		if msg.Kind == "action" {
			name = "* " + name
		}
		if msg.Recipient != "" {
			name = m.color("[私信] ", "#D6A8FF", false) + name
		}
		head += name + " "
		body := msg.Body
		if msg.ID > 0 {
			body += m.color(fmt.Sprintf("  ·%d", msg.ID), "#526078", false)
		}
		lines = append(lines, ansi.Hardwrap(head+body, width, true))
	}
	m.view.SetContent(strings.Join(lines, "\n"))
	if bottom {
		m.view.GotoBottom()
	}
}
func (m *model) refresh() {
	m.rooms, _ = m.store.VisibleRooms(m.user.Name, false)
	m.online = m.hub.Online()
	m.noticeCount = m.store.UnreadNotifications(m.user.Name)
}
func (m *model) moveLegacy(room, peer string) error {
	if peer != "" {
		u, e := m.store.User(peer)
		if e != nil {
			return fmt.Errorf("账户 %s 不存在", peer)
		}
		if u.Name == m.user.Name {
			return fmt.Errorf("不能给自己发私信")
		}
		peer = u.Name
	} else {
		if _, e := m.store.Room(room); e != nil {
			return fmt.Errorf("频道 #%s 不存在，可用 /create 创建", room)
		}
	}
	m.room, m.peer = room, peer
	m.hub.Move(m.client, room, peer)
	m.feed, _ = m.store.History(room, m.user.Name, peer, 100)
	delete(m.unread, peer)
	m.help = false
	m.refresh()
	m.rebuild(true)
	return nil
}
func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch x := msg.(type) {
	case userClickMsg:
		if m.lastClickName == x.Name && m.lastClickAt.Equal(x.At) {
			m.userCard(x.Name)
		}
		return m, nil
	case tea.WindowSizeMsg:
		width, height := normalizeTerminalSize(x.Width, x.Height)
		if width == m.w && height == m.h {
			return m, nil
		}
		m.w, m.h = width, height
		if m.composeKind != "" {
			m.editor.SetWidth(max(10, m.w-6))
			m.editor.SetHeight(max(4, m.h-9))
		}
		m.rebuild(false)
		return m, nil
	case clockMsg:
		m.frame++
		if m.frame%5 == 0 {
			m.flushRates()
		}
		if m.logged {
			if m.frame%3 == 0 {
				if u, ok := m.hub.RefreshClient(m.client); ok {
					m.user = u
				} else {
					return m, tea.Quit
				}
			}
			if m.frame%15 == 0 {
				m.refresh()
			}
			if m.frame%5 == 0 {
				m.saveView()
			}
			if m.frame%60 == 0 {
				m.store.TouchSession(m.client.ID)
			}
		}
		return m, tick()
	case passwordResult:
		m.busy = false
		if x.err != nil {
			m.notice = x.err.Error()
			return m, nil
		}
		m.endPassword()
		if user, err := m.store.User(m.user.Name); err == nil {
			m.user = user
			m.client.AuthEpoch = user.AuthEpoch
		} else {
			m.close()
			return m, tea.Quit
		}
		m.passwordVerifiedAt = time.Now()
		m.hub.Enforce()
		m.note("密码已修改")
		m.flash = "密码已修改"
		m.flashKind = ""
		m.flashUntil = time.Now().Add(8 * time.Second)
		m.maybeKeyPrompt()
		return m, nil
	case sshAuthResult:
		if m.logged {
			return m, nil
		}
		m.busy = false
		if x.err != nil {
			m.notice = "密钥未能登录，请使用账号密码"
			m.focusField(0)
			return m, nil
		}
		return m, m.completeLogin(x.user, true)
	case sshKeyBindResult:
		return m, m.finishKeyBinding(x)
	case maintenanceResult:
		m.finishMaintenance(x)
		return m, nil
	case authResult:
		if m.logged {
			return m, nil
		}
		m.busy = false
		m.notice = ""
		if x.err != nil {
			m.notice = x.err.Error()
			m.fields[1].SetValue("")
			m.fields[2].SetValue("")
			m.focusField(1)
			return m, nil
		}
		return m, m.completeLogin(x.user, false)
	case Event:
		m.consumeEvents(x)
		return m, m.waitEvent()
	case tea.MouseMsg:
		if handled, cmd := m.mouseInteractions(x); handled {
			return m, cmd
		}
		if m.busy {
			return m, nil
		}
		if x.Button == tea.MouseButtonWheelUp {
			if m.help {
				m.helpOffset = max(0, m.helpOffset-3)
			} else {
				m.scrollTimeline(-3)
			}
			return m, nil
		}
		if x.Button == tea.MouseButtonWheelDown {
			if m.help {
				m.helpOffset = min(70, m.helpOffset+3)
			} else {
				m.scrollTimeline(3)
			}
			return m, nil
		}
		if x.Button == tea.MouseButtonLeft && x.Action == tea.MouseActionPress {
			for _, b := range m.hits() {
				if x.X >= b.x && x.X < b.x+b.w && x.Y >= b.y && x.Y < b.y+b.h {
					cmd := m.click(b)
					return m, cmd
				}
			}
		}
		return m, nil
	case tea.KeyMsg:
		if x.String() == "ctrl+c" {
			return m, tea.Quit
		}
		if handled, cmd := m.keyInteractions(x); handled {
			return m, cmd
		}
		if !m.logged {
			if m.busy {
				return m, nil
			}
			switch x.String() {
			case "ctrl+r":
				m.register = !m.register
				m.notice = ""
				m.focusField(0)
				return m, nil
			case "tab":
				m.focusField((m.focus + 1) % m.fieldCount())
				return m, nil
			case "shift+tab":
				m.focusField((m.focus + m.fieldCount() - 1) % m.fieldCount())
				return m, nil
			case "enter":
				if m.focus == 0 {
					m.focusField(1)
					return m, nil
				}
				if m.register && m.focus < m.fieldCount()-1 {
					m.focusField(m.focus + 1)
					return m, nil
				}
				cmd := m.auth()
				return m, cmd
			}
			var cmd tea.Cmd
			m.fields[m.focus], cmd = m.fields[m.focus].Update(x)
			return m, cmd
		}
		if m.focus == 10 {
			cmd := m.passwordKeys(x)
			return m, cmd
		}
		switch x.String() {
		case "f1":
			m.help = !m.help
			return m, nil
		case "ctrl+b":
			m.showSidebar = !m.showSidebar
			m.rebuild(false)
			return m, nil
		case "esc":
			m.help = false
			return m, nil
		case "pgup":
			if m.help {
				m.helpOffset = max(0, m.helpOffset-10)
			} else {
				m.scrollTimeline(-max(1, m.view.Height/2))
			}
			return m, nil
		case "pgdown":
			if m.help {
				m.helpOffset = min(70, m.helpOffset+10)
			} else {
				m.scrollTimeline(max(1, m.view.Height/2))
			}
			return m, nil
		case "ctrl+end":
			m.view.GotoBottom()
			return m, nil
		case "up":
			if len(m.drafts) > 0 {
				m.draftIndex = min(m.draftIndex+1, len(m.drafts)-1)
				m.input.SetValue(m.drafts[len(m.drafts)-1-m.draftIndex])
				m.input.CursorEnd()
			}
			return m, nil
		case "down":
			m.draftIndex--
			if m.draftIndex < 0 {
				m.draftIndex = -1
				m.input.SetValue("")
			} else {
				m.input.SetValue(m.drafts[len(m.drafts)-1-m.draftIndex])
				m.input.CursorEnd()
			}
			return m, nil
		case "enter":
			line := strings.TrimSpace(m.input.Value())
			m.input.SetValue("")
			m.draftIndex = -1
			if line == "" {
				return m, nil
			}
			if !strings.HasPrefix(line, "/passwd") {
				m.drafts = append(m.drafts, line)
				if len(m.drafts) > 40 {
					m.drafts = m.drafts[1:]
				}
			}
			cmd := m.command(line)
			return m, cmd
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(x)
		return m, cmd
	}
	return m, nil
}
func (m model) fieldCount() int {
	if m.register {
		if m.formInvite {
			return 4
		}
		return 3
	}
	return 2
}
func (m *model) click(b hit) tea.Cmd {
	switch b.action {
	case "login", "register":
		m.register = b.action == "register"
		m.notice = ""
		m.focusField(0)
	case "field":
		var i int
		fmt.Sscan(b.value, &i)
		m.focusField(i)
	case "password-field":
		fmt.Sscan(b.value, &m.passFocus)
		for i := range m.fields {
			m.fields[i].Blur()
		}
		m.fields[m.passFocus].Focus()
	case "password-submit":
		return m.submitPassword()
	case "submit":
		return m.auth()
	case "room":
		if e := m.move(b.value, ""); e != nil {
			m.note(e.Error())
		}
	case "peer":
		if e := m.move("", b.value); e != nil {
			m.note(e.Error())
		}
	case "help":
		return m.command("/help")
	case "logout":
		return tea.Quit
	}
	return nil
}
func (m model) formGeometry() (x, y, width int) {
	width = min(66, m.w-4)
	x = (m.w - width) / 2
	y = max(1, (m.h-24)/2)
	return
}
func (m model) hitsLegacy() []hit {
	if m.focus == 10 {
		x, y, w := m.formGeometry()
		return []hit{{x + 2, y + 3, w - 4, 1, "password-field", "0"}, {x + 2, y + 6, w - 4, 1, "password-field", "1"}, {x + 2, y + 9, w - 4, 1, "password-field", "2"}, {x + 2, y + 11, w - 4, 1, "password-submit", ""}}
	}
	if !m.logged {
		x, y, w := m.formGeometry()
		h := []hit{{x + 2, y + 8, 12, 1, "login", ""}, {x + 18, y + 8, 14, 1, "register", ""}, {x + 2, y + 17, w - 4, 1, "submit", ""}}
		for i := 0; i < m.fieldCount(); i++ {
			h = append(h, hit{x + 2, y + 11 + i*2, w - 4, 1, "field", fmt.Sprint(i)})
		}
		return h
	}
	h := []hit{{max(0, m.w-13), 1, 6, 1, "help", ""}, {max(0, m.w-6), 1, 6, 1, "logout", ""}}
	side, _, height := m.dimensions()
	if side == 0 {
		return h
	}
	for i, r := range m.rooms {
		y := 7 + i
		if y >= 5+height {
			break
		}
		h = append(h, hit{1, y, side - 2, 1, "room", r.Name})
	}
	base := 9 + len(m.rooms)
	for i, n := range m.online {
		y := base + i
		if y >= 5+height {
			break
		}
		h = append(h, hit{1, y, side - 2, 1, "peer", n})
	}
	return h
}
func (m model) fitLine(s string, width int) string { return ansi.Truncate(s, max(1, width), "") }
func (m model) viewBase() string {
	if m.w < 40 || m.h < 24 {
		return fmt.Sprintf("MioChat\n窗口至少需要 40×24，当前 %d×%d。\n请放大终端窗口，Ctrl+C 退出。", m.w, m.h)
	}
	if !m.logged {
		return m.authView()
	}
	if m.focus == 10 {
		return m.passwordView()
	}
	side, width, height := m.dimensions()
	accent := m.accent()
	room := "#" + m.room
	if m.peer != "" {
		room = "@" + m.peer + " · 私信"
	}
	top := m.color(" MIOCHAT", accent, true) + m.color("  /  TERMINAL SOCIAL CLUB", "#70839B", false)
	buttons := m.color(m.toolbarText(), accent, false)
	top = ansi.Truncate(top, max(0, m.w-ansi.StringWidth(buttons)-1), "")
	top += strings.Repeat(" ", max(0, m.w-ansi.StringWidth(top)-ansi.StringWidth(buttons))) + buttons
	header := []string{m.color(strings.Repeat("━", m.w), "#344359", false), top, m.color("  "+room, accent, true) + m.color(fmt.Sprintf("    %s · %d 人在线", m.user.Name, len(m.online)), "#93A6BC", false)}
	topic := "单击用户卡片 · 双击戳 · 右键 @ / 回复"
	if m.peer == "" {
		for _, r := range m.rooms {
			if r.Name == m.room {
				topic = r.Topic
			}
		}
	}
	header = append(header, m.fitLine(m.color("  "+topic, "#93A6BC", false), m.w), m.color(strings.Repeat("─", m.w), "#344359", false))
	content := m.view.View()
	if m.help {
		content = m.helpView(width, height)
	}
	right := strings.Split(content, "\n")
	left := m.sidebar(height)
	body := []string{}
	for i := 0; i < height; i++ {
		line := ""
		if side > 0 {
			line = left[i] + m.color("│ ", "#344359", false)
		}
		r := ""
		if i < len(right) {
			r = right[i]
		}
		body = append(body, line+m.fitLine(r, width))
	}
	input := m.color("  > ", accent, true) + m.input.View()
	footer := m.color(" Enter 发送 · 滚轮/PgUp/PgDn 翻页 · Ctrl+End 最新 · F1 帮助", "#70839B", false)
	lines := append(header, body...)
	status := fmt.Sprintf(" @%s · 通知 %d", m.user.Name, m.noticeCount)
	if m.replyID > 0 {
		status = fmt.Sprintf(" 回复 #%d · Esc 取消回复", m.replyID)
	} else if m.newMessages > 0 {
		status = fmt.Sprintf(" %d 条新消息 · Ctrl+End 跳到最新", m.newMessages)
	} else if !m.feedAtLatest || !m.view.AtBottom() {
		status = " 浏览历史 · PgDn/滚轮继续 · Ctrl+End 最新"
	}
	if time.Now().Before(m.flashUntil) {
		status = " " + m.flash
		if m.flashKind == "poke" && m.settings["motion"] != "off" {
			frames := []string{"o --> ", "o --o ", "o <-o "}
			status = " " + frames[m.frame%len(frames)] + m.flash
		}
	}
	lines = append(lines, m.color(strings.Repeat("─", m.w), "#344359", false), m.fitLine(input, m.w), m.fitLine(footer, m.w), m.fitLine(m.color(status, "#93A6BC", false), m.w))
	return strings.Join(lines, "\n")
}
func (m model) sidebar(height int) []string {
	lines := []string{m.color(" 频道", m.accent(), true), ""}
	counts := m.hub.RoomCounts()
	for _, r := range m.rooms {
		prefix := "  # "
		if m.peer == "" && m.room == r.Name {
			prefix = "› # "
		}
		lines = append(lines, m.color(fmt.Sprintf("%s%-10s %d", prefix, r.Name, counts[r.Name]), m.accent(), m.peer == "" && m.room == r.Name))
	}
	lines = append(lines, "", m.color(" 在线 / 点击私聊", m.accent(), true))
	for _, n := range m.online {
		badge := ""
		if m.unread[n] > 0 {
			badge = fmt.Sprintf(" +%d", m.unread[n])
		}
		lines = append(lines, m.color("  "+n+badge, "#B8C7D9", n == m.user.Name))
	}
	for len(lines) < height {
		lines = append(lines, "")
	}
	for i := range lines {
		lines[i] = ansi.Truncate(lines[i], 20, "")
		lines[i] += strings.Repeat(" ", max(0, 20-ansi.StringWidth(lines[i])))
	}
	return lines
}
func (m model) authView() string {
	x, y, w := m.formGeometry()
	rows := []string{}
	for i := 0; i < y; i++ {
		rows = append(rows, "")
	}
	pad := strings.Repeat(" ", x)
	add := func(s string) { rows = append(rows, pad+m.fitLine(s, w)) }
	add(m.color("+"+strings.Repeat("-", w-2)+"+", "#344359", false))
	for _, l := range strings.Split(logo, "\n") {
		add("  " + m.color(l, m.accent(), true))
	}
	add("  " + m.color("TERMINAL SOCIAL CLUB  /  在字符之间相遇", "#93A6BC", false))
	if m.settings["motion"] != "off" {
		stars := []string{".     *       .      +", "  .      +       *   ", "*       .      +    .", "   +      *       .  "}
		add("  " + m.color(stars[m.frame%len(stars)], "#526078", false))
	} else {
		add("")
	}
	login, reg := "[ 登录 ]", "[ 注册账户 ]"
	if !m.register {
		login = m.color(login, m.accent(), true)
	} else {
		reg = m.color(reg, m.accent(), true)
	}
	add("  " + login + "       " + reg)
	add("")
	labels := []string{"用户名", "密码", "确认密码"}
	for i := 0; i < 3; i++ {
		if i < m.fieldCount() {
			f := m.fields[i]
			add("  " + m.formLabel(labels[i], f))
			add("  " + m.formInput(f, max(8, w-8)))
		} else {
			add("")
			add("")
		}
	}
	add("")
	button := "[ 进入大厅 / Enter ]"
	if m.register {
		button = "[ 创建账户并进入 / Enter ]"
	}
	if m.busy {
		button = "[ " + strings.Repeat("·", m.frame%3+1) + " 正在验证 ]"
	}
	add("  " + m.color(button, m.accent(), true))
	add("  " + m.color(m.notice, "#FF9A9A", false))
	add("")
	add("  Tab 切换输入框 · Ctrl+R 切换登录/注册")
	add("  鼠标可用 · 密码至少10字符 · F6动画 · Ctrl+C离开")
	add(m.color("+"+strings.Repeat("-", w-2)+"+", "#344359", false))
	return strings.Join(rows, "\n")
}
