package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	gossh "golang.org/x/crypto/ssh"
)

// Application credentials stay in the TUI. SSH's empty keyboard-interactive
// exchange only opens that UI when no client key was supplied.
func appUIAuthentication() gossh.AuthMethod {
	return gossh.KeyboardInteractive(func(user, instruction string, questions []string, echo []bool) ([]string, error) {
		if len(questions) != 0 {
			return nil, errors.New("unexpected SSH credential prompt")
		}
		return []string{}, nil
	})
}

func keyUITestSigner(t *testing.T) gossh.Signer {
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

func TestKeyReminderCheckboxPersistsAndKeepsKeyboardMouseUsable(t *testing.T) {
	s, hub := featureFixture(t)
	m := testModel(t, s, hub, "alice")
	m.settings = s.Settings("alice")
	m.maybeKeyPrompt()
	if !m.keyPrompt {
		t.Fatal("unbound account did not receive key invitation")
	}
	for _, width := range []int{40, 60, 100} {
		m.w, m.h = width, 24
		for _, line := range strings.Split(m.View(), "\n") {
			if ansi.StringWidth(line) > width {
				t.Fatalf("dialog exceeds %d columns", width)
			}
		}
		if len(strings.Split(m.View(), "\n")) > m.h {
			t.Fatal("dialog exceeds viewport height")
		}
		for _, h := range m.keyDialogHits() {
			if h.x < 0 || h.y < 0 || h.x+h.w > m.w || h.y >= m.h {
				t.Fatal("invalid key invitation hit region", h)
			}
		}
	}
	for _, h := range m.keyDialogHits() {
		if h.value == "reminder" {
			m.keyDialogMouse(tea.MouseMsg{X: h.x + 1, Y: h.y, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
		}
	}
	if s.Settings("alice")["key_reminder"] != "off" || !strings.Contains(m.View(), "[x]") {
		t.Fatal("checkbox did not persist")
	}
	m.keyDialogKey(tea.KeyMsg{Type: tea.KeyEsc})
	if m.keyPrompt {
		t.Fatal("escape did not dismiss invitation")
	}
	next := testModel(t, s, hub, "alice")
	next.settings = s.Settings("alice")
	next.maybeKeyPrompt()
	if next.keyPrompt {
		t.Fatal("disabled reminder returned on next login")
	}
	if err := s.Setting("alice", "key_reminder", "on"); err != nil {
		t.Fatal(err)
	}
	next.settings = s.Settings("alice")
	next.maybeKeyPrompt()
	if !next.keyPrompt {
		t.Fatal("reminder could not be restored")
	}
}

func TestKeyBindingRequiresVerifiedConnectionAndRecentPassword(t *testing.T) {
	s, hub := featureFixture(t)
	m := testModel(t, s, hub, "alice")
	m.beginKeyBinding()
	if m.keyForm != nil || m.panelKind != "key-instructions" {
		t.Fatal("a connection without proof was allowed to bind a public key")
	}
	key := keyUITestSigner(t)
	m.sshPublicKey = key.PublicKey()
	m.beginKeyBinding()
	if m.keyForm == nil {
		t.Fatal("signed connection cannot open binding form")
	}
	if cmd := m.submitKeyBinding(); cmd != nil {
		t.Fatal("binding without a recent password did not require reauthentication")
	}
	keys, _ := s.SSHKeys("alice")
	if len(keys) != 0 {
		t.Fatal("key activated without account confirmation")
	}
	m.keyForm.fields[1].SetValue("wrong-password")
	cmd := m.submitKeyBinding()
	if cmd == nil {
		t.Fatal("reauthentication was not scheduled")
	}
	result := cmd().(sshKeyBindResult)
	m.finishKeyBinding(result)
	if result.err == nil {
		t.Fatal("wrong password bound a device")
	}
	m.keyForm.fields[1].SetValue("test-password-123")
	cmd = m.submitKeyBinding()
	result = cmd().(sshKeyBindResult)
	m.finishKeyBinding(result)
	if result.err != nil || m.keyForm != nil {
		t.Fatal("confirmed current-device binding failed", result.err)
	}
	keys, _ = s.SSHKeys("alice")
	if len(keys) != 1 || keys[0].Fingerprint != gossh.FingerprintSHA256(key.PublicKey()) {
		t.Fatal("bound a key other than this connection's verified key")
	}
	// A pasted public key must never be accepted as an alternative activation path.
	m.sshPublicKey = nil
	m.sshKeyCommand("/key-add", strings.TrimSpace(string(gossh.MarshalAuthorizedKey(keyUITestSigner(t).PublicKey()))))
	keys, _ = s.SSHKeys("alice")
	if len(keys) != 1 || m.keyForm != nil {
		t.Fatal("unverified pasted public key was activated")
	}
}

func TestFreshPasswordConfirmsCurrentDeviceAndMultipleKeys(t *testing.T) {
	s, hub := featureFixture(t)
	m := testModel(t, s, hub, "alice")
	m.passwordVerifiedAt = time.Now()
	for _, label := range []string{"笔记本", "手机终端"} {
		m.sshPublicKey = keyUITestSigner(t).PublicKey()
		m.beginKeyBinding()
		m.keyForm.fields[0].SetValue(label)
		cmd := m.submitKeyBinding()
		if cmd == nil {
			t.Fatal("fresh password could not confirm current device")
		}
		result := cmd().(sshKeyBindResult)
		m.finishKeyBinding(result)
		if result.err != nil {
			t.Fatal(result.err)
		}
	}
	keys, _ := s.SSHKeys("alice")
	if len(keys) != 2 || keys[0].Label == keys[1].Label {
		t.Fatal("account did not retain independently named devices")
	}
}

func TestAutomaticKeyLoginRechecksRevocationAndSessionOwnership(t *testing.T) {
	s, hub := featureFixture(t)
	hub.Attach(s)
	signer := keyUITestSigner(t)
	key, err := s.BindSSHKey("alice", "设备", string(gossh.MarshalAuthorizedKey(signer.PublicKey())))
	if err != nil {
		t.Fatal(err)
	}
	u, err := s.AuthenticateSSHKey(signer.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	m := newModel(s, hub, context.Background(), io.Discard, "xterm", 100, 30, "192.0.2.10", func() {})
	m.sshPublicKey = signer.PublicKey()
	m.sshAutoUser = &u
	m.authenticatedSignal = &atomic.Bool{}
	if cmd := m.completeLogin(u, true); cmd == nil || !m.logged || m.keyPrompt || !m.authenticatedSignal.Load() {
		t.Fatal("bound key failed to enter the original account")
	}
	defer hub.Leave(m.client)
	if !s.SessionValid(m.client.ID) {
		t.Fatal("key session association failed")
	}
	if err = s.RevokeSSHKey("alice", key.ID); err != nil {
		t.Fatal(err)
	}
	if s.SessionValid(m.client.ID) {
		t.Fatal("revoked key session stayed valid")
	}
	late := newModel(s, hub, context.Background(), io.Discard, "xterm", 100, 30, "192.0.2.10", func() {})
	late.sshPublicKey = signer.PublicKey()
	if cmd := late.completeLogin(u, true); cmd != nil || late.logged {
		t.Fatal("revoked cached SSH identity raced into the room")
	}
}

func TestKeyBindingFormMasksPasswordAndKeepsCJKHitRegions(t *testing.T) {
	s, hub := featureFixture(t)
	m := testModel(t, s, hub, "alice")
	m.sshPublicKey = keyUITestSigner(t).PublicKey()
	m.beginKeyBinding()
	m.keyForm.fields[0].SetValue("中文设备")
	m.keyForm.fields[1].SetValue("do-not-display-this-password")
	for _, width := range []int{40, 60, 100} {
		m.w, m.h = width, 24
		view := m.View()
		if strings.Contains(view, "do-not-display") {
			t.Fatal("binding dialog disclosed plaintext password")
		}
		for _, line := range strings.Split(view, "\n") {
			if ansi.StringWidth(line) > width {
				t.Fatal("binding form overflow", width)
			}
		}
		for _, h := range m.keyDialogHits() {
			if h.value == "field1" {
				m.keyDialogMouse(tea.MouseMsg{X: h.x, Y: h.y, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
				if m.keyForm.focus != 1 {
					t.Fatal("masked password field mouse hit missed")
				}
			}
		}
	}
}

func TestKeyBindingDoesNotExtendPasswordAuthorizationWithoutRecheck(t *testing.T) {
	s, hub := featureFixture(t)
	m := testModel(t, s, hub, "alice")
	original := time.Now().Add(-time.Minute)
	m.passwordVerifiedAt = original
	m.sshPublicKey = keyUITestSigner(t).PublicKey()
	m.beginKeyBinding()
	result := m.submitKeyBinding()().(sshKeyBindResult)
	m.finishKeyBinding(result)
	if result.err != nil || !m.passwordVerifiedAt.Equal(original) {
		t.Fatal("password confirmation window was extended without checking a password", result.err)
	}
	m.beginKeyBinding()
	m.keyForm.fields[1].SetValue("test-password-123")
	result = m.submitKeyBinding()().(sshKeyBindResult)
	m.finishKeyBinding(result)
	if result.err != nil || !m.passwordVerifiedAt.After(original) {
		t.Fatal("explicit correct password did not refresh confirmation", result.err)
	}
}

func TestDelayedAutomaticAuthenticationCannotReplaceLoggedInUser(t *testing.T) {
	s, hub := featureFixture(t)
	bob, _ := s.User("bob")
	alice, _ := s.User("alice")
	m := newModel(s, hub, context.Background(), io.Discard, "xterm-256color", 100, 30, "192.0.2.10", func() {})
	updated, _ := m.Update(authResult{bob, nil})
	m = updated.(model)
	defer hub.Leave(m.client)
	if !m.logged || m.user.Name != "bob" {
		t.Fatal("initial password login failed")
	}
	updated, _ = m.Update(sshAuthResult{alice, nil})
	m = updated.(model)
	if m.user.Name != "bob" || m.client.Name != "bob" || !m.logged {
		t.Fatal("delayed key result replaced the authenticated identity")
	}
	var owner string
	var count int
	s.db.QueryRow(`SELECT user FROM sessions WHERE id=?`, m.client.ID).Scan(&owner)
	s.db.QueryRow(`SELECT count(*) FROM sessions WHERE id=?`, m.client.ID).Scan(&count)
	if owner != "bob" || count != 1 {
		t.Fatal("session belongs to a different identity", owner, count)
	}
}

func TestPasswordChangeConfirmationRemainsVisibleInKeyInvitation(t *testing.T) {
	s, hub := featureFixture(t)
	m := testModel(t, s, hub, "alice")
	m.user.MustReset = true
	updated, _ := m.Update(passwordResult{})
	m = updated.(model)
	if m.user.MustReset || !m.keyPrompt || !strings.Contains(m.View(), "密码已修改") {
		t.Fatal("key invitation hid the password-change confirmation")
	}
}
