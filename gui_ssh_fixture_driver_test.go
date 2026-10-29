package main

import (
	"bufio"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/mio-qwq/MioChat/internal/guitransport"
	gossh "golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Opt-in subprocess harness for the Windows Rust -> bridge.exe -> real SSH
// integration test. Every identity and file is freshly generated test data.
func TestGUISSHFixtureDriver(t *testing.T) {
	if os.Getenv("MIOCHAT_LOCAL_FIXTURE") != "synthetic-ssh-v1" {
		t.Skip("explicit loopback SSH harness only")
	}
	s := testStore(t)
	owner := registerUser(t, s, "ssh-preview")
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	device, err := gossh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.BindSSHKey(owner.Name, "temporary Windows integration", string(gossh.MarshalAuthorizedKey(device.PublicKey()))); err != nil {
		t.Fatal(err)
	}
	h := newHub()
	host := transportSigner(t)
	server := makeServer(s, h, host)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go server.Serve(listener)
	t.Cleanup(func() { h.stopUI(); server.Close(); h.uiTasks.Wait() })
	dir := t.TempDir()
	identity := filepath.Join(dir, "synthetic.key")
	known := filepath.Join(dir, "known_hosts")
	block, err := gossh.MarshalPrivateKeyWithPassphrase(private, "synthetic", []byte("temporary-synthetic-phrase"))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(identity, pem.EncodeToMemory(block), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(known, []byte(knownhosts.Line([]string{listener.Addr().String()}, host.PublicKey())+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, portText, _ := net.SplitHostPort(listener.Addr().String())
	port, _ := strconv.Atoi(portText)
	config := guitransport.Config{Type: "device-key-transport-v1", Host: "127.0.0.1", Port: uint16(port), User: "chat", KnownHosts: known, Identity: identity, Passphrase: "temporary-synthetic-phrase"}
	if err = json.NewEncoder(os.Stdout).Encode(config); err != nil {
		t.Fatal(err)
	}
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
}
