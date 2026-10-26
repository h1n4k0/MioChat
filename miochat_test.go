package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	gossh "golang.org/x/crypto/ssh"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, e := openStore(filepath.Join(t.TempDir(), "chat.db"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.db.Close() })
	return s
}
func registerUser(t *testing.T, s *Store, name string) User {
	t.Helper()
	u, e := s.Register(name, "test-password-123", "test-password-123")
	if e != nil {
		t.Fatal(e)
	}
	return u
}
func TestAccountsAndPasswordChange(t *testing.T) {
	s := testStore(t)
	u := registerUser(t, s, "中文用户")
	if u.Role != "member" {
		t.Fatal("registration must never grant administrator")
	}
	if _, e := s.Register("中文用户", "test-password-123", "test-password-123"); e == nil {
		t.Fatal("duplicate registration")
	}
	if _, e := s.Authenticate("中文用户", "wrong"); e == nil {
		t.Fatal("wrong password accepted")
	}
	if _, e := s.Authenticate("不存在", "wrong"); e == nil {
		t.Fatal("unknown account accepted")
	}
	if e := s.ChangePassword(u.Name, "test-password-123", "新密码-new-1234"); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Authenticate(u.Name, "test-password-123"); e == nil {
		t.Fatal("old password accepted")
	}
	if _, e := s.Authenticate(u.Name, "新密码-new-1234"); e != nil {
		t.Fatal(e)
	}
	var hash []byte
	s.db.QueryRow("SELECT hash FROM users WHERE name=?", u.Name).Scan(&hash)
	if bytes.Contains(hash, []byte("新密码")) {
		t.Fatal("plaintext password stored")
	}
}
func TestHistoryPrivacyAndRoomPermissions(t *testing.T) {
	s := testStore(t)
	alice := registerUser(t, s, "alice")
	bob := registerUser(t, s, "bob")
	s.Add(Message{Room: "lobby", Sender: "alice", Body: "大厅中文消息", Kind: "chat"})
	secret, _ := s.Add(Message{Sender: "alice", Recipient: "bob", Body: "private-secret", Kind: "chat"})
	msgs, e := s.History("lobby", "eve", "", 100)
	if e != nil || len(msgs) != 1 || msgs[0].Body != "大厅中文消息" {
		t.Fatalf("public history: %#v %v", msgs, e)
	}
	msgs, _ = s.History("", "alice", "bob", 100)
	if len(msgs) != 1 {
		t.Fatal("missing DM")
	}
	msgs, _ = s.History("", "eve", "bob", 100)
	if len(msgs) != 0 {
		t.Fatal("DM leaked")
	}
	found, _ := s.Search("lobby", "eve", "", "private")
	if len(found) != 0 {
		t.Fatal("search leaked DM")
	}
	if e = s.CreateRoom("alice-room", alice.Name); e != nil {
		t.Fatal(e)
	}
	if e = s.Topic(bob, "alice-room", "hijack"); e == nil {
		t.Fatal("topic permissions bypassed")
	}
	if e = s.Topic(alice, "alice-room", "中文话题"); e != nil {
		t.Fatal(e)
	}
	if e = s.Pin(alice, "alice-room", secret.ID, false); e == nil {
		t.Fatal("DM pinned publicly")
	}
}
func TestPersistentAccountsAndHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, e := openStore(path)
	if e != nil {
		t.Fatal(e)
	}
	registerUser(t, s, "persist")
	s.Add(Message{Room: "lobby", Sender: "persist", Body: "重启后保留", Kind: "chat"})
	s.db.Close()
	s, e = openStore(path)
	if e != nil {
		t.Fatal(e)
	}
	defer s.db.Close()
	if _, e = s.Authenticate("persist", "test-password-123"); e != nil {
		t.Fatal(e)
	}
	msgs, _ := s.History("lobby", "persist", "", 100)
	if len(msgs) != 1 {
		t.Fatal("history missing after restart")
	}
}
func TestTerminalInjectionAndChineseLayout(t *testing.T) {
	input := "你好\x1b]52;c;ZXZpbA==\a\x1b[31m世界\x1b[0m\u202e!"
	got := cleanText(input, 800)
	if got != "你好世界!" {
		t.Fatalf("unsafe text: %q", got)
	}
	for _, width := range []int{8, 20, 37} {
		for _, line := range strings.Split(ansi.Hardwrap("中文 English 混排，测试宽度与换行。", width, true), "\n") {
			if ansi.StringWidth(line) > width {
				t.Fatalf("width %d: %q", width, line)
			}
		}
	}
}
func TestMouseLoginRegistrationAndResponsiveUI(t *testing.T) {
	s := testStore(t)
	h := newHub()
	m := newModel(s, h, context.Background(), io.Discard, "xterm-256color", 100, 30, "192.0.2.10", func() {})
	if strings.ContainsRune(m.View(), 0) {
		t.Fatal("login UI contains NUL padding")
	}
	for _, b := range m.hits() {
		if b.action == "register" {
			out, _ := m.Update(tea.MouseMsg{X: b.x + 1, Y: b.y, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
			m = out.(model)
			break
		}
	}
	if !m.register {
		t.Fatal("registration click did not update model")
	}
	m.fields[0].SetValue("鼠标用户")
	m.fields[1].SetValue("test-password-123")
	m.fields[2].SetValue("test-password-123")
	m.focusField(2)
	out, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = out.(model)
	if cmd == nil || !m.busy {
		t.Fatal("submit failed to retain busy state")
	}
	out, _ = m.Update(cmd())
	m = out.(model)
	if !m.logged {
		t.Fatalf("registration failed: %s", m.notice)
	}
	// Dismiss the optional device-key invitation before exercising chat controls.
	m.keyDialogKey(tea.KeyMsg{Type: tea.KeyEsc})
	defer h.Leave(m.client)
	for _, b := range m.hits() {
		if b.action == "room" && b.value == "dev" {
			out, _ = m.Update(tea.MouseMsg{X: b.x + 1, Y: b.y, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
			m = out.(model)
			break
		}
	}
	if m.room != "dev" {
		t.Fatal("channel click did not change room")
	}
	for _, width := range []int{40, 60, 80, 100, 140} {
		out, _ = m.Update(tea.WindowSizeMsg{Width: width, Height: 30})
		m = out.(model)
		view := m.View()
		if strings.ContainsRune(view, 0) {
			t.Fatal("chat UI contains NUL padding")
		}
		for _, line := range strings.Split(view, "\n") {
			if ansi.StringWidth(line) > width {
				t.Fatalf("UI overflow at width=%d: %q", width, ansi.Strip(line))
			}
		}
	}
	m.input.SetValue("中文真的能发送")
	out, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = out.(model)
	messages, _ := s.History("dev", m.user.Name, "", 100)
	if len(messages) != 1 || messages[0].Body != "中文真的能发送" {
		t.Fatal("message was not sent")
	}
	m.command("/passwd")
	if m.focus != 10 {
		t.Fatal("password UI not open")
	}
	if m.fields[0].CharLimit < 256 {
		t.Fatal("old password field truncates long passwords")
	}
	m.passwordKeys(tea.KeyMsg{Type: tea.KeyEsc})
	if m.focus != 0 {
		t.Fatal("password cancel failed")
	}
}
func TestHubDoesNotLeakPrivateMessages(t *testing.T) {
	h := newHub()
	clients := []*Client{}
	for _, n := range []string{"alice", "bob", "eve"} {
		c := &Client{Name: n, Events: make(chan Event, 64), Close: func() {}}
		h.Join(c)
		clients = append(clients, c)
	}
	for _, c := range clients {
		for len(c.Events) > 0 {
			<-c.Events
		}
	}
	h.Broadcast(Message{Sender: "alice", Recipient: "bob", Body: "secret"})
	if len(clients[0].Events) != 1 || len(clients[1].Events) != 1 || len(clients[2].Events) != 0 {
		t.Fatal("private event routing leaked")
	}
}

type liveTerminal struct {
	client  *gossh.Client
	session *gossh.Session
	in      io.WriteCloser
	mu      sync.Mutex
	raw     bytes.Buffer
}

func (l *liveTerminal) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.raw.Write(p)
}
func (l *liveTerminal) output() string { l.mu.Lock(); defer l.mu.Unlock(); return l.raw.String() }
func (l *liveTerminal) wait(t *testing.T, text string) {
	t.Helper()
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(ansi.Strip(l.output()), text) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("missing %q in terminal: %q", text, ansi.Strip(l.output()))
}
func (l *liveTerminal) send(t *testing.T, s string) {
	t.Helper()
	if _, e := io.WriteString(l.in, s); e != nil {
		t.Fatal(e)
	}
}
func connectTerminal(t *testing.T, addr string, signer gossh.Signer) *liveTerminal {
	t.Helper()
	c, e := gossh.Dial("tcp", addr, &gossh.ClientConfig{User: "chat", Auth: []gossh.AuthMethod{appUIAuthentication()}, HostKeyCallback: gossh.FixedHostKey(signer.PublicKey()), Timeout: 5 * time.Second})
	if e != nil {
		t.Fatal(e)
	}
	session, e := c.NewSession()
	if e != nil {
		t.Fatal(e)
	}
	l := &liveTerminal{client: c, session: session}
	l.in, e = session.StdinPipe()
	if e != nil {
		t.Fatal(e)
	}
	session.Stdout = l
	session.Stderr = l
	if e = session.RequestPty("xterm-256color", 30, 100, gossh.TerminalModes{gossh.ECHO: 0}); e != nil {
		t.Fatal(e)
	}
	if e = session.Shell(); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { session.Close(); c.Close() })
	l.wait(t, "TERMINAL SOCIAL CLUB")
	return l
}
func (l *liveTerminal) register(t *testing.T, name string) {
	l.send(t, "\x12")
	time.Sleep(80 * time.Millisecond)
	l.send(t, name+"\r")
	time.Sleep(50 * time.Millisecond)
	l.send(t, "test-password-123\r")
	time.Sleep(50 * time.Millisecond)
	l.send(t, "test-password-123\r")
	l.wait(t, "欢迎，"+name)
	l.send(t, "\x1b")
}
func TestRealSSHChatChineseMouseAndIsolation(t *testing.T) {
	s := testStore(t)
	h := newHub()
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := gossh.NewSignerFromKey(key)
	srv := makeServer(s, h, signer)
	ln, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	a := connectTerminal(t, ln.Addr().String(), signer)
	a.register(t, "alice")
	b := connectTerminal(t, ln.Addr().String(), signer)
	b.register(t, "bob")
	c := connectTerminal(t, ln.Addr().String(), signer)
	c.register(t, "eve")
	a.send(t, "你好，世界！中文实时聊天\r")
	b.wait(t, "中文实时聊天")
	c.wait(t, "中文实时聊天")
	a.send(t, "/msg bob private-e2e-secret\r")
	a.wait(t, "已发送给 @bob")
	b.wait(t, "收到 @alice 的私信")
	b.send(t, "/dm alice\r")
	b.wait(t, "private-e2e-secret")
	if strings.Contains(c.output(), "private-e2e-secret") {
		t.Fatal("third party saw a private message")
	}
	// SGR mouse click on the #dev sidebar row: x=5,y=9 (1-based).
	a.send(t, "\x1b[<0;5;9M\x1b[<0;5;9m")
	time.Sleep(150 * time.Millisecond)
	a.send(t, "mouse-channel-proof\r")
	time.Sleep(200 * time.Millisecond)
	msgs, _ := s.History("dev", "alice", "", 100)
	if len(msgs) != 1 || msgs[0].Body != "mouse-channel-proof" {
		t.Fatalf("mouse channel switch failed: %#v", msgs)
	}
	if e = a.session.WindowChange(24, 60); e != nil {
		t.Fatal(e)
	}
	time.Sleep(100 * time.Millisecond)
	a.send(t, "窄窗口也能输入\r")
	time.Sleep(200 * time.Millisecond)
	msgs, _ = s.History("dev", "alice", "", 100)
	if len(msgs) != 2 {
		t.Fatal("resized session unusable")
	}
	if strings.Contains(a.output(), "test-password-123") {
		t.Fatal("password was echoed")
	}
	x, e := gossh.Dial("tcp", ln.Addr().String(), &gossh.ClientConfig{User: "chat", Auth: []gossh.AuthMethod{appUIAuthentication()}, HostKeyCallback: gossh.FixedHostKey(signer.PublicKey()), Timeout: time.Second})
	if e != nil {
		t.Fatal(e)
	}
	defer x.Close()
	execSession, _ := x.NewSession()
	if _, e = execSession.CombinedOutput("id"); e == nil {
		t.Fatal("exec request was accepted")
	}
	if _, e = x.Dial("tcp", "127.0.0.1:54321"); e == nil {
		t.Fatal("TCP forwarding was accepted")
	}
	fmt.Fprintln(io.Discard, "e2e complete")
}

func TestConcurrentBroadcastAndDatabaseWrites(t *testing.T) {
	s := testStore(t)
	h := newHub()
	for i := 0; i < 32; i++ {
		c := &Client{Name: fmt.Sprintf("user%d", i), Events: make(chan Event, 1024), Close: func() {}}
		if e := h.Join(c); e != nil {
			t.Fatal(e)
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				msg, e := s.Add(Message{Room: "lobby", Sender: fmt.Sprintf("user%d", n), Body: "并发消息与 UTF-8", Kind: "chat"})
				if e != nil {
					t.Error(e)
					return
				}
				h.Broadcast(msg)
				h.Online()
			}
		}(i)
	}
	wg.Wait()
	var count int
	if e := s.db.QueryRow("SELECT count(*) FROM messages").Scan(&count); e != nil || count != 320 {
		t.Fatalf("lost messages: %d %v", count, e)
	}
}

func TestRoomQuotaIsAtomic(t *testing.T) {
	s := testStore(t)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(n int) { defer wg.Done(); _ = s.CreateRoom(fmt.Sprintf("room-%d", n), "same-owner") }(i)
	}
	wg.Wait()
	var count int
	if e := s.db.QueryRow("SELECT count(*) FROM rooms WHERE owner='same-owner'").Scan(&count); e != nil || count != 5 {
		t.Fatalf("room quota raced: %d %v", count, e)
	}
}
func BenchmarkMessagePersistence(b *testing.B) {
	s, e := openStore(filepath.Join(b.TempDir(), "bench.db"))
	if e != nil {
		b.Fatal(e)
	}
	defer s.db.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, e = s.Add(Message{Room: "lobby", Sender: "benchmark", Body: "中文与 English 混排 performance test", Kind: "chat"}); e != nil {
			b.Fatal(e)
		}
	}
}
func BenchmarkFanout64(b *testing.B) {
	h := newHub()
	for i := 0; i < 64; i++ {
		c := &Client{Name: fmt.Sprint(i), Events: make(chan Event, 128), Close: func() {}}
		h.Join(c)
		go func() {
			for range c.Events {
			}
		}()
	}
	msg := Message{Room: "lobby", Sender: "bench", Body: "中文广播"}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.Broadcast(msg)
	}
	b.StopTimer()
	for c := range h.clients {
		close(c.Events)
	}
}
