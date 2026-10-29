package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mio-qwq/MioChat/internal/guitransport"
	gossh "golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func TestGUIDeviceTransportEncryptedKeyAndStrictHost(t *testing.T) {
	tests := []struct {
		name     string
		generate func() (any, error)
	}{
		{"ed25519", func() (any, error) { _, key, err := ed25519.GenerateKey(rand.Reader); return key, err }},
		{"ecdsa-p256", func() (any, error) { return ecdsa.GenerateKey(elliptic.P256(), rand.Reader) }},
		{"rsa3072-sha2", func() (any, error) { return rsa.GenerateKey(rand.Reader, 3072) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			private, err := test.generate()
			if err != nil {
				t.Fatal(err)
			}
			checkEncryptedKeyTransport(t, private)
		})
	}
}

func checkEncryptedKeyTransport(t *testing.T, private any) {
	t.Helper()
	s := testStore(t)
	owner := registerUser(t, s, "encrypted-synthetic")
	device, err := gossh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.BindSSHKey(owner.Name, "temporary", string(gossh.MarshalAuthorizedKey(device.PublicKey()))); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	identity := filepath.Join(dir, "synthetic-device.key")
	known := filepath.Join(dir, "known_hosts")
	write := func(path string, data []byte) {
		t.Helper()
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	block, err := gossh.MarshalPrivateKeyWithPassphrase(private, "synthetic", []byte("synthetic-passphrase"))
	if err != nil {
		t.Fatal(err)
	}
	write(identity, pem.EncodeToMemory(block))
	h := newHub()
	host := transportSigner(t)
	server := makeServer(s, h, host)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go server.Serve(listener)
	t.Cleanup(func() { h.stopUI(); server.Close(); h.uiTasks.Wait() })
	write(known, []byte(knownhosts.Line([]string{listener.Addr().String()}, host.PublicKey())+"\n"))
	_, portText, _ := net.SplitHostPort(listener.Addr().String())
	port, _ := strconv.Atoi(portText)
	config := guitransport.Config{Type: "device-key-transport-v1", Host: "127.0.0.1", Port: uint16(port), User: "chat", KnownHosts: known, Identity: identity, Passphrase: "synthetic-passphrase"}
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	t.Cleanup(func() { inputWriter.Close(); inputReader.Close(); outputReader.Close(); outputWriter.Close() })
	done := make(chan error, 1)
	go func() { done <- guitransport.Run(ctx, inputReader, outputWriter); outputWriter.Close() }()
	requests := json.NewEncoder(inputWriter)
	if err = requests.Encode(config); err != nil {
		t.Fatal(err)
	}
	responses := make(chan guiResponse, 64)
	go func() {
		defer close(responses)
		d := json.NewDecoder(outputReader)
		for {
			var r guiResponse
			if d.Decode(&r) != nil {
				return
			}
			select {
			case responses <- r:
			case <-ctx.Done():
				return
			}
		}
	}()
	f := guiWireFixture{t: t, writer: requests, responses: responses}
	hello := f.read("hello", "")
	if hello.DeviceKey == nil || !hello.DeviceKey.Bound {
		t.Fatal("encrypted key did not complete signed SSH proof")
	}
	f.send(guiRequest{Type: "login_key", RequestID: "key", Username: "forged-other"})
	if r := f.read("auth", "key"); r.User != owner.Name {
		t.Fatal("encrypted key login lost derived owner")
	}
	f.read("state", "")
	f.send(guiRequest{Type: "disconnect", RequestID: "bye"})
	f.read("status", "bye")
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("bridge did not stop after subsystem exit")
	}
	// Same generated key: no password, public-file or host-trust fallbacks.
	checkFailure := func(c guitransport.Config) {
		t.Helper()
		payload, _ := json.Marshal(c)
		err := guitransport.Run(t.Context(), strings.NewReader(string(payload)+"\n"), io.Discard)
		if err == nil {
			t.Fatal("invalid identity/host accepted")
		}
		if strings.Contains(err.Error(), c.Passphrase) && c.Passphrase != "" {
			t.Fatal("error leaked passphrase")
		}
	}
	wrong := config
	wrong.Passphrase = "incorrect-synthetic-passphrase"
	checkFailure(wrong)
	publicOnly := filepath.Join(dir, "public-only.key")
	write(publicOnly, gossh.MarshalAuthorizedKey(device.PublicKey()))
	wrong = config
	wrong.Identity = publicOnly
	checkFailure(wrong)
	badHosts := filepath.Join(dir, "changed_hosts")
	write(badHosts, []byte(knownhosts.Line([]string{listener.Addr().String()}, transportSigner(t).PublicKey())+"\n"))
	wrong = config
	wrong.KnownHosts = badHosts
	checkFailure(wrong)
	emptyHosts := filepath.Join(dir, "empty_hosts")
	write(emptyHosts, nil)
	wrong = config
	wrong.KnownHosts = emptyHosts
	checkFailure(wrong)
}
