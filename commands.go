package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

const helpText = `导航 / CHANNELS
 /rooms                 列出所有公共频道
 /join 频道             切换频道，如 /join dev
 /create 频道           创建并进入自己的频道
 /topic 内容            设置自己频道的话题
 /who                   查看在线账户

私聊 / DIRECT MESSAGES
 /dm 用户               打开与该用户的私聊
 /msg 用户 内容         直接发送私信，支持离线留言
 /back                  返回公共大厅

记录 / HISTORY
 /history [数量]        加载最近 1–200 条记录
 /search 关键词         搜索当前会话最近 200 条
 /pin 消息编号          置顶当前频道消息（创建者）
 /unpin 消息编号        取消置顶
 /pins                  显示当前频道置顶
 /clear                 清理自己的屏幕

账户 / ACCOUNT
 /profile [用户]        查看个人资料
 /bio 简介              设置个人介绍
 /status 状态           设置状态文字
 /afk [原因]            设置离开状态
 /theme 主题            aurora / sakura / amber / mono
 /passwd                打开隐藏输入的改密表单
 /ignore 用户           在当前连接屏蔽该用户
 /unignore 用户         取消屏蔽
 /logout                退出账户并断开连接

玩一会 / PLAY
 /me 动作               发送动作消息
 /roll [面数]           掷骰子，默认 6 面
 /coin                  抛硬币
 /8ball 问题            问问神奇八号球
 /shrug                 发送耸肩表情
 /art                   查看 ASCII 营地
 /time                  当前北京时间
 /stats                 大厅运行状态

管理 / MODERATION（管理员）
 /kick 用户             断开该用户的在线连接
 /ban 用户              停用账户并断开连接
 /unban 用户            恢复账户

键盘：Enter 发送 · F1 帮助 · Esc 返回
PgUp/PgDn 翻页 · Ctrl+End 回到底部
↑↓ 输入历史 · Ctrl+B 侧栏 · Ctrl+C 退出
鼠标：点频道切换 · 点用户私聊 · 滚轮翻页`

const campArt = `        .             *
   *         .                 .
              /\
        /\   /  \       /\
   ____/  \_/    \_____/  \____
        .----.      (  )
       /______\      )(     *
       | .  . |     /||\
       |__[]__|    /_||_\
       M I O C H A T   C A M P`

