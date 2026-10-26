package main

import (
	"context"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	ssh "charm.land/ssh"
	gossh "golang.org/x/crypto/ssh"
)

// An open, authenticated chat has no keyboard-idle deadline. These limits
// instead bound incomplete logins and connections that stop answering SSH.
type sshTransportPolicy struct {
	HandshakeTimeout        time.Duration
	ApplicationLoginTimeout time.Duration
	KeepaliveInterval       time.Duration
	KeepaliveMisses         int
}

func defaultSSHTransportPolicy() sshTransportPolicy {
	return sshTransportPolicy{
		HandshakeTimeout:        30 * time.Second,
		ApplicationLoginTimeout: 5 * time.Minute,
		KeepaliveInterval:       30 * time.Second,
		KeepaliveMisses:         3,
	}
}

type sshTransportKey struct{}

type sshTransportState struct {
	authenticated *atomic.Bool
	received      *atomic.Uint64
	close         func() error
	channelOpened atomic.Bool
	keepaliveOnce sync.Once
}

func configureSSHTransport(srv *ssh.Server, policy sshTransportPolicy) {
	srv.HandshakeTimeout = policy.HandshakeTimeout
	srv.IdleTimeout = 0
	srv.MaxTimeout = 0
	srv.ServerConfigCallback = func(ssh.Context) *gossh.ServerConfig {
		algorithms := gossh.SupportedAlgorithms()
		return &gossh.ServerConfig{
			Config: gossh.Config{
				Ciphers:      algorithms.Ciphers,
				KeyExchanges: algorithms.KeyExchanges,
				MACs:         algorithms.MACs,
			},
			PublicKeyAuthAlgorithms: algorithms.PublicKeyAuths,
			MaxAuthTries:            10,
		}
	}
	// Accepting a key here proves possession at the SSH layer only. Account
	// authority is resolved from Session.PublicKey after signature verification.
	// An unbound key still has to pass the ordinary application password login.
	srv.PublicKeyHandler = func(_ ssh.Context, key ssh.PublicKey) bool {
		return ValidateSSHKey(key) == nil
	}
	// OpenSSH first probes "none"; accepting that would prevent it from ever
	// offering a key. A zero-prompt keyboard-interactive fallback preserves the
	// existing terminal account/password form for users without a key.
	srv.KeyboardInteractiveHandler = func(_ ssh.Context, _ gossh.KeyboardInteractiveChallenge) bool { return true }
	srv.ChannelHandlers["session"] = func(server *ssh.Server, conn *gossh.ServerConn, channel gossh.NewChannel, ctx ssh.Context) {
		if state, ok := ctx.Value(sshTransportKey{}).(*sshTransportState); ok {
			if !state.channelOpened.CompareAndSwap(false, true) {
				_ = channel.Reject(gossh.ResourceShortage, "MioChat permits one session channel per SSH connection")
				return
			}
			state.keepaliveOnce.Do(func() {
				go runSSHKeepalives(ctx, conn, state.close, state.received, policy)
			})
		}
		ssh.DefaultSessionHandler(server, conn, channel, ctx)
	}
}

func initializeSSHConnection(ctx ssh.Context, conn *trackedConn, policy sshTransportPolicy) {
	if tcp, ok := conn.Conn.(*net.TCPConn); ok {
		_ = tcp.SetKeepAliveConfig(net.KeepAliveConfig{Enable: true, Idle: 30 * time.Second, Interval: 10 * time.Second, Count: 3})
	}
	state := &sshTransportState{authenticated: &atomic.Bool{}, received: &conn.received, close: conn.Close}
	ctx.SetValue(sshTransportKey{}, state)
	if policy.ApplicationLoginTimeout <= 0 {
		return
	}
	go func() {
		timer := time.NewTimer(policy.ApplicationLoginTimeout)
		defer timer.Stop()
		select {
		case <-ctx.Done():
		case <-timer.C:
			if !state.authenticated.Load() {
				_ = state.close()
			}
		}
	}()
}

type sshRequestSender interface {
	SendRequest(string, bool, []byte) (bool, []byte, error)
}

type sshSessionReader struct {
	reader          io.Reader
	closeConnection func()
	closeOnce       sync.Once
}

func sshSessionInput(reader io.Reader, closeConnection func()) io.Reader {
	return &sshSessionReader{reader: reader, closeConnection: closeConnection}
}

func (reader *sshSessionReader) Read(buffer []byte) (int, error) {
	n, err := reader.reader.Read(buffer)
	if err != nil {
		// Bubble Tea treats input EOF as an ended reader, not necessarily a
		// finished program. One-channel sessions must release their transport.
		reader.closeOnce.Do(reader.closeConnection)
	}
	return n, err
}

func runSSHKeepalives(ctx context.Context, sender sshRequestSender, closeConn func() error, received *atomic.Uint64, policy sshTransportPolicy) {
	if policy.KeepaliveInterval <= 0 || policy.KeepaliveMisses <= 0 {
		return
	}
	ticker := time.NewTicker(policy.KeepaliveInterval)
	defer ticker.Stop()
	var pending <-chan error
	missed := 0
	var lastReceived uint64
	if received != nil {
		lastReceived = received.Load()
	}
	for {
		select {
		case <-ctx.Done():
			_ = closeConn()
			return
		case err := <-pending:
			if err != nil {
				_ = closeConn()
				return
			}
			// A failure response means "unsupported request", not a dead peer.
			pending = nil
			missed = 0
		case <-ticker.C:
			if pending != nil {
				// Some clients fail to answer unknown global requests. Actual
				// inbound SSH traffic still proves they are present and usable.
				if received != nil {
					current := received.Load()
					if current != lastReceived {
						lastReceived = current
						missed = 0
						continue
					}
				}
				missed++
				if missed >= policy.KeepaliveMisses {
					_ = closeConn()
					return
				}
				continue
			}
			result := make(chan error, 1)
			if received != nil {
				lastReceived = received.Load()
			}
			pending = result
			go func() {
				_, _, err := sender.SendRequest("keepalive@openssh.com", true, nil)
				result <- err
			}()
		}
	}
}
