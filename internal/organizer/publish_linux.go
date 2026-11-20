//go:build linux

package organizer

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func publishNoReplace(source, target string) error {
	err := unix.Renameat2(
		unix.AT_FDCWD,
		source,
		unix.AT_FDCWD,
		target,
		unix.RENAME_NOREPLACE,
	)
	if err == nil {
		return nil
	}
	if !errors.Is(err, unix.ENOSYS) &&
		!errors.Is(err, unix.EINVAL) &&
		!errors.Is(err, unix.EOPNOTSUPP) {
		return err
	}
	return publishViaHardLink(source, target)
}

func publishViaHardLink(source, target string) error {
	if err := os.Link(source, target); err != nil {
		return err
	}
	return os.Remove(source)
}
