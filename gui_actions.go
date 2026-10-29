package main

// Native actions use Store/Hub directly. No PTY, terminal View, ANSI screen or
// client-supplied actor is involved. The command spelling matches v2.1.0.
import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

type guiScope struct{ room, peer string }

func (s guiScope) key() string {
	if s.peer != "" {
		return "dm:" + s.peer
	}
	return s.room
}
func guiResolveScope(store *Store, user, key string) (guiScope, error) {
	if strings.HasPrefix(key, "dm:") {
		u, err := store.Resolve(strings.TrimPrefix(key, "dm:"))
		if err != nil {
			return guiScope{}, err
		}
		if u.Name == user {
			return guiScope{}, errors.New("不能给自己发私信")
		}
		return guiScope{peer: u.Name}, nil
	}
	if !store.CanRead(user, key) {
		return guiScope{}, errors.New("无权访问频道")
	}
	return guiScope{room: key}, nil
}
func guiScopeForMessage(m Message, viewer string) guiScope {
	if m.Recipient == "" {
		return guiScope{room: m.Room}
	}
	peer := m.Sender
	if peer == viewer {
		peer = m.Recipient
	}
	return guiScope{peer: peer}
}
func guiMessageForViewer(m Message, viewer string) guiMessage {
	r := guiMessageFrom(m)
	r.Room = guiScopeForMessage(m, viewer).key()
	return r
}

type guiPanelAction struct {
	Label   string `json:"label"`
	Command string `json:"command"`
	Fill    bool   `json:"fill,omitempty"`
}
type guiPanel struct {
	Moderation *guiModerationView `json:"moderation,omitempty"`
	Community  *guiCommunityView  `json:"community,omitempty"`
	Statistics *guiStatistics     `json:"statistics,omitempty"`
	ServerTime string             `json:"server_time,omitempty"`
	Kind       string             `json:"kind,omitempty"`
	Scope      string             `json:"scope,omitempty"`
	Rooms      []guiRoomCard      `json:"rooms,omitempty"`
	Members    []guiRoomMember    `json:"members,omitempty"`
	Details    *guiRoomCard       `json:"details,omitempty"`
	Title      string             `json:"title"`
	Lines      []string           `json:"lines,omitempty"`
	Actions    []guiPanelAction   `json:"actions,omitempty"`
}
type guiActionResult struct {
	configurationReceipt *guiConfigurationReceipt
	managementReceipt    *guiManagementReceipt
	kickTarget           string
	refreshState         bool
	sanctionReceipt      *guiSanctionReceipt
	enforcePolicy        bool
	reportReceipt        *guiReportReceipt
	wordReceipt          *guiWordReceipt
	guessReceipt         *guiGuessReceipt
	ticReceipt           *guiTicReceipt
	communityReceipt     *guiCommunityReceipt
	directTarget         *guiDirectTarget
	clearView            bool
	mention              string
	profile              *guiProfile
	center               *guiCenter
	preferences          *guiPreferences
	at_latest            bool
	draft                *guiDraft
	form                 string
	scope                guiScope
	replyTo              int64
	panel                *guiPanel
	messages             []Message
	message              *Message
	replace              bool
	status               string
	closeSession         bool
}

func guiMessagesPanel(title string, messages []Message) *guiPanel {
	p := &guiPanel{Title: title}
	for _, m := range messages {
		p.Actions = append(p.Actions, guiPanelAction{Label: fmt.Sprintf("#%d @%s %s", m.ID, m.Sender, cleanText(m.Body, 80)), Command: fmt.Sprintf("/jump %d", m.ID)})
	}
	return p
}

