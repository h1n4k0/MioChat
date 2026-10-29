package main

import (
	"fmt"
	gossh "golang.org/x/crypto/ssh"
	"reflect"
	"strings"
	"testing"
)

func TestGUIDraftCASConflictAndOwnerBoundBackup(t *testing.T) {
	s := testStore(t)
	alice := registerUser(t, s, "draft-alice")
	bob := registerUser(t, s, "draft-bob")
	h := newHub()
	f := newGUIWireFixture(t, s, h)
	f.send(guiRequest{Type: "login", RequestID: "auth", Username: alice.Name, Password: "test-password-123"})
	f.read("auth", "auth")
	f.read("state", "")
	f.send(guiRequest{Type: "draft_load", RequestID: "load", Room: "lobby"})
	baseline := f.read("draft", "load")
	if baseline.Draft == nil || baseline.Draft.Version != 0 {
		t.Fatal("missing explicit baseline")
	}
	if _, err := s.SavePositionVersioned(alice.Name, "room:lobby", Position{Draft: "other device draft"}); err != nil {
		t.Fatal(err)
	}
	f.send(guiRequest{Type: "draft_save", RequestID: "save", Room: "lobby", Version: 0, Body: "this device draft"})
	conflict := f.read("draft_conflict", "save")
	if conflict.Draft == nil || !conflict.Draft.Conflict || conflict.Draft.Version != 1 {
		t.Fatal(conflict)
	}
	if stored := s.Position(alice.Name, "room:lobby"); stored.Draft != "other device draft" {
		t.Fatal("CAS overwrote remote draft")
	}
	backup, found, err := s.ConflictDraft(alice.Name, "room:lobby", conflict.Draft.BackupID)
	if err != nil || !found || backup.Draft != "this device draft" {
		t.Fatal("local conflict was not preserved", err)
	}
	if _, found, err := s.ConflictDraft(bob.Name, "room:lobby", conflict.Draft.BackupID); err != nil || found {
		t.Fatal("backup crossed account boundary", err)
	}
	f.send(guiRequest{Type: "draft_keep", RequestID: "keep", Room: "lobby", Version: 1, Body: "this device draft", BackupID: conflict.Draft.BackupID})
	saved := f.read("draft_saved", "keep")
	if saved.Draft == nil || saved.Draft.Version != 2 {
		t.Fatal(saved)
	}
	if stored := s.Position(alice.Name, "room:lobby"); stored.Draft != "this device draft" {
		t.Fatal(stored)
	}
	if _, found, err := s.ConflictDraft(alice.Name, "room:lobby", conflict.Draft.BackupID); err != nil || found {
		t.Fatal("resolved backup retained unexpectedly", err)
	}
	f.send(guiRequest{Type: "draft_save", RequestID: "stale-scope", Room: "dev", Version: 0, Body: "must not move"})
	f.read("error", "stale-scope")
	if stored := s.Position(alice.Name, "room:dev"); stored.Draft != "" {
		t.Fatal("stale-scope draft changed destination")
	}
}

func TestGUIDraftReadFailureIsNotAnEmptyBaseline(t *testing.T) {
	s := testStore(t)
	u := registerUser(t, s, "draft-read-user")
	if _, err := s.db.Exec("DROP TABLE preferences"); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"draft_load", "draft_save"} {
		if _, err := guiDraftRequest(s, u, guiScope{room: "lobby"}, "fixture", guiRequest{Type: kind, Room: "lobby", Body: "must remain local"}); err == nil {
			t.Fatal("failed read treated as empty baseline", kind)
		}
	}
	var count int
	if err := s.db.QueryRow("SELECT count(*) FROM position_versions WHERE user=?", u.Name).Scan(&count); err != nil || count != 0 {
		t.Fatal("failed baseline caused a write", err)
	}
}

func TestGUISendClearsOnlyMatchingSavedDraftVersion(t *testing.T) {
	s := testStore(t)
	u := registerUser(t, s, "draft-send-user")
	h := newHub()
	f := newGUIWireFixture(t, s, h)
	f.send(guiRequest{Type: "login", RequestID: "auth", Username: u.Name, Password: "test-password-123"})
	f.read("auth", "auth")
	f.read("state", "")
	f.send(guiRequest{Type: "draft_load", RequestID: "load", Room: "lobby"})
	f.read("draft", "load")
	f.send(guiRequest{Type: "draft_save", RequestID: "save", Room: "lobby", Version: 0, Body: "saved outgoing fixture"})
	f.read("draft_saved", "save")
	f.send(guiRequest{Type: "send_message", RequestID: "send", Room: "lobby", Version: 1, Body: "saved outgoing fixture"})
	if r := f.read("sent", "send"); r.Draft == nil || r.Draft.Body != "" || r.Draft.Version != 2 {
		t.Fatal("matching draft was not cleared")
	}
	if _, err := s.SavePositionVersioned(u.Name, "room:lobby", Position{Version: 2, Draft: "same text from another device"}); err != nil {
		t.Fatal(err)
	}
	f.send(guiRequest{Type: "send_message", RequestID: "stale", Room: "lobby", Version: 2, Body: "same text from another device"})
	if r := f.read("sent", "stale"); r.Message == nil {
		t.Fatal("committed send was not acknowledged")
	}
	if p := s.Position(u.Name, "room:lobby"); p.Version != 3 || p.Draft != "same text from another device" {
		t.Fatal("send erased a concurrent device draft")
	}
}

