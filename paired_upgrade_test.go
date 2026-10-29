package main

import (
	"encoding/json"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"
)

// Both clients use the same production SSH server and synthetic database.
// An anonymous SSH identity is not an application account. No public service
// or user's credentials are contacted by this compatibility regression.
func TestPairedServerLegacyPTYAndGUIApplicationLogin(t *testing.T) {
	store := testStore(t)
	registerUser(t, store, "legacy")
	registerUser(t, store, "native")
	hub := newHub()
	signer := transportSigner(t)
	server := makeServer(store, hub, signer)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go server.Serve(listener)
	t.Cleanup(func() {
		hub.stopUI()
		_ = server.Close()
		hub.uiTasks.Wait()
	})

	legacy := connectTerminal(t, listener.Addr().String(), signer)
	// Existing-account login, not registration or SSH password authentication.
	legacy.send(t, "legacy\r")
	time.Sleep(60 * time.Millisecond)
	legacy.send(t, "test-password-123\r")
	legacy.wait(t, "欢迎，legacy")
	legacy.send(t, "\x1b")

	client, err := gossh.Dial("tcp", listener.Addr().String(), &gossh.ClientConfig{
		User: "chat", Auth: []gossh.AuthMethod{transportAnonymousAuth()},
		HostKeyCallback: gossh.FixedHostKey(signer.PublicKey()), Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	in, err := session.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := session.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = session.RequestSubsystem(guiSubsystemName); err != nil {
		t.Fatal(err)
	}

	type decoded struct {
		response guiResponse
		err      error
	}
	responses := make(chan decoded, 32)
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go func() {
		defer close(responses)
		decoder := json.NewDecoder(out)
		for {
			var response guiResponse
			err := decoder.Decode(&response)
			select {
			case responses <- decoded{response, err}:
			case <-stop:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	readUntil := func(match func(guiResponse) bool) guiResponse {
		t.Helper()
		// A single deadline includes unrelated live messages; do not restart a
		// per-frame timeout and allow a noisy session to hang this test.
		timer := time.NewTimer(8 * time.Second)
		defer timer.Stop()
		for {
			select {
			case item, ok := <-responses:
				if !ok || item.err != nil {
					t.Fatalf("GUI stream ended: %v", item.err)
				}
				if match(item.response) {
					return item.response
				}
			case <-timer.C:
				t.Fatal("bounded GUI response deadline exceeded")
			}
		}
	}
	write := func(request guiRequest) {
		t.Helper()
		if err := json.NewEncoder(in).Encode(request); err != nil {
			t.Fatal(err)
		}
	}
	hello := readUntil(func(r guiResponse) bool { return r.Type == "hello" })
	if hello.Protocol != guiProtocolVersion {
		t.Fatalf("incompatible hello: %d", hello.Protocol)
	}
	write(guiRequest{Type: "send_message", RequestID: "before-login", Room: "lobby", Body: "must-not-publish"})
	if r := readUntil(func(r guiResponse) bool { return r.RequestID == "before-login" }); r.Type != "error" {
		t.Fatalf("anonymous transport acquired application permissions: %s", r.Type)
	}
	write(guiRequest{Type: "login", RequestID: "explicit-login", Username: "native", Password: "test-password-123"})
	if r := readUntil(func(r guiResponse) bool { return r.RequestID == "explicit-login" }); r.Type != "auth" || r.User != "native" {
		t.Fatalf("application login failed: type=%s user=%s", r.Type, r.User)
	}
	if r := readUntil(func(r guiResponse) bool { return r.Type == "state" }); r.Room != "lobby" {
		t.Fatal("wrong initial room")
	}
	write(guiRequest{Type: "send_message", RequestID: "native-to-pty", Room: "lobby", Body: "native-to-legacy-synthetic"})
	if r := readUntil(func(r guiResponse) bool { return r.RequestID == "native-to-pty" }); r.Type != "sent" || r.Message == nil || r.Message.Body != "native-to-legacy-synthetic" {
		t.Fatal("GUI message was not acknowledged")
	}
	legacy.wait(t, "native-to-legacy-synthetic")
	legacy.send(t, "legacy-to-native-synthetic\r")
	readUntil(func(r guiResponse) bool {
		return r.Type == "message" && r.Message != nil && r.Message.Body == "legacy-to-native-synthetic"
	})
	// Escape followed immediately by text is a terminal Alt chord, not two
	// independent user actions. Observe actual VT cells before typing again;
	// cumulative output cannot prove that a modal has closed.
	grid := newCIVTGrid(100, 30)
	consumed := 0
	waitTerminalView := func(marker, forbidden string) {
		t.Helper()
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			raw := legacy.output()
			if len(raw) > consumed {
				_, _ = grid.Write([]byte(raw[consumed:]))
				consumed = len(raw)
			}
			view, _ := grid.snapshot()
			if strings.Contains(view, marker) && !strings.Contains(view, forbidden) {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		view, _ := grid.snapshot()
		t.Fatalf("bounded terminal view deadline: want %q without %q; view:\n%s", marker, forbidden, view)
	}
	legacy.send(t, "/help\r")
	waitTerminalView("COMMAND GUIDE", "legacy-to-native-synthetic")
	legacy.send(t, "\x1b")
	waitTerminalView("legacy-to-native-synthetic", "COMMAND GUIDE")
	legacy.send(t, "legacy-after-help-synthetic\r")
	readUntil(func(r guiResponse) bool {
		return r.Type == "message" && r.Message != nil && r.Message.Body == "legacy-after-help-synthetic"
	})
	if strings.Contains(legacy.output(), "test-password-123") {
		t.Fatal("application password leaked into terminal output")
	}
	history, err := store.History("lobby", "native", "", 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range history {
		if message.Body == "must-not-publish" {
			t.Fatal("pre-login message was stored")
		}
	}
	write(guiRequest{Type: "disconnect", RequestID: "done"})
	if r := readUntil(func(r guiResponse) bool { return r.RequestID == "done" }); r.Type != "status" || r.Status != "bye" {
		t.Fatal("GUI disconnect was not acknowledged")
	}
	if err := in.Close(); err != nil && err != io.EOF {
		t.Fatal(err)
	}
}
