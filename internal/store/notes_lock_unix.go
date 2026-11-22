//go:build unix

package store

import (
	"errors"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

func (s *Store) noteLock(shared bool) (func(), error) {
	if err := s.resolveDatabasePath(); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(filepath.Dir(s.databasePath), ".notes.lock"), os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, classify(err)
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		if err != nil {
			return nil, classify(err)
		}
		return nil, failure("storage_invalid", "note lock must be a regular file")
	}
	operation := unix.LOCK_EX
	if shared {
		operation = unix.LOCK_SH
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		err = unix.Flock(int(f.Fd()), operation|unix.LOCK_NB)
		if err == nil {
			return func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN); _ = f.Close() }, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			f.Close()
			return nil, classify(err)
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, failure("busy", "note storage is busy; retry the operation")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
