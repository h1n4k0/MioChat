//go:build unix

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestDotActualInheritedPipeRuntimeDefaultOffAndInvalidFDs(t *testing.T) {
	if runtime, e := prepareDotPrivatePipes(nil, dotPipeConfig{ReadFD: -1, WriteFD: -1}); e != nil || runtime != nil {
		t.Fatal("default runtime started", runtime, e)
	}
	for _, config := range []dotPipeConfig{{Enabled: true, ReadFD: -1, WriteFD: -1}, {ReadFD: 3, WriteFD: 4}, {Enabled: true, ReadFD: 3, WriteFD: 3}} {
		if _, e := prepareDotPrivatePipes(nil, config); e == nil {
			t.Fatal("invalid config accepted", config)
		}
	}
	f := newDotFixture(t)
	ordinary, e := os.CreateTemp(t.TempDir(), "not-a-pipe")
	if e != nil {
		t.Fatal(e)
	}
	defer ordinary.Close()
	if _, e = dotDuplicatePipeFD(int(ordinary.Fd()), false); e == nil {
		t.Fatal("ordinary file accepted")
	}
	input, writer, e := os.Pipe()
	if e != nil {
		t.Fatal(e)
	}
	defer input.Close()
	defer writer.Close()
	output, reader, e := os.Pipe()
	if e != nil {
		t.Fatal(e)
	}
	defer output.Close()
	defer reader.Close()
	// os.Pipe returns read then write. Cross-direction inputs must be rejected.
	if _, e = prepareDotPrivatePipes(f.s, dotPipeConfig{Enabled: true, ReadFD: int(writer.Fd()), WriteFD: int(reader.Fd())}); e == nil {
		t.Fatal("write-end input accepted")
	}
	if _, e = f.s.db.Exec(`UPDATE config SET value='off' WHERE key='dot_enabled'`); e != nil {
		t.Fatal(e)
	}
	if _, e = prepareDotPrivatePipes(f.s, dotPipeConfig{Enabled: true, ReadFD: int(input.Fd()), WriteFD: int(reader.Fd())}); e != errDotDisabled {
		t.Fatal("flag bypassed feature off", e)
	}
}
func TestDotActualInheritedPipeRuntimeProfileAndParentDisconnect(t *testing.T) {
	f := newDotFixture(t)
	input, writeInput, e := os.Pipe()
	if e != nil {
		t.Fatal(e)
	}
	defer input.Close()
	defer writeInput.Close()
	readOutput, output, e := os.Pipe()
	if e != nil {
		t.Fatal(e)
	}
	defer readOutput.Close()
	defer output.Close()
	runtime, e := prepareDotPrivatePipes(f.s, dotPipeConfig{Enabled: true, ReadFD: int(input.Fd()), WriteFD: int(output.Fd())})
	if e != nil {
		t.Fatal(e)
	}
	defer runtime.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runtime.Serve(ctx) }()
	request := dotBridgeRequest{ID: "synthetic-real-fd", Method: "profile", Identity: f.identity}
	raw, e := json.Marshal(request)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = writeInput.Write(append(raw, '\n')); e != nil {
		t.Fatal(e)
	}
	line, e := bufio.NewReader(readOutput).ReadBytes('\n')
	if e != nil {
		t.Fatal(e)
	}
	var reply dotBridgeReply
	if e = json.Unmarshal(line, &reply); e != nil || reply.ID != request.ID || reply.Error != "" {
		t.Fatal(reply, e)
	}
	// Closing the supervisor's request end yields EOF in the owned duplicate too.
	writeInput.Close()
	select {
	case e := <-done:
		if e == nil {
			t.Fatal("parent failure reported healthy")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pipe EOF did not stop bridge")
	}
}