func randomNumber(n int64) int64 {
	v, e := rand.Int(rand.Reader, big.NewInt(n))
	if e != nil {
		return 0
	}
	return v.Int64()
}
func (m *model) send(body, kind, recipient string) error {
	if m.positionUnknown {
		return fmt.Errorf("草稿尚未读取，已暂停发送，请先重试加载")
	}
	if len([]rune(body)) > 800 {
		return fmt.Errorf("单条消息最多 800 个字符，可用 /post 发布长帖")
	}
	room := m.room
	if recipient != "" {
		room = ""
	}
	msg, err := m.store.Send(Message{Room: room, Sender: m.user.Name, Recipient: recipient, Body: body, Kind: kind, ReplyTo: m.replyID})
	if err == nil {
		if m.replyID > 0 {
			m.flash = ""
			m.flashUntil = time.Time{}
		}
		m.replyID = 0
		m.hub.Broadcast(msg)
		if recipient == m.peer {
			if loadErr := m.loadLatest(100); loadErr != nil {
				m.timelineError(fmt.Errorf("消息已发送，但载入最新记录失败：%w", loadErr))
			}
		}
	}
	return err
}
func (m *model) command(line string) tea.Cmd {
	if m.logged && m.positionUnknown && line != "/quit" && line != "/logout" && line != "/exit" {
		m.showPositionLoadError(nil)
		return nil
	}
	if m.logged {
		u, valid := m.hub.RefreshClient(m.client)
		if !valid {
			return nil
		}
		m.user = u
		if u.MustReset && line != "/passwd" && line != "/quit" && line != "/logout" && line != "/exit" {
			m.note("请先使用 /passwd 修改临时密码")
			return nil
		}
	}
	if !m.limiter.Allow() {
		m.rateDrops++
		m.flash = "操作过于频繁，请稍候"
		m.flashUntil = time.Now().Add(3 * time.Second)
		return nil
	}
	if handled, cmd := m.extendedCommand(line); handled {
		return cmd
	}
	if !strings.HasPrefix(line, "/") {
		if e := m.send(line, "chat", m.peer); e != nil {
			m.note(e.Error())
		}
		return nil
	}
	parts := strings.SplitN(line, " ", 2)
	cmd := strings.ToLower(parts[0])
	arg := ""
	if len(parts) > 1 {
		arg = strings.TrimSpace(parts[1])
	}
	var err error
	switch cmd {
	case "/quit", "/exit", "/logout":
		return tea.Quit
	case "/join":
		if arg == "" {
			m.note("用法：/join dev")
		} else {
			err = m.move(strings.TrimPrefix(arg, "#"), "")
		}
	case "/back":
		err = m.move("lobby", "")
		if err == nil {
			_ = m.store.Profile(m.user.Name, "status", "")
		}
	case "/topic":
		if m.peer != "" {
			err = fmt.Errorf("私信没有公共话题")
		} else if arg == "" {
			r, _ := m.store.Room(m.room)
			m.note(r.Topic)
		} else {
			err = m.store.Topic(m.user, m.room, arg)
			m.refresh()
		}
	case "/dm":
		if arg == "" {
			m.note("用法：/dm 用户")
		} else {
			err = m.move("", arg)
		}
	case "/msg":
		p := strings.SplitN(arg, " ", 2)
		if len(p) < 2 {
			err = fmt.Errorf("用法：/msg 用户 消息")
		} else {
			var u User
			u, err = m.store.User(p[0])
			if err == nil && u.Name == m.user.Name {
				err = fmt.Errorf("不能给自己发私信")
			}
			if err == nil {
				err = m.send(p[1], "chat", u.Name)
				if err == nil {
					m.note("已发送给 @" + u.Name)
				}
			}
		}
	case "/history":
		n := 100
		if arg != "" {
			n, err = strconv.Atoi(arg)
		}
		if err == nil {
			err = m.loadLatest(n)
			if err != nil {
				m.timelineError(err)
				return nil
			}
		}
	case "/pin", "/unpin":
		var id int64
		id, err = strconv.ParseInt(arg, 10, 64)
		if err == nil {
			err = m.store.Pin(m.user, m.room, id, cmd == "/unpin")
		}
		if err == nil {
			m.note("置顶已更新")
		}
	case "/clear":
		m.feed = nil
		m.feedAtLatest = true
		m.rebuild(true)
	case "/bio", "/status":
		err = m.store.Profile(m.user.Name, strings.TrimPrefix(cmd, "/"), arg)
		if err == nil {
			m.note("资料已更新")
		}
	case "/afk":
		if arg == "" {
			arg = "暂时离开"
		}
		err = m.store.Profile(m.user.Name, "status", "AFK · "+arg)
		if err == nil {
			m.note("已设置离开状态")
		}
	case "/passwd":
		m.focus = 10
		m.passFocus = 0
		m.notice = ""
		for i := range m.fields {
			m.fields[i].SetValue("")
			m.fields[i].CharLimit = 256
			m.fields[i].EchoMode = 1
			m.fields[i].Blur()
		}
		m.fields[0].Focus()
	case "/me":
		err = m.send(arg, "action", m.peer)
	case "/roll":
		sides := int64(6)
		if arg != "" {
			sides, err = strconv.ParseInt(arg, 10, 64)
		}
		if err == nil {
			if sides < 2 || sides > 1000000 {
				err = fmt.Errorf("面数范围：2–1000000")
			} else {
				err = m.send(fmt.Sprintf("掷出了 %d（%d 面骰）", randomNumber(sides)+1, sides), "action", m.peer)
			}
		}
	case "/coin":
		result := "正面"
		if randomNumber(2) == 0 {
			result = "反面"
		}
		err = m.send("抛硬币："+result, "action", m.peer)
	case "/8ball":
		if arg == "" {
			err = fmt.Errorf("用法：/8ball 今天能写完吗？")
		} else {
			answers := []string{"毫无疑问", "很有希望", "再想一想", "现在还不好说", "不如先喝杯水", "机会渺茫", "大胆去做", "问问你的直觉"}
			err = m.send("问："+arg+" / 八号球："+answers[randomNumber(int64(len(answers)))], "action", m.peer)
		}
	case "/shrug":
		err = m.send("¯\\_(ツ)_/¯", "chat", m.peer)
	case "/art":
		for _, l := range strings.Split(campArt, "\n") {
			m.note(l)
		}
	case "/time":
		m.note(time.Now().In(time.FixedZone("CST", 8*3600)).Format("2006-01-02 15:04:05 CST"))
	default:
		err = fmt.Errorf("未知命令 %s，按 F1 查看帮助", cleanText(cmd, 40))
	}
	if err != nil {
		m.note(err.Error())
	}
	return nil
}
func (m model) helpView(width, height int) string {
	// The help is scrollable using the same viewport rather than being clipped.
	text := m.color(" MIOCHAT / COMMAND GUIDE", m.accent(), true) + "\n" + helpText
	lines := strings.Split(ansi.Hardwrap(text, width, true), "\n")
	start := min(m.helpOffset, max(0, len(lines)-height))
	end := min(len(lines), start+height)
	return strings.Join(lines[start:end], "\n")
}

