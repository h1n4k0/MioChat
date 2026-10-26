package main

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ssh "charm.land/ssh"
	gossh "golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func transportSigner(t *testing.T) gossh.Signer {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

func transportAnonymousAuth() gossh.AuthMethod {
	return gossh.KeyboardInteractive(func(_, _ string, questions []string, _ []bool) ([]string, error) {
		if len(questions) != 0 {
			return nil, fmt.Errorf("application fallback unexpectedly asked an SSH password")
		}
		return []string{}, nil
	})
}

type transportFixtureMode int

const (
	transportWait transportFixtureMode = iota
	transportFinish
	transportEcho
)

func transportFixture(t *testing.T, policy sshTransportPolicy, authenticate bool, modes ...transportFixtureMode) (string, gossh.Signer, *ssh.Server) {
	t.Helper()
	store := testStore(t)
	hub := newHub()
	signer := transportSigner(t)
	server := makeServerWithPolicy(store, hub, signer, policy)
	mode := transportWait
	if len(modes) > 0 {
		mode = modes[0]
	}
	server.Handler = func(session ssh.Session) {
		if state, ok := session.Context().Value(sshTransportKey{}).(*sshTransportState); ok && authenticate {
			state.authenticated.Store(true)
		}
		if key := session.PublicKey(); key != nil {
			fmt.Fprintln(session, gossh.FingerprintSHA256(key))
		} else {
			fmt.Fprintln(session, "anonymous")
		}
		if mode == transportFinish {
			_ = session.Exit(0)
			return
		}
		if mode == transportEcho {
			_, _ = io.Copy(session, session)
		} else {
			_, _ = io.Copy(io.Discard, session)
		}
	}
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
	return listener.Addr().String(), signer, server
}

