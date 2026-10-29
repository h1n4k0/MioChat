// Package guitransport implements only the native GUI's SSH subsystem pipe.
// It reuses the server's existing x/crypto dependency to support encrypted
// device keys without command-line/environment passphrases or an SSH agent.
package guitransport

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

type Config struct {
	Type       string `json:"type"`
	Host       string `json:"host"`
	Port       uint16 `json:"port"`
	User       string `json:"ssh_user"`
	KnownHosts string `json:"known_hosts"`
	Identity   string `json:"identity"`
	Passphrase string `json:"passphrase"`
}

func atom(value string, max int, host bool) bool {
	if value == "" || len(value) > max || value[0] == '-' {
		return false
	}
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' && !host || host && (r == '.' || r == ':' || r == '[' || r == ']') {
			continue
		}
		return false
	}
	return true
}

func loadSigner(path, passphrase string) (ssh.Signer, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot open selected device key")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 1<<20 {
		return nil, errors.New("selected device key must be a regular file under 1MiB")
	}
	data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return nil, errors.New("cannot read selected device key")
	}
	defer clear(data)
	var signer ssh.Signer
	if passphrase == "" {
		signer, err = ssh.ParsePrivateKey(data)
	} else {
		secret := []byte(passphrase)
		defer clear(secret)
		signer, err = ssh.ParsePrivateKeyWithPassphrase(data, secret)
	}
	if err != nil {
		return nil, errors.New("cannot unlock selected device key; check its format and passphrase")
	}
	return signer, nil
}

// Run reads one bounded private initialization frame, then forwards only
// miochat-gui-v1 over verified SSH. It never runs a shell, opens a PTY, discovers
// default keys, forwards an agent/port or writes credentials to disk.
func Run(ctx context.Context, input io.Reader, output io.Writer) error {
	reader := bufio.NewReaderSize(input, 4096)
	var frame []byte
	for {
		part, prefix, err := reader.ReadLine()
		if err != nil {
			return errors.New("missing transport initialization")
		}
		if len(frame)+len(part) > 64*1024 {
			return errors.New("transport initialization too large")
		}
		frame = append(frame, part...)
		if !prefix {
			break
		}
	}
	var config Config
	err := json.Unmarshal(frame, &config)
	clear(frame)
	if err != nil || config.Type != "device-key-transport-v1" || !atom(config.Host, 253, true) || !atom(config.User, 63, false) || config.Port == 0 || config.Identity == "" || config.KnownHosts == "" || len(config.Passphrase) > 4096 {
		return errors.New("invalid transport initialization")
	}
	signer, err := loadSigner(config.Identity, config.Passphrase)
	config.Passphrase = ""
	if err != nil {
		return err
	}
	verify, err := knownhosts.New(config.KnownHosts)
	if err != nil {
		return errors.New("cannot read application known hosts")
	}
	address := net.JoinHostPort(strings.Trim(config.Host, "[]"), strconv.Itoa(int(config.Port)))
	conn, err := (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext(ctx, "tcp", address)
	if err != nil {
		return errors.New("cannot reach requested SSH destination")
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	sshConfig := &ssh.ClientConfig{User: config.User, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: verify}
	clientConn, channels, requests, err := ssh.NewClientConn(conn, address, sshConfig)
	if err != nil {
		return errors.New("SSH key authentication or host verification failed")
	}
	_ = conn.SetDeadline(time.Time{})
	client := ssh.NewClient(clientConn, channels, requests)
	defer client.Close()
	session, err := client.NewSession()
	if err != nil {
		return errors.New("cannot open GUI SSH channel")
	}
	defer session.Close()
	stdin, err := session.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		return err
	}
	if err = session.RequestSubsystem("miochat-gui-v1"); err != nil {
		return errors.New("server does not provide miochat-gui-v1")
	}
	done := make(chan error, 2)
	go func() { _, copyErr := io.Copy(stdin, reader); _ = stdin.Close(); done <- copyErr }()
	go func() { _, copyErr := io.Copy(output, stdout); done <- copyErr }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case copyErr := <-done:
		return copyErr
	}
}
