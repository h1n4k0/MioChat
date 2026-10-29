package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func nativeWord(t *testing.T, s *Store, u User, r guiRequest, m guessMutation) guiWordReceipt {
	t.Helper()
	out, e := s.wordForSessionContext(t.Context(), u.Name, r.sessionID, m)
	if e != nil {
		t.Fatal(e)
	}
	return out
}
func TestGUIWordRulesMaskedProjectionAndNoSecretReceipt(t *testing.T) {
	s, a, ar, b, br := numberFixture(t)
	made := nativeWord(t, s, a, ar, guessMutation{start: true, room: "lobby", value: "猫咪"})
	raw, _ := json.Marshal(made)
	if strings.Contains(string(raw), "猫咪") || made.Requested != "" || made.Value != "" || made.Feedback != "" {
		t.Fatal("start leaked answer")
	}
	if _, e := s.wordForSessionContext(t.Context(), a.Name, ar.sessionID, guessMutation{expectedID: made.ObjectID, expectedRoom: "lobby", value: "猫"}); e == nil {
		t.Fatal("owner guessed")
	}
	wrong := nativeWord(t, s, b, br, guessMutation{expectedID: made.ObjectID, expectedRoom: "lobby", value: "猫"})
	if wrong.Closed || wrong.Feedback != "猫 _ · 失误 0/6" {
		t.Fatal(wrong)
	}
	var before int
	s.db.QueryRow(`SELECT count(*) FROM object_events`).Scan(&before)
	if _, e := s.wordForSessionContext(t.Context(), b.Name, br.sessionID, guessMutation{expectedID: made.ObjectID, expectedRoom: "lobby", value: "猫"}); e == nil {
		t.Fatal("duplicate guessed")
	}
	var after int
	s.db.QueryRow(`SELECT count(*) FROM object_events`).Scan(&after)
	if after != before {
		t.Fatal("duplicate wrote")
	}
	panel, e := guiCommunityPanel(s, b, guiScope{room: "lobby"}, br, "/object", fmt.Sprint(made.ObjectID))
	if e != nil {
		t.Fatal(e)
	}
	raw, _ = json.Marshal(panel)
	if strings.Contains(string(raw), "猫咪") || strings.Contains(string(raw), `"secret"`) || panel.Community.Detail.Game.Masked != "猫 _" || !panel.Community.Detail.Game.CanGuess {
		t.Fatal("active projection leak", string(raw))
	}
	won := nativeWord(t, s, b, br, guessMutation{expectedID: made.ObjectID, expectedRoom: "lobby", value: "咪"})
	if !won.Closed || !strings.Contains(won.Feedback, "答案：猫咪") {
		t.Fatal(won)
	}
	panel, e = guiCommunityPanel(s, a, guiScope{room: "lobby"}, ar, "/object", fmt.Sprint(made.ObjectID))
	if e != nil || panel.Community.Detail.Game.Secret != "猫咪" {
		t.Fatal(e)
	}
	made = nativeWord(t, s, a, ar, guessMutation{start: true, room: "lobby", value: "猫咪"})
	for i := range 6 {
		r := nativeWord(t, s, b, br, guessMutation{expectedID: made.ObjectID, expectedRoom: "lobby", value: fmt.Sprint(i)})
		if r.Closed != (i == 5) {
			t.Fatal("wrong limit", r)
		}
	}
}
func TestGUIWordDistinctCapAndAtomicWinnerFailure(t *testing.T) {
	s, a, ar, b, br := numberFixture(t)
	made := nativeWord(t, s, a, ar, guessMutation{start: true, room: "lobby", value: "abcdefghijklmnopqrstuvwx"})
	guesses := []string{}
	text := "abcdefghijklmnopqrstuvw"
	for i := 0; i < len(text); i++ {
		for j := i + 1; j <= len(text); j++ {
			guesses = append(guesses, text[i:j])
		}
	}
	for _, v := range guesses[:128] {
		r := nativeWord(t, s, b, br, guessMutation{expectedID: made.ObjectID, expectedRoom: "lobby", value: v})
		if r.Closed {
			t.Fatal("unexpected covered x")
		}
	}
	if _, e := s.wordForSessionContext(t.Context(), b.Name, br.sessionID, guessMutation{expectedID: made.ObjectID, expectedRoom: "lobby", value: guesses[128]}); e == nil {
		t.Fatal("cap bypass")
	}
	if e := s.CloseObject(a.Name, made.ObjectID); e != nil {
		t.Fatal(e)
	}
	made = nativeWord(t, s, a, ar, guessMutation{start: true, room: "lobby", value: "synthetic"})
	if _, e := s.db.Exec(`CREATE TRIGGER synthetic_word_badge BEFORE INSERT ON badges WHEN NEW.badge='解谜者' BEGIN SELECT RAISE(ABORT,'synthetic fault');END`); e != nil {
		t.Fatal(e)
	}
	r, e := s.wordForSessionContext(t.Context(), b.Name, br.sessionID, guessMutation{expectedID: made.ObjectID, expectedRoom: "lobby", value: "synthetic"})
	if e == nil || r.Committed {
		t.Fatal("fault committed")
	}
	o, e := s.Object(a.Name, made.ObjectID)
	if e != nil || o.Closed {
		t.Fatal(e)
	}
	var n int
	if e = s.db.QueryRow(`SELECT count(*) FROM object_events WHERE object_id=?`, made.ObjectID).Scan(&n); e != nil || n != 0 {
		t.Fatal(n, e)
	}
}
func TestGUIWordDraftSafetyAndExplicitClearCAS(t *testing.T) {
	s, a, ar, _, _ := numberFixture(t)
	scope := guiScope{room: "lobby"}
	for _, body := range []string{"/word \nstart hidden", "/word \r\nstart hidden", "safe\n/word \nSTART hidden", "/word start hidden", " /WORD\tSTART hidden", "text\n /word start hidden", "/wo\u200brd st\u2060art hidden", "\x1b[31m/word start hidden"} {
		for _, kind := range []string{"draft_save", "draft_keep", "draft_suspend"} {
			r := ar
			r.Type = kind
			r.Body = body
			r.Room = "lobby"
			var e error
			if kind == "draft_suspend" {
				_, e = guiSuspendDraft(s, a, ar.sessionID, r)
			} else {
				_, e = guiDraftRequest(s, a, scope, ar.sessionID, r)
			}
			if e == nil || strings.Contains(e.Error(), "hidden") {
				t.Fatal(kind, e)
			}
		}
	}
	var n int
	s.db.QueryRow(`SELECT count(*) FROM draft_conflicts`).Scan(&n)
	if n != 0 {
		t.Fatal("secret backed up")
	}
	p, e := s.PositionWithError(a.Name, scopeFor("lobby", ""))
	if e != nil || p.Draft != "" {
		t.Fatal(p, e)
	}
	// Simulate an already-saved legacy draft, never pretend it was not stored.
	p.Draft = "/word start old-secret"
	version, e := s.SavePositionVersioned(a.Name, scopeFor("lobby", ""), p)
	if e != nil {
		t.Fatal(e)
	}
	r := ar
	r.Type = "draft_load"
	r.Room = "lobby"
	loaded, e := guiDraftRequest(s, a, scope, ar.sessionID, r)
	if e != nil || loaded.Draft.Body != p.Draft || loaded.Draft.Version != version {
		t.Fatal("legacy not honestly loaded", e)
	}
	newer := p
	newer.Version = version
	newer.Draft = "other device ordinary"
	if _, e = s.SavePositionVersioned(a.Name, scopeFor("lobby", ""), newer); e != nil {
		t.Fatal(e)
	}
	r.Type = "draft_save"
	r.Body = ""
	r.Version = version
	conflict, e := guiDraftRequest(s, a, scope, ar.sessionID, r)
	if e != nil || conflict.Type != "draft_conflict" {
		t.Fatal(conflict, e)
	}
	current, e := s.PositionWithError(a.Name, scopeFor("lobby", ""))
	if e != nil || current.Draft != "other device ordinary" {
		t.Fatal("stale clear lost newer draft", e)
	}
}
func TestGUIWordQueuedRevocationOldObjectAndCommitWarnings(t *testing.T) {
	s, a, ar, b, br := numberFixture(t)
	made := nativeWord(t, s, a, ar, guessMutation{start: true, room: "lobby", value: "synthetic"})
	blocker, e := s.db.Begin()
	if e != nil {
		t.Fatal(e)
	}
	defer blocker.Rollback()
	waits := s.db.Stats().WaitCount
	done := make(chan error, 1)
	go func() {
		_, err := s.wordForSessionContext(t.Context(), b.Name, br.sessionID, guessMutation{expectedID: made.ObjectID, expectedRoom: "lobby", value: "s"})
		done <- err
	}()
	deadline := time.Now().Add(3 * time.Second)
	for s.db.Stats().WaitCount <= waits {
		if time.Now().After(deadline) {
			t.Fatal("not queued")
		}
		time.Sleep(time.Millisecond)
	}
	if _, e = blocker.Exec(`UPDATE sessions SET revoked=1 WHERE id=?`, br.sessionID); e != nil {
		t.Fatal(e)
	}
	if e = blocker.Commit(); e != nil {
		t.Fatal(e)
	}
	if e = <-done; e == nil {
		t.Fatal("revoked guessed")
	}
	if e = s.CloseObject(a.Name, made.ObjectID); e != nil {
		t.Fatal(e)
	}
	fresh := nativeWord(t, s, a, ar, guessMutation{start: true, room: "lobby", value: "next-word"})
	_, br = communityReader(t, s, b.Name, "word-fresh-session")
	if _, e = s.wordForSessionContext(t.Context(), b.Name, br.sessionID, guessMutation{expectedID: made.ObjectID, expectedRoom: "lobby", value: "n"}); e == nil {
		t.Fatal("old game guessed new")
	}
	if e = s.CloseObject(a.Name, fresh.ObjectID); e != nil {
		t.Fatal(e)
	}
	h := newHub()
	h.Attach(s)
	if _, e = s.db.Exec(`CREATE TRIGGER synthetic_word_announce BEFORE INSERT ON messages BEGIN SELECT RAISE(ABORT,'synthetic announcement fault');END`); e != nil {
		t.Fatal(e)
	}
	ar.Body = "/word start hidden-answer"
	ar.ExpectedRoom = "lobby"
	result, e := guiExecuteAction(s, h, a, guiScope{room: "lobby"}, 0, ar)
	if e != nil || result.wordReceipt == nil || !result.wordReceipt.Committed || !strings.Contains(result.status, "公告未发送") {
		t.Fatal(result, e)
	}
	raw, _ := json.Marshal(result.wordReceipt)
	if strings.Contains(string(raw), "hidden-answer") {
		t.Fatal("receipt leak")
	}
}

