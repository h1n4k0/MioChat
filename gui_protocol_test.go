package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"
)

func TestGUIProtocolRegisterListAndSend(t *testing.T) {
	store, err := openStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.db.Close()
	hub := newHub()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clientReader, serverInput := io.Pipe()
	serverOutput, clientWriter := io.Pipe()
	defer clientReader.Close()
	defer serverInput.Close()
	defer serverOutput.Close()
	defer clientWriter.Close()
	done := make(chan error, 1)
	go func() {
		done <- runGUIProtocol(ctx, clientReader, clientWriter, "192.0.2.20", "test", func() {}, store, hub)
	}()
	requests := json.NewEncoder(serverInput)
	responses := bufio.NewScanner(serverOutput)
	responseCh := make(chan guiResponse, 16)
	go func() {
		for responses.Scan() {
			var response guiResponse
			if json.Unmarshal(responses.Bytes(), &response) == nil {
				responseCh <- response
			}
		}
		close(responseCh)
	}()
	read := func() guiResponse {
		t.Helper()
		select {
		case response, ok := <-responseCh:
			if !ok {
				t.Fatalf("response ended: %v", responses.Err())
			}
			return response
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for protocol response")
		}
		return guiResponse{}
	}
	readMatch := func(predicate func(guiResponse) bool) guiResponse {
		t.Helper()
		for {
			response := read()
			if predicate(response) {
				return response
			}
		}
	}
	if response := read(); response.Type != "hello" || response.Protocol != guiProtocolVersion {
		t.Fatalf("unexpected hello: %#v", response)
	}
	if err := requests.Encode(guiRequest{Type: "register", RequestID: "a1", Username: "alice", Password: "correct-horse", Confirm: "correct-horse"}); err != nil {
		t.Fatal(err)
	}
	if response := readMatch(func(response guiResponse) bool { return response.RequestID == "a1" }); response.Type != "auth" || response.User != "alice" {
		t.Fatalf("unexpected auth: %#v", response)
	}
	if response := readMatch(func(response guiResponse) bool { return response.Type == "state" }); response.Room != "lobby" || len(response.Rooms) < 4 {
		t.Fatalf("unexpected state: %#v", response)
	}
	if err := requests.Encode(guiRequest{Type: "send_message", RequestID: "s1", Room: "dev", Body: "stale"}); err != nil {
		t.Fatal(err)
	}
	if response := readMatch(func(response guiResponse) bool { return response.RequestID == "s1" }); response.Type != "error" {
		t.Fatalf("stale-room send was not rejected: %#v", response)
	}
	if err := requests.Encode(guiRequest{Type: "send_message", RequestID: "s2", Room: "lobby", Body: "hello native GUI"}); err != nil {
		t.Fatal(err)
	}
	if response := readMatch(func(response guiResponse) bool { return response.RequestID == "s2" }); response.Type != "sent" || response.Message == nil || response.Message.Body != "hello native GUI" {
		t.Fatalf("unexpected send acknowledgement: %#v", response)
	}
	if response := readMatch(func(response guiResponse) bool { return response.Type == "message" }); response.Message == nil || response.Message.Body != "hello native GUI" {
		t.Fatalf("unexpected live message: %#v", response)
	}
	history, err := store.History("lobby", "alice", "", 10)
	if err != nil || len(history) != 1 || history[0].Body != "hello native GUI" {
		t.Fatalf("message was not stored: %#v, %v", history, err)
	}
	if err := requests.Encode(guiRequest{Type: "disconnect", RequestID: "d1"}); err != nil {
		t.Fatal(err)
	}
	if response := readMatch(func(response guiResponse) bool { return response.RequestID == "d1" }); response.Type != "status" || response.Status != "bye" {
		t.Fatalf("unexpected disconnect: %#v", response)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("protocol did not stop after disconnect")
	}
}
