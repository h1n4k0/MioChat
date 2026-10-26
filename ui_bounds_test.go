package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	gossh "golang.org/x/crypto/ssh"
)

func TestTerminalDimensionsAreBoundedBeforeRendering(t *testing.T) {
	s := testStore(t)
	maximum := int(^uint(0) >> 1)
	for _, dimensions := range [][2]int{{maximum, maximum}, {100, 100000000}, {-1, -1}, {0, 0}} {
		m := newModel(s, newHub(), context.Background(), io.Discard, "xterm-256color", dimensions[0], dimensions[1], "192.0.2.10", func() {})
		if m.w > maxTerminalWidth || m.h > maxTerminalHeight || m.w < minTerminalWidth || m.h < minTerminalHeight || m.w*m.h > maxTerminalCells {
			t.Fatal("unsafe constructor dimensions", m.w, m.h)
		}
		// Assert bounds before asking the model to allocate its displayed rows.
		view := m.View()
		if len(strings.Split(view, "\n")) > maxTerminalHeight || len(view) > 256000 {
			t.Fatal("bounded login form produced excessive output")
		}
		for _, row := range strings.Split(view, "\n") {
			if ansi.StringWidth(row) > maxTerminalWidth {
				t.Fatal("login form exceeds width bound")
			}
		}
		updated, _ := m.Update(tea.WindowSizeMsg{Width: maximum, Height: maximum})
		m = updated.(model)
		if m.w != maxTerminalWidth || m.h != maxTerminalHeight {
			t.Fatal("unsafe resize dimensions", m.w, m.h)
		}
		m.input.Width = 17
		updated, _ = m.Update(tea.WindowSizeMsg{Width: maximum, Height: maximum})
		m = updated.(model)
		if m.input.Width != 17 {
			t.Fatal("identical normalized resize rebuilt the model")
		}
	}
}

