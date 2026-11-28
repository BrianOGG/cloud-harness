//go:build unix

package context

import (
	"errors"
	"os"
	"syscall"
)

// Lock the directory inode; publishing an edit replaces the file inode.
func lockSkillDirectory(dir *os.Root) (func(), error) {
	f, err := dir.Open(".")
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, skillConflict()
		}
		return nil, err
	}
	return func() { _ = f.Close() }, nil
}
