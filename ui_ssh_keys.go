package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	gossh "golang.org/x/crypto/ssh"
)

type sshAuthResult struct {
	user User
	err  error
}

type sshKeyBindResult struct {
	key              SSHKey
	err              error
	verifiedPassword bool
}

type sshKeyForm struct {
	fields [2]textinput.Model
	focus  int
	busy   bool
	notice string
}

// Recheck both the key and the account while associating an established app
// session. A key revoked after SSH negotiation cannot race its way into a room.
func (s *Store) AttachSSHSessionKey(sessionID, name string, key gossh.PublicKey) error {
	if err := ValidateSSHKey(key); err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = sshKeyUserQ(tx, name); err != nil {
		return errors.New("此密钥当前不能用于登录")
	}
	if valid, err := sessionCurrentQ(tx, sessionID, name); err != nil || !valid {
		return errCredentialsChanged
	}
	result, err := tx.Exec(`INSERT INTO session_credentials(session_id,key_id) SELECT s.id,k.id FROM sessions s JOIN ssh_keys k ON k.user=s.user WHERE s.id=? AND s.user=? AND s.closed=0 AND s.revoked=0 AND k.fingerprint=? AND k.revoked=0`, sessionID, name, gossh.FingerprintSHA256(key))
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil || n != 1 {
		return errors.New("密钥已撤销或会话无效，请重新登录")
	}
	return tx.Commit()
}

func (m *model) completeLogin(user User, viaKey bool) tea.Cmd {
	// Only the first successful authentication owns this model and session.
	// A delayed key result cannot replace a completed password login.
	if m.logged {
		return nil
	}
	u, err := m.store.User(user.Name)
	if err != nil || m.ctx.Err() != nil || u.Banned || m.store.accountBanned(user.Name) || (viaKey && u.MustReset) {
		m.notice = "此账户当前不能登录，请使用账号密码重试"
		return nil
	}
	previousName := m.client.Name
	previousEpoch := m.client.AuthEpoch
	m.client.Name = u.Name
	m.client.AuthEpoch = user.AuthEpoch
	if err = m.hub.Join(m.client); err != nil {
		m.client.Name = previousName
		m.client.AuthEpoch = previousEpoch
		m.notice = err.Error()
		return nil
	}
	if viaKey {
		if err = m.store.AttachSSHSessionKey(m.client.ID, u.Name, m.sshPublicKey); err != nil {
			m.hub.Leave(m.client)
			m.client.Name = previousName
			m.client.AuthEpoch = previousEpoch
			m.notice = err.Error()
			return nil
		}
	} else {
		m.passwordVerifiedAt = time.Now()
	}
	m.user = u
	m.logged = true
	if m.authenticatedSignal != nil {
		m.authenticatedSignal.Store(true)
	}
	m.settings = m.store.Settings(u.Name)
	for name := range m.store.Relations(u.Name, "block") {
		m.ignored[name] = true
	}
	for i := range m.fields {
		m.fields[i].SetValue("")
		m.fields[i].Blur()
	}
	m.input.Focus()
	m.forceMove("lobby")
	if m.settings["welcome"] != "off" {
		m.note("欢迎，" + u.Name + "。右键用户名 @；右键消息回复；双击用户名戳一戳。")
	}
	if u.MustReset {
		m.command("/passwd")
		m.notice = "请先修改管理员设置的临时密码"
	} else if !viaKey {
		m.maybeKeyPrompt()
	}
	return tea.Batch(m.waitEvent(), textinput.Blink)
}

func (m *model) maybeKeyPrompt() {
	if !m.logged || m.user.MustReset || m.settings["key_reminder"] == "off" {
		return
	}
	keys, err := m.store.SSHKeys(m.user.Name)
	if err == nil && len(keys) == 0 {
		m.keyPrompt = true
		m.keyPromptFocus = 1
	}
}

func (m *model) keyInstructions() {
	m.keyPrompt = false
	m.openPanel("为设备添加 SSH 密钥", "key-instructions", []string{
		"私钥留在自己的设备；每台设备可使用不同密钥。",
		"推荐 Ed25519：ssh-keygen -t ed25519",
		"也可选择：ssh-keygen -t ecdsa -b 256",
		"或 RSA：ssh-keygen -t rsa -b 3072",
		"已有密钥可直接使用，无需重新生成。请为私钥设置口令。",
		"使用所选私钥重新连接：",
		"ssh -t -i ~/.ssh/id_ed25519 -o IdentitiesOnly=yes chat@example.com",
		"选择 ECDSA/RSA 时，将路径改成 ~/.ssh/id_ecdsa 或 ~/.ssh/id_rsa。",
		"第一次输入原聊天室账号密码，再确认绑定本次连接的公钥。",
		"绑定后用同一密钥连接会直接进入原账号。/keys 管理全部设备。",
	}, nil)
}

