//go:build windows

package buffer

import (
	"os"

	"golang.org/x/sys/windows"
)

// lockFile takes an exclusive lock on f without waiting; the lock goes
// with the process, so a crash never leaves it behind.
func lockFile(f *os.File) error {
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
}