func TestGUIWordRealWirePublicStartGuessAndDraftPreserved(t *testing.T) {
	s, a, _, b, _ := numberFixture(t)
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
	fa.send(guiRequest{Type: "draft_load", RequestID: "before", Room: "lobby"})
	before := fa.read("draft", "before").Draft
	time.Sleep(750 * time.Millisecond)
	fa.send(guiRequest{Type: "command", RequestID: "start", Room: "lobby", ExpectedRoom: "lobby", WordMode: "start", Body: "/word start start"})
	made := fa.read("action", "start")
	if made.WordReceipt == nil || !made.WordReceipt.Committed || made.WordReceipt.Operation != "start" || made.WordReceipt.Requested != "" || made.Panel.Community.Detail.Game.Secret != "" {
		t.Fatal("secret create receipt/projection")
	}
	id := made.WordReceipt.ObjectID
	fb.send(guiRequest{Type: "command", RequestID: "guess", Room: "lobby", ExpectedRoom: "lobby", GameID: id, WordMode: "guess", Body: "/word start"})
	won := fb.read("action", "guess")
	if won.WordReceipt == nil || won.WordReceipt.Operation != "guess" || won.WordReceipt.Requested != "start" || !won.WordReceipt.Closed || won.Panel.Community.Detail.Game.Secret != "start" {
		t.Fatal("public start guess was reinterpreted", won)
	}
	time.Sleep(750 * time.Millisecond)
	fa.send(guiRequest{Type: "draft_load", RequestID: "after", Room: "lobby"})
	after := fa.read("draft", "after").Draft
	if before == nil || after == nil || *before != *after {
		t.Fatal("word modified ordinary draft")
	}
	time.Sleep(750 * time.Millisecond)
	fb.send(guiRequest{Type: "command", RequestID: "missing-intent", Room: "lobby", WordMode: "guess", Body: "/word start"})
	if r := fb.read("error", "missing-intent"); !strings.Contains(r.Error, "意图不完整") {
		t.Fatal(r)
	}
}

