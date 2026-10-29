package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	ssh "charm.land/ssh"
	tea "github.com/charmbracelet/bubbletea"
	gossh "golang.org/x/crypto/ssh"
)

func hostSigner(path string) (gossh.Signer, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		_, key, e := ed25519.GenerateKey(rand.Reader)
		if e != nil {
			return nil, e
		}
		der, e := x509.MarshalPKCS8PrivateKey(key)
		if e != nil {
			return nil, e
		}
		data = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
		f, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return nil, e
		}
		_, err = f.Write(data)
		if e = f.Close(); err == nil {
			err = e
		}
	}
	if err != nil {
		return nil, err
	}
	return gossh.ParsePrivateKey(data)
}

type trackedConn struct {
	net.Conn
	once     sync.Once
	release  func()
	received atomic.Uint64
}

func (c *trackedConn) Close() error { err := c.Conn.Close(); c.once.Do(c.release); return err }
func (c *trackedConn) Read(data []byte) (int, error) {
	n, err := c.Conn.Read(data)
	if n > 0 {
		c.received.Add(1)
	}
	return n, err
}

type sessionKey struct{}

func makeServer(store *Store, hub *Hub, signer gossh.Signer) *ssh.Server {
	return makeServerWithPolicy(store, hub, signer, defaultSSHTransportPolicy())
}

