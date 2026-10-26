package main

import (
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	ssh "charm.land/ssh"
	gossh "golang.org/x/crypto/ssh"
)

func TestSSHRejectsCommandSubsystemAndForwardingRequests(t *testing.T) {
	hub := newHub()
	host := transportSigner(t)
	server := makeServer(testStore(t), hub, host)
	var handlerCalls atomic.Int32
	server.Handler = func(session ssh.Session) {
		handlerCalls.Add(1)
		_ = session.Exit(0)
	}
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
	address := listener.Addr().String()
	dial := func() *gossh.Client {
		t.Helper()
		client, err := gossh.Dial("tcp", address, &gossh.ClientConfig{User: "chat", Auth: []gossh.AuthMethod{transportAnonymousAuth()}, HostKeyCallback: gossh.FixedHostKey(host.PublicKey()), Timeout: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = client.Close() })
		return client
	}
	for _, kind := range []string{"exec", "subsystem"} {
		t.Run(kind, func(t *testing.T) {
			client := dial()
			channel, requests, err := client.OpenChannel("session", nil)
			if err != nil {
				t.Fatal(err)
			}
			defer channel.Close()
			go gossh.DiscardRequests(requests)
			payload := gossh.Marshal(struct{ Value string }{"miochat-boundary-test"})
			accepted, err := channel.SendRequest(kind, true, payload)
			if err != nil || accepted {
				t.Fatal("non-interactive execution request was not explicitly rejected", accepted, err)
			}
		})
	}
	for _, kind := range []string{"direct-tcpip", "direct-streamlocal@openssh.com", "forwarded-tcpip"} {
		t.Run(kind, func(t *testing.T) {
			client := dial()
			channel, _, err := client.OpenChannel(kind, nil)
			if err == nil {
				channel.Close()
				t.Fatal("forwarding channel was accepted")
			}
			var denied *gossh.OpenChannelError
			if !errors.As(err, &denied) || denied.Reason != gossh.UnknownChannelType {
				t.Fatal("channel failed without proving policy rejection", err)
			}
		})
	}
	t.Run("remote-forward", func(t *testing.T) {
		client := dial()
		accepted, _, err := client.SendRequest("tcpip-forward", true, gossh.Marshal(struct {
			Host string
			Port uint32
		}{"127.0.0.1", 0}))
		if err != nil || accepted {
			t.Fatal("remote forwarding request was not explicitly rejected", accepted, err)
		}
	})
	if handlerCalls.Load() != 0 {
		t.Fatal("rejected request reached the interactive application handler")
	}
}
