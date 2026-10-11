//go:build unix

package helps

import (
	"os"

	"golang.org/x/sys/unix"
)

func lockClaudeBudget(path string) (func(), bool, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	fresh := err == nil
	if os.IsExist(err) {
		f, err = os.OpenFile(path, os.O_RDWR, 0600)
	}
	if err != nil {
		return nil, false, err
	}
	if err = unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, false, err
	}
	return func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN); _ = f.Close() }, fresh, nil
}