func makeServerWithPolicy(store *Store, hub *Hub, signer gossh.Signer, policy sshTransportPolicy) *ssh.Server {
	hub.Attach(store)
	var mu sync.Mutex
	perIP := map[string]int{}
	total := 0
	srv := &ssh.Server{HostSigners: []ssh.Signer{signer}, Version: "MioChat", ChannelHandlers: map[string]ssh.ChannelHandler{}, RequestHandlers: map[string]ssh.RequestHandler{}, SubsystemHandlers: map[string]ssh.SubsystemHandler{}}
	configureSSHTransport(srv, policy)
	srv.ConnCallback = func(ctx ssh.Context, c net.Conn) net.Conn {
		ip, _, _ := net.SplitHostPort(c.RemoteAddr().String())
		mu.Lock()
		if total >= 80 || perIP[ip] >= 8 {
			mu.Unlock()
			c.Close()
			return nil
		}
		total++
		perIP[ip]++
		mu.Unlock()
		ctx.SetValue(sessionKey{}, &atomic.Int32{})
		tracked := &trackedConn{Conn: c, release: func() {
			mu.Lock()
			total--
			perIP[ip]--
			if perIP[ip] == 0 {
				delete(perIP, ip)
			}
			mu.Unlock()
		}}
		// Reserve capacity before any database wait. A failed admission check
		// closes the tracked connection and releases exactly that reservation.
		if store.IPBannedContext(ctx, ip) {
			_ = tracked.Close()
			return nil
		}
		// The SSH dependency does not cancel its initial context when this
		// callback returns nil. Rejected connections must not start a login
		// timer that then waits for its full application deadline.
		initializeSSHConnection(ctx, tracked, policy)
		return tracked
	}
	srv.SessionRequestCallback = func(s ssh.Session, kind string) bool {
		if kind != "shell" && !(kind == "subsystem" && s.Subsystem() == guiSubsystemName) {
			return false
		}
		v, ok := s.Context().Value(sessionKey{}).(*atomic.Int32)
		return ok && v.CompareAndSwap(0, 1)
	}
	srv.SubsystemHandlers[guiSubsystemName] = func(s ssh.Session) {
		closeSession := func() { _ = s.Close() }
		if state, ok := s.Context().Value(sshTransportKey{}).(*sshTransportState); ok {
			defer state.close()
			closeSession = func() { _ = state.close() }
		}
		if !hub.beginUI() {
			_ = s.Close()
			return
		}
		defer hub.uiTasks.Done()
		ip, _, _ := net.SplitHostPort(s.RemoteAddr().String())
		version := string(s.Context().ClientVersion())
		guiContext := context.Context(s.Context())
		if key := s.PublicKey(); key != nil {
			guiContext = context.WithValue(guiContext, guiSSHProofKey{}, key)
		}
		if err := runGUIProtocol(guiContext, s, s, ip, version, closeSession, store, hub); err != nil && !errors.Is(err, context.Canceled) {
			store.Diagnostic("gui.session.error", err.Error())
		}
	}
	srv.Handler = func(s ssh.Session) {
		closeSession := func() { _ = s.Close() }
		// This service permits one shell per connection. Once it exits, retaining
		// the SSH transport would only reserve a connection slot indefinitely.
		if state, ok := s.Context().Value(sshTransportKey{}).(*sshTransportState); ok {
			defer state.close()
			closeSession = func() { _ = state.close() }
		}
		if !hub.beginUI() {
			s.Close()
			return
		}
		defer hub.uiTasks.Done()
		pty, windows, ok := s.Pty()
		if !ok {
			fmt.Fprintln(s, "MioChat needs an interactive terminal. Connect with: ssh -t chat@HOST")
			s.Exit(1)
			return
		}
		ip, _, _ := net.SplitHostPort(s.RemoteAddr().String())
		initialWidth, initialHeight := normalizeTerminalSize(pty.Window.Width, pty.Window.Height)
		m := newModel(store, hub, s.Context(), s, pty.Term, initialWidth, initialHeight, ip, closeSession)
		m.client.Version = string(s.Context().ClientVersion())
		m.sshPublicKey = s.PublicKey()
		if m.sshPublicKey != nil {
			if key, err := store.FindSSHKey(m.sshPublicKey); err == nil {
				u := User{Name: key.User}
				m.sshAutoUser = &u
				m.busy = true
				m.notice = "正在使用设备密钥登录…"
			}
		}
		if state, ok := s.Context().Value(sshTransportKey{}).(*sshTransportState); ok {
			m.authenticatedSignal = state.authenticated
		}
		defer hub.Leave(m.client)
		// SSH sessions are not local TTY files. Prime Bubble Tea's renderer
		// explicitly before allowing a visible frame or deduplicating resizes.
		m.awaitingTerminalSize = true
		p := tea.NewProgram(m, tea.WithContext(s.Context()), tea.WithInput(sshSessionInput(s, closeSession)), tea.WithOutput(s), tea.WithAltScreen(), tea.WithMouseCellMotion())
		go func() {
			p.Send(tea.WindowSizeMsg{Width: initialWidth, Height: initialHeight})
			lastWidth, lastHeight := initialWidth, initialHeight
			var timer *time.Timer
			var ready <-chan time.Time
			var width, height int
			defer func() {
				if timer != nil {
					timer.Stop()
				}
			}()
			for {
				select {
				case size, ok := <-windows:
					if !ok {
						return
					}
					width, height = normalizeTerminalSize(size.Width, size.Height)
					if ready == nil && (width != lastWidth || height != lastHeight) {
						if timer == nil {
							timer = time.NewTimer(50 * time.Millisecond)
						} else {
							timer.Reset(50 * time.Millisecond)
						}
						ready = timer.C
					}
				case <-ready:
					ready = nil
					if width != lastWidth || height != lastHeight {
						p.Send(tea.WindowSizeMsg{Width: width, Height: height})
						lastWidth, lastHeight = width, height
					}
				case <-s.Context().Done():
					return
				}
			}
		}()
		result, err := p.Run()
		if end, ok := result.(model); ok {
			end.saveView()
		}
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, tea.ErrProgramKilled) {
			log.Printf("session ended: %v", err)
			store.Diagnostic("session.error", err.Error())
		}
	}
	return srv
}
func run() error {
	bind := flag.String("listen", "127.0.0.1:2222", "SSH listen address")
	state := flag.String("state", ".state", "Persistent state directory")
	promote := flag.String("promote", "", "Promote an already registered account to administrator and exit")
	owner := flag.String("owner", "", "Grant owner role to a registered account and exit")
	version := flag.Bool("version", false, "Print application version")
	check := flag.Bool("check", false, "Validate configuration and database")
	backup := flag.String("backup", "", "Create a consistent snapshot in a new directory")
	restore := flag.String("restore", "", "Restore a validated snapshot while the instance is stopped")
	dotPipes := flag.Bool("dot-private-pipes", false, "Enable explicitly inherited trusted dot adapter pipes (default off)")
	dotReadFD := flag.Int("dot-read-fd", -1, "Explicit inherited dot request pipe descriptor")
	dotWriteFD := flag.Int("dot-write-fd", -1, "Explicit inherited dot response pipe descriptor")
	flag.Parse()
	dotConfig := dotPipeConfig{Enabled: *dotPipes, ReadFD: *dotReadFD, WriteFD: *dotWriteFD}
	if err := dotConfig.validate(); err != nil {
		return err
	}
	if dotConfig.Enabled && (*version || *check || *promote != "" || *owner != "" || *backup != "" || *restore != "") {
		return errors.New("dot private pipes cannot be mixed with one-shot operator actions")
	}

	if *version {
		fmt.Println(appVersion)
		return nil
	}
	if err := os.MkdirAll(*state, 0700); err != nil {
		return err
	}
	if *backup != "" {
		_, err := MakeSnapshot(*state, *backup)
		return err
	}
	if *restore != "" {
		return RestoreSnapshot(*state, *restore)
	}
	var release func()
	if *promote == "" && *owner == "" {
		var err error
		release, err = lockState(*state)
		if err != nil {
			return err
		}
		defer release()
	}
	store, err := openStore(filepath.Join(*state, "miochat.db"))
	if err != nil {
		return err
	}
	defer store.db.Close()
	if *promote != "" {
		return store.Promote(*promote)
	}
	if *owner != "" {
		return store.PromoteOwner(*owner)
	}
	if *check {
		lines, err := store.Checks()
		for _, line := range lines {
			fmt.Println(line)
		}
		return err
	}
	dotRuntime, err := prepareDotPrivatePipes(store, dotConfig)
	if err != nil {
		return err
	}
	defer dotRuntime.Close()
	if _, err = store.db.Exec(`UPDATE sessions SET closed=? WHERE closed=0`, time.Now().Unix()); err != nil {
		return err
	}
	signer, err := hostSigner(filepath.Join(*state, "host_ed25519"))
	if err != nil {
		return err
	}
	hub := newHub()
	srv := makeServer(store, hub, signer)
	defer func() { hub.stopUI(); srv.Close(); hub.uiTasks.Wait() }()
	listener, err := net.Listen("tcp", *bind)
	if err != nil {
		return err
	}
	log.Printf("MioChat listening on %s · host key %s", listener.Addr(), gossh.FingerprintSHA256(signer.PublicKey()))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() { <-ctx.Done(); hub.stopUI(); srv.Close() }()
	var dotResult chan error
	if dotRuntime != nil {
		dotResult = make(chan error, 1)
		go func() { dotResult <- dotRuntime.Serve(ctx); srv.Close() }()
	}
	err = srv.Serve(listener)
	if dotResult != nil {
		select {
		case dotErr := <-dotResult:
			if dotErr != nil && !errors.Is(dotErr, context.Canceled) {
				return errors.New("trusted dot adapter pipe stopped; integration unavailable")
			}
		default:
		}
	}

	if errors.Is(err, ssh.ErrServerClosed) {
		return nil
	}
	return err
}
func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
