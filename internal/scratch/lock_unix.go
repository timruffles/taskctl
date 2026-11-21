//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly

package scratch

import (
	"errors"
	"os"
	"syscall"
)

func tryCleanupLock(file *os.File) (func(), bool, error) {
	err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return func() { _ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN) }, true, nil
}
