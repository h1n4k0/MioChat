//go:build !unix

package main

import (
	"errors"
	"os"
)

func dotDuplicatePipeFD(fd int, read bool) (*os.File, error) {
	return nil, errors.New("trusted dot inherited-pipe runtime is unavailable on this platform")
}
