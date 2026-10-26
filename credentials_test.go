package main

import (
	"bytes"
	"testing"

	gossh "golang.org/x/crypto/ssh"
)

func credentialSession(t *testing.T, s *Store, name, id string) *Client {
	t.Helper()
	u, err := s.User(name)
	if err != nil {
		t.Fatal(err)
	}
	c := &Client{ID: id, Name: name, IP: "192.0.2.42", Version: "credential-test", AuthEpoch: u.AuthEpoch}
	if err = s.RecordSession(c); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRegistrationResultCannotAdoptAResetCredentialGeneration(t *testing.T) {
	s, _ := featureFixture(t)
	if _, err := s.ResetPassword("admin", "bob"); err != nil {
		t.Fatal(err)
	}
	current, err := s.User("bob")
	if err != nil || current.AuthEpoch == 0 {
		t.Fatal("reset did not advance generation", err)
	}
	late, err := s.registrationUser("bob")
	if err != nil {
		t.Fatal(err)
	}
	if late.AuthEpoch != 0 {
		t.Fatal("post-registration lookup adopted another credential proof")
	}
	c := &Client{ID: "late-registration", Name: "bob", IP: "192.0.2.42", AuthEpoch: late.AuthEpoch}
	if err = s.RecordSession(c); err == nil {
		t.Fatal("stale registration proof created a current session")
	}
}

func credentialBytes(t *testing.T, s *Store, name string) ([]byte, []byte) {
	t.Helper()
	var hash, salt []byte
	if err := s.db.QueryRow(`SELECT hash,salt FROM users WHERE name=?`, name).Scan(&hash, &salt); err != nil {
		t.Fatal(err)
	}
	return hash, salt
}

func TestCredentialProofCannotOverwriteAdministratorReset(t *testing.T) {
	s, _ := featureFixture(t)
	oldSession := credentialSession(t, s, "bob", "old-password-session")
	oldLogin, err := s.Authenticate("bob", "test-password-123")
	if err != nil {
		t.Fatal(err)
	}
	_, pending, err := s.verifyPassword("bob", "test-password-123")
	if err != nil {
		t.Fatal(err)
	}
	// This is the exact boundary after old-password verification but before
	// its new hash is committed. No timing sleeps or probabilistic race needed.
	replacementSalt := []byte("replacement-salt")
	replacementHash := passwordHash("attacker-selected-password", replacementSalt)
	temporary, err := s.ResetPassword("admin", "bob")
	if err != nil {
		t.Fatal(err)
	}
	resetHash, resetSalt := credentialBytes(t, s, "bob")
	for _, sessionID := range []string{oldSession.ID, ""} {
		if err = s.commitPasswordChange(pending, replacementHash, replacementSalt, sessionID); err == nil {
			t.Fatal("in-flight old password overwrote administrator recovery")
		}
	}
	afterHash, afterSalt := credentialBytes(t, s, "bob")
	if !bytes.Equal(resetHash, afterHash) || !bytes.Equal(resetSalt, afterSalt) {
		t.Fatal("failed stale commit changed credential bytes")
	}
	u, err := s.Authenticate("bob", temporary)
	if err != nil || !u.MustReset || u.AuthEpoch != oldLogin.AuthEpoch+1 {
		t.Fatal("administrator recovery state was lost", u.AuthEpoch, err)
	}
	if _, err = s.Authenticate("bob", "attacker-selected-password"); err == nil {
		t.Fatal("stale password replacement became usable")
	}
	if err = s.RecordSession(&Client{ID: "late-old-login", Name: "bob", AuthEpoch: oldLogin.AuthEpoch}); err == nil {
		t.Fatal("late password authentication created a session after reset")
	}
	if s.SessionValid(oldSession.ID) {
		t.Fatal("reset left its old session active")
	}
	var count int
	s.db.QueryRow(`SELECT count(*) FROM sessions WHERE id='late-old-login'`).Scan(&count)
	if count != 0 {
		t.Fatal("rejected login left a partial durable session")
	}
}

func TestCredentialGenerationInvalidatesLegacySessionMetadata(t *testing.T) {
	s, _ := featureFixture(t)
	if _, err := s.db.Exec(`INSERT INTO sessions(id,user,ip,client,started,seen) VALUES('legacy-session','alice','192.0.2.44','compatibility',1,1)`); err != nil {
		t.Fatal(err)
	}
	if !s.SessionValid("legacy-session") {
		t.Fatal("generation zero session was not compatible")
	}
	if err := s.ChangePassword("alice", "test-password-123", "new-password-456"); err != nil {
		t.Fatal(err)
	}
	// Even clearing an old row's revoked flag cannot revive an old generation.
	if _, err := s.db.Exec(`UPDATE sessions SET revoked=0 WHERE id='legacy-session'`); err != nil {
		t.Fatal(err)
	}
	if s.SessionValid("legacy-session") {
		t.Fatal("a legacy session resurrected after its credential generation changed")
	}
}

func TestCredentialEpochBlocksDelayedLoginAndPreservesCurrentSession(t *testing.T) {
	s, _ := featureFixture(t)
	current := credentialSession(t, s, "alice", "keep-current")
	other := credentialSession(t, s, "alice", "revoke-other")
	oldLogin, err := s.Authenticate("alice", "test-password-123")
	if err != nil {
		t.Fatal(err)
	}
	signer := keyUITestSigner(t)
	key, err := s.BindSSHKey("alice", "device", string(gossh.MarshalAuthorizedKey(signer.PublicKey())))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ChangePasswordForSession("alice", "test-password-123", "new-password-456", current.ID); err != nil {
		t.Fatal(err)
	}
	if !s.SessionValid(current.ID) || s.SessionValid(other.ID) {
		t.Fatal("password change did not atomically preserve only the current session")
	}
	fresh, err := s.Authenticate("alice", "new-password-456")
	if err != nil || fresh.AuthEpoch != oldLogin.AuthEpoch+1 || fresh.MustReset {
		t.Fatal("new password generation is invalid", err)
	}
	if err = s.RecordSession(&Client{ID: "late-before-change", Name: "alice", AuthEpoch: oldLogin.AuthEpoch}); err == nil {
		t.Fatal("old verified password crossed the password change boundary")
	}
	if err = s.RecordSession(&Client{ID: "fresh-after-change", Name: "alice", AuthEpoch: fresh.AuthEpoch}); err != nil {
		t.Fatal("fresh password login failed", err)
	}
	keyUser, err := s.AuthenticateSSHKey(signer.PublicKey())
	if err != nil || keyUser.AuthEpoch != fresh.AuthEpoch {
		t.Fatal("ordinary password change revoked a retained device key", key.ID, err)
	}
}

func TestCredentialCommitRejectsRevokedOrForeignSession(t *testing.T) {
	s, _ := featureFixture(t)
	current := credentialSession(t, s, "alice", "revoked-change")
	foreign := credentialSession(t, s, "bob", "foreign-change")
	_, proof, err := s.verifyPassword("alice", "test-password-123")
	if err != nil {
		t.Fatal(err)
	}
	salt := []byte("replacement-salt")
	hash := passwordHash("new-password-456", salt)
	if err = s.RevokeSession("alice", current.ID); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{current.ID, foreign.ID, "nonexistent-session"} {
		if err = s.commitPasswordChange(proof, hash, salt, id); err == nil {
			t.Fatal("invalid session changed password", id)
		}
	}
	if _, err = s.Authenticate("alice", "test-password-123"); err != nil {
		t.Fatal("failed stale commits changed the old password", err)
	}
	if err = s.ChangePasswordForSession("alice", "test-password-123", "new-password-456", ""); err == nil {
		t.Fatal("user-facing password API accepted an empty session")
	}
}

func TestCredentialCompetingProofsAndAuditFailureRemainAtomic(t *testing.T) {
	s, _ := featureFixture(t)
	current := credentialSession(t, s, "alice", "audit-keep")
	_, first, err := s.verifyPassword("alice", "test-password-123")
	if err != nil {
		t.Fatal(err)
	}
	_, second, err := s.verifyPassword("alice", "test-password-123")
	if err != nil {
		t.Fatal(err)
	}
	salt := []byte("replacement-salt")
	hash := passwordHash("new-password-456", salt)
	if _, err = s.db.Exec(`CREATE TRIGGER fail_password_audit BEFORE INSERT ON audit WHEN new.action='password.change' BEGIN SELECT RAISE(ABORT,'injected audit failure');END;`); err != nil {
		t.Fatal(err)
	}
	beforeHash, beforeSalt := credentialBytes(t, s, "alice")
	if err = s.commitPasswordChange(first, hash, salt, current.ID); err == nil {
		t.Fatal("password change ignored audit failure")
	}
	afterHash, afterSalt := credentialBytes(t, s, "alice")
	u, _ := s.User("alice")
	if !bytes.Equal(beforeHash, afterHash) || !bytes.Equal(beforeSalt, afterSalt) || u.AuthEpoch != first.epoch || !s.SessionValid(current.ID) {
		t.Fatal("audit failure did not roll back credential/session state")
	}
	if _, err = s.db.Exec(`DROP TRIGGER fail_password_audit`); err != nil {
		t.Fatal(err)
	}
	if err = s.commitPasswordChange(first, hash, salt, current.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.commitPasswordChange(second, passwordHash("competing-password-789", salt), salt, current.ID); err == nil {
		t.Fatal("two proofs from one generation both committed")
	}
}