func transportShell(t *testing.T, address string, host gossh.Signer, auth ...gossh.AuthMethod) (*gossh.Client, *gossh.Session, string) {
	t.Helper()
	client, err := gossh.Dial("tcp", address, &gossh.ClientConfig{User: "chat", Auth: auth, HostKeyCallback: gossh.FixedHostKey(host.PublicKey()), Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Shell(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	return client, session, strings.TrimSpace(line)
}

func TestSSHTransportSafeAlgorithmsAndAnonymousFallback(t *testing.T) {
	address, host, server := transportFixture(t, defaultSSHTransportPolicy(), true)
	if server.IdleTimeout != 0 || server.MaxTimeout != 0 || server.HandshakeTimeout == 0 {
		t.Fatal("connected users must not have idle/absolute expiry, but handshakes must be bounded")
	}
	config := server.ServerConfigCallback(nil)
	if config.NoClientAuth {
		t.Fatal("none authentication prevents clients from offering their public keys")
	}
	for _, algorithm := range config.PublicKeyAuthAlgorithms {
		if algorithm == gossh.KeyAlgoRSA || algorithm == "ssh-dss" {
			t.Fatalf("unsafe signature algorithm offered: %s", algorithm)
		}
	}
	key := transportSigner(t)
	_, _, identity := transportShell(t, address, host, gossh.PublicKeys(key), transportAnonymousAuth())
	if identity != gossh.FingerprintSHA256(key.PublicKey()) {
		t.Fatal("signed key was not attached to the authenticated session")
	}
	_, _, identity = transportShell(t, address, host, transportAnonymousAuth())
	if identity != "anonymous" {
		t.Fatal("keyless fallback acquired a public-key identity")
	}
}

type transportWrongSigner struct {
	public gossh.PublicKey
	wrong  gossh.Signer
}

func (s transportWrongSigner) PublicKey() gossh.PublicKey { return s.public }
func (s transportWrongSigner) Sign(random io.Reader, data []byte) (*gossh.Signature, error) {
	return s.wrong.Sign(random, data)
}

type transportUnsignedOffer struct{ public gossh.PublicKey }

func (s transportUnsignedOffer) PublicKey() gossh.PublicKey { return s.public }
func (s transportUnsignedOffer) Sign(io.Reader, []byte) (*gossh.Signature, error) {
	return nil, fmt.Errorf("client declined signing after the unsigned key probe")
}

func TestSSHTransportUnsignedOfferDoesNotCarryIdentityToFallback(t *testing.T) {
	address, host, _ := transportFixture(t, defaultSSHTransportPolicy(), true)
	claim, wrong := transportSigner(t), transportSigner(t)
	unsigned := transportUnsignedOffer{public: claim.PublicKey()}
	_, _, identity := transportShell(t, address, host, gossh.PublicKeys(unsigned), transportAnonymousAuth())
	if identity != "anonymous" {
		t.Fatal("an offered key without a valid signature became a session identity")
	}
	forged := transportWrongSigner{public: claim.PublicKey(), wrong: wrong}
	client, err := gossh.Dial("tcp", address, &gossh.ClientConfig{User: "chat", Auth: []gossh.AuthMethod{gossh.PublicKeys(forged)}, HostKeyCallback: gossh.FixedHostKey(host.PublicKey()), Timeout: time.Second})
	if err == nil {
		client.Close()
		t.Fatal("a signature produced by a different private key was accepted")
	}
}

func TestSSHTransportRSASHA2AcceptedSHA1Rejected(t *testing.T) {
	address, host, _ := transportFixture(t, defaultSSHTransportPolicy(), true)
	private, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	_, _, identity := transportShell(t, address, host, gossh.PublicKeys(signer))
	if identity != gossh.FingerprintSHA256(signer.PublicKey()) {
		t.Fatal("RSA-SHA2 failed")
	}
	sha1Only, err := gossh.NewSignerWithAlgorithms(signer.(gossh.AlgorithmSigner), []string{gossh.KeyAlgoRSA})
	if err != nil {
		t.Fatal(err)
	}
	client, err := gossh.Dial("tcp", address, &gossh.ClientConfig{User: "chat", Auth: []gossh.AuthMethod{gossh.PublicKeys(sha1Only)}, HostKeyCallback: gossh.FixedHostKey(host.PublicKey()), Timeout: time.Second})
	if err == nil {
		client.Close()
		t.Fatal("SHA-1 RSA signature was accepted")
	}
}

func TestSSHTransportIdleAuthenticatedClientAndLoginDeadline(t *testing.T) {
	policy := sshTransportPolicy{HandshakeTimeout: time.Second, ApplicationLoginTimeout: 100 * time.Millisecond, KeepaliveInterval: 20 * time.Millisecond, KeepaliveMisses: 3}
	address, host, _ := transportFixture(t, policy, true)
	client, _, _ := transportShell(t, address, host, transportAnonymousAuth())
	// The standard Go SSH client rejects unknown global keepalive requests with
	// a failure response. That is still a valid liveness response.
	time.Sleep(350 * time.Millisecond)
	if _, _, err := client.SendRequest("still-here", true, nil); err != nil {
		t.Fatalf("authenticated user was kicked without keyboard input: %v", err)
	}
	address, host, _ = transportFixture(t, policy, false)
	anonymous, _, _ := transportShell(t, address, host, transportAnonymousAuth())
	done := make(chan error, 1)
	go func() { done <- anonymous.Wait() }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("unfinished application login occupied a connection indefinitely")
	}
}

func transportIgnoringGlobals(t *testing.T, address string, host gossh.Signer) (gossh.Conn, gossh.Channel) {
	t.Helper()
	raw, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	connection, channels, requests, err := gossh.NewClientConn(raw, address, &gossh.ClientConfig{User: "chat", Auth: []gossh.AuthMethod{transportAnonymousAuth()}, HostKeyCallback: gossh.FixedHostKey(host.PublicKey()), Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	go func() {
		for range requests {
			// Consume requests without answering: model a stuck SSH peer.
		}
	}()
	_ = channels
	channel, channelRequests, err := connection.OpenChannel("session", nil)
	if err != nil {
		t.Fatal(err)
	}
	go gossh.DiscardRequests(channelRequests)
	if _, err = channel.SendRequest("shell", true, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = bufio.NewReader(channel).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	return connection, channel
}

func TestSSHTransportUnansweredKeepaliveClosesConnection(t *testing.T) {
	policy := sshTransportPolicy{HandshakeTimeout: time.Second, ApplicationLoginTimeout: time.Second, KeepaliveInterval: 20 * time.Millisecond, KeepaliveMisses: 3}
	address, host, _ := transportFixture(t, policy, true)
	connection, _ := transportIgnoringGlobals(t, address, host)
	done := make(chan error, 1)
	go func() { done <- connection.Wait() }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("dead keepalive connection was not closed")
	}
}

func TestSSHTransportActivePeerCanIgnoreKeepalives(t *testing.T) {
	policy := sshTransportPolicy{HandshakeTimeout: time.Second, ApplicationLoginTimeout: time.Second, KeepaliveInterval: 20 * time.Millisecond, KeepaliveMisses: 3}
	address, host, _ := transportFixture(t, policy, true)
	connection, channel := transportIgnoringGlobals(t, address, host)
	for i := 0; i < 20; i++ {
		if _, err := channel.Write([]byte("x")); err != nil {
			t.Fatal("active client was disconnected merely for ignoring keepalive requests:", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, _, err := connection.SendRequest("still-here", true, nil); err != nil {
		t.Fatal("active SSH traffic was not considered live:", err)
	}
}

func TestSSHTransportHandshakeDeadline(t *testing.T) {
	policy := sshTransportPolicy{HandshakeTimeout: 60 * time.Millisecond, ApplicationLoginTimeout: time.Second, KeepaliveInterval: time.Second, KeepaliveMisses: 3}
	address, _, _ := transportFixture(t, policy, false)
	connection, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_ = connection.SetReadDeadline(time.Now().Add(time.Second))
	reader := bufio.NewReader(connection)
	if _, err = reader.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	if _, err = reader.ReadByte(); err == nil {
		t.Fatal("unfinished handshake remained open")
	} else if e, ok := err.(net.Error); ok && e.Timeout() {
		t.Fatal("connection only closed at the test's own read deadline")
	}
}

func TestSSHTransportNativeOpenSSHKeylessCommand(t *testing.T) {
	executable, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("native OpenSSH client unavailable")
	}
	address, host, _ := transportFixture(t, defaultSSHTransportPolicy(), true, transportFinish)
	_, port, _ := net.SplitHostPort(address)
	known := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(known, []byte(knownhosts.Line([]string{address}, host.PublicKey())+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "-F", "none", "-p", port, "-o", "UserKnownHostsFile="+known, "-o", "StrictHostKeyChecking=yes", "-o", "PreferredAuthentications=keyboard-interactive", "-o", "ConnectTimeout=3", "-T", "chat@127.0.0.1")
	output, err := command.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "anonymous") {
		t.Fatalf("ordinary SSH command could not reach the account form: %v: %s", err, output)
	}
}

func TestSSHTransportLongIdle(t *testing.T) {
	if os.Getenv("MIOCHAT_LONG_IDLE_TEST") != "1" {
		t.Skip("set MIOCHAT_LONG_IDLE_TEST=1 to exercise a real 130-second idle SSH connection")
	}
	address, host, server := transportFixture(t, defaultSSHTransportPolicy(), true, transportEcho)
	if server.IdleTimeout != 0 || server.MaxTimeout != 0 {
		t.Fatal("authenticated sessions must not have an idle or absolute expiry")
	}
	key := transportSigner(t)
	client, err := gossh.Dial("tcp", address, &gossh.ClientConfig{User: "chat", Auth: []gossh.AuthMethod{gossh.PublicKeys(key)}, HostKeyCallback: gossh.FixedHostKey(host.PublicKey()), Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := session.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = session.Shell(); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(stdout)
	identity, err := reader.ReadString('\n')
	if err != nil || strings.TrimSpace(identity) != gossh.FingerprintSHA256(key.PublicKey()) {
		t.Fatal("real SSH key authentication did not establish the expected session", err)
	}
	disconnected := make(chan error, 1)
	go func() { disconnected <- client.Wait() }()
	idleStarted := time.Now()
	t.Log("leaving the authenticated SSH channel idle for 130 seconds; only SSH keepalive responses are allowed")
	timer := time.NewTimer(130 * time.Second)
	defer timer.Stop()
	select {
	case err := <-disconnected:
		t.Fatalf("authenticated client disconnected while idle after %v: %v", time.Since(idleStarted), err)
	case <-timer.C:
	}
	if _, err = stdin.Write([]byte("x")); err != nil {
		t.Fatal("channel could not send a byte after the old two-minute threshold:", err)
	}
	echoed := make(chan error, 1)
	go func() {
		value, err := reader.ReadByte()
		if err == nil && value != 'x' {
			err = fmt.Errorf("unexpected echo byte: %q", value)
		}
		echoed <- err
	}()
	select {
	case err := <-echoed:
		if err != nil {
			t.Fatal("channel did not echo the byte after prolonged idle:", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("channel stopped responding after prolonged idle")
	}
	t.Logf("real signed SSH channel remained usable after %.3f seconds without application input", time.Since(idleStarted).Seconds())
}

func transportProductionFixture(t *testing.T) (*Store, *Hub, string, gossh.Signer) {
	t.Helper()
	store, hub := testStore(t), newHub()
	host := transportSigner(t)
	server := makeServer(store, hub, host)
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
	return store, hub, listener.Addr().String(), host
}

func transportClientConfig(host gossh.Signer, methods ...gossh.AuthMethod) *gossh.ClientConfig {
	return &gossh.ClientConfig{User: "chat", Auth: methods, HostKeyCallback: gossh.FixedHostKey(host.PublicKey()), Timeout: time.Second}
}

func transportExpectClosed(t *testing.T, client *gossh.Client) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- client.Wait() }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("finished single-channel session kept its SSH transport and connection slot alive")
	}
}

func TestSSHTransportNoPTYExitRestoresPerIPQuota(t *testing.T) {
	_, _, address, host := transportProductionFixture(t)
	clients := make([]*gossh.Client, 0, 8)
	for i := 0; i < 8; i++ {
		client, err := gossh.Dial("tcp", address, transportClientConfig(host, transportAnonymousAuth()))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = client.Close() })
		clients = append(clients, client)
	}
	if client, err := gossh.Dial("tcp", address, transportClientConfig(host, transportAnonymousAuth())); err == nil {
		client.Close()
		t.Fatal("ninth simultaneous connection from one IP bypassed the quota")
	}
	session, err := clients[0].NewSession()
	if err != nil {
		t.Fatal(err)
	}
	session.Stdout, session.Stderr = io.Discard, io.Discard
	_ = session.Shell() // No PTY: the production handler deliberately rejects it.
	transportExpectClosed(t, clients[0])
	deadline := time.Now().Add(2 * time.Second)
	for {
		client, err := gossh.Dial("tcp", address, transportClientConfig(host, transportAnonymousAuth()))
		if err == nil {
			client.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("closed single-channel connection did not release its per-IP slot:", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSSHTransportRejectsSecondSessionChannelBeforeShell(t *testing.T) {
	address, host, _ := transportFixture(t, defaultSSHTransportPolicy(), true)
	client, err := gossh.Dial("tcp", address, transportClientConfig(host, transportAnonymousAuth()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	first, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	second, err := client.NewSession()
	if err == nil {
		second.Close()
		t.Fatal("second session channel was accepted before the first shell even started")
	}
	var rejection *gossh.OpenChannelError
	if !errors.As(err, &rejection) || rejection.Reason != gossh.ResourceShortage {
		t.Fatal("second session was not rejected with the explicit channel resource limit:", err)
	}
	stdout, err := first.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = first.Shell(); err != nil {
		t.Fatal("rejecting another channel damaged the original channel:", err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "anonymous" {
		t.Fatal("original channel became unusable after rejecting another session:", err)
	}
}

func TestSSHTransportProductionSessionCloseReleasesConnection(t *testing.T) {
	for _, name := range []string{"ctrl-c", "close-write", "close-channel", "hub-kick", "revoke-key"} {
		t.Run(name, func(t *testing.T) {
			store, hub, address, host := transportProductionFixture(t)
			registerUser(t, store, "exituser")
			key := transportSigner(t)
			bound, err := store.BindSSHKey("exituser", "synthetic fixture", string(gossh.MarshalAuthorizedKey(key.PublicKey())))
			if err != nil {
				t.Fatal(err)
			}
			client, err := gossh.Dial("tcp", address, transportClientConfig(host, gossh.PublicKeys(key)))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = client.Close() })
			session, err := client.NewSession()
			if err != nil {
				t.Fatal(err)
			}
			stdin, err := session.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			session.Stdout, session.Stderr = io.Discard, io.Discard
			if err = session.RequestPty("xterm-256color", 30, 100, gossh.TerminalModes{}); err != nil {
				t.Fatal(err)
			}
			if err = session.Shell(); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(2 * time.Second)
			for {
				var attached int
				if err = store.db.QueryRow(`SELECT count(*) FROM session_credentials c JOIN sessions s ON s.id=c.session_id WHERE c.key_id=? AND s.user='exituser' AND s.closed=0 AND s.revoked=0`, bound.ID).Scan(&attached); err != nil {
					t.Fatal(err)
				}
				if attached == 1 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("real production UI did not finish key login")
				}
				time.Sleep(10 * time.Millisecond)
			}
			switch name {
			case "ctrl-c":
				_, err = stdin.Write([]byte{3})
			case "close-write":
				err = stdin.Close()
			case "close-channel":
				err = session.Close()
			case "hub-kick":
				hub.Kick("exituser")
			case "revoke-key":
				err = store.RevokeSSHKey("exituser", bound.ID)
				hub.Enforce()
			}
			if err != nil {
				t.Fatal(err)
			}
			transportExpectClosed(t, client)
			deadline = time.Now().Add(2 * time.Second)
			for len(hub.Online()) != 0 {
				if time.Now().After(deadline) {
					t.Fatal("terminated connection left a player in the online hub")
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}
