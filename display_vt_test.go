package main

// Decode the renderer's VT cell operations so regressions cannot pass merely
// because an expected title occurs somewhere in cumulative SSH output.
// This grid covers the renderer's control sequences and CJK text, not arbitrary
// terminal emulation or vendor-specific scrollback reflow.

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
	gossh "golang.org/x/crypto/ssh"
)

type ciVTCell struct {
	text string
	cont bool
}

type ciVTGrid struct {
	mu          sync.Mutex
	cells       [][]ciVTCell
	w, h, x, y  int
	sx, sy      int
	wrap        bool
	pending     []byte
	lastInput   time.Time
	unsupported map[string]bool
}

func newCIVTGrid(w, h int) *ciVTGrid {
	v := &ciVTGrid{wrap: true}
	v.resize(w, h)
	return v
}

func (v *ciVTGrid) resize(w, h int) {
	cells := make([][]ciVTCell, h)
	for y := range cells {
		cells[y] = make([]ciVTCell, w)
		if y < len(v.cells) {
			copy(cells[y], v.cells[y])
		}
	}
	v.cells, v.w, v.h = cells, w, h
	v.x, v.y = min(v.x, w-1), min(v.y, h-1)
}

func (v *ciVTGrid) Resize(w, h int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.resize(w, h)
}

func (v *ciVTGrid) clearCell(x, y int) {
	if x < 0 || x >= v.w || y < 0 || y >= v.h {
		return
	}
	if v.cells[y][x].cont && x > 0 {
		v.cells[y][x-1] = ciVTCell{}
	}
	if x+1 < v.w && v.cells[y][x+1].cont {
		v.cells[y][x+1] = ciVTCell{}
	}
	v.cells[y][x] = ciVTCell{}
}

func (v *ciVTGrid) linefeed() {
	v.y++
	if v.y >= v.h {
		copy(v.cells, v.cells[1:])
		v.cells[v.h-1] = make([]ciVTCell, v.w)
		v.y = v.h - 1
	}
}

func (v *ciVTGrid) draw(r rune) {
	width := ansi.StringWidth(string(r))
	if width == 0 {
		if v.x > 0 {
			x := min(v.x-1, v.w-1)
			if v.cells[v.y][x].cont && x > 0 {
				x--
			}
			v.cells[v.y][x].text += string(r)
		}
		return
	}
	if v.x >= v.w || v.x+width > v.w {
		if v.wrap {
			v.x = 0
			v.linefeed()
		} else {
			v.x = max(0, v.w-width)
		}
	}
	v.clearCell(v.x, v.y)
	if width == 2 && v.x+1 < v.w {
		v.clearCell(v.x+1, v.y)
		v.cells[v.y][v.x+1] = ciVTCell{cont: true}
	}
	v.cells[v.y][v.x] = ciVTCell{text: string(r)}
	v.x = min(v.w, v.x+width)
}

func (v *ciVTGrid) csi(parameters string, final byte) {
	private := strings.HasPrefix(parameters, "?")
	parameters = strings.TrimLeft(parameters, "?><!")
	fields := strings.Split(parameters, ";")
	values := make([]int, len(fields))
	for i, field := range fields {
		_, _ = fmt.Sscan(field, &values[i])
	}
	param := func(i, fallback int) int {
		if i >= len(values) || values[i] == 0 {
			return fallback
		}
		return values[i]
	}
	switch final {
	case 'H', 'f':
		v.y, v.x = min(v.h-1, param(0, 1)-1), min(v.w-1, param(1, 1)-1)
	case 'G':
		v.x = min(v.w-1, param(0, 1)-1)
	case 'd':
		v.y = min(v.h-1, param(0, 1)-1)
	case 'A':
		v.y = max(0, v.y-param(0, 1))
	case 'B':
		v.y = min(v.h-1, v.y+param(0, 1))
	case 'C':
		v.x = min(v.w-1, v.x+param(0, 1))
	case 'D':
		v.x = max(0, v.x-param(0, 1))
	case 'E':
		v.y, v.x = min(v.h-1, v.y+param(0, 1)), 0
	case 'F':
		v.y, v.x = max(0, v.y-param(0, 1)), 0
	case 'J':
		if values[0] == 3 {
			return // Saved scrollback only; it does not erase visible cells.
		}
		for y := 0; y < v.h; y++ {
			for x := 0; x < v.w; x++ {
				at := y*v.w + x
				cursor := v.y*v.w + min(v.x, v.w-1)
				if values[0] == 2 || values[0] == 0 && at >= cursor || values[0] == 1 && at <= cursor {
					v.clearCell(x, y)
				}
			}
		}
	case 'K':
		for x := 0; x < v.w; x++ {
			if values[0] == 2 || values[0] == 0 && x >= v.x || values[0] == 1 && x <= v.x {
				v.clearCell(x, v.y)
			}
		}
	case 'X':
		for x := v.x; x < min(v.w, v.x+param(0, 1)); x++ {
			v.clearCell(x, v.y)
		}
	case 's':
		v.sx, v.sy = v.x, v.y
	case 'u':
		v.x, v.y = min(v.w-1, v.sx), min(v.h-1, v.sy)
	case 'h', 'l':
		if private {
			for _, value := range values {
				if value == 7 {
					v.wrap = final == 'h'
				}
				if value == 1049 && final == 'h' {
					for y := range v.cells {
						clear(v.cells[y])
					}
					v.x, v.y = 0, 0
				}
			}
		}
	// SGR, cursor visibility, mouse/bracketed-paste modes and DA/DSR queries
	// have no text-cell effect in the current output stream.
	case 'm', 'n', 'c', 't', 'q':
	default:
		if v.unsupported == nil {
			v.unsupported = map[string]bool{}
		}
		v.unsupported["CSI "+parameters+string(final)] = true
	}
}