func guiExecuteAction(store *Store, hub *Hub, user User, scope guiScope, replyTo int64, request guiRequest) (guiActionResult, error) {
	r := guiActionResult{scope: scope, replyTo: replyTo, status: "已完成"}
	if _, err := guiResolveScope(store, user.Name, scope.key()); err != nil {
		return r, err
	}
	command, arg := splitArgument(strings.TrimSpace(request.Body))
	args := strings.Fields(arg)
	command = strings.ToLower(command)
	if request.SelfIPForce != nil && command != "/banip" {
		return r, errors.New("当前IP影响确认仅用于封禁IP操作")
	}
	messageID := func() (int64, error) {
		first, _ := splitArgument(arg)
		id, err := strconv.ParseInt(first, 10, 64)
		if err != nil || id <= 0 {
			return 0, errors.New("请输入有效消息编号")
		}
		return id, nil
	}
	move := func(key string) error {
		s, err := guiResolveScope(store, user.Name, key)
		if err == nil {
			r.scope = s
			r.replyTo = 0
		}
		return err
	}
	send := func(body, kind string) error {
		if utf8.RuneCountInString(body) > 800 {
			return errors.New("单条消息最多 800 个字符")
		}
		m, err := store.SendForSessionContext(request.ctx, Message{Room: scope.room, Recipient: scope.peer, Sender: user.Name, Body: body, Kind: kind, ReplyTo: r.replyTo}, request.sessionID)
		if err == nil {
			hub.Broadcast(m)
			r.message = &m
			r.replyTo = 0
		}
		return err
	}
	var err error
	switch command {
	case "/resetpass", "/invitecode":
		return r, errors.New("请使用原生一次性管理凭据确认表单；不会同步生成或重试")
	case "/backup", "/check", "/canceljob":
		return r, errors.New("请使用原生后台作业面板；不会同步执行或猜测取消目标")
	case "/post", "/draw":
		if command == "/post" {
			if _, e := parseInt64(arg); e == nil {
				r.panel, err = guiCommunityPanel(store, user, scope, request, "/object", arg)
				return r, err
			}
		}
		return r, errors.New("请在原生长文/画板编辑器中编辑；不会将整篇正文作为聊天命令提交")
	case "/config", "/maintenance":
		return guiConfigurationAction(store, user, scope, replyTo, request, command, arg)
	case "/slowmode", "/kick", "/role":
		return guiManagementAction(store, user, scope, replyTo, request, command, arg)
	case "/mute", "/unmute", "/gmute", "/ungmute", "/ban", "/unban", "/banip", "/unbanip":
		return guiSanctionAction(store, user, scope, replyTo, request, command, arg)
	case "/report", "/resolve":
		return guiReportAction(store, user, scope, replyTo, request, command, arg)
	case "/admin", "/whois", "/ip", "/ipusers", "/logins", "/bans", "/audit", "/recalls", "/reports", "/reportview", "/revisions", "/errors", "/health":
		r.panel, err = guiModerationRead(store, hub, user, scope, request, command, arg)
		if err != nil {
			err = errors.New("管理查询未完成：请核对目标、当前权限和会话后重试；未返回数据")
		}

	case "/word":
		return guiWordAction(store, hub, user, scope, replyTo, request, arg)
	case "/guess":
		return guiGuessAction(store, hub, user, scope, replyTo, request, arg)
	case "/ttt":
		return guiTicAction(store, hub, user, scope, replyTo, request, arg)
	case "/poll", "/vote", "/event", "/rsvp", "/unrsvp", "/close", "/comment":
		return guiCommunityMutationAction(store, hub, user, scope, replyTo, request, command, arg)
	case "/board", "/gallery", "/events", "/results", "/participants", "/daily", "/object":
		r.panel, err = guiCommunityPanel(store, user, scope, request, command, arg)
	case "/me", "/roll", "/coin", "/8ball", "/shrug":
		var body, kind string
		body, kind, err = guiSimpleMessage(command, arg)
		if err == nil {
			err = send(body, kind)
		}
	case "/stats", "/time", "/art":
		if len(args) != 0 {
			return r, errors.New("this command takes no arguments")
		}
		r.panel, err = guiInformationPanel(store, hub, request, command)
	case "/profile", "/badges", "/avatar", "/afk", "/bio", "/status", "/follow", "/unfollow", "/friend", "/unfriend", "/ignore", "/unignore", "/note", "/unnote":
		target, value := splitArgument(arg)
		if (command == "/profile" || command == "/badges") && len(args) > 1 {
			return r, errors.New("usage: /profile [user]")
		}
		if command == "/bio" || command == "/status" || command == "/avatar" || command == "/afk" {
			target = user.Name
			value = arg
		} else if command != "/profile" && command != "/badges" && command != "/note" && len(args) != 1 {
			return r, errors.New("usage: relationship command user")
		}
		if command != "/profile" && command != "/badges" {
			err = guiProfileMutation(store, user.Name, request.sessionID, command, target, value)
			if err == nil {
				hub.Refresh()
			}
		}
		if err == nil {
			r.profile, err = guiProfileSnapshot(store, hub, user.Name, target)
		}
	case "/mention", "/poke":
		if len(args) != 1 {
			return r, errors.New("usage: /mention or /poke user")
		}
		var target string
		if command == "/mention" {
			other, e := store.Resolve(arg)
			err = e
			target = other.Name
			if err == nil {
				r.mention = target
				r.status = "已插入提及"
			}
		} else {
			target, err = store.pokeForSession(user.Name, arg, request.sessionID)
			if err == nil {
				hub.NotifyUser(target, "poke", user.Name+" 戳了戳你")
				r.status = "你戳了戳 @" + target
			}
		}
	case "/contacts", "/friends", "/following", "/notifications", "/notifyclear", "/accept", "/decline":
		section := "contacts"
		if command == "/notifications" || command == "/notifyclear" {
			section = "notifications"
		}
		if command == "/friends" {
			section = "friends"
		}
		if command == "/following" {
			section = "following"
		}
		if command == "/accept" || command == "/decline" {
			if len(args) != 1 {
				return r, errors.New("usage: /accept or /decline user")
			}
			err = guiCenterMutation(store, user.Name, request.sessionID, command, args[0])
		} else if len(args) != 0 {
			return r, errors.New("this command takes no arguments")
		} else if command == "/notifyclear" {
			err = guiCenterMutation(store, user.Name, request.sessionID, command, "")
		}
		if err == nil {
			r.center, err = guiCenterSnapshot(store, user.Name)
			if r.center != nil {
				r.center.Section = section
			}
		}
		if err == nil && (command == "/accept" || command == "/decline" || command == "/notifyclear") {
			hub.Refresh()
		}
	case "/pref", "/dnd", "/theme":
		if command == "/pref" {
			if len(args) != 2 {
				return r, errors.New("usage: /pref setting on/off")
			}
			err = store.settingForSession(user.Name, args[0], args[1], request.sessionID)
		} else if command == "/dnd" {
			if len(args) != 1 {
				return r, errors.New("usage: /dnd on/off")
			}
			err = store.settingForSession(user.Name, "dnd", args[0], request.sessionID)
		} else {
			err = guiSetTheme(store, user.Name, arg, request.sessionID)
		}
		if err == nil {
			r.preferences, err = guiPreferencesSnapshot(store, user.Name)
			hub.PreferencesChanged(user.Name)
		}
	case "/passwd":
		r.form = "password"
		r.status = "修改账户密码"
	case "/sessions":
		target := user.Name
		if arg != "" {
			other, resolveErr := store.Resolve(arg)
			if resolveErr != nil {
				return r, resolveErr
			}
			target = other.Name
		}

		items, readErr := store.Sessions(user.Name, target)
		if readErr != nil {
			return r, readErr
		}
		r.panel = &guiPanel{Title: "会话管理"}
		for _, item := range items {
			state := "在线"
			if item.Closed > 0 {
				state = "已断开"
			}
			if item.Revoked {
				state = "已撤销"
			}
			r.panel.Lines = append(r.panel.Lines, fmt.Sprintf("%s · %s · %s · %s", item.ID, item.IP, item.Client, state), "开始 "+formatUnix(item.Started)+" · 活动 "+formatUnix(item.Seen))
			if item.Closed == 0 && !item.Revoked {
				r.panel.Actions = append(r.panel.Actions, guiPanelAction{Label: "断开 " + item.ID, Command: "/disconnect " + item.ID})
			}
		}
	case "/disconnect":
		err = store.RevokeSession(user.Name, arg)
		if err == nil {
			if arg == request.sessionID {
				r.closeSession = true
				r.status = "当前会话已退出"
			} else {
				hub.KickSession(arg)
			}
		}
	case "/logoutothers":
		valid, checkErr := sessionCurrentQ(store.readDB(), request.sessionID, user.Name)
		if checkErr != nil || !valid || request.sessionID == "" {
			return r, errors.New("当前会话无效，请重新登录")
		}
		err = store.RevokeOthers(user.Name, request.sessionID)
		if err == nil {
			hub.Enforce()
			r.status = "其他设备已退出，当前会话保留"
		}
	case "/logout", "/quit", "/exit":
		r.closeSession = true
		r.status = "当前会话已退出"
	case "/help", "/commands", "/menu":
		r.panel = &guiPanel{Kind: "menu", Scope: scope.key(), Title: "聊天功能", Lines: []string{"可点击功能，或在输入框使用与 TUI 相同的命令。尚未移植的功能会明确提示。"}}
		for _, c := range commandCatalog {
			if guiImplementedCommand(c.Name) {
				r.panel.Actions = append(r.panel.Actions, guiPanelAction{Label: c.Description + "  " + c.Name, Command: c.Name + " ", Fill: c.Args != ""})
			}
		}
	case "/dm":
		err = move("dm:" + arg)
	case "/msg":
		target, body := splitArgument(arg)
		if target == "" || strings.TrimSpace(body) == "" {
			return r, errors.New("用法：/msg 用户 消息")
		}
		if utf8.RuneCountInString(body) > 800 {
			return r, errors.New("单条消息最多 800 个字符")
		}
		// /msg is a one-off send, not a conversation switch. Resolve the
		// exact account as in TUI; the Store rechecks recipient and quote ACL.
		var other User
		other, err = store.User(target)
		if err == nil {
			var message Message
			message, err = store.SendForSessionContext(request.ctx, Message{Sender: user.Name, Recipient: other.Name, Body: body, Kind: "chat", ReplyTo: replyTo}, request.sessionID)
			if err == nil {
				hub.Broadcast(message)
				r.message = &message
				r.directTarget = &guiDirectTarget{Requested: target, Canonical: other.Name}
				r.replyTo = 0
				r.status = "已发送给 @" + other.Name
			}
		}
	case "/join":
		err = move(strings.TrimPrefix(arg, "#"))
	case "/back":
		err = move("lobby")
		if err == nil {
			err = guiProfileMutation(store, user.Name, request.sessionID, "/back", user.Name, "")
		}
		if err == nil {
			hub.Refresh()
		}
	case "/rooms", "/archives":
		r.panel, err = guiRoomDirectory(store, user.Name, command == "/archives")
	case "/who", "/recent":
		names := hub.Online()
		if command == "/recent" {
			names = store.Recent(user.Name)
		}
		r.panel = &guiPanel{Title: "联系人"}
		for _, name := range names {
			if name != user.Name {
				r.panel.Actions = append(r.panel.Actions, guiPanelAction{Label: "@" + name, Command: "/dm " + name})
			}
		}
	case "/create":
		if len(args) < 1 || len(args) > 2 || (len(args) == 2 && args[1] != "private") {
			return r, errors.New("用法：/create 频道 [private]")
		}
		err = store.CreateRoomAccess(args[0], user.Name, len(args) == 2)
		if err == nil {
			err = move(args[0])
			hub.Refresh()
		}
	case "/roominfo":
		if scope.peer != "" {
			return r, errors.New("请在频道内查看介绍")
		}
		r.panel, err = guiRoomPeople(store, user.Name, scope.room, false)
	case "/members":
		if scope.peer != "" {
			return r, errors.New("请在频道内查看成员")
		}
		r.panel, err = guiRoomPeople(store, user.Name, scope.room, true)
	case "/invite", "/remove", "/roommod", "/unmod", "/leave":
		if scope.peer != "" {
			return r, errors.New("room actions require a channel")
		}
		if command == "/leave" {
			if len(args) != 0 {
				return r, errors.New("usage: /leave")
			}
			err = store.leaveRoomForSession(user.Name, scope.room, request.sessionID)
			if err == nil {
				err = move("lobby")
				hub.Refresh()
			}
		} else {
			if len(args) != 1 {
				return r, errors.New("usage: /invite or /remove user")
			}
			var target User
			target, err = store.Resolve(args[0])
			if err == nil {
				action := strings.TrimPrefix(command, "/")
				if action == "roommod" {
					action = "mod"
				}
				err = store.roomMemberForSession(user.Name, scope.room, target.Name, action, request.sessionID)
			}
			if err == nil {
				if command == "/remove" {
					hub.RemoveRoomUser(target.Name, scope.room)
				}
				hub.Refresh()
				// A moderator may remove their own membership. Do not read the
				// old room again after that transaction revoked their access.
				if !store.CanRead(user.Name, scope.room) {
					err = move("lobby")
				} else {
					r.panel, err = guiRoomPeople(store, user.Name, scope.room, true)
				}
			}
		}
	case "/roomset", "/archive", "/unarchive", "/roompin", "/subscribe", "/roommute":
		if scope.peer != "" {
			return r, errors.New("room actions require a channel")
		}
		if command == "/roomset" {
			key, value := splitArgument(arg)
			err = store.roomSettingForSession(user.Name, scope.room, key, value, request.sessionID)
		} else if command == "/archive" || command == "/unarchive" {
			if arg != "" {
				return r, errors.New("archive takes no arguments")
			}
			value := "on"
			if command == "/unarchive" {
				value = "off"
			}
			err = store.roomSettingForSession(user.Name, scope.room, "archive", value, request.sessionID)
		} else {
			if arg != "on" && arg != "off" {
				return r, errors.New("room preference: on/off")
			}
			key := map[string]string{"/roompin": "pinned", "/subscribe": "subscribed", "/roommute": "muted"}[command]
			err = guiRoomPreference(store, user.Name, request.sessionID, scope.room, key, arg == "on")
		}
		if err == nil {
			hub.Refresh()
			hub.PreferencesChanged(user.Name)
			r.panel, err = guiRoomPeople(store, user.Name, scope.room, false)
			if err == nil {
				r.preferences, err = guiPreferencesSnapshot(store, user.Name)
			}
		}
	case "/topic":
		if scope.peer != "" {
			return r, errors.New("私信没有公共话题")
		}
		if arg == "" {
			room, readErr := store.Room(scope.room)
			if readErr != nil {
				return r, readErr
			}
			r.panel = &guiPanel{Title: "频道话题", Lines: []string{room.Topic}}
			return r, nil
		}
		err = store.Topic(user, scope.room, arg)
		if err == nil {
			hub.Refresh()
		}
	case "/clear":
		if arg != "" {
			return r, errors.New("用法：/clear")
		}
		// This is a connection-local view instruction. Never mutate history,
		// drafts, read positions, notifications, or another device.
		r.clearView = true
		r.at_latest = true
		r.status = "已清理当前屏幕；历史记录和草稿保留"
	case "/history", "/older":
		limit := 100
		if command == "/history" && arg != "" {
			limit, err = strconv.Atoi(arg)
		}
		if err != nil || limit < 1 || limit > 200 {
			return r, errors.New("历史数量范围：1–200")
		}
		before := int64(0)
		if command == "/older" {
			before = request.Before
			if before <= 0 {
				return r, errors.New("没有更早消息的游标")
			}
		}
		r.messages, err = store.Page(scope.room, user.Name, scope.peer, before, limit)
		r.replace = command == "/history"
		r.at_latest = r.replace
		r.status = fmt.Sprintf("已加载 %d 条消息", len(r.messages))
	case "/search":
		var messages []Message
		messages, err = store.SearchAll(scope.room, user.Name, scope.peer, arg)
		r.panel = guiMessagesPanel("搜索结果", messages)
	case "/jump":
		var id int64
		id, err = messageID()
		if err == nil {
			var m Message
			m, err = store.Message(user.Name, id)
			if err == nil {
				r.scope = guiScopeForMessage(m, user.Name)
				r.replyTo = 0
				r.messages, err = store.WindowFrom(r.scope.room, user.Name, r.scope.peer, id)
				if err == nil {
					latest, readErr := store.History(r.scope.room, user.Name, r.scope.peer, 1)
					err = readErr
					r.at_latest = lastMessageID(r.messages) >= lastMessageID(latest)
				}
				r.replace = true
			}
		}
	case "/reply":
		var id int64
		id, err = messageID()
		if err == nil {
			var m Message
			m, err = store.Message(user.Name, id)
			if err == nil && guiScopeForMessage(m, user.Name) != scope {
				err = errors.New("只能回复当前对话的消息")
			}
			if err == nil {
				r.replyTo = id
				_, body := splitArgument(arg)
				if body != "" {
					err = send(body, "chat")
				}
			}
		}
	case "/cancelreply":
		r.replyTo = 0
	case "/edit", "/recall":
		var id int64
		id, err = messageID()
		_, body := splitArgument(arg)
		if command == "/edit" && utf8.RuneCountInString(body) > 800 {
			return r, errors.New("单条消息最多 800 个字符")
		}
		if err == nil {
			err = store.UpdateMessageForSessionContext(request.ctx, user.Name, request.sessionID, scope.key(), id, body, command == "/recall")
		}
		if err == nil {
			hub.MessageChanged(id)
		}
	case "/react":
		if len(args) != 2 {
			return r, errors.New("用法：/react 编号 like/laugh/question/heart/wow/sad")
		}
		var id int64
		id, err = messageID()
		if err == nil {
			err = store.React(user.Name, id, args[1])
		}
		if err == nil {
			hub.MessageChanged(id)
		}
	case "/bookmark", "/unbookmark":
		var id int64
		id, err = messageID()
		if err == nil {
			err = store.Bookmark(user.Name, id, command == "/unbookmark")
		}
	case "/bookmarks":
		var messages []Message
		messages, err = store.Bookmarks(user.Name)
		r.panel = guiMessagesPanel("我的收藏", messages)
	case "/pin", "/unpin":
		if scope.peer != "" {
			return r, errors.New("私信不能频道置顶")
		}
		var id int64
		id, err = messageID()
		if err == nil {
			err = store.Pin(user, scope.room, id, command == "/unpin")
		}
	case "/pins":
		if scope.peer != "" {
			return r, errors.New("私信没有频道置顶")
		}
		var messages []Message
		messages, err = store.Pins(scope.room)
		if err == nil {
			store.decorate(messages, user.Name)
		}
		r.panel = guiMessagesPanel("频道置顶", messages)
	case "/read":
		var id int64
		id, err = messageID()
		if err == nil {
			var m Message
			m, err = store.Message(user.Name, id)
			if err == nil {
				r.panel = guiMessageContext(store, user, scope, m)
			}
		}
	default:
		result, handled, accountErr := guiAccountAction(store, hub, user, scope, command, arg, r)
		if handled {
			return result, accountErr
		}
		err = fmt.Errorf("GUI 尚未移植 %s，请查看功能对照表", command)
	}
	return r, err
}

