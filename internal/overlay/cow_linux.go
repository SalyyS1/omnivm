//go:build linux

package overlay

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// tryReflinkClone attempts an O(1) Copy-on-Write file clone using the Linux
// FICLONE ioctl (supported by XFS, Btrfs, bcachefs, and OCFS2).
// Returns (true, nil) when blocks were cloned without physical data copy,
// (false, nil) when the underlying filesystem does not support reflinks,
// or (false, err) on unexpected I/O errors.
func tryReflinkClone(dst, src *os.File) (bool, error) {
	err := unix.IoctlFileClone(int(dst.Fd()), int(src.Fd()))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, unix.EOPNOTSUPP) ||
		errors.Is(err, unix.EXDEV) ||
		errors.Is(err, unix.EINVAL) ||
		errors.Is(err, unix.ENOSYS) ||
		errors.Is(err, unix.ENOTTY) ||
		errors.Is(err, syscall.EOPNOTSUPP) {
		return false, nil
	}
	return false, err
}
