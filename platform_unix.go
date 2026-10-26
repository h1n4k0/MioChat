//go:build !windows

package main

import (
	"fmt"
	"golang.org/x/sys/unix"
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
	if e = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); e != nil {
		f.Close()
		return nil, fmt.Errorf("状态目录正在使用，请先停止对应实例：%w", e)
	}
	release := func() { unix.Flock(int(f.Fd()), unix.LOCK_UN); f.Close() }
	if e = recoverPendingRestore(state); e != nil {
		release()
		return nil, e
	}
	return release, nil
}
func diskFree(path string) (uint64, error) {
	var stat unix.Statfs_t
	e := unix.Statfs(path, &stat)
	return uint64(stat.Bavail) * uint64(stat.Bsize), e
}
func replaceFile(source, target string) error { return os.Rename(source, target) }

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