func (m *model) beginKeyBinding() {
	m.keyPrompt = false
	if m.sshPublicKey == nil || ValidateSSHKey(m.sshPublicKey) != nil {
		m.keyInstructions()
		return
	}
	m.closePanel()
	form := &sshKeyForm{}
	for i := range form.fields {
		field := textinput.New()
		field.Prompt = ""
		field.Width = max(10, min(60, m.w-10))
		field.CharLimit = 256
		field.KeyMap.Paste.SetEnabled(false)
		form.fields[i] = field
	}
	form.fields[0].CharLimit = 40
	form.fields[0].SetValue("当前设备")
	form.fields[0].CursorEnd()
	form.fields[0].Focus()
	form.fields[1].EchoMode = textinput.EchoPassword
	form.fields[1].EchoCharacter = '*'
	m.keyForm = form
}

func (m *model) submitKeyBinding() tea.Cmd {
	form := m.keyForm
	if form == nil || form.busy || !m.logged || m.sshPublicKey == nil {
		return nil
	}
	if !m.hub.authAllowed(m.ip, false) {
		form.notice = "尝试过于频繁，请稍后再试"
		return nil
	}
	label, password := form.fields[0].Value(), form.fields[1].Value()
	form.fields[1].SetValue("")
	fresh := !m.passwordVerifiedAt.IsZero() && time.Since(m.passwordVerifiedAt) < 2*time.Minute
	if !fresh && password == "" {
		form.notice = "请确认当前账号密码"
		form.focus = 1
		form.fields[0].Blur()
		form.fields[1].Focus()
		return nil
	}
	key := m.sshPublicKey // Only the successfully signed key of this connection.
	if err := ValidateSSHKey(key); err != nil {
		form.notice = err.Error()
		return nil
	}
	form.busy = true
	form.notice = "正在确认并绑定…"
	store, hub, ctx, user, sessionID := m.store, m.hub, m.ctx, m.user.Name, m.client.ID
	return func() tea.Msg {
		select {
		case hub.AuthSlots <- struct{}{}:
		case <-ctx.Done():
			return sshKeyBindResult{err: context.Canceled}
		}
		defer func() { <-hub.AuthSlots }()
		verifiedPassword := password != "" || !fresh
		if verifiedPassword {
			u, err := store.Authenticate(user, password)
			if err != nil || u.MustReset {
				return sshKeyBindResult{err: errors.New("当前密码验证失败，请重试")}
			}
		}
		if ctx.Err() != nil {
			return sshKeyBindResult{err: ctx.Err()}
		}
		bound, err := store.BindSSHKeyForSession(sessionID, user, label, string(gossh.MarshalAuthorizedKey(key)))
		return sshKeyBindResult{key: bound, err: err, verifiedPassword: verifiedPassword}
	}
}

func (m *model) finishKeyBinding(result sshKeyBindResult) tea.Cmd {
	if m.keyForm == nil {
		return nil
	}
	m.keyForm.busy = false
	if result.err != nil {
		m.keyForm.notice = result.err.Error()
		return nil
	}
	m.keyForm = nil
	if result.verifiedPassword {
		m.passwordVerifiedAt = time.Now()
	}
	m.note("设备密钥已绑定；下次使用这把密钥即可直接登录")
	m.openSSHKeys()
	return nil
}

func (m *model) openSSHKeys() {
	keys, err := m.store.SSHKeys(m.user.Name)
	if err != nil {
		m.note(err.Error())
		return
	}
	lines := []string{fmt.Sprintf("@%s · 已绑定 %d / 10 把密钥", m.user.Name, len(keys)), "每台设备可选不同密钥；只有你能管理自己的绑定。"}
	actions := []panelAction{{"绑定当前设备密钥", "/key-add", false}, {"添加另一台设备 / 选择算法", "/key-guide", false}}
	for _, key := range keys {
		used := "尚未使用"
		if key.LastUsed > 0 {
			used = formatUnix(key.LastUsed)
		}
		lines = append(lines, fmt.Sprintf("#%d %s · %s", key.ID, key.Label, key.Algorithm), key.Fingerprint, "最近使用："+used)
		actions = append(actions, panelAction{fmt.Sprintf("撤销 #%d %s", key.ID, key.Label), fmt.Sprintf("/key-remove %d", key.ID), false})
	}
	if m.settings["key_reminder"] == "off" {
		actions = append(actions, panelAction{"恢复无密钥登录提醒", "/pref key_reminder on", false})
	} else {
		actions = append(actions, panelAction{"关闭无密钥登录提醒", "/pref key_reminder off", false})
	}
	m.openPanel("我的 SSH 密钥", "ssh-keys", lines, actions)
}