func (v *ciVTGrid) Write(data []byte) (int, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.lastInput = time.Now()
	v.pending = append(v.pending, data...)
	input := v.pending
	i := 0
	for i < len(input) {
		b := input[i]
		if b == 0x1b {
			if i+1 == len(input) {
				break
			}
			if input[i+1] == '[' {
				end := i + 2
				for end < len(input) && (input[end] < 0x40 || input[end] > 0x7e) {
					end++
				}
				if end == len(input) {
					break
				}
				v.csi(string(input[i+2:end]), input[end])
				i = end + 1
				continue
			}
			if input[i+1] == ']' || input[i+1] == 'P' {
				end := i + 2
				for end < len(input) && input[end] != 7 && !(input[end] == 0x1b && end+1 < len(input) && input[end+1] == '\\') {
					end++
				}
				if end == len(input) {
					break
				}
				i = end + 1
				if input[end] == 0x1b {
					i++
				}
				continue
			}
			switch input[i+1] {
			case '7':
				v.sx, v.sy = v.x, v.y
			case '8':
				v.x, v.y = min(v.w-1, v.sx), min(v.h-1, v.sy)
			case 'D':
				v.linefeed()
			}
			i += 2
			continue
		}
		switch b {
		case '\r':
			v.x = 0
		case '\n':
			v.linefeed()
		case '\b':
			v.x = max(0, v.x-1)
		case '\t':
			v.x = min(v.w-1, (v.x/8+1)*8)
		default:
			if b >= 0x20 && b != 0x7f {
				if !utf8.FullRune(input[i:]) {
					v.pending = append(v.pending[:0], input[i:]...)
					return len(data), nil
				}
				r, size := utf8.DecodeRune(input[i:])
				v.draw(r)
				i += size
				continue
			}
		}
		i++
	}
	v.pending = append(v.pending[:0], input[i:]...)
	return len(data), nil
}

func (v *ciVTGrid) snapshot() (string, time.Time) {
	v.mu.Lock()
	defer v.mu.Unlock()
	var screen strings.Builder
	for _, row := range v.cells {
		for _, cell := range row {
			if !cell.cont {
				if cell.text == "" {
					screen.WriteByte(' ')
				} else {
					screen.WriteString(cell.text)
				}
			}
		}
		screen.WriteByte('\n')
	}
	return screen.String(), v.lastInput
}

func TestTerminalGridEraseAndSplitCJK(t *testing.T) {
	v := newCIVTGrid(20, 4)
	_, _ = v.Write([]byte("old-old-old\r\nCHAT_QA_OLD"))
	_, _ = v.Write([]byte("\x1b[Hnew\x1b[K\r\n\x1b[2K"))
	for _, b := range []byte("中文") {
		_, _ = v.Write([]byte{b})
	}
	text, _ := v.snapshot()
	if strings.Contains(text, "old") || strings.Contains(text, "CHAT_QA") || !strings.Contains(text, "中文") {
		t.Fatalf("VT erase/split-UTF8 handling is wrong:\n%s", text)
	}
	_, _ = v.Write([]byte("\x1b[H\x1b[2J"))
	text, _ = v.snapshot()
	if strings.TrimSpace(text) != "" {
		t.Fatal("erase-screen did not clear all cells")
	}
}