func TestRealSSHModeratelyOversizedPTYStillRendersBoundedLogin(t *testing.T) {
	s := testStore(t)
	hub := newHub()
	signer := keyUITestSigner(t)
	server := makeServer(s, hub, signer)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close(); hub.uiTasks.Wait() })
	client, err := gossh.Dial("tcp", listener.Addr().String(), &gossh.ClientConfig{User: "chat", Auth: []gossh.AuthMethod{appUIAuthentication()}, HostKeyCallback: gossh.FixedHostKey(signer.PublicKey()), Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	terminal := &liveTerminal{client: client, session: session}
	terminal.in, err = session.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	session.Stdout, session.Stderr = terminal, terminal
	// This is large enough to exceed the policy, but remains safe even if a
	// future regression were to remove the policy before the test can observe it.
	if err = session.RequestPty("xterm-256color", 400, 1000, gossh.TerminalModes{gossh.ECHO: 0}); err != nil {
		t.Fatal(err)
	}
	if err = session.Shell(); err != nil {
		t.Fatal(err)
	}
	terminal.wait(t, "TERMINAL SOCIAL CLUB")
	if len(terminal.output()) > 256000 {
		t.Fatal("SSH login output exceeds bounded dimensions")
	}
}

func TestRemovedPrivateMemberCannotPublishSuspendedCompositionInLobby(t *testing.T) {
	s, hub := featureFixture(t)
	if err := s.CreateRoomAccess("private-draft", "alice", true); err != nil {
		t.Fatal(err)
	}
	if err := s.RoomMember("alice", "private-draft", "bob", "invite"); err != nil {
		t.Fatal(err)
	}
	m := testModel(t, s, hub, "bob")
	if err := m.move("private-draft", ""); err != nil {
		t.Fatal(err)
	}
	m.startCompose("post", "私密草稿")
	m.editor.SetValue("PRIVATE_DRAFT_BODY 不得公开")
	if err := s.RoomMember("alice", "private-draft", "bob", "remove"); err != nil {
		t.Fatal(err)
	}
	updated, _ := m.Update(Event{MoveRoom: "lobby"})
	m = updated.(model)
	if m.room != "lobby" || m.composeRoom != "private-draft" || !m.composeFrozen {
		t.Fatal("editor lost its immutable origin", m.room, m.composeRoom, m.composeFrozen)
	}
	if !strings.Contains(m.View(), "private-draft") || !strings.Contains(m.View(), "发布已暂停") {
		t.Fatal("suspended editor does not identify its original room")
	}
	m.composeKey(tea.KeyMsg{Type: tea.KeyF4})
	var objects int
	s.db.QueryRow(`SELECT count(*) FROM objects WHERE payload LIKE '%PRIVATE_DRAFT_BODY%'`).Scan(&objects)
	if objects != 0 {
		t.Fatal("removed private content was published to another room")
	}
	var body string
	s.db.QueryRow(`SELECT body FROM compositions WHERE user='bob' AND room='private-draft' AND kind='post'`).Scan(&body)
	if body != "PRIVATE_DRAFT_BODY 不得公开" {
		t.Fatal("suspended owner draft was not preserved")
	}
	if err := s.SaveSuspendedComposition("eve", "private-draft", "post", "", "attempt"); err == nil {
		t.Fatal("suspended save created a new unauthorized draft")
	}
	m.composeKey(tea.KeyMsg{Type: tea.KeyEsc})
	if m.composeKind != "" {
		t.Fatal("preserved suspended editor could not close")
	}
	_, hidden := s.Composition("bob", "private-draft", "post")
	if hidden != "" {
		t.Fatal("suspended save granted revoked room read access")
	}
}

func TestHistoricalDirectMessagesKeepBoundedFeedAndDatabaseHistory(t *testing.T) {
	s, hub := featureFixture(t)
	const total = 10000
	if _, err := s.db.Exec(`WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<10000) INSERT INTO messages(room,sender,recipient,body,kind,at) SELECT '','bob','alice','历史私信','chat',1700000000+x FROM n`); err != nil {
		t.Fatal(err)
	}
	m := testModel(t, s, hub, "alice")
	if err := m.move("", "bob"); err != nil {
		t.Fatal(err)
	}
	m.feed = nil
	for id := int64(1); id <= 100; id++ {
		m.feed = append(m.feed, Message{ID: id, Sender: "bob", Recipient: "alice", Body: "历史私信", Kind: "chat", At: 1700000000 + id})
	}
	m.feedAtLatest = false
	m.latestMessageID = 100
	m.rebuild(false)
	m.view.SetYOffset(0)
	if m.view.AtBottom() {
		t.Fatal("fixture must be reading historical messages")
	}
	for id := int64(101); id <= total; id++ {
		m.consumeEvents(Event{Message: &Message{ID: id, Sender: "bob", Recipient: "alice", Body: "历史私信", Kind: "chat", At: 1700000000 + id}})
		if len(m.feed) > maxFeedMessages || len(m.messageCache) > maxCachedMessages {
			t.Fatal("direct-message memory window exceeded its independent limit")
		}
	}
	if m.newMessages != total-100 || m.feed[0].ID != 1 {
		t.Fatal("reading anchor or unread arrival count was lost", m.newMessages, m.feed[0].ID)
	}
	var count int
	s.db.QueryRow(`SELECT count(*) FROM messages`).Scan(&count)
	if count != total {
		t.Fatal("memory cap deleted durable messages", count)
	}
	m.keyInteractions(tea.KeyMsg{Type: tea.KeyCtrlEnd})
	if m.feed[len(m.feed)-1].ID != total || m.newMessages != 0 {
		t.Fatal("return-to-latest did not reload durable messages")
	}
}

func TestRepeatedWordGuessCannotGrowMutableStateOrEvents(t *testing.T) {
	s, _ := featureFixture(t)
	id, err := s.NewGuess("alice", "arcade", "word", "ab")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Guess("bob", "arcade", "word", "a"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10000; i++ {
		if _, err = s.Guess("bob", "arcade", "word", "a"); err == nil {
			t.Fatal("duplicate guess was accepted")
		}
	}
	var payload string
	var events int
	s.db.QueryRow(`SELECT payload FROM objects WHERE id=?`, id).Scan(&payload)
	s.db.QueryRow(`SELECT count(*) FROM object_events WHERE object_id=?`, id).Scan(&events)
	var game GuessGame
	if err = json.Unmarshal([]byte(payload), &game); err != nil || len(game.Guesses) != 1 || events != 1 || game.Wrong != 0 {
		t.Fatal("duplicate guesses expanded state", len(game.Guesses), events, game.Wrong, err)
	}
	// Old, oversized states fail before their JSON is decoded or updated.
	if _, err = s.db.Exec(`UPDATE objects SET payload=? WHERE id=?`, strings.Repeat("x", maxCommunityPayloadBytes+1), id); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Guess("bob", "arcade", "word", "b"); err == nil {
		t.Fatal("oversized legacy game payload was accepted")
	}
}

func TestIdlePositionSavesDoNotWriteAndConflictsRetainOwnerDraft(t *testing.T) {
	s, hub := featureFixture(t)
	a := testModel(t, s, hub, "alice")
	b := testModel(t, s, hub, "alice")
	a.saveView()
	version := s.Position("alice", "room:lobby").Version
	for i := 0; i < 100; i++ {
		a.saveView()
	}
	if got := s.Position("alice", "room:lobby").Version; got != version {
		t.Fatal("idle view performed dirty-state writes", version, got)
	}
	a.input.SetValue("设备A已经保存")
	a.saveView()
	b.input.SetValue("设备B保留的本地草稿")
	b.saveView()
	if !b.positionConflict || b.panelKind != "draft-conflict" {
		t.Fatal("concurrent draft update did not require explicit resolution")
	}
	if got := s.Position("alice", "room:lobby").Draft; got != "设备A已经保存" {
		t.Fatal("stale device silently overwrote the newer draft", got)
	}
	private, found, err := s.ConflictDraft("alice", "room:lobby", b.client.ID)
	if err != nil || !found || private.Draft != b.input.Value() {
		t.Fatal("local conflicted draft was not preserved", found, err)
	}
	if _, found, _ := s.ConflictDraft("bob", "room:lobby", b.client.ID); found {
		t.Fatal("another account read a conflict draft")
	}
	for i := 0; i < 10; i++ {
		b.saveView()
	}
	if got := s.Position("alice", "room:lobby").Draft; got != "设备A已经保存" {
		t.Fatal("conflicted autosave overwrote the remote draft")
	}
	b.resolvePositionConflict(true)
	if b.positionConflict || s.Position("alice", "room:lobby").Draft != "设备B保留的本地草稿" {
		t.Fatal("explicit local resolution failed")
	}
	if _, found, _ = s.ConflictDraft("alice", "room:lobby", b.client.ID); found {
		t.Fatal("resolved conflict backup remained active")
	}
	t.Log(fmt.Sprintf("owner conflict draft resolved at version %d", b.positionVersion))
}

func TestReadProgressCollisionAdoptsRemoteDraftWithoutConflictModal(t *testing.T) {
	s, hub := featureFixture(t)
	a := testModel(t, s, hub, "alice")
	b := testModel(t, s, hub, "alice")
	a.input.SetValue("另一设备的新草稿")
	a.saveView()
	message := mustSend(t, s, "bob", "lobby", "", "新消息")
	b.feed = []Message{message}
	b.rebuild(true)
	b.saveView()
	if b.positionConflict || b.panelKind == "draft-conflict" || b.input.Value() != "另一设备的新草稿" {
		t.Fatal("read-only collision created a draft conflict or lost the remote draft")
	}
	stored := s.Position("alice", "room:lobby")
	if stored.Draft != "另一设备的新草稿" || stored.LastRead < message.ID {
		t.Fatal("read metadata merge overwrote the remote draft or regressed progress")
	}
	version := stored.Version
	for i := 0; i < 10; i++ {
		b.saveView()
	}
	if s.Position("alice", "room:lobby").Version != version {
		t.Fatal("idle merged view kept writing metadata")
	}
}

func TestDraftWriteFailureBlocksNormalMoveAndForcedMoveKeepsOneEmergency(t *testing.T) {
	s, hub := featureFixture(t)
	if err := s.CreateRoomAccess("restricted-input", "alice", true); err != nil {
		t.Fatal(err)
	}
	if err := s.RoomMember("alice", "restricted-input", "bob", "invite"); err != nil {
		t.Fatal(err)
	}
	m := testModel(t, s, hub, "bob")
	if err := m.move("restricted-input", ""); err != nil {
		t.Fatal(err)
	}
	m.input.SetValue("PRIVATE_UNSENT_INPUT")
	if _, err := s.db.Exec(`CREATE TRIGGER fail_position BEFORE INSERT ON position_versions BEGIN SELECT RAISE(ABORT,'injected position write failure');END;CREATE TRIGGER fail_conflict BEFORE INSERT ON draft_conflicts BEGIN SELECT RAISE(ABORT,'injected draft backup failure');END`); err != nil {
		t.Fatal(err)
	}
	if err := m.move("dev", ""); err == nil || m.room != "restricted-input" || m.input.Value() != "PRIVATE_UNSENT_INPUT" {
		t.Fatal("normal move discarded a draft that could not be saved")
	}
	if err := s.RoomMember("alice", "restricted-input", "bob", "remove"); err != nil {
		t.Fatal(err)
	}
	updated, _ := m.Update(Event{MoveRoom: "lobby"})
	m = updated.(model)
	if m.room != "lobby" || m.input.Value() != "" || m.emergencyDraft == nil || m.emergencyDraft.Scope != "room:restricted-input" || m.emergencyDraft.Position.Draft != "PRIVATE_UNSENT_INPUT" {
		t.Fatal("forced move leaked, lost or re-scoped the unsaved input")
	}
	if m.panelKind != "emergency-draft" || !strings.Contains(m.View(), "未能落盘") {
		t.Fatal("emergency draft did not produce a visible persistence warning")
	}
	updated, _ = m.Update(Event{Refresh: true})
	m = updated.(model)
	if !strings.Contains(m.View(), "未能落盘") {
		t.Fatal("asynchronous refresh hid the unsaved draft warning")
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("do-not-overwrite")})
	m = updated.(model)
	if m.input.Value() != "" || m.emergencyDraft.Position.Draft != "PRIVATE_UNSENT_INPUT" {
		t.Fatal("another draft overwrote the emergency slot")
	}
	if err := m.forceMove("dev"); err != nil || m.emergencyDraft == nil || m.emergencyDraft.Scope != "room:restricted-input" {
		t.Fatal("subsequent mandatory move replaced the original emergency draft", err)
	}
	if _, err := s.db.Exec(`DROP TRIGGER fail_position;DROP TRIGGER fail_conflict`); err != nil {
		t.Fatal(err)
	}
	if err := m.saveView(); err != nil || m.emergencyDraft != nil {
		t.Fatal("recovered storage did not preserve the emergency draft", err)
	}
	backup, found, err := s.ConflictDraft("bob", "room:restricted-input", m.client.ID)
	if err != nil || !found || backup.Draft != "PRIVATE_UNSENT_INPUT" {
		t.Fatal("emergency persistence changed its owner or origin", found, err)
	}
	var published int
	s.db.QueryRow(`SELECT count(*) FROM messages WHERE body='PRIVATE_UNSENT_INPUT'`).Scan(&published)
	if published != 0 {
		t.Fatal("unsent private input became a public message")
	}
}

func TestOversizedStoredObjectIsNotRenderedAndCanStillBeClosed(t *testing.T) {
	s, _ := featureFixture(t)
	id, err := s.NewGuess("alice", "arcade", "word", "ab")
	if err != nil {
		t.Fatal(err)
	}
	oversized := strings.Repeat("界", maxCommunityPayloadBytes/3+1)
	if _, err = s.db.Exec(`UPDATE objects SET payload=? WHERE id=?`, oversized, id); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Object("bob", id); !errors.Is(err, ErrObjectTooLarge) {
		t.Fatal("oversized bytes were decoded as a normal object", err)
	}
	items, err := s.Objects("bob", "arcade", "word")
	if err != nil || len(items) != 1 || len(items[0].Payload) > 128 || !strings.Contains(items[0].Title, "状态过大") {
		t.Fatal("oversized list entry did not use bounded metadata", err)
	}
	if err = s.CloseObject("eve", id); err == nil {
		t.Fatal("size fallback relaxed object ownership")
	}
	if err = s.CloseObject("alice", id); err != nil {
		t.Fatal("owner cannot close an oversized stored game", err)
	}
	var payload string
	var closed bool
	s.db.QueryRow(`SELECT payload,closed FROM objects WHERE id=?`, id).Scan(&payload, &closed)
	if payload != oversized || !closed {
		t.Fatal("closing an oversized game deleted or rewrote its durable payload")
	}
	if _, err = s.NewGuess("alice", "arcade", "word", "cd"); err != nil {
		t.Fatal("closed oversized game prevents a new bounded game", err)
	}
}
