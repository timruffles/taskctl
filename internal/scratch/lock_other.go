//go:build !darwin && !linux && !freebsd && !netbsd && !openbsd && !dragonfly

package scratch

import (
	"fmt"
	"os"
)

func tryCleanupLock(file *os.File) (func(), bool, error) {
	return nil, false, fmt.Errorf("scratch cleanup requires operating-system file locking; supported on Linux, macOS and BSD")
}