func TestRealSSHVTGridPanelTransitions(t *testing.T) {
	s := testStore(t)
	password := "synthetic-vt-password-123"
	for _, name := range []string{"vtviewer", "vtactor"} {
		if _, err := s.Register(name, password, password); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`UPDATE user_meta SET settings='{"motion":"off","key_reminder":"off","welcome":"off"}' WHERE name=?`, name); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= 80; i++ {
		result, err := s.db.Exec(`INSERT INTO messages(room,sender,recipient,body,kind,at) VALUES('lobby','vtactor','',?,'chat',?)`, fmt.Sprintf("CHAT_QA_%03d 中文实屏消息 English mixed text，用于分辨面板是否残留上一帧。", i), 1793000000+i)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := result.LastInsertId()
		if i <= 36 {
			if _, err = s.db.Exec(`INSERT INTO notifications(user,kind,actor,ref,body,seen,at) VALUES('vtviewer','mention','vtactor',?,?,0,?)`, id, fmt.Sprintf("NOTIFY_QA_%03d 中文通知长标题 %s", i, strings.Repeat("滚动筛选应清除旧画面。", 1+i%5)), 1793000000+i); err != nil {
				t.Fatal(err)
			}
		}
	}
	hub := newHub()
	host := transportSigner(t)
	server := makeServer(s, hub, host)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go server.Serve(listener)
	t.Cleanup(func() {
		hub.stopUI()
		_ = server.Close()
		hub.uiTasks.Wait()
	})
	for _, size := range [][2]int{{100, 30}, {80, 24}, {40, 24}, {140, 40}} {
		t.Run(fmt.Sprintf("%dx%d", size[0], size[1]), func(t *testing.T) {
			client, err := gossh.Dial("tcp", listener.Addr().String(), &gossh.ClientConfig{User: "chat", Auth: []gossh.AuthMethod{transportAnonymousAuth()}, HostKeyCallback: gossh.FixedHostKey(host.PublicKey()), Timeout: 3 * time.Second})
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			session, err := client.NewSession()
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			vt := newCIVTGrid(size[0], size[1])
			session.Stdout, session.Stderr = vt, vt
			input, err := session.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err = session.RequestPty("xterm-256color", size[1], size[0], nil); err != nil {
				t.Fatal(err)
			}
			if err = session.Shell(); err != nil {
				t.Fatal(err)
			}
			lastAction := time.Now()
			wait := func(marker string) string {
				t.Helper()
				deadline := time.Now().Add(5 * time.Second)
				for time.Now().Before(deadline) {
					text, last := vt.snapshot()
					if strings.Contains(text, marker) && time.Since(lastAction) >= 150*time.Millisecond && time.Since(last) >= 100*time.Millisecond {
						return text
					}
					time.Sleep(10 * time.Millisecond)
				}
				text, _ := vt.snapshot()
				t.Fatalf("missing stable VT marker %q:\n%s", marker, text)
				return ""
			}
			send := func(value string) {
				t.Helper()
				lastAction = time.Now()
				if _, err := input.Write([]byte(value)); err != nil {
					t.Fatal(err)
				}
			}
			click := func(labels ...string) {
				t.Helper()
				text, _ := vt.snapshot()
				for y, line := range strings.Split(text, "\n") {
					for _, label := range labels {
						if index := strings.Index(line, label); index >= 0 {
							x := ansi.StringWidth(line[:index]) + 2
							send(fmt.Sprintf("\x1b[<0;%d;%dM\x1b[<0;%d;%dm", x, y+1, x, y+1))
							return
						}
					}
				}
				t.Fatalf("toolbar labels %q absent in VT cells:\n%s", labels, text)
			}
			wait("TERMINAL SOCIAL CLUB")
			send("vtviewer\r")
			wait("密码")
			send(password + "\r")
			wait("CHAT_QA_")
			panels := []struct {
				labels           []string
				title, forbidden string
			}{
				{[]string{"[菜单]"}, "COMMAND GUIDE", "/join"},
				{[]string{"[帮助]", "[?]"}, "COMMAND GUIDE", "/join"},
				{[]string{"[通知", "[!"}, "通知中心", "NOTIFY_QA_"},
			}
			for cycle := 0; cycle < 2; cycle++ {
				for _, panel := range panels {
					click(panel.labels...)
					if text := wait(panel.title); strings.Contains(text, "CHAT_QA_") {
						t.Fatalf("chat cells survived panel transition:\n%s", text)
					}
					send("\x1b[<65;8;10M\x1b[<65;8;10M")
					wait(panel.title)
					send("NO_MATCH_QA_ZZZ")
					if text := wait("NO_MATCH_QA_ZZZ"); strings.Contains(text, panel.forbidden) {
						t.Fatalf("old panel items survived a no-match filter:\n%s", text)
					}
					send("\x1b")
					if text := wait("CHAT_QA_"); strings.Contains(text, "NO_MATCH_QA_ZZZ") || strings.Contains(text, "COMMAND GUIDE") || strings.Contains(text, "通知中心") {
						t.Fatalf("panel cells survived Escape:\n%s", text)
					}
				}
			}
			for _, panel := range panels {
				click(panel.labels...)
				wait(panel.title)
				for _, resized := range [][2]int{{40, 24}, {140, 40}, size} {
					vt.Resize(resized[0], resized[1])
					lastAction = time.Now()
					if err := session.WindowChange(resized[1], resized[0]); err != nil {
						t.Fatal(err)
					}
					if text := wait(panel.title); strings.Contains(text, "CHAT_QA_") {
						t.Fatalf("chat cells survived panel-open resize:\n%s", text)
					}
				}
				send("\x1b")
				wait("CHAT_QA_")
			}
			vt.mu.Lock()
			unknown := fmt.Sprint(vt.unsupported)
			unsupported := len(vt.unsupported)
			vt.mu.Unlock()
			if unsupported != 0 {
				t.Fatal("unmodeled VT layout sequences require review", unknown)
			}
		})
	}
}