func (m *model) sshKeyCommand(command, arg string) (bool, tea.Cmd) {
	switch command {
	case "/keys", "/key-add", "/key-bind", "/key-guide", "/key-remove", "/key-remove-confirm", "/key-rename":
	default:
		return false, nil
	}
	if !m.logged || m.user.MustReset {
		m.note("请先完成账号登录和密码设置")
		return true, nil
	}
	switch command {
	case "/keys":
		m.openSSHKeys()
	case "/key-add", "/key-bind":
		if arg != "" {
			m.note("使用 /key-add 绑定本次已验证的设备密钥，无需粘贴公钥")
		} else {
			m.beginKeyBinding()
		}
	case "/key-guide":
		m.keyInstructions()
	case "/key-remove", "/key-remove-confirm":
		id, err := strconv.ParseInt(arg, 10, 64)
		if err != nil || id <= 0 {
			m.note("用法：/key-remove 密钥编号")
			break
		}
		if command == "/key-remove-confirm" {
			if err = m.store.RevokeSSHKey(m.user.Name, id); err != nil {
				m.note(err.Error())
			} else {
				m.hub.Enforce()
				m.openSSHKeys()
			}
			break
		}
		keys, err := m.store.SSHKeys(m.user.Name)
		found := false
		if err == nil {
			for _, key := range keys {
				if key.ID == id {
					found = true
					m.openPanel("确认撤销设备密钥", "key-revoke", []string{key.Label, key.Fingerprint, "仅断开通过这把密钥登录的会话；账号密码仍可使用。"}, []panelAction{{"确认撤销", fmt.Sprintf("/key-remove-confirm %d", id), false}, {"取消", "/keys", false}})
				}
			}
		}
		if !found {
			m.note("没有找到你拥有的有效密钥")
		}
	case "/key-rename":
		rawID, label := splitArgument(arg)
		id, err := strconv.ParseInt(rawID, 10, 64)
		if err == nil && id > 0 {
			err = m.store.RenameSSHKey(m.user.Name, id, label)
		}
		if err != nil || id <= 0 {
			m.note("请使用 /key-rename 密钥编号 新名称，且只能修改自己的密钥")
		} else {
			m.openSSHKeys()
		}
	}
	return true, nil
}

// Dialog geometry and mouse hit regions come from the same wrapped lines.
func (m model) keyDialogContent() ([]string, []hit, int) {
	width := max(16, min(74, m.w-2))
	inner := width - 4
	var rows []string
	var hits []hit
	add := func(text string) { rows = append(rows, strings.Split(ansi.Hardwrap(text, inner, true), "\n")...) }
	action := func(label, value string, selected bool) {
		if selected {
			label = m.color("> "+label, m.accent(), true)
		} else {
			label = "  " + label
		}
		hits = append(hits, hit{2, len(rows), inner, 1, "key-dialog", value})
		add(label)
	}
	if m.keyPrompt {
		add(m.color("MIOCHAT / 更方便地登录", m.accent(), true))
		add("")
		add("欢迎，" + m.user.Name)
		if m.flash == "密码已修改" && time.Now().Before(m.flashUntil) {
			add(m.flash)
		}
		add("要给账号添加设备密钥吗？以后可直接进入聊天室。")
		if m.sshPublicKey != nil {
			add("本次设备：" + m.sshPublicKey.Type())
			add(gossh.FingerprintSHA256(m.sshPublicKey))
		} else {
			add("本次没有提供密钥，可以查看添加步骤。")
		}
		add("")
		check := "[ ] 不再提醒"
		if m.settings["key_reminder"] == "off" {
			check = "[x] 不再提醒"
		}
		action(check, "reminder", m.keyPromptFocus == 0)
		primary := "[绑定本次设备密钥]"
		if m.sshPublicKey == nil {
			primary = "[查看添加步骤]"
		}
		action(primary, "bind", m.keyPromptFocus == 1)
		action("[稍后再说]", "later", m.keyPromptFocus == 2)
		add("")
		add("Tab/↑↓ 切换 · Space 勾选 · Enter 确认 · Esc 稍后")
	} else if form := m.keyForm; form != nil {
		add(m.color("MIOCHAT / 绑定当前设备", m.accent(), true))
		add("")
		add("只绑定本次 SSH 连接已验证持有的公钥。")
		if m.sshPublicKey != nil {
			add(m.sshPublicKey.Type())
			add(gossh.FingerprintSHA256(m.sshPublicKey))
		}
		add("")
		add(m.formLabel("设备名称", form.fields[0]))
		field := form.fields[0]
		hits = append(hits, hit{2, len(rows), inner, 1, "key-dialog", "field0"})
		add(m.formInput(field, inner-1))
		add(m.formLabel("当前密码（刚登录可留空）", form.fields[1]))
		field = form.fields[1]
		hits = append(hits, hit{2, len(rows), inner, 1, "key-dialog", "field1"})
		add(m.formInput(field, inner-1))
		add("")
		action("[确认绑定 / Enter]", "save", form.focus == 2)
		action("[取消 / Esc]", "cancel", false)
		if form.notice != "" {
			add(m.color(cleanText(form.notice, 100), "#FF9A9A", false))
		}
		add("Tab 切换 · Enter 下一项/确认 · Esc 取消")
	}
	return rows, hits, width
}

