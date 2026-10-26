//go:build windows

package main

import (
	"fmt"
	"golang.org/x/sys/windows"
	"os"
	"path/filepath"
)

func lockState(state string) (func(), error) {
	if e := os.MkdirAll(state, 0700); e != nil {
		return nil, e
	}
	f, e := os.OpenFile(filepath.Join(state, ".miochat.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	var overlap windows.Overlapped
	if e = windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlap); e != nil {
		f.Close()
		return nil, fmt.Errorf("状态目录正在使用，请先停止对应实例：%w", e)
	}
	release := func() { windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &overlap); f.Close() }
	if e = recoverPendingRestore(state); e != nil {
		release()
		return nil, e
	}
	return release, nil
}
func diskFree(path string) (uint64, error) {
	p, e := windows.UTF16PtrFromString(path)
	if e != nil {
		return 0, e
	}
	var available, total, free uint64
	e = windows.GetDiskFreeSpaceEx(p, &available, &total, &free)
	return available, e
}
func replaceFile(source, target string) error {
	from, e := windows.UTF16PtrFromString(source)
	if e != nil {
		return e
	}
	to, e := windows.UTF16PtrFromString(target)
	if e != nil {
		return e
	}
	return windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}

// Files are flushed by File.Sync and installed by a write-through native move.
// Windows has no portable directory fsync operation.
func syncDirectory(path string) error { return nil }
