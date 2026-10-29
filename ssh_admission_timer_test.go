package main

import (
	ssh "charm.land/ssh"
	"context"
	gossh "golang.org/x/crypto/ssh"
	"net"
	"sync"
	"testing"
	"time"
)

// Synthetic ConnCallback context: no listener, SSH handshake or real account.
type admissionTimerContext struct {
	context.Context
	sync.Mutex
	values sync.Map
}

func (c *admissionTimerContext) Value(k any) any {
	if v, ok := c.values.Load(k); ok {
		return v
	}
	return c.Context.Value(k)
}
func (c *admissionTimerContext) SetValue(k, v any)   { c.values.Store(k, v) }
func (*admissionTimerContext) User() string          { return "synthetic" }
func (*admissionTimerContext) SessionID() string     { return "synthetic" }
func (*admissionTimerContext) ClientVersion() string { return "synthetic" }
func (*admissionTimerContext) ServerVersion() string { return "synthetic" }
func (*admissionTimerContext) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP("192.0.2.27"), Port: 1024}
}
func (*admissionTimerContext) LocalAddr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1025}
}
func (*admissionTimerContext) Permissions() *ssh.Permissions {
	return &ssh.Permissions{Permissions: &gossh.Permissions{}}
}

var _ ssh.Context = (*admissionTimerContext)(nil)

type admissionTimerConn struct {
	net.Conn
	once   sync.Once
	closed chan struct{}
}

func (c *admissionTimerConn) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP("192.0.2.27"), Port: 1024}
}
func (c *admissionTimerConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}
func admissionTimerPair(t *testing.T) (*admissionTimerContext, *admissionTimerConn, context.CancelFunc) {
	t.Helper()
	base, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	a, b := net.Pipe()
	c := &admissionTimerConn{Conn: a, closed: make(chan struct{})}
	t.Cleanup(func() { _ = c.Close(); _ = b.Close() })
	return &admissionTimerContext{Context: base}, c, cancel
}
func TestSSHAdmissionDeniedNeverStartsApplicationTimer(t *testing.T) {
	for _, reason := range []string{"banned", "policy-query-error", "cancelled"} {
		t.Run(reason, func(t *testing.T) {
			s := testStore(t)
			h := newHub()
			srv := makeServerWithPolicy(s, h, transportSigner(t), defaultSSHTransportPolicy())
			ctx, c, cancel := admissionTimerPair(t)
			switch reason {
			case "banned":
				if _, e := s.db.Exec(`INSERT INTO sanctions(kind,target,room,expires,reason,actor,at) VALUES('ip','192.0.2.0/24','',0,'synthetic','synthetic',1)`); e != nil {
					t.Fatal(e)
				}
			case "policy-query-error":
				if _, e := s.db.Exec(`DROP TABLE sanctions`); e != nil {
					t.Fatal(e)
				}
			case "cancelled":
				cancel()
			}
			if srv.ConnCallback(ctx, c) != nil {
				t.Fatal("admission unexpectedly allowed")
			}
			select {
			case <-c.closed:
			default:
				t.Fatal("denied socket not closed")
			}
			if ctx.Value(sshTransportKey{}) != nil {
				t.Error("DENIED_ADMISSION_CREATED_APPLICATION_TIMER_STATE")
			}
		})
	}
}
func TestSSHAdmissionRejectReleasesReservationExactlyOnce(t *testing.T) {
	s := testStore(t)
	h := newHub()
	srv := makeServerWithPolicy(s, h, transportSigner(t), defaultSSHTransportPolicy())
	if _, e := s.db.Exec(`INSERT INTO sanctions(kind,target,room,expires,reason,actor,at) VALUES('ip','192.0.2.0/24','',0,'synthetic','synthetic',1)`); e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 12; i++ {
		ctx, c, _ := admissionTimerPair(t)
		if srv.ConnCallback(ctx, c) != nil {
			t.Fatal("denied allowed")
		}
	}
	if _, e := s.db.Exec(`UPDATE sanctions SET revoked=1`); e != nil {
		t.Fatal(e)
	}
	s.invalidateIPPolicy()
	var admitted []net.Conn
	for i := 0; i < 8; i++ {
		ctx, c, _ := admissionTimerPair(t)
		v := srv.ConnCallback(ctx, c)
		if v == nil || ctx.Value(sshTransportKey{}) == nil {
			t.Fatal("denied reservation leaked", i)
		}
		admitted = append(admitted, v)
	}
	ctx, c, _ := admissionTimerPair(t)
	if srv.ConnCallback(ctx, c) != nil || ctx.Value(sshTransportKey{}) != nil {
		t.Fatal("per-IP capacity not enforced before timer")
	}
	_ = admitted[0].Close()
	_ = admitted[0].Close()
	ctx, c, _ = admissionTimerPair(t)
	extra := srv.ConnCallback(ctx, c)
	if extra == nil {
		t.Fatal("one slot not released")
	}
	defer extra.Close()
	ctx, c, _ = admissionTimerPair(t)
	if srv.ConnCallback(ctx, c) != nil {
		t.Fatal("double close released twice")
	}
	for _, v := range admitted {
		_ = v.Close()
	}
}
func TestSSHAdmissionAcceptedApplicationTimeoutStillApplies(t *testing.T) {
	defaults := defaultSSHTransportPolicy()
	if defaults.HandshakeTimeout != 30*time.Second || defaults.ApplicationLoginTimeout != 5*time.Minute {
		t.Fatal("default budgets changed")
	}
	for _, authenticated := range []bool{false, true} {
		t.Run(map[bool]string{false: "incomplete", true: "authenticated"}[authenticated], func(t *testing.T) {
			s := testStore(t)
			policy := defaults
			policy.ApplicationLoginTimeout = 250 * time.Millisecond
			srv := makeServerWithPolicy(s, newHub(), transportSigner(t), policy)
			ctx, c, _ := admissionTimerPair(t)
			got := srv.ConnCallback(ctx, c)
			if got == nil {
				t.Fatal("accepted address denied")
			}
			defer got.Close()
			state, ok := ctx.Value(sshTransportKey{}).(*sshTransportState)
			if !ok {
				t.Fatal("missing transport state")
			}
			state.authenticated.Store(authenticated)
			if authenticated {
				select {
				case <-c.closed:
					t.Fatal("authenticated idle connection closed")
				case <-time.After(350 * time.Millisecond):
				}
			}
			if !authenticated {
				select {
				case <-c.closed:
				case <-time.After(time.Second):
					t.Fatal("incomplete login not timed out")
				}
			}
		})
	}
}
