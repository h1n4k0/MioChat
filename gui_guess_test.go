package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func numberFixture(t *testing.T) (*Store, User, guiRequest, User, guiRequest) {
	t.Helper()
	s := testStore(t)
	registerUser(t, s, "number-owner")
	registerUser(t, s, "number-player")
	a, ar := communityReader(t, s, "number-owner", "number-owner-session")
	b, br := communityReader(t, s, "number-player", "number-player-session")
	return s, a, ar, b, br
}
func nativeNumber(t *testing.T, s *Store, u User, r guiRequest, m guessMutation) guiGuessReceipt {
	t.Helper()
	out, err := s.guessForSessionContext(t.Context(), u.Name, r.sessionID, m)
	if err != nil {
		t.Fatal(m, err)
	}
	return out
}
func fixedNumber(t *testing.T, s *Store, id int64) {
	t.Helper()
	payload, _ := json.Marshal(GuessGame{Secret: "5", Max: 10})
	if _, err := s.db.Exec(`UPDATE objects SET payload=? WHERE id=?`, string(payload), id); err != nil {
		t.Fatal(err)
	}
}
func TestGUINumberNumericRulesRepeatedAndActiveSecretProjection(t *testing.T) {
	s, a, ar, b, br := numberFixture(t)
	made := nativeNumber(t, s, a, ar, guessMutation{start: true, room: "lobby", value: "10"})
	fixedNumber(t, s, made.ObjectID)
	for range 150 {
		r := nativeNumber(t, s, b, br, guessMutation{room: "lobby", expectedRoom: "lobby", expectedID: made.ObjectID, value: "1"})
		if r.Closed || r.Feedback != "偏小" || r.Value != "1" {
			t.Fatal(r)
		}
	}
	for _, v := range []string{"05", "+5"} {
		r := nativeNumber(t, s, b, br, guessMutation{expectedID: made.ObjectID, expectedRoom: "lobby", value: v})
		if r.Closed || r.Feedback != "偏小" || r.Requested != v {
			t.Fatal("lexical win compatibility changed", r)
		}
	}
	obj, err := s.Object(a.Name, made.ObjectID)
	var state GuessGame
	json.Unmarshal([]byte(obj.Payload), &state)
	if err != nil || state.Wrong != 0 || len(state.Guesses) != 0 {
		t.Fatal(state, err)
	}
	br.communityRoom = "lobby"
	panel, err := guiGuessOverview(s, b, guiScope{peer: a.Name}, br)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(panel)
	if strings.Contains(string(data), `"secret"`) || strings.Contains(string(data), `"payload"`) || panel.Community.Detail.Game.Secret != "" || !panel.Community.Detail.Game.CanGuess || panel.Scope != "dm:"+a.Name {
		t.Fatal(string(data))
	}
	won := nativeNumber(t, s, b, br, guessMutation{expectedID: made.ObjectID, expectedRoom: "lobby", value: "5"})
	if !won.Closed || won.Feedback != "猜对了！答案是 5" {
		t.Fatal(won)
	}
	var count int
	if err = s.db.QueryRow(`SELECT count(*) FROM object_events WHERE object_id=?`, made.ObjectID).Scan(&count); err != nil || count != 153 {
		t.Fatal(count, err)
	}
	if err = s.db.QueryRow(`SELECT count(*) FROM badges WHERE user=? AND badge='解谜者'`, b.Name).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
}
func TestGUINumberCreationLimitsUniqueAndQueuedOldGame(t *testing.T) {
	s, a, ar, _, _ := numberFixture(t)
	for _, bad := range []string{"1", "10001", "not-number"} {
		if _, e := s.guessForSessionContext(t.Context(), a.Name, ar.sessionID, guessMutation{start: true, room: "lobby", value: bad}); e == nil {
			t.Fatal("invalid limit", bad)
		}
	}
	for _, value := range []string{"", "2", "10000"} {
		made := nativeNumber(t, s, a, ar, guessMutation{start: true, room: "lobby", value: value})
		want := 100
		if value == "2" {
			want = 2
		}
		if value == "10000" {
			want = 10000
		}
		if made.Maximum != want {
			t.Fatal(made)
		}
		if e := s.CloseObject(a.Name, made.ObjectID); e != nil {
			t.Fatal(e)
		}
	}
	outcomes := make(chan error, 2)
	for range 2 {
		go func() {
			_, e := s.guessForSessionContext(t.Context(), a.Name, ar.sessionID, guessMutation{start: true, room: "lobby", value: "10"})
			outcomes <- e
		}()
	}
	successes := 0
	for range 2 {
		if <-outcomes == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatal("concurrent starts", successes)
	}
	var old int64
	if e := s.db.QueryRow(`SELECT id FROM objects WHERE kind='guess' AND closed=0`).Scan(&old); e != nil {
		t.Fatal(e)
	}
	blocker, e := s.db.Begin()
	if e != nil {
		t.Fatal(e)
	}
	defer blocker.Rollback()
	waits := s.db.Stats().WaitCount
	done := make(chan error, 1)
	go func() {
		_, err := s.guessForSessionContext(t.Context(), a.Name, ar.sessionID, guessMutation{expectedID: old, expectedRoom: "lobby", value: "1"})
		done <- err
	}()
	deadline := time.Now().Add(3 * time.Second)
	for s.db.Stats().WaitCount <= waits {
		if time.Now().After(deadline) {
			t.Fatal("guess not queued")
		}
		time.Sleep(time.Millisecond)
	}
	if _, e = blocker.Exec(`UPDATE objects SET closed=1 WHERE id=?`, old); e != nil {
		t.Fatal(e)
	}
	fresh, e := newGuessQ(blocker, a.Name, "lobby", "guess", "10")
	if e != nil {
		t.Fatal(e)
	}
	if e = blocker.Commit(); e != nil {
		t.Fatal(e)
	}
	select {
	case e = <-done:
		if e == nil || !strings.Contains(e.Error(), "游戏已变化") {
			t.Fatal(e)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("guess did not finish")
	}
	var count int
	if e = s.db.QueryRow(`SELECT count(*) FROM object_events WHERE object_id=?`, fresh).Scan(&count); e != nil || count != 0 {
		t.Fatal(count, e)
	}
}
func TestGUINumberPermissionsObjectKindAndFaultAtomicity(t *testing.T) {
	s, a, ar, b, br := numberFixture(t)
	if e := s.CreateRoomAccess("number-private", a.Name, true); e != nil {
		t.Fatal(e)
	}
	if e := s.RoomMember(a.Name, "number-private", b.Name, "invite"); e != nil {
		t.Fatal(e)
	}
	made := nativeNumber(t, s, a, ar, guessMutation{start: true, room: "number-private", value: "10"})
	fixedNumber(t, s, made.ObjectID)
	for _, field := range []string{"readonly", "archived"} {
		column := field
		if field == "archived" {
			column = "archived"
		}
		if _, e := s.db.Exec(`UPDATE room_meta SET ` + column + `=1 WHERE name='number-private'`); e != nil {
			t.Fatal(e)
		}
		if _, e := s.guessForSessionContext(t.Context(), b.Name, br.sessionID, guessMutation{expectedID: made.ObjectID, expectedRoom: "number-private", value: "1"}); e == nil {
			t.Fatal("policy bypass", field)
		}
		if _, e := s.db.Exec(`UPDATE room_meta SET ` + column + `=0 WHERE name='number-private'`); e != nil {
			t.Fatal(e)
		}
	}
	if _, e := s.guessForSessionContext(t.Context(), b.Name, br.sessionID, guessMutation{expectedID: made.ObjectID, expectedRoom: "lobby", value: "1"}); e == nil {
		t.Fatal("room spoof")
	}
	word, e := s.NewGuess(a.Name, "number-private", "word", "synthetic-word")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.guessForSessionContext(t.Context(), b.Name, br.sessionID, guessMutation{expectedID: word, expectedRoom: "number-private", value: "1"}); e == nil {
		t.Fatal("kind spoof")
	}
	if _, e = s.db.Exec(`CREATE TRIGGER synthetic_number_badge BEFORE INSERT ON badges WHEN NEW.badge='解谜者' BEGIN SELECT RAISE(ABORT,'synthetic badge failure'); END;`); e != nil {
		t.Fatal(e)
	}
	failed, e := s.guessForSessionContext(t.Context(), b.Name, br.sessionID, guessMutation{expectedID: made.ObjectID, expectedRoom: "number-private", value: "5"})
	if e == nil || failed.Committed || failed.ObjectID != 0 {
		t.Fatal(failed, e)
	}
	obj, e := s.Object(a.Name, made.ObjectID)
	if e != nil || obj.Closed {
		t.Fatal(obj, e)
	}
	var count int
	if e = s.db.QueryRow(`SELECT count(*) FROM object_events WHERE object_id=?`, made.ObjectID).Scan(&count); e != nil || count != 0 {
		t.Fatal(count, e)
	}
	if _, e = s.db.Exec(`DELETE FROM members WHERE room='number-private' AND user=?`, b.Name); e != nil {
		t.Fatal(e)
	}
	if _, e = s.guessForSessionContext(t.Context(), b.Name, br.sessionID, guessMutation{expectedID: made.ObjectID, expectedRoom: "number-private", value: "1"}); e == nil {
		t.Fatal("revoked member")
	}
	if _, e = s.guessForSessionContext(t.Context(), a.Name, "", guessMutation{start: true, room: "lobby", value: "10"}); e == nil {
		t.Fatal("empty session")
	}
	if _, e = s.guessForSessionContext(nil, a.Name, ar.sessionID, guessMutation{start: true, room: "lobby", value: "10"}); e == nil {
		t.Fatal("nil context")
	}
}
func TestGUINumberActualPostCommitWarningsKeepSingleGame(t *testing.T) {
	for _, fault := range []string{"announce", "projection"} {
		t.Run(fault, func(t *testing.T) {
			s, a, ar, _, _ := numberFixture(t)
			h := newHub()
			h.Attach(s)
			sql := `CREATE TRIGGER synthetic_number_announce BEFORE INSERT ON messages BEGIN SELECT RAISE(ABORT,'synthetic announcement failure'); END;`
			if fault == "projection" {
				sql = `CREATE TRIGGER synthetic_number_projection AFTER INSERT ON messages BEGIN UPDATE objects SET payload='{' WHERE kind='guess'; END;`
			}
			if _, e := s.db.Exec(sql); e != nil {
				t.Fatal(e)
			}
			ar.Body = "/guess start 10"
			ar.ExpectedRoom = "lobby"
			result, e := guiExecuteAction(s, h, a, guiScope{room: "lobby"}, 81, ar)
			if e != nil || result.guessReceipt == nil || !result.guessReceipt.Committed || result.replyTo != 81 {
				t.Fatal(result, e)
			}
			warning := "公告未发送"
			if fault == "projection" {
				warning = "详情暂不可读"
			}
			if !strings.Contains(result.status, warning) {
				t.Fatal(result.status)
			}
			var n int
			if e = s.db.QueryRow(`SELECT count(*) FROM objects`).Scan(&n); e != nil || n != 1 {
				t.Fatal(n, e)
			}
		})
	}
}
func TestGUINumberWireOverviewMutationAndUnchangedDraft(t *testing.T) {
	s, a, _, _, _ := numberFixture(t)
	if _, e := s.SavePositionVersioned(a.Name, "room:lobby", Position{Draft: "unchanged numeric draft", ReplyTo: 0, Anchor: 9, Offset: 2}); e != nil {
		t.Fatal(e)
	}
	before, e := s.PositionWithError(a.Name, "room:lobby")
	if e != nil {
		t.Fatal(e)
	}
	f := newGUIWireFixture(t, s, newHub())
	f.send(guiRequest{Type: "login", RequestID: "login", Username: a.Name, Password: "test-password-123"})
	f.read("auth", "login")
	f.read("state", "")
	f.send(guiRequest{Type: "command", RequestID: "view", Room: "lobby", Body: "/guess"})
	empty := f.read("action", "view")
	if empty.Panel == nil || empty.Panel.Scope != "lobby" || empty.Panel.Community.Detail != nil {
		t.Fatal(empty)
	}
	time.Sleep(750 * time.Millisecond)
	f.send(guiRequest{Type: "command", RequestID: "start", Room: "lobby", ExpectedRoom: "lobby", Body: "/guess start 10"})
	made := f.read("action", "start")
	if made.GuessReceipt == nil || made.GuessReceipt.Maximum != 10 {
		t.Fatal(made)
	}
	fixedNumber(t, s, made.GuessReceipt.ObjectID)
	time.Sleep(750 * time.Millisecond)
	f.send(guiRequest{Type: "command", RequestID: "guess", Room: "lobby", ExpectedRoom: "lobby", GameID: made.GuessReceipt.ObjectID, Body: "/guess 1"})
	r := f.read("action", "guess")
	if r.GuessReceipt == nil || r.GuessReceipt.Closed || r.GuessReceipt.Feedback != "偏小" || r.Room != "lobby" || r.Draft != nil || r.Replace {
		t.Fatal(r)
	}
	data, _ := json.Marshal(r)
	if strings.Contains(string(data), `"secret"`) {
		t.Fatal(string(data))
	}
	after, e := s.PositionWithError(a.Name, "room:lobby")
	if e != nil || after != before {
		t.Fatal(before, after, e)
	}
	if !guiImplementedCommand("/guess") {
		t.Fatal("missing native menu")
	}
}

func TestGUINumberRememberedDMTargetAndQueuedSession(t *testing.T) {
	s, a, ar, b, _ := numberFixture(t)
	if e := s.CreateRoomAccess("number-other", a.Name, false); e != nil {
		t.Fatal(e)
	}
	h := newHub()
	h.Attach(s)
	ar.Body = "/guess start 10"
	ar.ExpectedRoom = "lobby"
	ar.communityRoom = "number-other"
	result, e := guiExecuteAction(s, h, a, guiScope{peer: b.Name}, 73, ar)
	if e == nil || result.guessReceipt != nil {
		t.Fatal("DM destination changed without refusal", result, e)
	}
	var n int
	if e = s.db.QueryRow(`SELECT count(*) FROM objects`).Scan(&n); e != nil || n != 0 {
		t.Fatal(n, e)
	}
	blocker, e := s.db.Begin()
	if e != nil {
		t.Fatal(e)
	}
	defer blocker.Rollback()
	waits := s.db.Stats().WaitCount
	done := make(chan error, 1)
	go func() {
		_, err := s.guessForSessionContext(t.Context(), a.Name, ar.sessionID, guessMutation{start: true, room: "lobby", value: "10"})
		done <- err
	}()
	deadline := time.Now().Add(3 * time.Second)
	for s.db.Stats().WaitCount <= waits {
		if time.Now().After(deadline) {
			t.Fatal("creation not queued")
		}
		time.Sleep(time.Millisecond)
	}
	if _, e = blocker.Exec(`UPDATE sessions SET revoked=1 WHERE id=?`, ar.sessionID); e != nil {
		t.Fatal(e)
	}
	if e = blocker.Commit(); e != nil {
		t.Fatal(e)
	}
	select {
	case e = <-done:
		if e == nil {
			t.Fatal("queued revoked session created game")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("creation did not finish")
	}
	if e = s.db.QueryRow(`SELECT count(*) FROM objects`).Scan(&n); e != nil || n != 0 {
		t.Fatal(n, e)
	}
}