func TestGUINativeDeviceKeyOwnershipAndConfirmation(t *testing.T) {
	s := testStore(t)
	alice := registerUser(t, s, "key-alice")
	bob := registerUser(t, s, "key-bob")
	// Administrator status must not expand this private owner-only key list.
	if _, err := s.db.Exec("UPDATE users SET role='admin' WHERE name=?", alice.Name); err != nil {
		t.Fatal(err)
	}
	alice, err := s.User(alice.Name)
	if err != nil {
		t.Fatal(err)
	}
	aKey, bKey := transportSigner(t), transportSigner(t)
	owned, err := s.BindSSHKey(alice.Name, "synthetic current", string(gossh.MarshalAuthorizedKey(aKey.PublicKey())))
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := s.BindSSHKey(bob.Name, "synthetic foreign", string(gossh.MarshalAuthorizedKey(bKey.PublicKey())))
	if err != nil {
		t.Fatal(err)
	}
	h := newHub()
	h.Attach(s)
	scope := guiScope{room: "lobby"}
	run := func(command string) (guiActionResult, error) {
		return guiExecuteAction(s, h, alice, scope, 0, guiRequest{Body: command})
	}
	r, err := run("/keys")
	if err != nil || r.panel == nil {
		t.Fatal(r, err)
	}
	// Native presentation groups this key's operations first, followed by one
	// global bind action. Rename fills an editable command; remove opens its
	// confirmation; bind only opens the guarded signed-key form.
	type action struct {
		Command string
		Fill    bool
	}
	var actual []action
	for _, item := range r.panel.Actions {
		if item.Label == "" {
			t.Fatal("unlabelled key action")
		}
		actual = append(actual, action{item.Command, item.Fill})
	}
	want := []action{{fmt.Sprintf("/key-rename %d ", owned.ID), true}, {fmt.Sprintf("/key-remove %d", owned.ID), false}, {"/key-add", false}}
	if !reflect.DeepEqual(actual, want) {
		t.Fatalf("unexpected owner-key operations/order: got %v want %v", actual, want)
	}
	lines := strings.Join(r.panel.Lines, "\n")
	if !strings.Contains(lines, owned.Fingerprint) || strings.Contains(lines, foreign.Fingerprint) {
		t.Fatal("key list omitted owner or exposed foreign key")
	}
	for _, command := range []string{"/key-add", "/key-bind"} {
		form, err := run(command)
		if err != nil || form.form != "key_bind" || form.scope != scope || form.message != nil {
			t.Fatal("binding action must only open native form", form, err)
		}
		if _, err = run(command + " ssh-ed25519 public-material-is-not-proof"); err == nil {
			t.Fatal("binding command accepted pasted public material")
		}
	}
	if keys, err := s.SSHKeys(alice.Name); err != nil || len(keys) != 1 || keys[0].ID != owned.ID {
		t.Fatal("opening key forms wrote credentials", err)
	}
	if _, err := run(fmt.Sprintf("/key-rename %d renamed fixture", owned.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := run(fmt.Sprintf("/key-rename %d forbidden", foreign.ID)); err == nil {
		t.Fatal("renamed foreign key")
	}
	if _, err := run(fmt.Sprintf("/key-remove %d", foreign.ID)); err == nil {
		t.Fatal("read foreign key confirmation")
	}
	if r, err := run(fmt.Sprintf("/key-remove %d", owned.ID)); err != nil || r.panel == nil || len(r.panel.Actions) != 1 {
		t.Fatal(r, err)
	}
	if keys, err := s.SSHKeys(alice.Name); err != nil || len(keys) != 1 {
		t.Fatal("confirmation step already revoked", err)
	}
	if _, err := run(fmt.Sprintf("/key-remove-confirm %d", owned.ID)); err != nil {
		t.Fatal(err)
	}
	if keys, err := s.SSHKeys(alice.Name); err != nil || len(keys) != 0 {
		t.Fatal("confirmed key not revoked", err)
	}
	if keys, err := s.SSHKeys(bob.Name); err != nil || len(keys) != 1 {
		t.Fatal("foreign key changed", err)
	}
}