type passwordResult struct{ err error }

func (m *model) submitPassword() tea.Cmd {
	if m.busy {
		return nil
	}
	if !m.hub.authAllowed(m.ip, false) {
		m.notice = "尝试过于频繁，请稍后再试"
		return nil
	}
	if m.fields[1].Value() != m.fields[2].Value() {
		m.notice = "新密码不一致"
		return nil
	}
	old, next, name, sessionID := m.fields[0].Value(), m.fields[1].Value(), m.user.Name, m.client.ID
	m.busy = true
	store, hub, ctx := m.store, m.hub, m.ctx
	return func() tea.Msg {
		select {
		case hub.AuthSlots <- struct{}{}:
		case <-ctx.Done():
			return passwordResult{context.Canceled}
		}
		defer func() { <-hub.AuthSlots }()
		return passwordResult{store.ChangePasswordForSession(name, old, next, sessionID)}
	}
}
func (m *model) passwordKeys(k tea.KeyMsg) tea.Cmd {
	if m.busy {
		return nil
	}
	switch k.String() {
	case "esc":
		if m.user.MustReset {
			m.notice = "必须先修改临时密码"
			return nil
		}
		m.endPassword()
		return nil
	case "tab":
		m.passFocus = (m.passFocus + 1) % 3
	case "shift+tab":
		m.passFocus = (m.passFocus + 2) % 3
	case "enter":
		if m.passFocus < 2 {
			m.passFocus++
		} else {
			return m.submitPassword()
		}
	default:
		var cmd tea.Cmd
		m.fields[m.passFocus], cmd = m.fields[m.passFocus].Update(k)
		return cmd
	}
	for i := range m.fields {
		m.fields[i].Blur()
	}
	m.fields[m.passFocus].Focus()
	return nil
}
func (m *model) endPassword() {
	for i := range m.fields {
		m.fields[i].SetValue("")
		m.fields[i].Blur()
	}
	m.focus = 0
	m.notice = ""
	m.busy = false
	m.input.Focus()
}
func (m model) passwordView() string {
	x, y, w := m.formGeometry()
	lines := make([]string, y)
	pad := strings.Repeat(" ", x+2)
	lines = append(lines, pad+m.color("MIOCHAT / 修改密码", m.accent(), true), pad+"", pad+m.formLabel("旧密码", m.fields[0]), pad+m.formInput(m.fields[0], w-4), pad+"", pad+m.formLabel("新密码（至少 10 个字符）", m.fields[1]), pad+m.formInput(m.fields[1], w-4), pad+"", pad+m.formLabel("再次输入新密码", m.fields[2]), pad+m.formInput(m.fields[2], w-4), pad+"", pad+m.color("[ 保存新密码 / Enter ]", m.accent(), true), pad+m.fitLine(m.notice, w-4), pad+"Tab 切换 · Esc 取消")
	return strings.Join(lines, "\n")
}