func TestGUIWordPublicModeNeverCreatesForReservedGuess(t *testing.T) {
	s, a, ar, b, br := numberFixture(t)
	h := newHub()
	h.Attach(s)
	for _, value := range []string{"start", "START", "start secret"} {
		made := nativeWord(t, s, a, ar, guessMutation{start: true, room: "lobby", value: value})
		br.Body = "/word " + value
		br.ExpectedRoom = "lobby"
		br.GameID = made.ObjectID
		br.WordMode = "guess"
		result, e := guiExecuteAction(s, h, b, guiScope{room: "lobby"}, 0, br)
		if e != nil || result.wordReceipt == nil || result.wordReceipt.Operation != "guess" || result.wordReceipt.ObjectID != made.ObjectID || !result.wordReceipt.Closed {
			t.Fatal(value, e)
		}
	}
	for _, bad := range []guiRequest{{Body: "/word start secret", WordMode: "guess", ExpectedRoom: "lobby"}, {Body: "/word start secret", WordMode: "guess", GameID: 1}, {Body: "/word not-start", WordMode: "start", ExpectedRoom: "lobby"}} {
		bad.ctx = t.Context()
		bad.sessionID = ar.sessionID
		if _, e := guiExecuteAction(s, h, a, guiScope{room: "lobby"}, 0, bad); e == nil {
			t.Fatal("incomplete native mode accepted")
		}
	}
	var n int
	if e := s.db.QueryRow(`SELECT count(*) FROM objects WHERE kind='word'`).Scan(&n); e != nil || n != 3 {
		t.Fatal("guess invoked creation", n, e)
	}
}