func guiImplementedCommand(command string) bool {
	switch command {
	case "/resetpass", "/invitecode", "/backup", "/check", "/canceljob", "/post", "/draw":
		return true
	case "/config", "/maintenance", "/slowmode", "/kick", "/role":
		return true
	case "/mute", "/unmute", "/gmute", "/ungmute", "/ban", "/unban", "/banip", "/unbanip", "/report", "/resolve":
		return true
	case "/admin", "/whois", "/ip", "/ipusers", "/bans", "/audit", "/recalls", "/reports", "/reportview", "/revisions", "/errors", "/health":
		return true
	case "/guess", "/word":
		return true
	case "/ttt":
		return true
	case "/poll", "/vote", "/event", "/rsvp", "/unrsvp", "/close", "/comment":
		return true
	case "/roomset", "/archive", "/unarchive", "/roompin", "/subscribe", "/roommute":
		return true
	case "/profile", "/badges", "/avatar", "/afk", "/bio", "/status", "/follow", "/unfollow", "/friend", "/unfriend", "/ignore", "/unignore", "/note", "/unnote":
		return true
	case "/mention", "/poke":
		return true
	case "/contacts", "/friends", "/following", "/notifications", "/notifyclear", "/accept", "/decline":
		return true
	case "/pref", "/dnd", "/theme":
		return true
	case "/invite", "/remove", "/roommod", "/unmod", "/leave":
		return true
	case "/keys", "/key-add", "/key-bind", "/key-guide", "/key-rename", "/key-remove", "/key-remove-confirm", "/draft-conflicts", "/draft-restore":
		return true
	case "/passwd":
		return true
	case "/sessions", "/logins", "/disconnect", "/logoutothers", "/logout", "/quit", "/exit":
		return true
	case "/board", "/gallery", "/events", "/results", "/participants", "/daily", "/object", "/commands", "/me", "/roll", "/coin", "/8ball", "/shrug", "/stats", "/time", "/art", "/clear", "/help", "/menu", "/dm", "/msg", "/join", "/back", "/rooms", "/archives", "/who", "/recent", "/create", "/roominfo", "/members", "/topic", "/history", "/older", "/search", "/jump", "/reply", "/cancelreply", "/edit", "/recall", "/react", "/bookmark", "/unbookmark", "/bookmarks", "/pin", "/unpin", "/pins", "/read":
		return true
	}
	return false
}
