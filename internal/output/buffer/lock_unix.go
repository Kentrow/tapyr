//go:build !windows

package buffer

import (
	"os"
	"syscall"
)

// lockFile takes an exclusive lock on f without waiting; the lock goes
// with the process, so a crash never leaves it behind.
func lockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}
