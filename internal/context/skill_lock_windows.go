//go:build windows

package context

import (
	"errors"
	"io/fs"
	"os"

	"golang.org/x/sys/windows"
)

func lockSkillDirectory(dir *os.Root) (func(), error) {
	const name = ".cloud-harness-skill.lock"
	f, err := dir.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if errors.Is(err, fs.ErrExist) {
		info, err := dir.Lstat(name)
		if err != nil || !info.Mode().IsRegular() {
			return nil, skillConflict()
		}
		f, err = dir.Open(name)
		if err != nil {
			return nil, err
		}
		opened, err := f.Stat()
		if err != nil || !os.SameFile(info, opened) {
			_ = f.Close()
			return nil, skillConflict()
		}
	} else if err != nil {
		return nil, err
	}
	err = windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
	if err != nil {
		_ = f.Close()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, skillConflict()
		}
		return nil, err
	}
	return func() {
		_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &windows.Overlapped{})
		_ = f.Close()
	}, nil
}