func TestGUIWordOldConflictLabelsAreStaticAndBackupsRemain(t *testing.T) {
	s, a, ar, _, _ := numberFixture(t)
	for i, body := range []string{"/word start synthetic-hidden-answer", "/word \nstart synthetic-hidden-answer"} {
		_, r := communityReader(t, s, a.Name, fmt.Sprintf("old-word-%d", i))
		if e := s.SaveConflictDraft(a.Name, scopeFor("lobby", ""), r.sessionID, Position{Draft: body}); e != nil {
			t.Fatal(e)
		}
	}
	h := newHub()
	h.Attach(s)
	ar.Body = "/draft-conflicts"
	result, e := guiExecuteAction(s, h, a, guiScope{room: "lobby"}, 0, ar)
	if e != nil || result.panel == nil || len(result.panel.Actions) != 2 {
		t.Fatal(result, e)
	}
	raw, _ := json.Marshal(result.panel)
	if strings.Contains(string(raw), "synthetic-hidden-answer") || !strings.Contains(string(raw), "正文不预览") {
		t.Fatal("secret backup preview")
	}
	drafts, e := s.ConflictDrafts(a.Name)
	if e != nil || len(drafts) != 2 {
		t.Fatal("backups removed", e)
	}
	for _, d := range drafts {
		if !strings.Contains(d.Position.Draft, "synthetic-hidden-answer") {
			t.Fatal("backup altered")
		}
	}
}
