//go:build linux

package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"
)

func TestLive64(t *testing.T) {
	if os.Getenv("MIOCHAT_LOAD_TEST") != "1" {
		t.Skip("set MIOCHAT_LOAD_TEST=1 for the isolated 64-client load gate")
	}
	s := testStore(t)
	for i := 0; i < 64; i++ {
		name := fmt.Sprintf("load%02d", i)
		registerUser(t, s, name)
		// The load gate exercises chat fanout; onboarding is covered by UI tests.
		if err := s.Setting(name, "key_reminder", "off"); err != nil {
			t.Fatal(err)
		}
	}
	if _, e := s.db.Exec(`WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<50000) INSERT INTO messages(room,sender,recipient,body,kind,at) SELECT 'lobby','load00','','中文历史 seed '||x,'chat',1700000000 FROM n`); e != nil {
		t.Fatal(e)
	}
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := gossh.NewSignerFromKey(key)
	hub := newHub()
	srv := makeServer(s, hub, signer)
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	go srv.Serve(listener)
	t.Cleanup(func() { srv.Close() })
	clients := make([]*liveTerminal, 64)
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			source := net.ParseIP(fmt.Sprintf("127.0.0.%d", i/8+2))
			dialer := net.Dialer{LocalAddr: &net.TCPAddr{IP: source}, Timeout: 5 * time.Second}
			raw, e := dialer.Dial("tcp", listener.Addr().String())
			if e != nil {
				t.Error(e)
				return
			}
			connection, channels, requests, e := gossh.NewClientConn(raw, listener.Addr().String(), &gossh.ClientConfig{User: "chat", Auth: []gossh.AuthMethod{appUIAuthentication()}, HostKeyCallback: gossh.FixedHostKey(signer.PublicKey())})
			if e != nil {
				raw.Close()
				t.Error(e)
				return
			}
			client := gossh.NewClient(connection, channels, requests)
			session, e := client.NewSession()
			if e != nil {
				client.Close()
				t.Error(e)
				return
			}
			terminal := &liveTerminal{client: client, session: session}
			terminal.in, e = session.StdinPipe()
			if e != nil {
				t.Error(e)
				return
			}
			session.Stdout = terminal
			session.Stderr = terminal
			if e = session.RequestPty("xterm-256color", 30, 100, gossh.TerminalModes{gossh.ECHO: 0}); e != nil {
				t.Error(e)
				return
			}
			if e = session.Shell(); e != nil {
				t.Error(e)
				return
			}
			clients[i] = terminal
			t.Cleanup(func() { session.Close(); client.Close() })
			terminal.wait(t, "TERMINAL SOCIAL CLUB")
			io.WriteString(terminal.in, fmt.Sprintf("load%02d\r", i))
			time.Sleep(80 * time.Millisecond)
			io.WriteString(terminal.in, "test-password-123\r")
			terminal.wait(t, fmt.Sprintf("欢迎，load%02d", i))
		}(i)
	}
	wg.Wait()
	if t.Failed() {
		return
	}
	if len(hub.Online()) != 64 {
		t.Fatalf("only %d accounts online", len(hub.Online()))
	}
	for i, c := range clients {
		wg.Add(1)
		go func(i int, c *liveTerminal) {
			defer wg.Done()
			io.WriteString(c.in, fmt.Sprintf("并发消息 %02d\r", i))
		}(i, c)
	}
	wg.Wait()
	deadline := time.Now().Add(10 * time.Second)
	count := 0
	for time.Now().Before(deadline) {
		s.db.QueryRow(`SELECT count(*) FROM messages`).Scan(&count)
		if count == 50064 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if count != 50064 {
		t.Fatalf("concurrent messages missing: %d", count)
	}
	start := time.Now()
	io.WriteString(clients[0].in, "LOAD_FINAL_MARKER\r")
	latencies := make([]time.Duration, 64)
	for i, c := range clients {
		wg.Add(1)
		go func(i int, c *liveTerminal) {
			defer wg.Done()
			limit := time.Now().Add(10 * time.Second)
			for time.Now().Before(limit) {
				if strings.Contains(c.output(), "LOAD_FINAL_MARKER") {
					latencies[i] = time.Since(start)
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
			t.Errorf("client %d missed final broadcast", i)
		}(i, c)
	}
	wg.Wait()
	var peak time.Duration
	for _, v := range latencies {
		peak = max(peak, v)
	}
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	t.Logf("64 real SSH clients; 50,000 historical rows; 64 concurrent posts; full fanout %s; combined server+clients Go heap %.1f MiB", peak, float64(memory.HeapAlloc)/(1<<20))
}
