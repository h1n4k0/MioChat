package main

import (
	"testing"
	"time"
)

func TestGUIExitAliasesAcknowledgeAndCloseOnlyCurrentSession(t *testing.T) {
	for _, command := range []string{"/logout", "/quit", "/exit"} {
		t.Run(command, func(t *testing.T) {
			s := testStore(t)
			owner := registerUser(t, s, "exit-owner")
			other := registerUser(t, s, "exit-other")
			for _, client := range []*Client{{ID: "other-own-session", Name: owner.Name, AuthEpoch: owner.AuthEpoch, Ctx: t.Context()}, {ID: "foreign-session", Name: other.Name, AuthEpoch: other.AuthEpoch, Ctx: t.Context()}} {
				if err := s.RecordSession(client); err != nil {
					t.Fatal(err)
				}
			}
			f := newGUIWireFixture(t, s, newHub())
			f.send(guiRequest{Type: "login", RequestID: "login", Username: owner.Name, Password: "test-password-123"})
			f.read("auth", "login")
			f.read("state", "")
			var current string
			if err := s.db.QueryRow("SELECT id FROM sessions WHERE user=? AND client='native-fixture/gpui'", owner.Name).Scan(&current); err != nil {
				t.Fatal(err)
			}
			f.send(guiRequest{Type: "command", RequestID: "exit-correlated", Room: "lobby", Body: command, Username: other.Name})
			if r := f.read("action", "exit-correlated"); r.Status != "bye" || r.Panel != nil || r.Message != nil {
				t.Fatal("exit must acknowledge without sending chat or opening a panel", r)
			}
			deadline := time.Now().Add(2 * time.Second)
			for s.SessionValid(current) && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if s.SessionValid(current) || !s.SessionValid("other-own-session") || !s.SessionValid("foreign-session") {
				t.Fatal("exit changed another device/account or retained the current session")
			}
			history, err := s.History("lobby", owner.Name, "", 10)
			if err != nil || len(history) != 0 {
				t.Fatal("exit spelling was published as a message", err)
			}
		})
	}
}
