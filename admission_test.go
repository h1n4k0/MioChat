package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"
)

func TestAuthSourceLimiterHasAHardCapacityAndExpiresWithoutEviction(t *testing.T) {
	var limiter authLimiter
	now := time.Unix(1700000000, 0)
	first := "2001:db8::1"
	if !limiter.allow(first, now, time.Minute, 1) {
		t.Fatal("first source was rejected")
	}
	accepted := 1
	for n := 2; n <= 10000; n++ {
		address := fmt.Sprintf("2001:db8::%x", n)
		if _, err := netip.ParseAddr(address); err != nil {
			t.Fatal("test did not use a valid source address", err)
		}
		if limiter.allow(address, now, time.Minute, 1) {
			accepted++
		}
	}
	if accepted != maxAuthSources || len(limiter.sources) != maxAuthSources || len(limiter.expiry) != maxAuthSources {
		t.Fatal("source limiter exceeded its hard capacity", accepted, len(limiter.sources), len(limiter.expiry))
	}
	if limiter.allow(first, now.Add(59*time.Second), time.Minute, 1) {
		t.Fatal("new source churn evicted a live source's exhausted limit")
	}
	if !limiter.allow("2001:db8:1::1", now.Add(time.Minute), time.Minute, 1) {
		t.Fatal("expired capacity was not released at the exact boundary")
	}
	if len(limiter.sources) != 1 || len(limiter.expiry) != 1 {
		t.Fatal("expired entries remained in the map or expiry heap")
	}
	if !limiter.allow(first, now.Add(time.Minute), time.Minute, 1) {
		t.Fatal("expired source did not receive a fresh window")
	}
}

func TestIPPolicyInvalidatesImmediatelyAndHonorsExpiry(t *testing.T) {
	s, _ := featureFixture(t)
	address := "198.51.100.27"
	if s.IPBannedContext(context.Background(), address) {
		t.Fatal("clean address was denied")
	}
	if err := s.Sanction("admin", "ip", "198.51.100.0/24", "", "1h", "synthetic", false); err != nil {
		t.Fatal(err)
	}
	if !s.IPBannedContext(context.Background(), address) {
		t.Fatal("new sanction reused a stale allow cache")
	}
	if err := s.Sanction("admin", "ip", "198.51.100.0/24", "", "0", "", true); err != nil {
		t.Fatal(err)
	}
	if s.IPBannedContext(context.Background(), address) {
		t.Fatal("removed sanction reused a stale deny cache")
	}
	// Evaluate the loader's expiry using a deterministic clock, without waiting
	// for wall-clock seconds to tick over.
	now := time.Unix(1700000000, 500000000)
	expires := now.Unix() + 1
	if _, err := s.db.Exec(`INSERT INTO sanctions(kind,target,room,expires,reason,actor,at) VALUES('ip','198.51.100.0/24','',?,'synthetic','admin',?)`, expires, now.Unix()); err != nil {
		t.Fatal(err)
	}
	prefixes, until, err := s.loadIPPolicy(context.Background(), now)
	if err != nil || len(prefixes) != 1 || !until.Equal(time.Unix(expires, 0)) {
		t.Fatal("cache lifetime exceeds the earliest sanction expiry", prefixes, until, err)
	}
	prefixes, _, err = s.loadIPPolicy(context.Background(), time.Unix(expires, 0))
	if err != nil || len(prefixes) != 0 {
		t.Fatal("expired sanction remained effective", prefixes, err)
	}
	s.ipPolicy.mu.Lock()
	s.ipPolicy.prefixes = []netip.Prefix{netip.MustParsePrefix("198.51.100.0/24")}
	s.ipPolicy.until = time.Now().Add(-time.Second)
	s.ipPolicy.mu.Unlock()
	if s.IPBannedContext(context.Background(), address) {
		t.Fatal("expired cached deny policy was not reloaded")
	}
}

func TestIPPolicyDatabaseWaitIsCancellableAndFailsClosed(t *testing.T) {
	s := testStore(t)
	reader := s.policyDB()
	reader.SetMaxOpenConns(1)
	lease, err := reader.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	started := time.Now()
	if !s.IPBannedContext(ctx, "198.51.100.27") {
		t.Fatal("admission allowed a source when the policy query was cancelled")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatal("policy query ignored its caller's deadline", elapsed)
	}
	if reader.Stats().WaitCount == 0 {
		t.Fatal("test did not exercise a database connection wait")
	}
	if err = lease.Close(); err != nil {
		t.Fatal(err)
	}
	if s.IPBannedContext(context.Background(), "198.51.100.27") {
		t.Fatal("cancelled cache load left future callers permanently denied")
	}
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	if !s.IPBannedContext(ctx, "198.51.100.27") {
		t.Fatal("already-cancelled admission reused the warm allow cache")
	}
}

