package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func ticFixture(t *testing.T) (*Store, User, guiRequest, User, guiRequest) {
	t.Helper()
	s := testStore(t)
	registerUser(t, s, "tic-alice")
	registerUser(t, s, "tic-bob")
	a, ar := communityReader(t, s, "tic-alice", "tic-a-session")
	b, br := communityReader(t, s, "tic-bob", "tic-b-session")
	return s, a, ar, b, br
}
func ticNative(t *testing.T, s *Store, u User, r guiRequest, m ticMutation) guiTicReceipt {
	t.Helper()
	out, e := s.ticForSessionContext(t.Context(), u.Name, r.sessionID, m)
	if e != nil {
		t.Fatal(m, e)
	}
	return out
}
func TestGUITicNativeRevisionZeroJumpReplayAndClosed(t *testing.T) {
	s, a, ar, b, br := ticFixture(t)
	made := ticNative(t, s, a, ar, ticMutation{operation: "challenge", room: "lobby", expectedRoom: "lobby", target: "TIC-BOB"})
	if made.ObjectID <= 0 || made.Revision != 0 || made.Requested != "TIC-BOB" || made.Canonical != b.Name || !made.Committed {
		t.Fatal(made)
	}
	zero := int64(0)
	if _, e := s.ticForSessionContext(t.Context(), a.Name, ar.sessionID, ticMutation{operation: "accept", id: made.ObjectID, expectedRevision: &zero}); e == nil {
		t.Fatal("X accepted")
	}
	accept := ticNative(t, s, b, br, ticMutation{operation: "accept", id: made.ObjectID, expectedRevision: &zero})
	if accept.Revision <= 0 {
		t.Fatal(accept)
	}
	if _, e := s.ticForSessionContext(t.Context(), b.Name, br.sessionID, ticMutation{operation: "accept", id: made.ObjectID, expectedRevision: &zero}); e == nil {
		t.Fatal("duplicate accept")
	}
	// Revisions are global event IDs, not board-local counters.
	other := ticNative(t, s, a, ar, ticMutation{operation: "challenge", room: "lobby", target: b.Name})
	for range 4 {
		if e := s.Comment(a.Name, other.ObjectID, "unrelated event"); e != nil {
			t.Fatal(e)
		}
	}
	first := ticNative(t, s, a, ar, ticMutation{operation: "move", id: made.ObjectID, cell: 1, expectedRevision: &accept.Revision})
	if first.Revision <= accept.Revision+1 || first.Cell != 1 {
		t.Fatal(accept, first)
	}
	second := ticNative(t, s, b, br, ticMutation{operation: "move", id: made.ObjectID, cell: 4, expectedRevision: &first.Revision})
	if _, e := s.ticForSessionContext(t.Context(), a.Name, ar.sessionID, ticMutation{operation: "move", id: made.ObjectID, cell: 2, expectedRevision: &accept.Revision}); e == nil {
		t.Fatal("old request passed after two turns")
	}
	third := ticNative(t, s, a, ar, ticMutation{operation: "move", id: made.ObjectID, cell: 2, expectedRevision: &second.Revision})
	if e := s.CloseObject(a.Name, made.ObjectID); e != nil {
		t.Fatal(e)
	}
	if _, e := s.ticForSessionContext(t.Context(), b.Name, br.sessionID, ticMutation{operation: "move", id: made.ObjectID, cell: 5, expectedRevision: &third.Revision}); e == nil {
		t.Fatal("same-revision closed board accepted")
	}
	if _, e := s.ticForSessionContext(t.Context(), a.Name, "", ticMutation{operation: "challenge", room: "lobby", target: b.Name}); e == nil {
		t.Fatal("empty session")
	}
	if _, e := s.ticForSessionContext(nil, a.Name, ar.sessionID, ticMutation{operation: "challenge", room: "lobby", target: b.Name}); e == nil {
		t.Fatal("nil context")
	}
}
func TestGUITicNativeEveryWinningLineDrawAndFaultRollback(t *testing.T) {
	s, a, ar, b, br := ticFixture(t)
	for _, line := range [][3]int{{0, 1, 2}, {3, 4, 5}, {6, 7, 8}, {0, 3, 6}, {1, 4, 7}, {2, 5, 8}, {0, 4, 8}, {2, 4, 6}} {
		made := ticNative(t, s, a, ar, ticMutation{operation: "challenge", room: "lobby", target: b.Name})
		zero := int64(0)
		accepted := ticNative(t, s, b, br, ticMutation{operation: "accept", id: made.ObjectID, expectedRevision: &zero})
		game := TicTacToe{X: a.Name, O: b.Name, Turn: a.Name, Accepted: true}
		game.Cells[line[0]] = "X"
		game.Cells[line[1]] = "X"
		n := 0
		for cell := range 9 {
			if cell != line[0] && cell != line[1] && cell != line[2] && n < 2 {
				game.Cells[cell] = "O"
				n++
			}
		}
		data, _ := json.Marshal(game)
		if _, e := s.db.Exec(`UPDATE objects SET payload=? WHERE id=?`, string(data), made.ObjectID); e != nil {
			t.Fatal(e)
		}
		ticNative(t, s, a, ar, ticMutation{operation: "move", id: made.ObjectID, cell: line[2] + 1, expectedRevision: &accepted.Revision})
		obj, e := s.Object(a.Name, made.ObjectID)
		if e != nil || !obj.Closed {
			t.Fatal(obj, e)
		}
		json.Unmarshal([]byte(obj.Payload), &game)
		if game.Winner != a.Name {
			t.Fatal(line, game)
		}
	}
	made := ticNative(t, s, a, ar, ticMutation{operation: "challenge", room: "lobby", target: b.Name})
	zero := int64(0)
	last := ticNative(t, s, b, br, ticMutation{operation: "accept", id: made.ObjectID, expectedRevision: &zero})
	for index, cell := range []int{1, 2, 3, 5, 4, 6, 8, 7, 9} {
		u, r := a, ar
		if index%2 == 1 {
			u, r = b, br
		}
		last = ticNative(t, s, u, r, ticMutation{operation: "move", id: made.ObjectID, cell: cell, expectedRevision: &last.Revision})
	}
	obj, e := s.Object(a.Name, made.ObjectID)
	var game TicTacToe
	json.Unmarshal([]byte(obj.Payload), &game)
	if e != nil || !obj.Closed || game.Winner != "draw" {
		t.Fatal(obj, game, e)
	}
	made = ticNative(t, s, a, ar, ticMutation{operation: "challenge", room: "lobby", target: b.Name})
	last = ticNative(t, s, b, br, ticMutation{operation: "accept", id: made.ObjectID, expectedRevision: &zero})
	before, e := s.Object(a.Name, made.ObjectID)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.db.Exec(`CREATE TRIGGER synthetic_tic_event BEFORE INSERT ON object_events BEGIN SELECT RAISE(ABORT,'synthetic game event fault'); END;`); e != nil {
		t.Fatal(e)
	}
	result, e := s.ticForSessionContext(t.Context(), a.Name, ar.sessionID, ticMutation{operation: "move", id: made.ObjectID, cell: 1, expectedRevision: &last.Revision})
	if e == nil || result.ObjectID != 0 || result.Committed {
		t.Fatal(result, e)
	}
	after, e := s.Object(a.Name, made.ObjectID)
	if e != nil || after.Payload != before.Payload || after.Closed != before.Closed {
		t.Fatal(before, after, e)
	}
}
func TestGUITicQueuedTargetBlockAndOwnerSession(t *testing.T) {
	for _, kind := range []string{"target-block", "session"} {
		t.Run(kind, func(t *testing.T) {
			s, a, ar, b, _ := ticFixture(t)
			blocker, e := s.db.Begin()
			if e != nil {
				t.Fatal(e)
			}
			defer blocker.Rollback()
			waits := s.db.Stats().WaitCount
			done := make(chan error, 1)
			go func() {
				_, err := s.ticForSessionContext(context.Background(), a.Name, ar.sessionID, ticMutation{operation: "challenge", room: "lobby", target: b.Name})
				done <- err
			}()
			deadline := time.Now().Add(3 * time.Second)
			for s.db.Stats().WaitCount <= waits {
				if time.Now().After(deadline) {
					t.Fatal("not queued")
				}
				time.Sleep(time.Millisecond)
			}
			if kind == "session" {
				_, e = blocker.Exec(`UPDATE sessions SET revoked=1 WHERE id=?`, ar.sessionID)
			} else {
				_, e = blocker.Exec(`INSERT INTO relations(owner,target,kind,at) VALUES(?,?,'block',1)`, b.Name, a.Name)
			}
			if e != nil {
				t.Fatal(e)
			}
			if e = blocker.Commit(); e != nil {
				t.Fatal(e)
			}
			select {
			case e = <-done:
				if e == nil {
					t.Fatal("queued creation bypassed", kind)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("didn't finish")
			}
			var count int
			if e = s.db.QueryRow(`SELECT count(*) FROM objects`).Scan(&count); e != nil || count != 0 {
				t.Fatal(count, e)
			}
		})
	}
}
func TestGUITicWireTwoParticipantsAndExpectedZero(t *testing.T) {
	s, a, _, b, _ := ticFixture(t)
	h := newHub()
	fa, fb := newGUIWireFixture(t, s, h), newGUIWireFixture(t, s, h)
	for i, f := range []*guiWireFixture{&fa, &fb} {
		name := a.Name
		if i == 1 {
			name = b.Name
		}
		f.send(guiRequest{Type: "login", RequestID: "login", Username: name, Password: "test-password-123"})
		f.read("auth", "login")
		f.read("state", "")
	}
	fa.send(guiRequest{Type: "command", RequestID: "create", Room: "lobby", ExpectedRoom: "lobby", Body: "/ttt " + b.Name})
	created := fa.read("action", "create")
	if created.TicReceipt == nil || !created.TicReceipt.Committed || created.TicReceipt.Operation != "challenge" || created.TicReceipt.ObjectID <= 0 || created.TicReceipt.Room != "lobby" || created.TicReceipt.OriginScope != "lobby" || created.TicReceipt.Revision != 0 {
		t.Fatal(created)
	}
	id := created.TicReceipt.ObjectID
	if game := requireTicWireGame(t, fa, created, id, "create-read"); game.CanAccept {
		t.Fatal("challenger can accept own game")
	}
	zero := int64(0)
	fb.send(guiRequest{Type: "command", RequestID: "accept", Room: "lobby", ExpectedRoom: "lobby", TicVersion: &zero, Body: fmt.Sprintf("/ttt accept %d", id)})
	accepted := fb.read("action", "accept")
	if accepted.TicReceipt == nil || accepted.TicReceipt.Revision <= 0 {
		t.Fatal(accepted)
	}
	time.Sleep(750 * time.Millisecond)
	fa.send(guiRequest{Type: "command", RequestID: "move", Room: "lobby", ExpectedRoom: "lobby", TicVersion: &accepted.TicReceipt.Revision, Body: fmt.Sprintf("/ttt %d 1", id)})
	moved := fa.read("action", "move")
	if moved.TicReceipt == nil || !moved.TicReceipt.Committed || moved.TicReceipt.ObjectID != id || moved.TicReceipt.Cell != 1 {
		t.Fatal(moved)
	}
	if game := requireTicWireGame(t, fa, moved, id, "move-read"); len(game.Cells) != 9 || game.Cells[0] != "X" {
		t.Fatal("committed move missing from explicit board read", game)
	}
	time.Sleep(750 * time.Millisecond)
	fb.send(guiRequest{Type: "command", RequestID: "stale", Room: "lobby", ExpectedRoom: "lobby", TicVersion: &zero, Body: fmt.Sprintf("/ttt %d 2", id)})
	denied := fb.read("error", "stale")
	if !strings.Contains(denied.Error, "棋盘已变化") {
		t.Fatal(denied)
	}
}

func TestGUITicBadgeFailureRollsBackWinnerAndBothAwards(t *testing.T) {
	s, a, ar, b, br := ticFixture(t)
	made := ticNative(t, s, a, ar, ticMutation{operation: "challenge", room: "lobby", target: b.Name})
	zero := int64(0)
	accepted := ticNative(t, s, b, br, ticMutation{operation: "accept", id: made.ObjectID, expectedRevision: &zero})
	game := TicTacToe{X: a.Name, O: b.Name, Turn: a.Name, Accepted: true, Cells: [9]string{"X", "X", "", "O", "O"}}
	data, _ := json.Marshal(game)
	if _, e := s.db.Exec(`UPDATE objects SET payload=? WHERE id=?`, string(data), made.ObjectID); e != nil {
		t.Fatal(e)
	}
	if _, e := s.db.Exec(`CREATE TRIGGER synthetic_second_tic_badge BEFORE INSERT ON badges WHEN NEW.user='tic-bob' AND NEW.badge='棋手' BEGIN SELECT RAISE(ABORT,'synthetic second badge fault'); END;`); e != nil {
		t.Fatal(e)
	}
	result, e := s.ticForSessionContext(t.Context(), a.Name, ar.sessionID, ticMutation{operation: "move", id: made.ObjectID, cell: 3, expectedRevision: &accepted.Revision})
	if e == nil || result.Committed {
		t.Fatal(result, e)
	}
	obj, e := s.Object(a.Name, made.ObjectID)
	if e != nil || obj.Closed || obj.Payload != string(data) {
		t.Fatal(obj, e)
	}
	var n int
	if e = s.db.QueryRow(`SELECT count(*) FROM badges WHERE badge='棋手'`).Scan(&n); e != nil || n != 0 {
		t.Fatal(n, e)
	}
}
func TestGUITicActualFollowupFailuresAndRememberedRoomBinding(t *testing.T) {
	s, a, ar, b, _ := ticFixture(t)
	h := newHub()
	h.Attach(s)
	for _, room := range []string{"tic-room-a", "tic-room-b"} {
		if e := s.CreateRoomAccess(room, a.Name, false); e != nil {
			t.Fatal(e)
		}
	}
	ar.Body = "/ttt " + b.Name
	ar.communityRoom = "tic-room-b"
	ar.ExpectedRoom = "tic-room-a"
	if _, e := guiExecuteAction(s, h, a, guiScope{peer: b.Name}, 0, ar); e == nil {
		t.Fatal("same-DM roundtrip retargeted game")
	}
	ar.communityRoom = "tic-room-a"
	if _, e := s.db.Exec(`CREATE TRIGGER synthetic_tic_projection AFTER INSERT ON objects BEGIN UPDATE objects SET payload='{' WHERE id=NEW.id; END;CREATE TRIGGER synthetic_tic_notice BEFORE INSERT ON notifications BEGIN SELECT RAISE(ABORT,'synthetic notice fault'); END;CREATE TRIGGER synthetic_tic_announcement BEFORE INSERT ON messages BEGIN SELECT RAISE(ABORT,'synthetic announce fault'); END;`); e != nil {
		t.Fatal(e)
	}
	result, e := guiExecuteAction(s, h, a, guiScope{peer: b.Name}, 72, ar)
	if e != nil || result.ticReceipt == nil || !result.ticReceipt.Committed || result.ticReceipt.Room != "tic-room-a" || result.replyTo != 72 || !strings.Contains(result.status, "公告未发送") || !strings.Contains(result.status, "邀请通知未写入") || !strings.Contains(result.status, "详情暂不可读") {
		t.Fatal(result, e)
	}
	var n int
	if e = s.db.QueryRow(`SELECT count(*) FROM objects`).Scan(&n); e != nil || n != 1 {
		t.Fatal(n, e)
	}
}

// A committed operation may have an unavailable optional post-commit projection.
// Only the exact existing fallback is accepted, followed by a separate read;
// never retry the challenge/move or skip the original board assertions.
func requireTicWireGame(t *testing.T, f guiWireFixture, response guiResponse, id int64, readID string) *guiGameView {
	t.Helper()
	panel := response.Panel
	if panel == nil || panel.Community == nil || panel.Community.Detail == nil || panel.Community.Detail.Game == nil {
		requireTicCommittedFallback(t, response, id)
		time.Sleep(750 * time.Millisecond)
		f.send(guiRequest{Type: "command", RequestID: readID, Room: "lobby", Body: fmt.Sprintf("/object %d", id)})
		read := f.read("action", readID)
		if read.TicReceipt != nil {
			t.Fatal("read-only refresh returned a mutation receipt")
		}
		panel = read.Panel
	}
	if panel == nil || panel.Kind != "community" || panel.Scope != "lobby" || panel.Community == nil || panel.Community.Detail == nil || panel.Community.Detail.Game == nil {
		t.Fatalf("missing full game after explicit read: panel=%t", panel != nil)
	}
	detail := panel.Community.Detail
	if detail.Card.ID != id || detail.Card.Kind != "ttt" || detail.Card.Room != "lobby" || len(detail.Game.Cells) != 9 {
		t.Fatal("wrong game projection", detail)
	}
	return detail.Game
}
func requireTicCommittedFallback(t *testing.T, response guiResponse, id int64) {
	t.Helper()
	prefix := fmt.Sprintf("已提交 /ttt · #%d · #lobby", id)
	simple := prefix + "；详情暂不可读，请刷新而非重复提交"
	withNoticeFailure := prefix + "；公告未发送，请勿重复提交条目；详情暂不可读，请刷新而非重复提交"
	p := response.Panel
	r := response.TicReceipt
	if r == nil || !r.Committed || r.ObjectID != id || r.Room != "lobby" || r.OriginScope != "lobby" || (response.Status != simple && response.Status != withNoticeFailure) || p == nil || p.Kind != "community" || p.Scope != "lobby" || p.Title != "操作已提交" || p.Community != nil || len(p.Lines) != 1 || p.Lines[0] != response.Status || len(p.Actions) != 1 || p.Actions[0].Label != "重新读取详情" || p.Actions[0].Command != fmt.Sprintf("/object %d", id) || p.Actions[0].Fill {
		t.Fatalf("not the exact committed/read-unavailable fallback: receipt=%+v panel=%+v status=%q", r, p, response.Status)
	}
}
func TestGUITicCommittedFallbackKeepsOneObjectAndExplicitRead(t *testing.T) {
	s, a, request, b, _ := ticFixture(t)
	scope := guiScope{room: "lobby"}
	request.ExpectedRoom = "lobby"
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	first, err := s.readDB().Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := s.readDB().Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	// Invoke the real GUI action after the outer transport routing stage. Holding
	// readers before the entire wire request would also block its preflight ACL
	// reads, before the mutation. This handler still reauthorizes in the writer
	// transaction and uses the actual post-commit announcement/projection paths.
	result, err := guiTicAction(s, newHub(), a, scope, 0, request, b.Name)
	if err != nil {
		t.Fatal(err)
	}
	created := guiResponse{Type: "action", Room: scope.key(), Status: result.status, Panel: result.panel, TicReceipt: result.ticReceipt}
	if created.TicReceipt == nil || created.TicReceipt.Operation != "challenge" || created.TicReceipt.Revision != 0 {
		t.Fatal("bad creation receipt", created.TicReceipt)
	}
	id := created.TicReceipt.ObjectID
	requireTicCommittedFallback(t, created, id)
	var count int
	var room, owner, kind, payload string
	if err = s.db.QueryRow(`SELECT count(*) FROM objects`).Scan(&count); err != nil || count != 1 {
		t.Fatal("duplicate or absent committed object", count, err)
	}
	if err = s.db.QueryRow(`SELECT room,owner,kind,payload FROM objects WHERE id=?`, id).Scan(&room, &owner, &kind, &payload); err != nil {
		t.Fatal(err)
	}
	var stored TicTacToe
	if err = json.Unmarshal([]byte(payload), &stored); err != nil {
		t.Fatal(err)
	}
	revision, err := ticRevisionQ(s.db, id)
	if err != nil || revision != 0 || room != "lobby" || owner != a.Name || kind != "ttt" || stored.X != a.Name || stored.O != b.Name || stored.Turn != a.Name || stored.Accepted || stored.Cells != [9]string{} {
		t.Fatal("committed board identity/state changed", revision, stored, err)
	}
	if err = first.Close(); err != nil {
		t.Fatal(err)
	}
	if err = second.Close(); err != nil {
		t.Fatal(err)
	}
	// No mutation replay: a separate, fresh-session authorized detail read.
	panel, err := guiCommunityPanel(s, a, scope, request, "/object", fmt.Sprint(id))
	if err != nil || panel == nil || panel.Community == nil || panel.Community.Detail == nil || panel.Community.Detail.Game == nil {
		t.Fatal("explicit detail read unavailable", err)
	}
	detail := panel.Community.Detail
	if panel.Scope != "lobby" || detail.Card.ID != id || detail.Card.Room != "lobby" || detail.Card.Kind != "ttt" || len(detail.Game.Cells) != 9 {
		t.Fatal("wrong same-ID detail", detail)
	}
	game := detail.Game
	if game.CanAccept || game.Accepted || game.X != a.Name || game.O != b.Name || game.Turn != a.Name || game.Revision != 0 {
		t.Fatal("explicit read lost original player semantics", game)
	}
	if err = s.db.QueryRow(`SELECT count(*) FROM objects`).Scan(&count); err != nil || count != 1 {
		t.Fatal("read duplicated mutation", count, err)
	}
}
