package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestGUICommunityNativeTransactionsAndDistinctParticipationPermissions(t *testing.T) {
	s := testStore(t)
	registerUser(t, s, "mutation-owner")
	registerUser(t, s, "mutation-reader")
	if err := s.CreateRoomAccess("mutation-room", "mutation-owner", true); err != nil {
		t.Fatal(err)
	}
	if err := s.RoomMember("mutation-owner", "mutation-room", "mutation-reader", "invite"); err != nil {
		t.Fatal(err)
	}
	owner, ownerReq := communityReader(t, s, "mutation-owner", "mutation-owner-session")
	reader, readerReq := communityReader(t, s, "mutation-reader", "mutation-reader-session")
	write := func(user User, request guiRequest, mutation communityMutation) int64 {
		t.Helper()
		id, err := s.CommunityForSessionContext(t.Context(), user.Name, request.sessionID, mutation)
		if err != nil {
			t.Fatal(mutation.command, err)
		}
		return id
	}
	poll := write(owner, ownerReq, communityMutation{command: "/poll", room: "mutation-room", text: "native poll|One|Two"})
	write(reader, readerReq, communityMutation{command: "/vote", objectID: poll, choice: 1})
	write(reader, readerReq, communityMutation{command: "/vote", objectID: poll, choice: 2})
	var choice string
	if err := s.db.QueryRow(`SELECT value FROM participation WHERE object_id=? AND user=?`, poll, reader.Name).Scan(&choice); err != nil || choice != "2" {
		t.Fatal(choice, err)
	}
	event := write(owner, ownerReq, communityMutation{command: "/event", room: "mutation-room", text: "native event|" + time.Now().Add(time.Hour).In(time.FixedZone("CST", 8*3600)).Format("2006-01-02 15:04") + "|description | with pipe"})
	write(reader, readerReq, communityMutation{command: "/comment", objectID: event, text: "first comment"})
	if _, err := s.db.Exec(`UPDATE room_meta SET readonly=1 WHERE name='mutation-room'`); err != nil {
		t.Fatal(err)
	}
	// Being unable to send does not revoke the original readable-room RSVP right.
	write(reader, readerReq, communityMutation{command: "/rsvp", objectID: event})
	write(reader, readerReq, communityMutation{command: "/rsvp", objectID: event})
	write(reader, readerReq, communityMutation{command: "/unrsvp", objectID: event})
	write(reader, readerReq, communityMutation{command: "/unrsvp", objectID: event})
	for _, mutation := range []communityMutation{{command: "/vote", objectID: poll, choice: 1}, {command: "/comment", objectID: event, text: "must fail"}, {command: "/close", objectID: event}} {
		if _, err := s.CommunityForSessionContext(t.Context(), reader.Name, readerReq.sessionID, mutation); err == nil {
			t.Fatal("permission bypass", mutation)
		}
	}
	write(owner, ownerReq, communityMutation{command: "/close", objectID: event})
	write(owner, ownerReq, communityMutation{command: "/close", objectID: event})
	if _, err := s.CommunityForSessionContext(t.Context(), owner.Name, ownerReq.sessionID, communityMutation{command: "/comment", objectID: event, text: "after close"}); err == nil {
		t.Fatal("comment after close")
	}
	if _, err := s.db.Exec(`UPDATE objects SET payload=? WHERE id=?`, strings.Repeat("x", maxCommunityPayloadBytes+1), poll); err != nil {
		t.Fatal(err)
	}
	if err := s.Comment(owner.Name, poll, "too large"); !errors.Is(err, ErrObjectTooLarge) {
		t.Fatal("oversized comment contract changed", err)
	}
	write(owner, ownerReq, communityMutation{command: "/close", objectID: poll})
	if _, err := s.CommunityForSessionContext(t.Context(), owner.Name, "", communityMutation{command: "/poll", room: "lobby", text: "no session|A|B"}); err == nil {
		t.Fatal("empty session fell back to TUI")
	}
	if _, err := s.CommunityForSessionContext(nil, owner.Name, ownerReq.sessionID, communityMutation{command: "/poll", room: "lobby", text: "nil context|A|B"}); err == nil {
		t.Fatal("nil context accepted")
	}
}
func TestGUICommunityQueuedAuthorityRecheckedInsideWriter(t *testing.T) {
	for _, kind := range []string{"revoked", "reset", "epoch", "key", "room", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			s := testStore(t)
			registerUser(t, s, "queue-owner")
			registerUser(t, s, "queue-reader")
			if err := s.CreateRoomAccess("queue-private", "queue-owner", true); err != nil {
				t.Fatal(err)
			}
			if err := s.RoomMember("queue-owner", "queue-private", "queue-reader", "invite"); err != nil {
				t.Fatal(err)
			}
			user, request := communityReader(t, s, "queue-reader", "queued-community")
			var boundKey int64
			if kind == "key" {
				key, e := s.BindSSHKey(user.Name, "synthetic queued key", testSSHKeyText(testSSHKeySigner(t).PublicKey()))
				if e != nil {
					t.Fatal(e)
				}
				boundKey = key.ID
				if _, e = s.db.Exec(`INSERT INTO session_credentials(session_id,key_id) VALUES(?,?)`, request.sessionID, boundKey); e != nil {
					t.Fatal(e)
				}
			}
			blocker, err := s.db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			waits := s.db.Stats().WaitCount
			done := make(chan error, 1)
			go func() {
				_, e := s.CommunityForSessionContext(ctx, user.Name, request.sessionID, communityMutation{command: "/poll", room: "queue-private", text: "never committed|A|B"})
				done <- e
			}()
			deadline := time.Now().Add(3 * time.Second)
			for s.db.Stats().WaitCount <= waits {
				if time.Now().After(deadline) {
					t.Fatal("writer did not queue")
				}
				time.Sleep(time.Millisecond)
			}
			switch kind {
			case "revoked":
				_, err = blocker.Exec(`UPDATE sessions SET revoked=1 WHERE id=?`, request.sessionID)
			case "reset":
				_, err = blocker.Exec(`UPDATE user_meta SET must_reset=1 WHERE name=?`, user.Name)
			case "epoch":
				_, err = blocker.Exec(`INSERT INTO credential_versions(user,epoch) VALUES(?,1) ON CONFLICT(user) DO UPDATE SET epoch=epoch+1`, user.Name)
			case "key":
				_, err = blocker.Exec(`UPDATE ssh_keys SET revoked=1 WHERE id=?`, boundKey)
			case "room":
				_, err = blocker.Exec(`DELETE FROM members WHERE room='queue-private' AND user=?`, user.Name)
			case "cancel":
				cancel()
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = blocker.Commit(); err != nil {
				t.Fatal(err)
			}
			select {
			case err = <-done:
				if err == nil {
					t.Fatal("queued mutation bypassed", kind)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("queued mutation did not finish")
			}
			var count int
			if err = s.db.QueryRow(`SELECT count(*) FROM objects`).Scan(&count); err != nil || count != 0 {
				t.Fatal("unauthorized object write", count, err)
			}
		})
	}
}
func TestGUICommunityBadgeFailureRollsBackObjectAndParticipation(t *testing.T) {
	s := testStore(t)
	registerUser(t, s, "fault-owner")
	registerUser(t, s, "fault-reader")
	owner, ownerReq := communityReader(t, s, "fault-owner", "fault-owner-session")
	reader, readerReq := communityReader(t, s, "fault-reader", "fault-reader-session")
	eventText := "future|" + time.Now().Add(time.Hour).In(time.FixedZone("CST", 8*3600)).Format("2006-01-02 15:04") + "|details"
	if _, err := s.db.Exec(`CREATE TRIGGER fail_event_badge BEFORE INSERT ON badges WHEN NEW.badge='组织者' BEGIN SELECT RAISE(ABORT,'synthetic event badge failure'); END;`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommunityForSessionContext(t.Context(), owner.Name, ownerReq.sessionID, communityMutation{command: "/event", room: "lobby", text: eventText}); err == nil {
		t.Fatal("badge fault ignored")
	}
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM objects`).Scan(&count); err != nil || count != 0 {
		t.Fatal("partial event", count, err)
	}
	if _, err := s.db.Exec(`DROP TRIGGER fail_event_badge`); err != nil {
		t.Fatal(err)
	}
	id, err := s.NewEvent(owner.Name, "lobby", eventText)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`CREATE TRIGGER fail_rsvp_badge BEFORE INSERT ON badges WHEN NEW.badge='参与者' BEGIN SELECT RAISE(ABORT,'synthetic RSVP badge failure'); END;`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CommunityForSessionContext(t.Context(), reader.Name, readerReq.sessionID, communityMutation{command: "/rsvp", objectID: id}); err == nil {
		t.Fatal("RSVP badge fault ignored")
	}
	if err = s.db.QueryRow(`SELECT count(*) FROM participation WHERE object_id=?`, id).Scan(&count); err != nil || count != 0 {
		t.Fatal("partial RSVP", count, err)
	}
}
func TestCommunityEventParserPreservesPipesAndFutureBoundary(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.FixedZone("CST", 8*3600))
	title, payload, err := eventInput("Title|2026-10-27 12:01|one|two", now)
	if err != nil || title != "Title" || !strings.Contains(payload, "one|two") {
		t.Fatal(title, payload, err)
	}
	if _, _, err = eventInput("past|2026-10-27 11:59|body", now); err == nil {
		t.Fatal("past event accepted")
	}
	if _, _, err = pollInput("only|one"); err == nil {
		t.Fatal("one option accepted")
	}
	for _, n := range []int{2, 10} {
		text := "question" + strings.Repeat("|choice", n)
		if _, _, err := pollInput(text); err != nil {
			t.Fatal(fmt.Sprint(n), err)
		}
	}
}

func TestGUICommunityIntentRoomAndCommittedWarnings(t *testing.T) {
	s := testStore(t)
	registerUser(t, s, "intent-owner")
	registerUser(t, s, "intent-peer")
	u, request := communityReader(t, s, "intent-owner", "intent-session")
	for _, room := range []string{"intent-a", "intent-b"} {
		if err := s.CreateRoomAccess(room, u.Name, false); err != nil {
			t.Fatal(err)
		}
	}
	hub := newHub()
	hub.Attach(s)
	scope := guiScope{peer: "intent-peer"}
	request.Body = "/poll captured A|one|two"
	request.ExpectedRoom = "intent-a"
	// The same visible DM can recur after A -> B -> DM. Only the internal
	// last-channel destination changed; client expected_room cannot authorize A.
	request.communityRoom = "intent-b"
	if _, err := guiExecuteAction(s, hub, u, scope, 91, request); err == nil {
		t.Fatal("roundtrip silently retargeted")
	}
	var count int
	s.db.QueryRow(`SELECT count(*) FROM objects`).Scan(&count)
	if count != 0 {
		t.Fatal(count)
	}
	request.communityRoom = "intent-a"
	result, err := guiExecuteAction(s, hub, u, scope, 91, request)
	if err != nil {
		t.Fatal(err)
	}
	receipt := result.communityReceipt
	if receipt == nil || !receipt.Committed || receipt.Room != "intent-a" || receipt.OriginScope != scope.key() || result.replyTo != 91 || result.scope != scope || result.message != nil {
		t.Fatalf("wrong receipt/result: %+v %+v", receipt, result)
	}
	request.Body = fmt.Sprintf("/comment %d retained input", receipt.ObjectID)
	request.ExpectedRoom = "intent-b"
	if _, err = guiExecuteAction(s, hub, u, scope, 91, request); err == nil {
		t.Fatal("stale object room accepted")
	}
	s.db.QueryRow(`SELECT count(*) FROM object_events`).Scan(&count)
	if count != 0 {
		t.Fatal(count)
	}
	// Both optional post-commit phases may fail: the actual handler uses this
	// exact orchestration helper, which cannot invoke the mutation again.
	announcements, projections := 0, 0
	warned := guiCommunityAfterCommit(guiActionResult{scope: scope, replyTo: 91}, *receipt, func() error { announcements++; return errors.New("synthetic announcement unavailable") }, func() (*guiPanel, error) { projections++; return nil, errors.New("synthetic read unavailable") })
	if announcements != 1 || projections != 1 || warned.communityReceipt != receipt && *warned.communityReceipt != *receipt || !strings.Contains(warned.status, "已提交") || warned.panel == nil {
		t.Fatal(warned)
	}
	s.db.QueryRow(`SELECT count(*) FROM objects`).Scan(&count)
	if count != 1 {
		t.Fatal(count)
	}
	// A real projection failure after a committed comment must also preserve the
	// committed receipt, not ordinary failure inviting duplicate comments.
	request.ExpectedRoom = "intent-a"
	committed, err := s.communityCommitForSessionContext(t.Context(), u.Name, request.sessionID, communityMutation{command: "/comment", objectID: receipt.ObjectID, text: "exactly once", expectedRoom: "intent-a"})
	if err != nil {
		t.Fatal(err)
	}
	committed.OriginScope = scope.key()
	guiCommunityAfterCommit(guiActionResult{scope: scope}, committed, func() error { return nil }, func() (*guiPanel, error) { return nil, errors.New("offline") })
	s.db.QueryRow(`SELECT count(*) FROM object_events`).Scan(&count)
	if count != 1 {
		t.Fatal(count)
	}
}

func TestGUICommunityWireChannelRoundtripAndReplyDraftIsolation(t *testing.T) {
	s := testStore(t)
	u := registerUser(t, s, "wire-community-owner")
	registerUser(t, s, "wire-community-peer")
	for _, room := range []string{"wire-community-a", "wire-community-b"} {
		if err := s.CreateRoomAccess(room, u.Name, false); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.SavePositionVersioned(u.Name, "room:lobby", Position{Draft: "keep reply draft", Anchor: 42, Offset: 3}); err != nil {
		t.Fatal(err)
	}
	before, err := s.PositionWithError(u.Name, "room:lobby")
	if err != nil {
		t.Fatal(err)
	}
	f := newGUIWireFixture(t, s, newHub())
	f.send(guiRequest{Type: "login", RequestID: "login", Username: u.Name, Password: "test-password-123"})
	f.read("auth", "login")
	f.read("state", "")
	selectScope := func(scope, id string) {
		t.Helper()
		time.Sleep(750 * time.Millisecond)
		f.send(guiRequest{Type: "select_room", RequestID: id, Room: scope})
		r := f.read("state", id)
		if r.Room != scope {
			t.Fatal(r)
		}
	}
	selectScope("wire-community-a", "a")
	selectScope("dm:wire-community-peer", "dm-a")
	time.Sleep(750 * time.Millisecond)
	f.send(guiRequest{Type: "command", RequestID: "capture", Room: "dm:wire-community-peer", Body: "/results"})
	captured := f.read("action", "capture")
	if captured.Panel.Community.Room != "wire-community-a" {
		t.Fatal(captured)
	}
	selectScope("wire-community-b", "b")
	selectScope("dm:wire-community-peer", "dm-b")
	time.Sleep(750 * time.Millisecond)
	f.send(guiRequest{Type: "command", RequestID: "stale", Room: "dm:wire-community-peer", ExpectedRoom: captured.Panel.Community.Room, Body: "/poll roundtrip|A|B"})
	if r := f.read("error", "stale"); !strings.Contains(r.Error, "目标频道") {
		t.Fatal(r)
	}
	var count int
	s.db.QueryRow(`SELECT count(*) FROM objects`).Scan(&count)
	if count != 0 {
		t.Fatal(count)
	}
	time.Sleep(750 * time.Millisecond)
	f.send(guiRequest{Type: "command", RequestID: "correct", Room: "dm:wire-community-peer", ExpectedRoom: "wire-community-b", Body: "/poll deliberate target|A|B"})
	r := f.read("action", "correct")
	if r.CommunityReceipt == nil || r.CommunityReceipt.Room != "wire-community-b" || r.Room != "dm:wire-community-peer" || r.Draft != nil || r.Replace || r.Message != nil {
		t.Fatal(r)
	}
	after, err := s.PositionWithError(u.Name, "room:lobby")
	if err != nil || after != before {
		t.Fatal(before, after, err)
	}
	for _, command := range []string{"/poll", "/vote", "/event", "/rsvp", "/unrsvp", "/close", "/comment"} {
		if !guiImplementedCommand(command) {
			t.Fatal("missing native menu", command)
		}
	}
}

func TestGUICommunityActualPostCommitFailuresNeverReturnFailedWrite(t *testing.T) {
	s := testStore(t)
	registerUser(t, s, "postcommit-owner")
	u, request := communityReader(t, s, "postcommit-owner", "postcommit-session")
	// Deliberate fixture failures: commit the object, then make its subsequent
	// typed read malformed and its optional announcement fail atomically.
	if _, err := s.db.Exec(`CREATE TRIGGER synthetic_projection_failure AFTER INSERT ON objects BEGIN UPDATE objects SET payload='{' WHERE id=NEW.id; END; CREATE TRIGGER synthetic_announcement_failure BEFORE INSERT ON messages BEGIN SELECT RAISE(ABORT,'synthetic announcement failure'); END;`); err != nil {
		t.Fatal(err)
	}
	request.Body = "/poll once only|A|B"
	request.ExpectedRoom = "lobby"
	hub := newHub()
	hub.Attach(s)
	result, err := guiExecuteAction(s, hub, u, guiScope{room: "lobby"}, 42, request)
	if err != nil || result.communityReceipt == nil || !result.communityReceipt.Committed || result.replyTo != 42 || !strings.Contains(result.status, "公告未发送") || !strings.Contains(result.status, "详情暂不可读") {
		t.Fatal(result, err)
	}
	var count int
	if err = s.db.QueryRow(`SELECT count(*) FROM objects`).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	if err = s.db.QueryRow(`SELECT count(*) FROM messages`).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
	if result.panel == nil || len(result.panel.Actions) != 1 || result.panel.Actions[0].Command != fmt.Sprintf("/object %d", result.communityReceipt.ObjectID) {
		t.Fatal(result.panel)
	}
}

func TestGUICommunityQueuedCommentCannotPassConcurrentClose(t *testing.T) {
	s := testStore(t)
	registerUser(t, s, "close-race-owner")
	u, request := communityReader(t, s, "close-race-owner", "close-race-session")
	id, err := s.Poll(u.Name, "lobby", "close race|A|B")
	if err != nil {
		t.Fatal(err)
	}
	blocker, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback()
	waits := s.db.Stats().WaitCount
	done := make(chan error, 1)
	go func() {
		_, e := s.CommunityForSessionContext(t.Context(), u.Name, request.sessionID, communityMutation{command: "/comment", objectID: id, text: "must not commit"})
		done <- e
	}()
	deadline := time.Now().Add(3 * time.Second)
	for s.db.Stats().WaitCount <= waits {
		if time.Now().After(deadline) {
			t.Fatal("comment not queued")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err = blocker.Exec(`UPDATE objects SET closed=1 WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	if err = blocker.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-done:
		if err == nil {
			t.Fatal("comment passed concurrent close")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("comment did not finish")
	}
	var count int
	if err = s.db.QueryRow(`SELECT count(*) FROM object_events WHERE object_id=?`, id).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
}