func TestConnectionQuotaRejectsNinthBeforeAnyPolicyDatabaseWait(t *testing.T) {
	s := testStore(t)
	hub := newHub()
	signer := transportSigner(t)
	server := makeServer(s, hub, signer)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go server.Serve(listener)
	clients := make([]*gossh.Client, 0, 8)
	t.Cleanup(func() {
		for _, client := range clients {
			_ = client.Close()
		}
		_ = server.Close()
	})
	for n := 0; n < 8; n++ {
		client, err := gossh.Dial("tcp", listener.Addr().String(), &gossh.ClientConfig{User: "chat", Auth: []gossh.AuthMethod{appUIAuthentication()}, HostKeyCallback: gossh.FixedHostKey(signer.PublicKey()), Timeout: 2 * time.Second})
		if err != nil {
			t.Fatalf("could not reserve connection %d: %v", n, err)
		}
		clients = append(clients, client)
	}
	reader := s.policyDB()
	reader.SetMaxOpenConns(1)
	lease, err := reader.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	s.invalidateIPPolicy()
	waits := reader.Stats().WaitCount
	raw, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	_ = raw.SetReadDeadline(time.Now().Add(time.Second))
	buffer := make([]byte, 1)
	if n, err := raw.Read(buffer); err != io.EOF || n != 0 {
		t.Fatal("ninth transport was not rejected before SSH admission", n, err)
	}
	if reader.Stats().WaitCount != waits {
		t.Fatal("over-quota transport tried to query the occupied policy database")
	}
}

func waitForPendingJoins(t *testing.T, hub *Hub, want int) {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		if hub.mu.TryLock() {
			actual := len(hub.joining)
			hub.mu.Unlock()
			if actual == want {
				return
			}
		}
		select {
		case <-deadline.C:
			t.Fatal("join did not reserve its slot outside the global mutex")
		case <-tick.C:
		}
	}
}

func admissionClient(id string) *Client {
	return &Client{ID: id, Name: "alice", IP: "192.0.2.10", Events: make(chan Event, 256), Close: func() {}, Ctx: context.Background()}
}

