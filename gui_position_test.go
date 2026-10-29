package main

import (
	"fmt"
	"testing"
)

func TestGUIPositionDraftAndHistoricalWindowRoundTrip(t *testing.T) {
	s := testStore(t)
	u := registerUser(t, s, "position-fixture")
	var messages []Message
	for i := 0; i < 260; i++ {
		messages = append(messages, mustSend(t, s, u.Name, "lobby", "", fmt.Sprintf("position fixture %d", i)))
	}
	h := newHub()
	f := newGUIWireFixture(t, s, h)
	f.send(guiRequest{Type: "login", RequestID: "auth", Username: u.Name, Password: "test-password-123"})
	f.read("auth", "auth")
	f.read("state", "")
	f.send(guiRequest{Type: "draft_load", RequestID: "baseline", Room: "lobby"})
	f.read("draft", "baseline")
	view := &guiViewPosition{Anchor: messages[10].ID, Offset: 3, LastRead: messages[100].ID}
	body := "第一行\n  second line\t "
	f.send(guiRequest{Type: "draft_save", RequestID: "save", Room: "lobby", Body: body, Version: 0, Position: view})
	if r := f.read("draft_saved", "save"); r.Draft == nil || r.Draft.Body != body || r.Draft.Position != *view {
		t.Fatal("editable text/position did not survive CAS")
	}
	f.send(guiRequest{Type: "draft_load", RequestID: "restore", Room: "lobby"})
	if r := f.read("draft", "restore"); r.Draft == nil || r.Draft.Body != body || r.Draft.Position != *view {
		t.Fatal("stored position did not restore")
	}
	f.send(guiRequest{Type: "view_window", RequestID: "window", Room: "lobby", Position: view})
	r := f.read("window", "window")
	if len(r.Messages) != 200 || r.Messages[0].ID != view.Anchor || r.AtLatest {
		t.Fatal("historical anchor did not select contiguous window")
	}
	f.send(guiRequest{Type: "view_newer", RequestID: "newer", Room: "lobby", Before: r.Messages[len(r.Messages)-1].ID})
	newer := f.read("window_append", "newer")
	if len(newer.Messages) != 50 || newer.Messages[0].ID != r.Messages[len(r.Messages)-1].ID+1 || !newer.AtLatest {
		t.Fatal("forward page was not contiguous")
	}
	f.send(guiRequest{Type: "view_window", RequestID: "latest", Room: "lobby", Position: &guiViewPosition{}})
	latest := f.read("window", "latest")
	if len(latest.Messages) != 100 || latest.Messages[0].ID != messages[160].ID || !latest.AtLatest {
		t.Fatal("latest-page restore failed")
	}
}

func TestGUIPositionRejectsCrossConversationReferences(t *testing.T) {
	s := testStore(t)
	u := registerUser(t, s, "position-owner")
	registerUser(t, s, "position-peer")
	otherRoom := mustSend(t, s, u.Name, "dev", "", "other room")
	dm := mustSend(t, s, "position-peer", "", u.Name, "private quote")
	for _, view := range []*guiViewPosition{
		{Anchor: otherRoom.ID, Offset: 2}, {LastRead: dm.ID}, {Anchor: -1}, {Offset: 1}, {Anchor: otherRoom.ID, Offset: 16001},
	} {
		if _, err := guiMergePosition(s, u.Name, guiScope{room: "lobby"}, Position{}, view); err == nil {
			t.Fatal("accepted invalid/cross-scope position", view)
		}
	}
}

func TestGUISuspendedDraftPreservesOnlyOwnerTextWithoutPublishing(t *testing.T) {
	s := testStore(t)
	owner := registerUser(t, s, "suspend-owner")
	reader := registerUser(t, s, "suspend-reader")
	foreign := registerUser(t, s, "suspend-foreign")
	if err := s.CreateRoomAccess("suspend-private", owner.Name, true); err != nil {
		t.Fatal(err)
	}
	if err := s.RoomMember(owner.Name, "suspend-private", reader.Name, "invite"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordSession(&Client{ID: "owned-fixture-session", Name: reader.Name, IP: "192.0.2.130", AuthEpoch: reader.AuthEpoch, Ctx: t.Context()}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordSession(&Client{ID: "foreign-fixture-session", Name: foreign.Name, IP: "192.0.2.131", AuthEpoch: foreign.AuthEpoch, Ctx: t.Context()}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SavePositionVersioned(reader.Name, "room:suspend-private", Position{Draft: "canonical before removal"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RoomMember(owner.Name, "suspend-private", reader.Name, "remove"); err != nil {
		t.Fatal(err)
	}
	request := guiRequest{Type: "draft_suspend", RequestID: "suspend", Room: "suspend-private", Body: "private\nunsent\t "}
	if _, err := guiSuspendDraft(s, reader, "foreign-fixture-session", request); err == nil {
		t.Fatal("saved a backup under another account's session")
	}
	if _, err := guiSuspendDraft(s, reader, "owned-fixture-session", request); err != nil {
		t.Fatal(err)
	}
	p, found, err := s.ConflictDraft(reader.Name, "room:suspend-private", "owned-fixture-session")
	if err != nil || !found || p.Draft != request.Body {
		t.Fatal("lost owner's suspended editable text", err)
	}
	if canonical := s.Position(reader.Name, "room:suspend-private"); canonical.Draft != "canonical before removal" {
		t.Fatal("suspension overwrote canonical draft")
	}
	var count int
	if err := s.db.QueryRow("SELECT count(*) FROM messages WHERE sender=?", reader.Name).Scan(&count); err != nil || count != 0 {
		t.Fatal("suspension published a message", err)
	}
	if _, err := s.History("suspend-private", reader.Name, "", 10); err == nil {
		t.Fatal("suspension restored room read access")
	}
}

func TestPositionDraftSanitizationKeepsEditableWhitespace(t *testing.T) {
	input := "  第一行\r\n\tsecond \x1b[31mline\x1b[0m\x00 "
	if got := cleanPositionDraft(input); got != "  第一行\n\tsecond line " {
		t.Fatal("draft normalization lost legitimate whitespace")
	}
}
