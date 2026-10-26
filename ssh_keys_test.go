package main

import (
	"bytes"
	"crypto/dsa"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"math/big"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"
)

func testSSHKeySigner(t *testing.T) gossh.Signer {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

func testSSHKeyText(key gossh.PublicKey) string {
	return strings.TrimSpace(string(gossh.MarshalAuthorizedKey(key)))
}

func TestSSHKeyAlgorithmsAndStrictPublicKeyParsing(t *testing.T) {
	publicKeys := []gossh.PublicKey{testSSHKeySigner(t).PublicKey()}
	for _, curve := range []elliptic.Curve{elliptic.P256(), elliptic.P384(), elliptic.P521()} {
		private, err := ecdsa.GenerateKey(curve, rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		public, err := gossh.NewPublicKey(&private.PublicKey)
		if err != nil {
			t.Fatal(err)
		}
		publicKeys = append(publicKeys, public)
	}
	private, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	rsaKey, err := gossh.NewPublicKey(&private.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	publicKeys = append(publicKeys, rsaKey)
	for _, public := range publicKeys {
		t.Run(public.Type(), func(t *testing.T) {
			if err := ValidateSSHKey(public); err != nil {
				t.Fatal(err)
			}
			parsed, normalized, err := ParseSSHKey(testSSHKeyText(public) + " synthetic-client@example.test\n")
			if err != nil || normalized != testSSHKeyText(public) || !bytes.Equal(parsed.Marshal(), public.Marshal()) {
				t.Fatalf("supported public key did not round trip: %v", err)
			}
		})
	}
	valid := testSSHKeyText(publicKeys[0])
	for name, input := range map[string]string{
		"empty": "", "garbage": "not a key", "private": "-----BEGIN OPENSSH PRIVATE KEY-----\nsynthetic\n-----END OPENSSH PRIVATE KEY-----",
		"multiple": valid + "\n" + valid, "options": `from="127.0.0.1" ` + valid,
		"command": `command="whoami" ` + valid, "oversize": strings.Repeat("a", 16*1024+1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := ParseSSHKey(input); err == nil {
				t.Fatal("unsafe key input accepted")
			}
		})
	}
	for _, bits := range []int{1024, 8193} {
		n := new(big.Int).Lsh(big.NewInt(1), uint(bits-1))
		n.SetBit(n, 0, 1)
		weak, err := gossh.NewPublicKey(&rsa.PublicKey{N: n, E: 65537})
		if err != nil {
			t.Fatal(err)
		}
		if err = ValidateSSHKey(weak); err == nil {
			t.Fatalf("RSA %d-bit resource/security bound not enforced", bits)
		}
	}
	dsaKey, err := gossh.NewPublicKey(&dsa.PublicKey{Parameters: dsa.Parameters{P: big.NewInt(23), Q: big.NewInt(11), G: big.NewInt(4)}, Y: big.NewInt(18)})
	if err != nil {
		t.Fatal(err)
	}
	if err = ValidateSSHKey(dsaKey); err == nil {
		t.Fatal("DSA key accepted")
	}
	certificate := &gossh.Certificate{Key: publicKeys[0], CertType: gossh.UserCert, ValidBefore: gossh.CertTimeInfinity, ValidPrincipals: []string{"alice"}}
	if err = certificate.SignCert(rand.Reader, testSSHKeySigner(t)); err != nil {
		t.Fatal(err)
	}
	if _, _, err = ParseSSHKey(testSSHKeyText(certificate)); err == nil {
		t.Fatal("certificate accepted as raw account key")
	}
}

func TestSSHKeyOwnerIsolationIdempotenceAndRevocation(t *testing.T) {
	s := testStore(t)
	registerUser(t, s, "alice")
	registerUser(t, s, "bob")
	first, second := testSSHKeySigner(t), testSSHKeySigner(t)
	key, err := s.BindSSHKey(" ALICE ", "桌面\x1b[31m\n电脑", testSSHKeyText(first.PublicKey()))
	if err != nil {
		t.Fatal(err)
	}
	if key.User != "alice" || key.Label != "桌面电脑" || key.Fingerprint != gossh.FingerprintSHA256(first.PublicKey()) {
		t.Fatal("key metadata was not normalized")
	}
	again, err := s.BindSSHKey("alice", "different name", testSSHKeyText(first.PublicKey()))
	if err != nil || again.ID != key.ID || again.Label != key.Label {
		t.Fatal("duplicate bind was not idempotent")
	}
	if _, err = s.BindSSHKey("bob", "the same key", testSSHKeyText(first.PublicKey())); err == nil {
		t.Fatal("same key was bound across different accounts")
	}
	other, err := s.BindSSHKey("alice", "手机", testSSHKeyText(second.PublicKey()))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RenameSSHKey("bob", key.ID, "stolen"); err == nil {
		t.Fatal("cross-owner rename allowed")
	}
	if err = s.RevokeSSHKey("bob", key.ID); err == nil {
		t.Fatal("cross-owner revocation allowed")
	}
	if err = s.RenameSSHKey("alice", key.ID, "家用电脑"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"first-key-session", "second-key-session", "password-session"} {
		if _, err = s.db.Exec(`INSERT INTO sessions(id,user,ip,client,started,seen) VALUES(?,'alice','192.0.2.5','synthetic',1,1)`, id); err != nil {
			t.Fatal(err)
		}
	}
	for session, id := range map[string]int64{"first-key-session": key.ID, "second-key-session": other.ID} {
		if _, err = s.db.Exec(`INSERT INTO session_credentials(session_id,key_id) VALUES(?,?)`, session, id); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.RevokeSSHKey("alice", key.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AuthenticateSSHKey(first.PublicKey()); err == nil || s.SessionValid("first-key-session") {
		t.Fatal("revoked key or its session remained valid")
	}
	if !s.SessionValid("second-key-session") || !s.SessionValid("password-session") {
		t.Fatal("unrelated key/password session was revoked")
	}
	keys, err := s.SSHKeys("alice")
	if err != nil || len(keys) != 1 || keys[0].ID != other.ID {
		t.Fatal("owner key list did not hide revoked keys")
	}
	keys, err = s.SSHKeys("bob")
	if err != nil || len(keys) != 0 {
		t.Fatal("owner key list leaked another account's keys")
	}
	rebound, err := s.BindSSHKey("alice", "重新绑定", testSSHKeyText(first.PublicKey()))
	if err != nil || rebound.ID != key.ID || rebound.Revoked || rebound.LastUsed != 0 {
		t.Fatal("own revoked key could not be safely rebound")
	}
	if s.SessionValid("first-key-session") {
		t.Fatal("rebinding resurrected a revoked old session")
	}
	if _, err = s.BindSSHKey("bob", "still shared", testSSHKeyText(first.PublicKey())); err == nil {
		t.Fatal("rebound key was shared across accounts")
	}
	var stored int
	if err = s.db.QueryRow(`SELECT count(*) FROM ssh_keys`).Scan(&stored); err != nil || stored != 2 {
		t.Fatal("revocation/rebinding removed key history or created duplicates")
	}
}

func TestSSHKeyAuthenticationChecksCurrentStateAndReadOnlyProbes(t *testing.T) {
	s := testStore(t)
	registerUser(t, s, "alice")
	signer := testSSHKeySigner(t)
	key, err := s.BindSSHKey("alice", "test", testSSHKeyText(signer.PublicKey()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE ssh_keys SET last_used=111 WHERE id=?`, key.ID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err = s.FindSSHKey(signer.PublicKey()); err != nil {
			t.Fatal(err)
		}
	}
	var lastUsed int64
	if err = s.db.QueryRow(`SELECT last_used FROM ssh_keys WHERE id=?`, key.ID).Scan(&lastUsed); err != nil || lastUsed != 111 {
		t.Fatal("read-only key probing changed usage records")
	}
	if _, err = s.AuthenticateSSHKey(testSSHKeySigner(t).PublicKey()); err == nil {
		t.Fatal("unbound key authenticated an account")
	}
	for _, state := range []struct{ name, set, clear string }{
		{"legacy ban", `UPDATE users SET banned=1 WHERE name='alice'`, `UPDATE users SET banned=0 WHERE name='alice'`},
		{"forced reset", `UPDATE user_meta SET must_reset=1 WHERE name='alice'`, `UPDATE user_meta SET must_reset=0 WHERE name='alice'`},
		{"active ban", `INSERT INTO sanctions(kind,target,expires,reason,actor,at) VALUES('user','alice',0,'synthetic','alice',1)`, `UPDATE sanctions SET revoked=1 WHERE kind='user' AND target='alice'`},
	} {
		t.Run(state.name, func(t *testing.T) {
			if _, err := s.db.Exec(state.set); err != nil {
				t.Fatal(err)
			}
			if _, err := s.AuthenticateSSHKey(signer.PublicKey()); err == nil {
				t.Fatal("account restriction bypassed by key")
			}
			if _, err := s.BindSSHKey("alice", "while restricted", testSSHKeyText(testSSHKeySigner(t).PublicKey())); err == nil {
				t.Fatal("restricted account added a key")
			}
			if _, err := s.db.Exec(state.clear); err != nil {
				t.Fatal(err)
			}
		})
	}
	if err = s.db.QueryRow(`SELECT last_used FROM ssh_keys WHERE id=?`, key.ID).Scan(&lastUsed); err != nil || lastUsed != 111 {
		t.Fatal("failed key authentication was recorded as successful usage")
	}
	if _, err = s.db.Exec(`UPDATE config SET value='on' WHERE key='maintenance'`); err != nil {
		t.Fatal(err)
	}
	u, err := s.AuthenticateSSHKey(signer.PublicKey())
	if err != nil || u.Name != "alice" {
		t.Fatal("key login changed the existing maintenance login policy")
	}
	if err = s.db.QueryRow(`SELECT last_used FROM ssh_keys WHERE id=?`, key.ID).Scan(&lastUsed); err != nil || lastUsed < time.Now().Unix()-3 {
		t.Fatal("successful key usage was not recorded")
	}
	if _, err = s.Send(Message{Sender: "alice", Room: "lobby", Body: "synthetic", Kind: "chat"}); err == nil {
		t.Fatal("maintenance posting restriction was bypassed")
	}
}

func TestSSHKeyConcurrentLimitAndUniqueOwnership(t *testing.T) {
	s := testStore(t)
	registerUser(t, s, "alice")
	registerUser(t, s, "bob")
	var allowed atomic.Int32
	var work sync.WaitGroup
	for i := 0; i < 16; i++ {
		public := testSSHKeyText(testSSHKeySigner(t).PublicKey())
		work.Add(1)
		go func(index int) {
			defer work.Done()
			if _, err := s.BindSSHKey("alice", fmt.Sprintf("device %d", index), public); err == nil {
				allowed.Add(1)
			}
		}(i)
	}
	work.Wait()
	keys, err := s.SSHKeys("alice")
	if err != nil || len(keys) != maxSSHKeysPerUser || allowed.Load() != maxSSHKeysPerUser {
		t.Fatalf("concurrent binds bypassed cap: admitted=%d listed=%d err=%v", allowed.Load(), len(keys), err)
	}
	if _, err = s.BindSSHKey("alice", "duplicate at cap", keys[0].PublicKey); err != nil {
		t.Fatal("idempotent bind consumed an extra slot")
	}
	if err = s.RevokeSSHKey("alice", keys[0].ID); err != nil {
		t.Fatal(err)
	}
	shared := testSSHKeyText(testSSHKeySigner(t).PublicKey())
	allowed.Store(0)
	for _, user := range []string{"alice", "bob"} {
		work.Add(1)
		go func(user string) {
			defer work.Done()
			if _, err := s.BindSSHKey(user, "same key", shared); err == nil {
				allowed.Add(1)
			}
		}(user)
	}
	work.Wait()
	if allowed.Load() != 1 {
		t.Fatal("same public key was concurrently shared between accounts")
	}
}

func TestSSHKeyPersistencePasswordChangeAndRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.db")
	s, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.db.Close() })
	registerUser(t, s, "alice")
	registerUser(t, s, "admin")
	if err = s.Promote("admin"); err != nil {
		t.Fatal(err)
	}
	signer := testSSHKeySigner(t)
	key, err := s.BindSSHKey("alice", "持久密钥", testSSHKeyText(signer.PublicKey()))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ChangePassword("alice", "test-password-123", "changed-password-456"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AuthenticateSSHKey(signer.PublicKey()); err != nil {
		t.Fatal("normal password change unexpectedly revoked a user's own keys")
	}
	if err = s.db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := s.SSHKeys("alice")
	if err != nil || len(keys) != 1 || keys[0].ID != key.ID || keys[0].Label != "持久密钥" {
		t.Fatal("bound keys did not survive reopening")
	}
	if _, err = s.ResetPassword("admin", "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AuthenticateSSHKey(signer.PublicKey()); err == nil {
		t.Fatal("administrator recovery left the previous key usable")
	}
	keys, err = s.SSHKeys("alice")
	if err != nil || len(keys) != 0 {
		t.Fatal("administrator recovery did not revoke all bound keys")
	}
	var rows int
	if err = s.db.QueryRow(`SELECT count(*) FROM ssh_keys WHERE user='alice' AND revoked=1`).Scan(&rows); err != nil || rows != 1 {
		t.Fatal("administrator recovery deleted key history")
	}
	var details []string
	entries, err := s.db.Query(`SELECT detail FROM audit WHERE action LIKE 'sshkey.%'`)
	if err != nil {
		t.Fatal(err)
	}
	defer entries.Close()
	for entries.Next() {
		var detail string
		if err = entries.Scan(&detail); err != nil {
			t.Fatal(err)
		}
		details = append(details, detail)
	}
	rendered, _ := json.Marshal(details)
	if strings.Contains(string(rendered), key.PublicKey) || strings.Contains(string(rendered), "PRIVATE KEY") {
		t.Fatal("audit stored raw key material")
	}
}

func TestSSHKeyWritesAndAuditAreAtomic(t *testing.T) {
	s := testStore(t)
	registerUser(t, s, "alice")
	signer := testSSHKeySigner(t)
	key, err := s.BindSSHKey("alice", "original label", testSSHKeyText(signer.PublicKey()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`INSERT INTO sessions(id,user,ip,client,started,seen) VALUES('key-session','alice','192.0.2.8','synthetic',1,1);INSERT INTO session_credentials(session_id,key_id) VALUES('key-session',?)`, key.ID); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TRIGGER test_key_audit BEFORE INSERT ON audit WHEN new.action LIKE 'sshkey.%' BEGIN SELECT RAISE(ABORT,'synthetic audit failure');END`,
		`CREATE TRIGGER test_key_audit BEFORE INSERT ON audit WHEN new.action LIKE 'sshkey.%' BEGIN SELECT RAISE(IGNORE);END`,
	} {
		if _, err = s.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
		if err = s.RevokeSSHKey("alice", key.ID); err == nil {
			t.Fatal("revocation succeeded without its audit record")
		}
		if !s.SessionValid("key-session") {
			t.Fatal("failed revocation left the related session revoked")
		}
		if err = s.RenameSSHKey("alice", key.ID, "should roll back"); err == nil {
			t.Fatal("rename succeeded without its audit record")
		}
		if _, err = s.BindSSHKey("alice", "should roll back", testSSHKeyText(testSSHKeySigner(t).PublicKey())); err == nil {
			t.Fatal("binding succeeded without its audit record")
		}
		keys, err := s.SSHKeys("alice")
		if err != nil || len(keys) != 1 || keys[0].Label != "original label" || keys[0].Revoked {
			t.Fatal("audit failure left a partial key mutation")
		}
		if _, err = s.db.Exec(`DROP TRIGGER test_key_audit`); err != nil {
			t.Fatal(err)
		}
	}
	var initialAudits int
	if err = s.db.QueryRow(`SELECT count(*) FROM audit WHERE action LIKE 'sshkey.%'`).Scan(&initialAudits); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`CREATE TRIGGER test_key_write BEFORE UPDATE ON ssh_keys BEGIN SELECT RAISE(IGNORE);END`); err != nil {
		t.Fatal(err)
	}
	if err = s.RenameSSHKey("alice", key.ID, "ignored change"); err == nil {
		t.Fatal("zero-row key update reported rename success")
	}
	if err = s.RevokeSSHKey("alice", key.ID); err == nil || !s.SessionValid("key-session") {
		t.Fatal("zero-row key update reported revoke success or partially revoked a session")
	}
	if _, err = s.AuthenticateSSHKey(signer.PublicKey()); err == nil {
		t.Fatal("zero-row key usage update reported authentication success")
	}
	var finalAudits int
	if err = s.db.QueryRow(`SELECT count(*) FROM audit WHERE action LIKE 'sshkey.%'`).Scan(&finalAudits); err != nil || initialAudits != finalAudits {
		t.Fatal("ignored key writes produced success audit records")
	}
}

func TestSSHKeyBindingRequiresAnActiveOwnerSession(t *testing.T) {
	s := testStore(t)
	registerUser(t, s, "alice")
	registerUser(t, s, "bob")
	base, err := s.BindSSHKey("alice", "existing key", testSSHKeyText(testSSHKeySigner(t).PublicKey()))
	if err != nil {
		t.Fatal(err)
	}
	for _, session := range []struct {
		id, user        string
		closed, revoked int
	}{
		{"valid", "alice", 0, 0}, {"closed", "alice", 1, 0}, {"revoked", "alice", 0, 1},
		{"other-owner", "bob", 0, 0}, {"key-revoked", "alice", 0, 0},
	} {
		if _, err = s.db.Exec(`INSERT INTO sessions(id,user,ip,client,started,seen,closed,revoked) VALUES(?,?,'192.0.2.9','synthetic',1,1,?,?)`, session.id, session.user, session.closed, session.revoked); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.db.Exec(`INSERT INTO session_credentials(session_id,key_id) VALUES('key-revoked',?);UPDATE ssh_keys SET revoked=1 WHERE id=?`, base.ID, base.ID); err != nil {
		t.Fatal(err)
	}
	for _, session := range []string{"", " ", "missing", "closed", "revoked", "other-owner", "key-revoked"} {
		t.Run(fmt.Sprintf("session=%q", session), func(t *testing.T) {
			if _, err := s.BindSSHKeyForSession(session, "alice", "should fail", testSSHKeyText(testSSHKeySigner(t).PublicKey())); err == nil {
				t.Fatal("inactive or wrong-owner session bound a new key")
			}
		})
	}
	var total int
	if err = s.db.QueryRow(`SELECT count(*) FROM ssh_keys`).Scan(&total); err != nil || total != 1 {
		t.Fatal("rejected sessions left credential records behind")
	}
	created, err := s.BindSSHKeyForSession("valid", " ALICE ", "current session", testSSHKeyText(testSSHKeySigner(t).PublicKey()))
	if err != nil || created.User != "alice" {
		t.Fatalf("active owner session could not bind key: %v", err)
	}
	if _, err = s.db.Exec(`UPDATE sessions SET revoked=1 WHERE id='valid'`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.BindSSHKeyForSession("valid", "alice", "idempotence is also gated", created.PublicKey); err == nil {
		t.Fatal("idempotent binding bypassed the durable session check")
	}
}
