package main

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"
)

func guiOverSignedSSH(t *testing.T, store *Store, hub *Hub, device gossh.Signer) (*guiWireFixture, *gossh.Client) {
	t.Helper()
	return guiSSHConnections(t, store, hub)(device)
}

func guiSSHConnections(t *testing.T, store *Store, hub *Hub) func(gossh.Signer) (*guiWireFixture, *gossh.Client) {
	t.Helper()
	host := transportSigner(t)
	server := makeServer(store, hub, host)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go server.Serve(listener)
	t.Cleanup(func() { hub.stopUI(); server.Close(); hub.uiTasks.Wait() })
	return func(device gossh.Signer) (*guiWireFixture, *gossh.Client) {
		method := transportAnonymousAuth()
		if device != nil {
			method = gossh.PublicKeys(device)
		}
		client, err := gossh.Dial("tcp", listener.Addr().String(), &gossh.ClientConfig{User: "chat", Auth: []gossh.AuthMethod{method}, HostKeyCallback: gossh.FixedHostKey(host.PublicKey()), Timeout: 3 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { client.Close() })
		session, err := client.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		in, err := session.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		out, err := session.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err = session.RequestSubsystem(guiSubsystemName); err != nil {
			t.Fatal(err)
		}
		responses := make(chan guiResponse, 64)
		go func() {
			defer close(responses)
			decoder := json.NewDecoder(out)
			for {
				var r guiResponse
				if decoder.Decode(&r) != nil {
					return
				}
				select {
				case responses <- r:
				case <-t.Context().Done():
					return
				}
			}
		}()
		return &guiWireFixture{t: t, writer: json.NewEncoder(in), responses: responses}, client
	}
}

func TestGUIDeviceKeySignedBindingAndDerivedLogin(t *testing.T) {
	s := testStore(t)
	registerUser(t, s, "key-admin")
	owner := registerUser(t, s, "key-owner")
	other := registerUser(t, s, "key-other")
	h := newHub()
	device := transportSigner(t)
	f, client := guiOverSignedSSH(t, s, h, device)
	hello := f.read("hello", "")
	if hello.DeviceKey == nil || hello.DeviceKey.Bound || hello.DeviceKey.Fingerprint != gossh.FingerprintSHA256(device.PublicKey()) {
		t.Fatal("hello lacked verified transport metadata")
	}
	f.send(guiRequest{Type: "login_key", RequestID: "unbound", Username: other.Name})
	f.read("error", "unbound")
	f.send(guiRequest{Type: "login", RequestID: "password", Username: owner.Name, Password: "test-password-123"})
	f.read("auth", "password")
	f.read("state", "")
	f.send(guiRequest{Type: "bind_key", RequestID: "bind", Username: other.Name, Body: "synthetic laptop"})
	bound := f.read("key_bound", "bind")
	if bound.DeviceKey == nil || !bound.DeviceKey.Bound {
		t.Fatal("signed key did not bind")
	}
	key, err := s.FindSSHKey(device.PublicKey())
	if err != nil || key.User != owner.Name {
		t.Fatal("client actor replaced authenticated owner")
	}
	// Arbitrary public JSON cannot replace the signed key of this connection.
	forged := transportSigner(t)
	if err = f.writer.Encode(map[string]any{"type": "bind_key", "request_id": "forged", "body": "ignored extra key", "public_key": string(gossh.MarshalAuthorizedKey(forged.PublicKey()))}); err != nil {
		t.Fatal(err)
	}
	f.read("key_bound", "forged")
	if _, err = s.FindSSHKey(forged.PublicKey()); err == nil {
		t.Fatal("pasted public material was accepted as ownership proof")
	}
	client.Close()
	keyHub := newHub()
	f2, _ := guiOverSignedSSH(t, s, keyHub, device)
	if r := f2.read("hello", ""); r.DeviceKey == nil || !r.DeviceKey.Bound {
		t.Fatal("bound metadata missing")
	}
	f2.send(guiRequest{Type: "login_key", RequestID: "key-login", Username: other.Name})
	if r := f2.read("auth", "key-login"); r.User != owner.Name {
		t.Fatal("key login trusted client username")
	}
	f2.read("state", "")
	f2.send(guiRequest{Type: "bind_key", RequestID: "key-is-not-password", Body: "no password"})
	f2.read("error", "key-is-not-password")
	f2.send(guiRequest{Type: "bind_key", RequestID: "key-with-password", Body: "reverified", Password: "test-password-123"})
	f2.read("key_bound", "key-with-password")
	var attached int
	if err = s.db.QueryRow("SELECT count(*) FROM session_credentials WHERE key_id=?", key.ID).Scan(&attached); err != nil || attached != 1 {
		t.Fatalf("key session association missing: %d %v", attached, err)
	}
	if err = s.RevokeSSHKey(owner.Name, key.ID); err != nil {
		t.Fatal(err)
	}
	keyHub.Enforce()
	f3, _ := guiOverSignedSSH(t, s, newHub(), device)
	f3.read("hello", "")
	f3.send(guiRequest{Type: "login_key", RequestID: "revoked"})
	f3.read("error", "revoked")
}

func TestGUIDeviceKeyAnonymousResetAndRecentPasswordGates(t *testing.T) {
	s := testStore(t)
	registerUser(t, s, "gate-admin")
	owner := registerUser(t, s, "gate-owner")
	device := transportSigner(t)
	key, err := s.BindSSHKey(owner.Name, "synthetic", string(gossh.MarshalAuthorizedKey(device.PublicKey())))
	if err != nil {
		t.Fatal(err)
	}
	f, _ := guiOverSignedSSH(t, s, newHub(), nil)
	if r := f.read("hello", ""); r.DeviceKey != nil {
		t.Fatal("anonymous session gained key proof")
	}
	f.send(guiRequest{Type: "login_key", RequestID: "no-proof", Username: owner.Name})
	f.read("error", "no-proof")
	f.send(guiRequest{Type: "login", RequestID: "normal", Username: owner.Name, Password: "test-password-123"})
	f.read("auth", "normal")
	f.read("state", "")
	f.send(guiRequest{Type: "bind_key", RequestID: "no-proof-bind", Body: "synthetic"})
	f.read("error", "no-proof-bind")
	// Synthetic durable session and signed context exercise expiry without a
	// two-minute sleep. Actual SSH ownership is covered by the previous test.
	client := &Client{ID: "recent-synthetic", Name: owner.Name, AuthEpoch: owner.AuthEpoch, IP: "192.0.2.91", Ctx: t.Context()}
	if err = s.RecordSession(client); err != nil {
		t.Fatal(err)
	}
	freshDevice := transportSigner(t)
	ctx := context.WithValue(t.Context(), guiSSHProofKey{}, freshDevice.PublicKey())
	verified := time.Now().Add(-3 * time.Minute)
	if _, err = guiBindDeviceKey(ctx, s, newHub(), owner, client, &verified, guiRequest{RequestID: "expired"}); err == nil {
		t.Fatal("expired password proof accepted")
	}
	if _, err = guiBindDeviceKey(ctx, s, newHub(), owner, client, &verified, guiRequest{RequestID: "wrong", Password: "incorrect-synthetic-password"}); err == nil {
		t.Fatal("wrong reauthentication password accepted")
	}
	if keys, err := s.SSHKeys(owner.Name); err != nil || len(keys) != 1 {
		t.Fatal("rejected reauthentication wrote a key", err)
	}
	if _, err = guiBindDeviceKey(ctx, s, newHub(), owner, client, &verified, guiRequest{RequestID: "fresh", Password: "test-password-123"}); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = s.BindSSHKeyForSessionContext(cancelled, client.ID, owner.Name, "cancelled", string(gossh.MarshalAuthorizedKey(transportSigner(t).PublicKey()))); err == nil {
		t.Fatal("cancelled key transaction committed")
	}
	keys, err := s.SSHKeys(owner.Name)
	if err != nil || len(keys) != 2 || keys[0].ID != key.ID {
		t.Fatal("cancelled operation changed keys")
	}
	if err = s.RevokeSession(owner.Name, client.ID); err != nil {
		t.Fatal(err)
	}
	revokedContext := context.WithValue(t.Context(), guiSSHProofKey{}, transportSigner(t).PublicKey())
	if _, err = guiBindDeviceKey(revokedContext, s, newHub(), owner, client, &verified, guiRequest{RequestID: "revoked-session"}); err == nil {
		t.Fatal("revoked live session bound a key with recent password")
	}
	if keys, err := s.SSHKeys(owner.Name); err != nil || len(keys) != 2 {
		t.Fatal("revoked session changed device keys", err)
	}
	if _, err = s.db.Exec("UPDATE user_meta SET must_reset=1 WHERE name=?", owner.Name); err != nil {
		t.Fatal(err)
	}
	f2, _ := guiOverSignedSSH(t, s, newHub(), device)
	f2.read("hello", "")
	f2.send(guiRequest{Type: "login_key", RequestID: "reset"})
	f2.read("error", "reset")
	f2.send(guiRequest{Type: "login", RequestID: "reset-password", Username: owner.Name, Password: "test-password-123"})
	if r := f2.read("auth", "reset-password"); r.Status != "password_reset_required" {
		t.Fatal("MustReset lost restricted login status")
	}
	f2.read("reset_required", "")
	f2.send(guiRequest{Type: "bind_key", RequestID: "reset-bind", Password: "test-password-123", Body: "must not bind"})
	f2.read("error", "reset-bind")
	if keys, err := s.SSHKeys(owner.Name); err != nil || len(keys) != 2 {
		t.Fatal("MustReset wrote device credentials", err)
	}
}
