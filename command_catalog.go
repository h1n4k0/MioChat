package main

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

type commandInfo struct {
	Name, Args, Description, Group string
	Admin                          bool
}

var commandCatalog = []commandInfo{
	{"/canceljob", "", "取消当前连接的后台维护作业", "OPERATIONS", true},
	{"/draft-conflicts", "", "找回本人的冲突草稿", "ACCOUNT", false},
	{"/keys", "", "我的设备密钥", "ACCOUNT", false},
	{"/key-add", "", "确认绑定当前连接的公钥", "ACCOUNT", false},
	{"/key-guide", "", "为其它设备添加密钥、选择算法", "ACCOUNT", false},
	{"/key-remove", "密钥编号", "确认撤销自己的设备密钥", "ACCOUNT", false},
	{"/key-rename", "密钥编号 新名称", "重命名自己的设备密钥", "ACCOUNT", false},
	{"/ipusers", "IP", "同IP成功登录账户（仅线索）", "MODERATION", true},
	{"/rooms", "", "频道列表", "CHANNELS", false}, {"/join", "频道", "切换频道", "CHANNELS", false}, {"/create", "频道 [private]", "创建公开/私密频道", "CHANNELS", false}, {"/roominfo", "", "频道介绍、规则与公告", "CHANNELS", false}, {"/roomset", "设置 值", "设置 private/readonly/category/rules/announcement/slow/transfer", "CHANNELS", false}, {"/topic", "话题", "设置频道话题", "CHANNELS", false}, {"/members", "", "频道成员与版主", "CHANNELS", false}, {"/invite", "用户", "邀请加入当前频道", "CHANNELS", false}, {"/remove", "用户", "移除频道成员", "CHANNELS", false}, {"/roommod", "用户", "设置频道版主", "CHANNELS", false}, {"/unmod", "用户", "取消频道版主", "CHANNELS", false}, {"/leave", "", "离开频道", "CHANNELS", false}, {"/archive", "", "归档当前频道", "CHANNELS", false}, {"/unarchive", "", "恢复归档频道", "CHANNELS", false}, {"/archives", "", "包含归档的频道列表", "CHANNELS", false}, {"/roompin", "on/off", "个人频道置顶", "CHANNELS", false}, {"/subscribe", "on/off", "订阅频道未读", "CHANNELS", false}, {"/roommute", "on/off", "频道通知静音", "CHANNELS", false},
	{"/dm", "用户", "打开私聊，支持离线消息", "MESSAGES", false}, {"/msg", "用户 内容", "发送私信", "MESSAGES", false}, {"/back", "", "返回大厅并清除离开状态", "MESSAGES", false}, {"/reply", "编号 [内容]", "选中或发送指定回复", "MESSAGES", false}, {"/cancelreply", "", "取消当前回复", "MESSAGES", false}, {"/mention", "用户", "把 @用户 插入输入框", "MESSAGES", false}, {"/recall", "编号", "撤回自己的消息（5分钟）", "MESSAGES", false}, {"/edit", "编号 内容", "编辑自己的消息（5分钟）", "MESSAGES", false}, {"/react", "编号 like/laugh/question/heart/wow/sad", "切换消息反应", "MESSAGES", false}, {"/history", "[数量]", "最近记录（最大200）", "HISTORY", false}, {"/older", "", "加载更早记录", "HISTORY", false}, {"/search", "关键词 [from:用户] [after:日期] [before:日期]", "全文筛选当前会话", "HISTORY", false}, {"/jump", "编号", "跳转消息所在会话", "HISTORY", false}, {"/read", "编号", "展开阅读消息", "HISTORY", false}, {"/bookmark", "编号", "收藏消息", "HISTORY", false}, {"/unbookmark", "编号", "取消收藏", "HISTORY", false}, {"/bookmarks", "", "查看收藏", "HISTORY", false}, {"/pin", "编号", "频道置顶（管理者）", "HISTORY", false}, {"/unpin", "编号", "取消置顶", "HISTORY", false}, {"/pins", "", "频道置顶列表", "HISTORY", false}, {"/clear", "", "仅清理自己的屏幕", "HISTORY", false},
	{"/notifications", "", "提及、回复、私信、戳与好友通知", "SOCIAL", false}, {"/notifyclear", "", "全部通知标为已读（不删除）", "SOCIAL", false}, {"/poke", "用户", "戳一戳，30秒冷却", "SOCIAL", false}, {"/profile", "[用户]", "用户卡片", "SOCIAL", false}, {"/who", "", "在线用户卡片列表", "SOCIAL", false}, {"/contacts", "", "好友、关注、申请与最近会话", "SOCIAL", false}, {"/recent", "", "最近私聊", "SOCIAL", false}, {"/follow", "用户", "关注", "SOCIAL", false}, {"/unfollow", "用户", "取消关注", "SOCIAL", false}, {"/following", "", "关注列表", "SOCIAL", false}, {"/friend", "用户", "申请好友", "SOCIAL", false}, {"/accept", "用户", "接受好友申请", "SOCIAL", false}, {"/decline", "用户", "拒绝好友申请", "SOCIAL", false}, {"/unfriend", "用户", "解除双向好友关系", "SOCIAL", false}, {"/friends", "", "好友列表", "SOCIAL", false}, {"/note", "用户 备注", "设置只对自己可见的备注", "SOCIAL", false}, {"/unnote", "用户", "删除私有备注", "SOCIAL", false}, {"/ignore", "用户", "持久屏蔽", "SOCIAL", false}, {"/unignore", "用户", "取消屏蔽", "SOCIAL", false},
	{"/bio", "简介", "修改简介", "ACCOUNT", false}, {"/status", "状态", "修改状态文字", "ACCOUNT", false}, {"/afk", "[原因]", "暂时离开", "ACCOUNT", false}, {"/theme", "aurora/sakura/amber/mono/light/contrast", "配色主题", "ACCOUNT", false}, {"/pref", "设置 on/off", "poke/motion/bell/dnd/compact/ascii/welcome/contrast", "ACCOUNT", false}, {"/dnd", "on/off", "勿扰模式", "ACCOUNT", false}, {"/avatar", "camp/cat/robot/planet/flower/ident", "ASCII头像", "ACCOUNT", false}, {"/badges", "[用户]", "活动成就", "ACCOUNT", false}, {"/passwd", "", "隐藏输入的改密表单", "ACCOUNT", false}, {"/logins", "[用户]", "自己的登录历史；管理员可查他人", "ACCOUNT", false}, {"/sessions", "[用户]", "会话管理", "ACCOUNT", false}, {"/disconnect", "会话ID", "断开有权管理的一个会话", "ACCOUNT", false}, {"/logoutothers", "", "退出自己的其他设备", "ACCOUNT", false}, {"/logout", "", "退出当前连接", "ACCOUNT", false},
	{"/poll", "问题|选项1|选项2", "创建频道投票", "COMMUNITY", false}, {"/vote", "编号 选项", "投票或修改选择", "COMMUNITY", false}, {"/results", "编号", "投票结果", "COMMUNITY", false}, {"/board", "", "长帖留言板", "COMMUNITY", false}, {"/post", "标题 或 编号", "编写长帖或阅读旧帖", "COMMUNITY", false}, {"/comment", "编号 内容", "评论社区条目", "COMMUNITY", false}, {"/draw", "标题", "ASCII画板（F2预览/F4发布）", "COMMUNITY", false}, {"/gallery", "", "ASCII作品展示", "COMMUNITY", false}, {"/event", "标题|YYYY-MM-DD HH:MM|介绍", "创建活动（北京时间）", "COMMUNITY", false}, {"/events", "", "频道活动", "COMMUNITY", false}, {"/rsvp", "编号", "报名活动", "COMMUNITY", false}, {"/unrsvp", "编号", "取消报名", "COMMUNITY", false}, {"/participants", "编号", "参与名单", "COMMUNITY", false}, {"/close", "编号", "结束自己的投票/活动/对局", "COMMUNITY", false}, {"/daily", "", "每日讨论话题", "COMMUNITY", false},
	{"/ttt", "用户 或 accept 编号 或 编号 格子", "邀请/接受/进行井字棋", "PLAY", false}, {"/guess", "start [上限] 或 数字", "猜数字", "PLAY", false}, {"/word", "start 答案 或 猜测", "猜词", "PLAY", false}, {"/me", "动作", "动作消息", "PLAY", false}, {"/roll", "[面数]", "掷骰子", "PLAY", false}, {"/coin", "", "抛硬币", "PLAY", false}, {"/8ball", "问题", "神奇八号球", "PLAY", false}, {"/shrug", "", "耸肩", "PLAY", false}, {"/art", "", "ASCII营地", "PLAY", false},
	{"/report", "消息编号 原因", "向管理员举报该条内容", "MODERATION", false}, {"/mute", "用户 期限 [原因]", "当前频道禁言", "MODERATION", false}, {"/unmute", "用户", "解除频道禁言", "MODERATION", false}, {"/slowmode", "秒数", "频道慢速，0关闭", "MODERATION", false}, {"/admin", "", "管理控制台", "MODERATION", true}, {"/whois", "用户/UID", "账户详情", "MODERATION", true}, {"/ip", "用户", "近期登录IP", "MODERATION", true}, {"/kick", "用户", "断开账户连接", "MODERATION", true}, {"/ban", "用户 [期限] [原因]", "封号，默认永久", "MODERATION", true}, {"/unban", "用户", "解封账户", "MODERATION", true}, {"/banip", "IP/CIDR 期限 [原因]", "仅聊天室封IP", "MODERATION", true}, {"/unbanip", "IP/CIDR", "解封IP", "MODERATION", true}, {"/bans", "", "封禁与禁言列表", "MODERATION", true}, {"/gmute", "用户 期限 [原因]", "全站禁言", "MODERATION", true}, {"/ungmute", "用户", "解除全站禁言", "MODERATION", true}, {"/audit", "", "管理员操作日志", "MODERATION", true}, {"/recalls", "", "公开频道撤回目录", "MODERATION", true}, {"/revisions", "消息编号", "授权查看修订/撤回原文", "MODERATION", false}, {"/reports", "", "举报队列", "MODERATION", true}, {"/reportview", "举报编号", "查看主动举报的一条消息", "MODERATION", true}, {"/resolve", "举报编号 resolved/rejected", "处理举报", "MODERATION", true}, {"/role", "用户 admin/moderator/member", "站长分配角色", "MODERATION", true}, {"/resetpass", "用户", "临时密码重置并强制改密", "MODERATION", true},
	{"/config", "设置 值", "注册/配额/维护/永久保留配置", "OPERATIONS", true}, {"/invitecode", "[期限] [次数]", "生成注册邀请码", "OPERATIONS", true}, {"/maintenance", "on/off [公告]", "维护模式", "OPERATIONS", true}, {"/backup", "", "创建一致性备份", "OPERATIONS", true}, {"/check", "", "数据库与配置校验", "OPERATIONS", true}, {"/errors", "", "异常与限流记录", "OPERATIONS", true}, {"/health", "", "运行资源看板", "OPERATIONS", true}, {"/stats", "", "公开运行统计", "INFO", false}, {"/time", "", "北京时间", "INFO", false}, {"/help", "", "可搜索、可点击的命令指南", "INFO", false}, {"/menu", "", "功能菜单", "INFO", false}, {"/quit", "", "退出聊天室", "INFO", false},
}