func TestPendingDatabaseJoinDoesNotBlockHubAndReservesAccountQuota(t *testing.T) {
	s, hub := featureFixture(t)
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	clients := []*Client{admissionClient("pending-one"), admissionClient("pending-two"), admissionClient("pending-three")}
	completed := make(chan error, len(clients))
	for _, client := range clients {
		go func(client *Client) { completed <- hub.Join(client) }(client)
	}
	waitForPendingJoins(t, hub, len(clients))
	online := make(chan []string, 1)
	go func() { online <- hub.Online() }()
	select {
	case names := <-online:
		if len(names) != 0 {
			t.Fatal("uncommitted sessions appeared online", names)
		}
	case <-time.After(time.Second):
		t.Fatal("writer wait retained the global hub mutex")
	}
	quota := make(chan error, 1)
	go func() { quota <- hub.Join(admissionClient("pending-fourth")) }()
	select {
	case err := <-quota:
		if err == nil {
			t.Fatal("pending joins were not included in the per-account quota")
		}
	case <-time.After(time.Second):
		t.Fatal("over-quota join waited for the writer")
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	for range clients {
		select {
		case err = <-completed:
			if err != nil {
				t.Fatal("reserved session failed after the writer was released", err)
			}
		case <-time.After(time.Second):
			t.Fatal("reserved join did not finish")
		}
	}
	if names := hub.Online(); len(names) != 1 || names[0] != "alice" {
		t.Fatal("joined sessions did not become visible", names)
	}
	for _, client := range clients {
		hub.Leave(client)
	}
}

func TestCancelledPendingJoinReleasesItsReservation(t *testing.T) {
	s, hub := featureFixture(t)
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	client := admissionClient("cancelled-pending")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client.Ctx = ctx
	done := make(chan error, 1)
	go func() { done <- hub.Join(client) }()
	waitForPendingJoins(t, hub, 1)
	cancel()
	select {
	case err = <-done:
		if err == nil {
			t.Fatal("cancelled connection joined the hub")
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled join still waits for the database writer")
	}
	hub.mu.Lock()
	remaining := len(hub.joining)
	hub.mu.Unlock()
	if remaining != 0 {
		t.Fatal("cancelled join retained capacity")
	}
}

func TestAggregatedSessionHealthChecksCredentialKeyAndBanState(t *testing.T) {
	for _, scenario := range []string{"healthy", "epoch", "revoked-session", "closed-session", "revoked-key", "wrong-key-owner", "banned-user", "active-sanction", "expired-sanction"} {
		t.Run(scenario, func(t *testing.T) {
			s, _ := featureFixture(t)
			key := keyUITestSigner(t)
			bound, err := s.BindSSHKey("alice", "synthetic device", string(gossh.MarshalAuthorizedKey(key.PublicKey())))
			if err != nil {
				t.Fatal(err)
			}
			client := admissionClient("health-session")
			if err = s.RecordSession(client); err != nil {
				t.Fatal(err)
			}
			if err = s.AttachSSHSessionKey(client.ID, "alice", key.PublicKey()); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "epoch":
				_, err = s.db.Exec(`INSERT INTO credential_versions(user,epoch) VALUES('alice',1) ON CONFLICT(user) DO UPDATE SET epoch=1`)
			case "revoked-session":
				_, err = s.db.Exec(`UPDATE sessions SET revoked=1 WHERE id=?`, client.ID)
			case "closed-session":
				_, err = s.db.Exec(`UPDATE sessions SET closed=1 WHERE id=?`, client.ID)
			case "revoked-key":
				_, err = s.db.Exec(`UPDATE ssh_keys SET revoked=1 WHERE id=?`, bound.ID)
			case "wrong-key-owner":
				_, err = s.db.Exec(`UPDATE ssh_keys SET user='bob' WHERE id=?`, bound.ID)
			case "banned-user":
				_, err = s.db.Exec(`UPDATE users SET banned=1 WHERE name='alice'`)
			case "active-sanction", "expired-sanction":
				expires := time.Now().Unix() + 600
				if scenario == "expired-sanction" {
					expires = time.Now().Unix() - 1
				}
				_, err = s.db.Exec(`INSERT INTO sanctions(kind,target,room,expires,reason,actor,at) VALUES('user','alice','',?,'synthetic','admin',?)`, expires, time.Now().Unix())
			}
			if err != nil {
				t.Fatal(err)
			}
			user, healthy := s.SessionHealth(client)
			want := scenario == "healthy" || scenario == "expired-sanction"
			if healthy != want || healthy && user.Name != "alice" {
				t.Fatal("unexpected health result", healthy, want, user.Name)
			}
		})
	}
}

func TestSessionHealthReadsDoNotWaitForTheWriter(t *testing.T) {
	s, _ := featureFixture(t)
	client := admissionClient("health-reader")
	if err := s.RecordSession(client); err != nil {
		t.Fatal(err)
	}
	if s.IPBannedContext(context.Background(), client.IP) {
		t.Fatal("clean policy could not be cached")
	}
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	done := make(chan bool, 1)
	go func() {
		user, valid := s.SessionHealth(client)
		done <- valid && user.Name == "alice"
	}()
	select {
	case healthy := <-done:
		if !healthy {
			t.Fatal("health lookup did not complete through the reader pool")
		}
	case <-time.After(time.Second):
		t.Fatal("health lookup used the occupied writer connection")
	}
}

func TestCanonicalIPUnbanRevokesLegacyEquivalentTargetsWithoutRewritingHistory(t *testing.T) {
	s, _ := featureFixture(t)
	targets := []string{"::ffff:198.51.100.27/120", "198.51.100.0/24", "198.51.101.0/24"}
	for _, target := range targets {
		if _, err := s.db.Exec(`INSERT INTO sanctions(kind,target,room,expires,reason,actor,at) VALUES('ip',?,'',0,'legacy synthetic','admin',1)`, target); err != nil {
			t.Fatal(err)
		}
	}
	if !s.IPBannedContext(context.Background(), "198.51.100.9") {
		t.Fatal("legacy mapped sanction was not honored")
	}
	if err := s.Sanction("admin", "ip", "198.51.100.0/24", "", "0", "", true); err != nil {
		t.Fatal(err)
	}
	if s.IPBannedContext(context.Background(), "198.51.100.9") {
		t.Fatal("canonical unban left a legacy equivalent sanction active")
	}
	for n, target := range targets {
		var actual string
		var revoked bool
		if err := s.db.QueryRow(`SELECT target,revoked FROM sanctions WHERE target=?`, target).Scan(&actual, &revoked); err != nil {
			t.Fatal(err)
		}
		if actual != target || revoked != (n < 2) {
			t.Fatal("unban changed history text or revoked a different range", actual, revoked)
		}
	}
}
