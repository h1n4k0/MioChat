package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func compositionWire(t *testing.T, s *Store, h *Hub, m model, scope guiScope, op, kind string, version *int64, title, body string) (guiResponse, error) {
	t.Helper()
	return guiCompositionRequestAction(t.Context(), s, h, m.user, scope, m.client.ID, guiRequest{Type: "composition", RequestID: "synthetic-editor", Room: scope.key(), Composition: &guiCompositionRequest{Operation: op, Room: "lobby", Kind: kind, ExpectedVersion: version, EditGeneration: 17, Title: title, Body: body}})
}
func TestGUICompositionNativeLifecycleCanonicalCASAndChatIsolation(t *testing.T) {
	for _, kind := range []string{"post", "art"} {
		t.Run(kind, func(t *testing.T) {
			s, h := featureFixture(t)
			m := testModel(t, s, h, "alice")
			scope := guiScope{room: "lobby"}
			if _, e := s.SavePositionVersioned("alice", "room:lobby", Position{Draft: "unrelated chat draft", ReplyTo: 42, Anchor: 42, Offset: 9}); e != nil {
				t.Fatal(e)
			}
			before, e := s.PositionWithError("alice", "room:lobby")
			if e != nil {
				t.Fatal(e)
			}
			out, e := compositionWire(t, s, h, m, scope, "load", kind, nil, "", "")
			if e != nil || out.Composition == nil || out.Composition.Snapshot.Version != 0 || out.Composition.Committed {
				t.Fatal(out, e)
			}
			zero := int64(0)
			if bad, e := compositionWire(t, s, h, m, scope, "reserve", kind, nil, "title", ""); e == nil || bad.Composition != nil {
				t.Fatal("omitted version accepted", bad, e)
			}
			out, e = compositionWire(t, s, h, m, scope, "reserve", kind, &zero, "title", "")
			if e != nil {
				t.Fatal(e)
			}
			v := out.Composition.Snapshot.Version
			raw := "\x1b[31mA\tB\r\nsecond\u200b\n"
			out, e = compositionWire(t, s, h, m, scope, "save", kind, &v, "title", raw)
			if e != nil || out.Composition.Snapshot.Body != "A    B\nsecond" || out.Composition.ExpectedVersion == nil || *out.Composition.ExpectedVersion != v || out.Composition.EditGeneration != 17 {
				t.Fatal(out, e)
			}
			saved := out.Composition.Snapshot
			if bad, e := compositionWire(t, s, h, m, scope, "publish", kind, &v, "title", "stale"); !errors.Is(e, ErrCompositionConflict) || bad.Composition != nil {
				t.Fatal("stale publication", bad, e)
			}
			out, e = compositionWire(t, s, h, m, scope, "remote", kind, nil, "", "")
			if e != nil || out.Composition.Snapshot != saved || out.Composition.Committed {
				t.Fatal(out, e)
			}
			if bad, e := compositionWire(t, s, h, m, scope, "reload", kind, &v, "", ""); !errors.Is(e, ErrCompositionConflict) || bad.Composition != nil {
				t.Fatal("stale reload", bad, e)
			}
			v = saved.Version
			out, e = compositionWire(t, s, h, m, scope, "reload", kind, &v, "", "")
			if e != nil || out.Composition.Snapshot != saved {
				t.Fatal(out, e)
			}
			out, e = compositionWire(t, s, h, m, scope, "publish", kind, &v, "title", raw)
			if e != nil || out.Composition == nil || out.Composition.ObjectID <= 0 || !out.Composition.Committed || out.Composition.Snapshot.Body != "" || out.Composition.Snapshot.Version <= v || out.Message != nil || out.Panel != nil || out.Draft != nil || out.ReplyTo != 0 {
				t.Fatal("publication receipt", out, e)
			}
			after, e := s.PositionWithError("alice", "room:lobby")
			if e != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("editor changed chat", before, after, e)
			}
			request := guiRequest{ctx: t.Context(), sessionID: m.client.ID, Body: "/post " + storageScalar(t, s, `SELECT max(id) FROM objects`)}
			view, e := guiExecuteAction(s, h, m.user, scope, 42, request)
			if e != nil || view.panel == nil || view.panel.Community == nil || view.panel.Community.Detail == nil || view.replyTo != 42 {
				t.Fatal("numeric post must read existing object", view, e)
			}
		})
	}
}

