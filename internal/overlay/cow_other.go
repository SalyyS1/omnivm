//go:build !linux

package overlay

import "os"

func tryReflinkClone(_, _ *os.File) (bool, error) {
	return false, nil
}
