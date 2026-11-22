//go:build windows

package security

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

func prepareTrustLockFile(f *os.File) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Size() > 0 {
		return nil
	}
	_, err = f.WriteAt([]byte{0}, 0)
	return err
}

func tryLockTrustFile(f *os.File) (bool, error) {
	overlapped := &windows.Overlapped{}
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, overlapped)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return false, nil
	}
	return false, err
}

func unlockTrustFile(f *os.File) error {
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &windows.Overlapped{})
}