func TestGUICompositionScopeCredentialsAndMixedFramesRejectWithoutProjection(t *testing.T) {
	s, h := featureFixture(t)
	m := testModel(t, s, h, "alice")
	scope := guiScope{room: "lobby"}
	zero := int64(0)
	for _, kind := range []string{"post", "art"} {
		out, e := compositionWire(t, s, h, m, scope, "reserve", kind, &zero, "private title", "")
		if e != nil {
			t.Fatal(e)
		}
		v := out.Composition.Snapshot.Version
		for _, badScope := range []guiScope{{peer: "bob"}, {room: "other"}} {
			for _, op := range []string{"load", "remote", "reload", "reserve", "save", "publish"} {
				if bad, e := compositionWire(t, s, h, m, badScope, op, kind, &v, "", ""); e == nil || bad.Composition != nil {
					t.Fatal(op, bad, e)
				}
			}
		}
		base := guiRequest{Type: "composition", RequestID: "private", Room: "lobby", Composition: &guiCompositionRequest{Operation: "load", Room: "lobby", Kind: kind}}
		for _, field := range []string{"body", "username", "password", "before", "version", "expected_room", "word_mode", "game_id", "reply_to"} {
			data, _ := json.Marshal(base)
			var mix map[string]any
			if e = json.Unmarshal(data, &mix); e != nil {
				t.Fatal(e)
			}
			if field == "before" || field == "version" || field == "game_id" || field == "reply_to" {
				mix[field] = 1
			} else {
				mix[field] = "synthetic unrelated"
			}
			data, _ = json.Marshal(mix)
			var request guiRequest
			if e = json.Unmarshal(data, &request); e != nil {
				t.Fatal(e)
			}
			if bad, e := guiCompositionRequestAction(t.Context(), s, h, m.user, scope, m.client.ID, request); e == nil || bad.Composition != nil {
				t.Fatal(field, bad, e)
			}
		}
		for _, session := range []string{"", "wrong-session"} {
			if bad, e := guiCompositionRequestAction(t.Context(), s, h, m.user, scope, session, base); e == nil || bad.Composition != nil {
				t.Fatal(session, bad, e)
			}
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if bad, e := guiCompositionRequestAction(ctx, s, h, m.user, scope, m.client.ID, base); e == nil || bad.Composition != nil {
			t.Fatal("cancelled read projected", bad, e)
		}
	}
	if _, e := s.db.Exec(`UPDATE sessions SET revoked=1 WHERE id=?`, m.client.ID); e != nil {
		t.Fatal(e)
	}
	if bad, e := compositionWire(t, s, h, m, scope, "remote", "post", nil, "", ""); e == nil || bad.Composition != nil {
		t.Fatal("revoked read", bad, e)
	}
}

func TestGUICompositionSuspendOwnLocalAfterACLRevocationAndCommitWarning(t *testing.T) {
	s, h := featureFixture(t)
	m := testModel(t, s, h, "alice")
	scope := guiScope{room: "lobby"}
	zero := int64(0)
	out, e := compositionWire(t, s, h, m, scope, "reserve", "post", &zero, "title", "")
	if e != nil {
		t.Fatal(e)
	}
	v := out.Composition.Snapshot.Version
	if _, e = s.db.Exec(`INSERT INTO room_meta(name,private) VALUES('lobby',1) ON CONFLICT(name) DO UPDATE SET private=1`); e != nil {
		t.Fatal(e)
	}
	if _, e = s.db.Exec(`DELETE FROM members WHERE room='lobby' AND user='alice'`); e != nil {
		t.Fatal(e)
	}
	if bad, e := compositionWire(t, s, h, m, guiScope{peer: "bob"}, "suspend", "post", &v, "title", "own retained text"); e != nil || bad.Composition == nil || bad.Composition.Snapshot.Body != "own retained text" {
		t.Fatal(bad, e)
	} else {
		v = bad.Composition.Snapshot.Version
	}
	if bad, e := compositionWire(t, s, h, m, scope, "remote", "post", nil, "", ""); e == nil || bad.Composition != nil {
		t.Fatal("revoked ACL read projected", bad, e)
	}
	if _, e = s.db.Exec(`UPDATE room_meta SET private=0 WHERE name='lobby'`); e != nil {
		t.Fatal(e)
	}
	if _, e = s.db.Exec(`CREATE TRIGGER synthetic_composition_announcement BEFORE INSERT ON messages BEGIN SELECT RAISE(ABORT,'synthetic announcement failure'); END`); e != nil {
		t.Fatal(e)
	}
	out, e = compositionWire(t, s, h, m, scope, "publish", "post", &v, "title", "committed text")
	if e != nil || out.Composition == nil || !out.Composition.Committed || out.Composition.ObjectID <= 0 || !strings.Contains(out.Status, "公告未发送") || storageScalar(t, s, `SELECT count(*) FROM objects`) != "1" {
		t.Fatal("postcommit failure misreported publication", out, e)
	}
}

func TestGUICompositionWireAuthMenuAndExclusiveFrame(t *testing.T) {
	s, h := featureFixture(t)
	f := newGUIWireFixture(t, s, h)
	r := guiRequest{Type: "composition", RequestID: "unauth-compose", Room: "lobby", Composition: &guiCompositionRequest{Operation: "load", Room: "lobby", Kind: "post"}}
	f.send(r)
	if out := f.read("error", r.RequestID); out.Composition != nil {
		t.Fatal(out)
	}
	f.send(guiRequest{Type: "login", RequestID: "compose-auth", Username: "alice", Password: "test-password-123"})
	f.read("auth", "compose-auth")
	f.read("state", "")
	r.RequestID = "compose-load"
	f.send(r)
	out := f.read("composition", r.RequestID)
	if out.Composition == nil || out.Composition.Snapshot.Version != 0 || out.Profile != nil || out.Draft != nil || out.Panel != nil || out.Message != nil {
		t.Fatal(out)
	}
	time.Sleep(750 * time.Millisecond)
	v := int64(0)
	r.RequestID = "compose-reserve"
	r.Composition.Operation = "reserve"
	r.Composition.ExpectedVersion = &v
	r.Composition.Title = "wire title"
	f.send(r)
	out = f.read("composition", r.RequestID)
	v = out.Composition.Snapshot.Version
	time.Sleep(750 * time.Millisecond)
	r.RequestID = "compose-save"
	r.Composition.Operation = "save"
	r.Composition.Body = "wire body\n"
	f.send(r)
	out = f.read("composition", r.RequestID)
	if out.Composition == nil || out.Composition.Snapshot.Body != "wire body" || !out.Composition.Committed {
		t.Fatal(out)
	}
	time.Sleep(750 * time.Millisecond)
	r.RequestID = "compose-old-version"
	r.Composition.Body = "must not overwrite"
	f.send(r)
	out = f.read("error", r.RequestID)
	if out.Composition != nil {
		t.Fatal(out)
	}
	time.Sleep(750 * time.Millisecond)
	f.send(guiRequest{Type: "command", RequestID: "compose-menu", Room: "lobby", Body: "/menu"})
	out = f.read("action", "compose-menu")
	for _, name := range []string{"/post", "/draw"} {
		found := false
		for _, a := range out.Panel.Actions {
			if strings.TrimSpace(a.Command) == name {
				found = true
			}
		}
		if !found {
			t.Fatal("missing native editor entry", name)
		}
	}
	if _, e := s.db.Exec(`UPDATE user_meta SET must_reset=1 WHERE name='alice'`); e != nil {
		t.Fatal(e)
	}
	time.Sleep(750 * time.Millisecond)
	r.RequestID = "compose-reset"
	r.Composition.Operation = "remote"
	r.Composition.ExpectedVersion = nil
	r.Composition.Title = ""
	r.Composition.Body = ""
	f.send(r)
	out = f.read("error", r.RequestID)
	if out.Composition != nil {
		t.Fatal("credential gate projected editor", out)
	}
}