func decodeJSON(text string, destination any) { _ = json.Unmarshal([]byte(text), destination) }
func splitArgument(arg string) (string, string) {
	p := strings.SplitN(strings.TrimSpace(arg), " ", 2)
	if len(p) == 1 {
		return p[0], ""
	}
	return p[0], strings.TrimSpace(p[1])
}
func (m *model) openMenu(adminOnly bool) {
	var actions []panelAction
	for _, c := range commandCatalog {
		if c.Admin && !isAdmin(m.user.Role) {
			continue
		}
		if adminOnly && !c.Admin {
			continue
		}
		actions = append(actions, panelAction{c.Group + "  " + c.Name + " — " + c.Description, c.Name + " ", true})
	}
	title := "MIOCHAT / CHANNELS · COMMAND GUIDE"
	if adminOnly {
		title = "管理员控制台 / MODERATION"
	}
	m.openPanel(title, "menu", []string{"输入关键词筛选，点击或 Enter 将命令放入输入框。", "鼠标：右键用户名 @ · 右键消息回复 · 双击用户名戳。", ""}, actions)
}
func (m *model) namesPanel(title string, names []string) {
	var actions []panelAction
	notes := m.store.Relations(m.user.Name, "note")
	for _, n := range names {
		label := n
		if notes[n] != "" {
			label += "（" + notes[n] + "）"
		}
		actions = append(actions, panelAction{label, "/profile " + n, false})
	}
	m.openPanel(title, "names", nil, actions)
}
func (m *model) objectList(kind string) {
	objects, e := m.store.Objects(m.user.Name, m.room, kind)
	if e != nil {
		m.note(e.Error())
		return
	}
	var actions []panelAction
	for _, o := range objects {
		state := ""
		if o.Closed {
			state = " [已结束]"
		}
		actions = append(actions, panelAction{fmt.Sprintf("#%d [%s] %s%s", o.ID, o.Kind, o.Title, state), "/object " + strconv.FormatInt(o.ID, 10), false})
	}
	m.openPanel("社区 / "+kind, "objects", nil, actions)
}
func (m *model) messageList(title string, messages []Message) {
	var actions []panelAction
	for _, v := range messages {
		actions = append(actions, panelAction{fmt.Sprintf("#%d @%s %s", v.ID, v.Sender, cleanText(v.Body, 70)), "/jump " + strconv.FormatInt(v.ID, 10), false})
	}
	m.openPanel(title, "messages", nil, actions)
}
func (m *model) jumpMessage(id int64) error {
	v, e := m.store.Message(m.user.Name, id)
	if e != nil {
		return e
	}
	peer := ""
	room := v.Room
	if v.Recipient != "" {
		peer = v.Sender
		if peer == m.user.Name {
			peer = v.Recipient
		}
	}
	if e = m.move(room, peer); e != nil {
		return e
	}
	window, e := m.store.WindowFrom(room, m.user.Name, peer, id)
	if e != nil {
		return e
	}
	m.feed = window
	m.rebuild(false)
	m.view.GotoTop()
	m.flash = fmt.Sprintf("定位消息 #%d", id)
	m.flashUntil = time.Now().Add(4 * time.Second)
	return nil
}
func (m *model) extendedCommand(line string) (bool, tea.Cmd) {
	if !strings.HasPrefix(line, "/") {
		return false, nil
	}
	command, arg := splitArgument(line)
	command = strings.ToLower(command)
	if handled, cmd := m.positionCommand(command, arg); handled {
		return true, cmd
	}
	if handled, cmd := m.sshKeyCommand(command, arg); handled {
		return true, cmd
	}
	args := strings.Fields(arg)
	var err error
	switch command {
	case "/help", "/commands", "/menu":
		m.openMenu(false)
	case "/admin":
		if u, e := m.store.User(m.user.Name); e != nil || !isAdmin(u.Role) {
			err = fmt.Errorf("需要管理员权限")
		} else {
			m.user = u
			m.openMenu(true)
		}
	case "/mention":
		u, e := m.store.Resolve(arg)
		err = e
		if err == nil {
			m.insertMention(u.Name)
		}
	case "/poke":
		m.poke(arg)
	case "/profile":
		if arg == "" {
			arg = m.user.Name
		}
		m.userCard(arg)
	case "/who":
		m.namesPanel("在线用户", m.hub.Online())
	case "/rooms", "/archives":
		rooms, e := m.store.VisibleRooms(m.user.Name, command == "/archives")
		err = e
		if e == nil {
			var actions []panelAction
			for _, r := range rooms {
				info, _ := m.store.RoomInfo(r.Name)
				label := "#" + r.Name
				if info.Private {
					label += " [私密]"
				}
				if info.Archived {
					label += " [归档]"
				}
				if info.Category != "" {
					label += " / " + info.Category
				}
				label += fmt.Sprintf(" · %d 未读", m.store.UnreadIn(m.user.Name, r.Name, ""))
				actions = append(actions, panelAction{label, "/join " + r.Name, false})
			}
			m.openPanel("频道目录", "rooms", nil, actions)
		}
	case "/create":
		if len(args) < 1 || len(args) > 2 {
			err = fmt.Errorf("用法：/create 频道 [private]")
			break
		}
		private := len(args) == 2 && args[1] == "private"
		if len(args) == 2 && !private {
			err = fmt.Errorf("可选参数为 private")
			break
		}
		err = m.store.CreateRoomAccess(args[0], m.user.Name, private)
		if err == nil {
			err = m.move(args[0], "")
		}
		m.hub.Refresh()
	case "/roominfo":
		if m.peer != "" {
			err = fmt.Errorf("请在频道内查看介绍")
			break
		}
		r, e := m.store.RoomInfo(m.room)
		err = e
		if e == nil {
			m.openPanel("频道 #"+m.room, "roominfo", []string{"话题：" + r.Topic, "创建者：" + r.Owner, fmt.Sprintf("私密=%v 只读=%v 归档=%v 慢速=%ds", r.Private, r.ReadOnly, r.Archived, r.Slow), "分类：" + r.Category, "规则：" + r.Rules, "公告：" + r.Announcement}, []panelAction{{"成员列表", "/members", false}, {"订阅频道", "/subscribe on", false}, {"频道置顶", "/roompin on", false}, {"静音频道", "/roommute on", false}})
		}
	case "/roomset":
		key, value := splitArgument(arg)
		err = m.store.RoomSetting(m.user.Name, m.room, key, value)
		if err == nil {
			m.hub.Refresh()
			m.note("频道设置已更新")
		}
	case "/members":
		names, e := m.store.RoomMembers(m.user.Name, m.room)
		err = e
		if e == nil {
			m.openPanel("频道成员", "members", names, nil)
		}
	case "/invite", "/remove", "/roommod", "/unmod":
		action := strings.TrimPrefix(command, "/")
		if action == "roommod" {
			action = "mod"
		}
		err = m.store.RoomMember(m.user.Name, m.room, arg, action)
		if err == nil {
			if action == "remove" {
				m.hub.RemoveRoomUser(accountKey(arg), m.room)
			}
			m.hub.Refresh()
			m.note("成员设置已更新")
		}
	case "/leave":
		old := m.room
		err = m.store.LeaveRoom(m.user.Name, old)
		if err == nil {
			err = m.move("lobby", "")
			m.hub.Refresh()
		}
	case "/archive", "/unarchive":
		value := "on"
		if command == "/unarchive" {
			value = "off"
		}
		err = m.store.RoomSetting(m.user.Name, m.room, "archive", value)
		if err == nil {
			m.hub.Refresh()
			m.note("归档状态已更新")
		}
	case "/roompin", "/subscribe", "/roommute":
		if arg != "on" && arg != "off" {
			err = fmt.Errorf("用法：%s on/off", command)
			break
		}
		key := map[string]string{"/roompin": "pinned", "/subscribe": "subscribed", "/roommute": "muted"}[command]
		err = m.store.Preference(m.user.Name, "room:"+m.room, key, arg == "on")
		if err == nil {
			m.refresh()
			m.note("频道偏好已保存")
		}
	case "/reply":
		idText, text := splitArgument(arg)
		id, e := parseInt64(idText)
		err = e
		if err == nil {
			err = m.selectReply(id)
		}
		if err == nil && text != "" {
			err = m.send(text, "chat", m.peer)
		}
	case "/cancelreply":
		m.replyID = 0
		m.flash = ""
	case "/recall", "/edit":
		idText, body := splitArgument(arg)
		id, e := parseInt64(idText)
		err = e
		if err == nil {
			err = m.store.UpdateMessage(m.user.Name, id, body, command == "/recall")
		}
		if err == nil {
			m.hub.MessageChanged(id)
			m.refreshMessage(id)
		}
	case "/react":
		if len(args) != 2 {
			err = fmt.Errorf("用法：/react 编号 like/laugh/question/heart/wow/sad")
			break
		}
		id, e := parseInt64(args[0])
		err = e
		if err == nil {
			err = m.store.React(m.user.Name, id, args[1])
		}
		if err == nil {
			m.hub.MessageChanged(id)
			m.refreshMessage(id)
		}
	case "/bookmark", "/unbookmark":
		id, e := parseInt64(arg)
		err = e
		if err == nil {
			err = m.store.Bookmark(m.user.Name, id, command == "/unbookmark")
		}
		if err == nil {
			m.note("收藏已更新")
		}
	case "/bookmarks":
		messages, e := m.store.Bookmarks(m.user.Name)
		err = e
		if e == nil {
			m.messageList("我的收藏", messages)
		}
	case "/older":
		m.loadOlder()
	case "/search":
		messages, e := m.store.SearchAll(m.room, m.user.Name, m.peer, arg)
		err = e
		if e == nil {
			m.messageList(fmt.Sprintf("搜索结果（%d，最多显示200条）", len(messages)), messages)
		}
	case "/pins":
		if !m.store.CanRead(m.user.Name, m.room) {
			err = fmt.Errorf("无权查看频道置顶")
			break
		}
		messages, e := m.store.Pins(m.room)
		err = e
		if e == nil && m.store.CanRead(m.user.Name, m.room) {
			m.messageList("频道置顶", messages)
		}
	case "/jump":
		id, e := parseInt64(arg)
		err = e
		if e == nil {
			err = m.jumpMessage(id)
		}
	case "/read":
		id, e := parseInt64(arg)
		err = e
		if e == nil {
			v, e := m.store.Message(m.user.Name, id)
			err = e
			if e == nil {
				m.openPanel(fmt.Sprintf("消息 #%d", id), "read", []string{"@" + v.Sender + " · " + formatUnix(v.At), v.Body}, []panelAction{{"回复", fmt.Sprintf("/reply %d", id), false}, {"收藏", fmt.Sprintf("/bookmark %d", id), false}})
				m.panelMessageID = id
			}
		}
	case "/notifications":
		items, e := m.store.Notifications(m.user.Name, false)
		err = e
		if e == nil {
			var actions []panelAction
			for _, v := range items {
				label := fmt.Sprintf("[%s] @%s %s", v.Kind, v.Actor, v.Body)
				cmd := "/profile " + v.Actor
				if v.Ref > 0 {
					cmd = fmt.Sprintf("/jump %d", v.Ref)
				}
				actions = append(actions, panelAction{label, cmd, false})
			}
			actions = append(actions, panelAction{"全部标为已读", "/notifyclear", false})
			m.openPanel("通知中心", "notifications", nil, actions)
		}
	case "/notifyclear":
		_, err = m.store.Notifications(m.user.Name, true)
		m.refresh()
		m.note("通知已标记为已读，记录仍保留")
	case "/follow", "/unfollow", "/friend", "/ignore", "/unignore", "/note", "/unnote":
		target, value := splitArgument(arg)
		kind := "follow"
		remove := false
		switch command {
		case "/unfollow":
			remove = true
		case "/friend":
			kind = "request"
		case "/ignore":
			kind = "block"
		case "/unignore":
			kind = "block"
			remove = true
		case "/note":
			kind = "note"
		case "/unnote":
			kind = "note"
			remove = true
		}
		err = m.store.Relation(m.user.Name, target, kind, value, remove)
		if err == nil {
			m.ignored = map[string]bool{}
			for name := range m.store.Relations(m.user.Name, "block") {
				m.ignored[name] = true
			}
			m.hub.Refresh()
			m.note("账户关系已保存")
		}
	case "/accept", "/decline":
		err = m.store.AcceptFriend(m.user.Name, arg, command == "/accept")
		if err == nil {
			m.note("好友申请已处理")
		}
	case "/unfriend":
		err = m.store.Unfriend(m.user.Name, arg)
		if err == nil {
			m.note("好友关系已解除")
		}
	case "/friends", "/following":
		kind := "friend"
		if command == "/following" {
			kind = "follow"
		}
		m.namesPanel("联系人", namesSorted(m.store.Relations(m.user.Name, kind)))
	case "/recent":
		m.namesPanel("最近私聊", m.store.Recent(m.user.Name))
	case "/contacts":
		var actions []panelAction
		for _, name := range m.store.PendingFriends(m.user.Name) {
			actions = append(actions, panelAction{"接受 @" + name + " 的好友申请", "/accept " + name, false}, panelAction{"拒绝 @" + name + " 的好友申请", "/decline " + name, false})
		}
		for _, name := range namesSorted(m.store.Relations(m.user.Name, "friend")) {
			actions = append(actions, panelAction{"好友 @" + name, "/profile " + name, false})
		}
		for _, name := range m.store.Recent(m.user.Name) {
			actions = append(actions, panelAction{"最近 @" + name, "/dm " + name, false})
		}
		for _, name := range namesSorted(m.store.Relations(m.user.Name, "follow")) {
			actions = append(actions, panelAction{"关注 @" + name, "/profile " + name, false})
		}
		m.openPanel("联系人 / 好友申请 / 最近会话", "contacts", nil, actions)
	case "/pref":
		if len(args) != 2 {
			err = fmt.Errorf("用法：/pref poke/motion/bell/dnd/compact/ascii/welcome/contrast/key_reminder on/off")
			break
		}
		err = m.store.Setting(m.user.Name, args[0], args[1])
		if err == nil {
			m.settings = m.store.Settings(m.user.Name)
			m.rebuild(false)
			m.note("偏好已保存")
		}
	case "/dnd":
		err = m.store.Setting(m.user.Name, "dnd", arg)
		if err == nil {
			m.settings = m.store.Settings(m.user.Name)
			m.note("勿扰设置已保存")
		}
	case "/avatar":
		err = m.store.Avatar(m.user.Name, arg)
		if err == nil {
			m.userCard(m.user.Name)
		}
	case "/badges":
		if arg == "" {
			arg = m.user.Name
		}
		u, e := m.store.Resolve(arg)
		err = e
		if e == nil {
			m.openPanel("@"+u.Name+" 的成就", "badges", m.store.BadgeNames(u.Name), nil)
		}
	case "/theme":
		switch arg {
		case "aurora", "sakura", "amber", "mono", "light", "contrast":
			err = m.store.Profile(m.user.Name, "theme", arg)
			if err == nil {
				m.user.Theme = arg
				m.rebuild(false)
			}
		default:
			err = fmt.Errorf("主题：aurora sakura amber mono light contrast")
		}
	case "/whois":
		u, e := m.store.User(m.user.Name)
		if e != nil || !isAdmin(u.Role) {
			err = fmt.Errorf("需要管理员权限")
		} else {
			m.userCard(arg)
		}
	case "/ip", "/logins":
		target := m.user.Name
		if arg != "" {
			u, e := m.store.Resolve(arg)
			err = e
			if e == nil {
				target = u.Name
			}
		}
		if err == nil {
			items, e := m.store.LoginHistory(m.user.Name, target)
			err = e
			if e == nil {
				var lines []string
				for _, v := range items {
					lines = append(lines, fmt.Sprintf("%s | %s | ok=%v | %s", formatUnix(v.At), v.IP, v.OK, v.Client))
				}
				m.openPanel("@"+target+" 登录记录（仅有权者可见）", "logins", lines, nil)
			}
		}
	case "/ipusers":
		names, e := m.store.IPUsers(m.user.Name, arg)
		err = e
		if e == nil {
			m.namesPanel("同IP账户（共享网络，仅作线索）", names)
		}
	case "/sessions":
		target := m.user.Name
		if arg != "" {
			u, e := m.store.Resolve(arg)
			err = e
			if e == nil {
				target = u.Name
			}
		}
		if err == nil {
			items, e := m.store.Sessions(m.user.Name, target)
			err = e
			if e == nil {
				var lines []string
				var actions []panelAction
				for _, v := range items {
					state := "在线"
					if v.Closed > 0 {
						state = "已断开"
					}
					if v.Revoked {
						state = "已撤销"
					}
					lines = append(lines, fmt.Sprintf("%s | %s | %s | %s", v.ID, v.IP, v.Client, state), "开始 "+formatUnix(v.Started)+" · 活动 "+formatUnix(v.Seen))
					if v.Closed == 0 && !v.Revoked {
						actions = append(actions, panelAction{"断开 " + v.ID, "/disconnect " + v.ID, false})
					}
				}
				m.openPanel("会话管理", "sessions", lines, actions)
			}
		}
	case "/disconnect":
		err = m.store.RevokeSession(m.user.Name, arg)
		if err == nil {
			m.hub.KickSession(arg)
			m.note("指定会话已断开")
		}
	case "/logoutothers":
		err = m.store.RevokeOthers(m.user.Name, m.client.ID)
		if err == nil {
			m.hub.Enforce()
			m.note("其他设备已退出")
		}
	case "/kick":
		u, e := m.store.User(m.user.Name)
		other, oe := m.store.Resolve(arg)
		if e != nil || oe != nil || !isAdmin(u.Role) || roleRank(other.Role) >= roleRank(u.Role) {
			err = fmt.Errorf("不能踢出此账户")
		} else {
			m.hub.Kick(other.Name)
			err = auditQ(m.store.db, m.user.Name, "user.kick", other.Name, "", "")
		}
	case "/ban", "/unban", "/mute", "/unmute", "/gmute", "/ungmute", "/banip", "/unbanip":
		if len(args) == 0 {
			err = fmt.Errorf("请提供用户或IP")
			break
		}
		kind := "user"
		room := ""
		if strings.Contains(command, "ip") {
			kind = "ip"
		}
		if strings.Contains(command, "mute") {
			kind = "mute"
			if command == "/mute" || command == "/unmute" {
				room = m.room
				if m.peer != "" {
					err = fmt.Errorf("频道禁言请在频道内执行")
					break
				}
			}
		}
		remove := strings.HasPrefix(command, "/un")
		duration := "permanent"
		reason := "管理员操作"
		if !remove && len(args) > 1 {
			duration = args[1]
		}
		if len(args) > 2 {
			reason = strings.Join(args[2:], " ")
		}
		if kind == "ip" && !remove {
			prefix, e := normalizePrefix(args[0])
			if e != nil {
				err = e
				break
			}
			ip, e := netip.ParseAddr(m.ip)
			if e == nil && prefix.Contains(ip.Unmap()) && !strings.Contains(reason, "--force") {
				err = fmt.Errorf("将影响你当前的聊天室连接；确认时在原因末尾加 --force，管理SSH不受影响")
				break
			}
		}
		err = m.store.Sanction(m.user.Name, kind, args[0], room, duration, reason, remove)
		if err == nil {
			m.hub.Enforce()
			m.note("管理状态已更新，已写入审计")
		}
	case "/slowmode":
		err = m.store.RoomSetting(m.user.Name, m.room, "slow", arg)
		if err == nil {
			m.note("慢速模式已更新")
		}
	case "/bans", "/audit", "/recalls", "/reports", "/errors":
		kind := strings.TrimPrefix(command, "/")
		lines, e := m.store.AdminList(m.user.Name, kind)
		err = e
		if e == nil {
			var actions []panelAction
			if command == "/recalls" {
				for _, line := range lines {
					if id, e := parseInt64(strings.TrimSpace(strings.SplitN(line, "|", 2)[0])); e == nil {
						actions = append(actions, panelAction{line, fmt.Sprintf("/revisions %d", id), false})
					}
				}
			} else if command == "/reports" {
				for _, line := range lines {
					if id, e := parseInt64(strings.TrimSpace(strings.SplitN(line, "|", 2)[0])); e == nil {
						actions = append(actions, panelAction{line, fmt.Sprintf("/reportview %d", id), false})
					}
				}
			}
			if len(actions) > 0 {
				lines = nil
			}
			m.openPanel("管理 / "+kind, "admin-list", lines, actions)
		}
	case "/revisions":
		id, e := parseInt64(arg)
		err = e
		if e == nil {
			revisions, e := m.store.Revisions(m.user.Name, id)
			err = e
			if e == nil {
				var lines []string
				for _, v := range revisions {
					lines = append(lines, formatUnix(v.At)+" @"+v.Actor+" "+v.Action, v.Body)
				}
				m.openPanel("消息修订与撤回原文", "revisions", lines, nil)
			}
		}
	case "/report":
		idText, reason := splitArgument(arg)
		id, e := parseInt64(idText)
		err = e
		if e == nil {
			err = m.store.Report(m.user.Name, id, reason)
		}
		if err == nil {
			m.note("举报已提交；授权管理员可查看你主动举报的这一条消息")
		}
	case "/reportview":
		id, e := parseInt64(arg)
		err = e
		if e == nil {
			v, e := m.store.ReportMessage(m.user.Name, id)
			err = e
			if e == nil {
				m.openPanel("举报内容（访问已审计）", "report", []string{fmt.Sprintf("消息 #%d @%s", v.ID, v.Sender), v.Body}, []panelAction{{"处理完成", fmt.Sprintf("/resolve %d resolved", id), false}, {"驳回", fmt.Sprintf("/resolve %d rejected", id), false}})
			}
		}
	case "/resolve":
		if len(args) != 2 {
			err = fmt.Errorf("用法：/resolve 举报编号 resolved/rejected")
			break
		}
		id, e := parseInt64(args[0])
		err = e
		if e == nil {
			err = m.store.ResolveReport(m.user.Name, id, args[1])
		}
		if err == nil {
			m.note("举报已处理")
		}
	case "/role":
		if len(args) != 2 {
			err = fmt.Errorf("用法：/role 用户 admin/moderator/member")
			break
		}
		err = m.store.SetRole(m.user.Name, args[0], args[1])
		if err == nil {
			m.hub.Refresh()
			m.note("角色已更新")
		}
	case "/resetpass":
		password, e := m.store.ResetPassword(m.user.Name, arg)
		err = e
		if e == nil {
			m.hub.Enforce()
			m.openPanel("临时密码（仅此次显示）", "secret", []string{password, "请安全交付给该用户；下次登录必须改密。", "原有设备密钥已撤销，用户改密后可重新绑定。", "原密码不可查看。临时密码不会写入管理日志。"}, nil)
		}
	case "/config":
		key, value := splitArgument(arg)
		if key == "" {
			err = fmt.Errorf("配置：registration / registration_quota / maintenance / maintenance_notice / retention")
			break
		}
		err = m.store.SetConfig(m.user.Name, key, value)
		if err == nil {
			m.hub.Refresh()
			m.note("配置已保存")
		}
	case "/maintenance":
		state, notice := splitArgument(arg)
		err = m.store.SetConfig(m.user.Name, "maintenance", state)
		if err == nil && notice != "" {
			err = m.store.SetConfig(m.user.Name, "maintenance_notice", notice)
		}
		if err == nil {
			m.hub.Refresh()
			m.note("维护状态已更新")
		}
	case "/invitecode":
		duration := "1d"
		uses := 1
		if len(args) > 0 {
			duration = args[0]
		}
		if len(args) > 1 {
			uses, err = strconv.Atoi(args[1])
		}
		if err == nil {
			code, e := m.store.InviteCode(m.user.Name, duration, uses)
			err = e
			if e == nil {
				m.openPanel("注册邀请码（仅此次显示）", "secret", []string{code, fmt.Sprintf("期限 %s · 使用次数 %d", duration, uses)}, nil)
			}
		}
	case "/backup":
		return true, m.startMaintenance("backup")
	case "/check":
		return true, m.startMaintenance("check")
	case "/canceljob":
		if m.maintenanceCancel != nil {
			m.maintenanceCancel()
			m.note("正在取消后台作业…")
		} else {
			m.note("当前连接没有后台维护作业")
		}
	case "/health", "/stats":
		if command == "/health" {
			u, e := m.store.User(m.user.Name)
			if e != nil || !isAdmin(u.Role) {
				err = fmt.Errorf("需要管理员权限")
				break
			}
		}
		lines := m.store.Metrics()
		lines = append(lines, m.hub.Stats())
		m.openPanel("运行看板", "stats", lines, nil)
	case "/post":
		if id, e := parseInt64(arg); e == nil {
			m.openObject(id)
		} else {
			m.startCompose("post", arg)
		}
	case "/draw":
		m.startCompose("art", arg)
	case "/board":
		m.objectList("post")
	case "/gallery":
		m.objectList("art")
	case "/events":
		m.objectList("event")
	case "/object", "/results":
		id, e := parseInt64(arg)
		err = e
		if e == nil {
			m.openObject(id)
		}
	case "/comment":
		idText, text := splitArgument(arg)
		id, e := parseInt64(idText)
		err = e
		if e == nil {
			err = m.store.Comment(m.user.Name, id, text)
		}
		if err == nil {
			m.openObject(id)
		}
	case "/poll":
		id, e := m.store.Poll(m.user.Name, m.room, arg)
		err = e
		if e == nil {
			m.announce(fmt.Sprintf("创建了投票 #%d", id), m.room)
			m.openObject(id)
		}
	case "/vote":
		if len(args) != 2 {
			err = fmt.Errorf("用法：/vote 投票编号 选项编号")
			break
		}
		id, e := parseInt64(args[0])
		err = e
		choice, ce := strconv.Atoi(args[1])
		if ce != nil {
			err = ce
		}
		if err == nil {
			err = m.store.Vote(m.user.Name, id, choice)
		}
		if err == nil {
			m.openObject(id)
		}
	case "/event":
		id, e := m.store.NewEvent(m.user.Name, m.room, arg)
		err = e
		if e == nil {
			m.announce(fmt.Sprintf("创建了活动 #%d", id), m.room)
			m.openObject(id)
		}
	case "/rsvp", "/unrsvp":
		id, e := parseInt64(arg)
		err = e
		if e == nil {
			err = m.store.RSVP(m.user.Name, id, command == "/rsvp")
		}
		if err == nil {
			m.openObject(id)
		}
	case "/participants":
		id, e := parseInt64(arg)
		err = e
		if e == nil {
			names, e := m.store.Participants(m.user.Name, id)
			err = e
			if e == nil {
				m.namesPanel("参与名单", names)
			}
		}
	case "/close":
		id, e := parseInt64(arg)
		err = e
		if e == nil {
			err = m.store.CloseObject(m.user.Name, id)
		}
		if err == nil {
			m.openObject(id)
		}
	case "/ttt":
		if len(args) == 1 {
			id, e := m.store.Challenge(m.user.Name, args[0], m.room)
			err = e
			if e == nil {
				m.announce(fmt.Sprintf("发起井字棋挑战 #%d", id), m.room)
				m.openObject(id)
			}
		} else if len(args) == 2 {
			accept := args[0] == "accept"
			idText := args[0]
			if accept {
				idText = args[1]
			}
			id, e := parseInt64(idText)
			err = e
			cell := 0
			if !accept {
				cell, e = strconv.Atoi(args[1])
				if e != nil {
					err = e
				}
			}
			if err == nil {
				_, err = m.store.TicMove(m.user.Name, id, cell, accept)
			}
			if err == nil {
				m.openObject(id)
			}
		} else {
			err = fmt.Errorf("用法：/ttt 用户 | /ttt accept 编号 | /ttt 编号 格子")
		}
	case "/guess", "/word":
		kind := strings.TrimPrefix(command, "/")
		first, rest := splitArgument(arg)
		if first == "start" {
			id, e := m.store.NewGuess(m.user.Name, m.room, kind, rest)
			err = e
			if e == nil {
				m.announce(fmt.Sprintf("开始了猜谜 #%d，使用 %s 猜测", id, command), m.room)
			}
		} else {
			result, e := m.store.Guess(m.user.Name, m.room, kind, arg)
			err = e
			if e == nil {
				m.announce(result, m.room)
			}
		}
	case "/daily":
		m.openPanel("今日话题", "daily", []string{time.Now().In(time.FixedZone("CST", 8*3600)).Format("2006-01-02"), dailyTopic()}, []panelAction{{"把话题放入输入框", dailyTopic() + " ", true}})
	default:
		return false, nil
	}
	if err != nil {
		m.note(err.Error())
	}
	return true, nil
}
