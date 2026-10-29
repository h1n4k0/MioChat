//go:build unix

package main

import (
	"errors"
	"golang.org/x/sys/unix"
	"os"
)

// Duplicate only an explicitly inherited pipe descriptor; never open a path or
// listener and never accept a TTY, regular file, socket or wrong-direction end.
func dotDuplicatePipeFD(fd int, read bool) (*os.File, error) {
	flags, e := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
	if e != nil {
		return nil, errors.New("dot inherited pipe unavailable")
	}
	want := unix.O_WRONLY
	if read {
		want = unix.O_RDONLY
	}
	if flags&unix.O_ACCMODE != want {
		return nil, errors.New("dot inherited pipe has wrong direction")
	}
	copy, e := unix.Dup(fd)
	if e != nil {
		return nil, errors.New("dot inherited pipe unavailable")
	}
	unix.CloseOnExec(copy)
	file := os.NewFile(uintptr(copy), "dot-private-pipe")
	info, e := file.Stat()
	if e != nil || info.Mode()&os.ModeNamedPipe == 0 {
		file.Close()
		return nil, errors.New("dot inherited descriptor is not a private pipe")
	}
	return file, nil
}