func (m model) keyDialogHits() []hit {
	rows, hits, width := m.keyDialogContent()
	x, y := max(0, (m.w-width)/2), max(0, (m.h-len(rows)-2)/2)
	var visible []hit
	for _, h := range hits {
		h.x += x
		h.y += y + 1
		if h.y < m.h-1 {
			visible = append(visible, h)
		}
	}
	return visible
}

func (m model) keyDialogView() string {
	rows, _, width := m.keyDialogContent()
	x, y := max(0, (m.w-width)/2), max(0, (m.h-len(rows)-2)/2)
	pad := strings.Repeat(" ", x)
	lines := make([]string, y)
	border := m.color("+"+strings.Repeat("-", width-2)+"+", "#526078", false)
	lines = append(lines, pad+border)
	for _, row := range rows {
		if len(lines) >= m.h-1 {
			break
		}
		fitted := m.fitLine(row, width-4)
		fitted += strings.Repeat(" ", max(0, width-4-ansi.StringWidth(fitted)))
		lines = append(lines, pad+m.color("| ", "#526078", false)+fitted+m.color(" |", "#526078", false))
	}
	lines = append(lines, pad+border)
	return strings.Join(lines, "\n")
}

func (m *model) keyDialogAction(action string) tea.Cmd {
	if m.keyPrompt {
		switch action {
		case "reminder":
			value := "off"
			if m.settings["key_reminder"] == "off" {
				value = "on"
			}
			if err := m.store.Setting(m.user.Name, "key_reminder", value); err != nil {
				m.note(err.Error())
			} else {
				m.settings = m.store.Settings(m.user.Name)
			}
		case "bind":
			m.beginKeyBinding()
		case "later":
			m.keyPrompt = false
		}
		return nil
	}
	form := m.keyForm
	if form == nil || form.busy {
		return nil
	}
	switch action {
	case "cancel":
		m.keyForm = nil
	case "save":
		return m.submitKeyBinding()
	case "field0", "field1":
		form.focus, _ = strconv.Atoi(strings.TrimPrefix(action, "field"))
		for i := range form.fields {
			form.fields[i].Blur()
		}
		return form.fields[form.focus].Focus()
	}
	return nil
}

func (m *model) keyDialogMouse(mouse tea.MouseMsg) tea.Cmd {
	if mouse.Action != tea.MouseActionPress || mouse.Button != tea.MouseButtonLeft {
		return nil
	}
	for _, h := range m.keyDialogHits() {
		if mouse.X >= h.x && mouse.X < h.x+h.w && mouse.Y == h.y {
			return m.keyDialogAction(h.value)
		}
	}
	return nil
}

func (m *model) keyDialogKey(key tea.KeyMsg) tea.Cmd {
	if m.keyPrompt {
		switch key.String() {
		case "esc":
			return m.keyDialogAction("later")
		case "tab", "down":
			m.keyPromptFocus = (m.keyPromptFocus + 1) % 3
		case "shift+tab", "up":
			m.keyPromptFocus = (m.keyPromptFocus + 2) % 3
		case " ", "space":
			return m.keyDialogAction("reminder")
		case "enter":
			return m.keyDialogAction([]string{"reminder", "bind", "later"}[m.keyPromptFocus])
		}
		return nil
	}
	form := m.keyForm
	if form == nil || form.busy {
		return nil
	}
	switch key.String() {
	case "esc":
		return m.keyDialogAction("cancel")
	case "tab", "down":
		form.focus = (form.focus + 1) % 3
	case "shift+tab", "up":
		form.focus = (form.focus + 2) % 3
	case "enter":
		if form.focus == 0 {
			form.focus = 1
		} else {
			return m.submitKeyBinding()
		}
	default:
		if form.focus < 2 {
			var cmd tea.Cmd
			form.fields[form.focus], cmd = form.fields[form.focus].Update(key)
			return cmd
		}
	}
	for i := range form.fields {
		form.fields[i].Blur()
	}
	if form.focus < 2 {
		return form.fields[form.focus].Focus()
	}
	return nil
}
