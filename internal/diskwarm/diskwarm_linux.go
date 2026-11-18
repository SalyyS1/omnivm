//go:build linux

package diskwarm

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func platformScanExtents(f *os.File, fileSize int64, maxBytes uint64) ([]DataExtent, uint64, bool) {
	if fileSize <= 0 || maxBytes == 0 {
		return nil, 0, true
	}
	fd := int(f.Fd())

	var extents []DataExtent
	var warmed uint64
	var skipped uint64
	var cur int64

	for cur < fileSize && warmed < maxBytes {
		dataStart, err := unix.Seek(fd, cur, unix.SEEK_DATA)
		if err != nil {
			if errors.Is(err, unix.ENXIO) {
				// Remaining region [cur, fileSize) is a trailing sparse hole.
				skipped += uint64(fileSize - cur)
				cur = fileSize
				break
			}
			// Filesystem does not support SEEK_DATA (EINVAL / EOPNOTSUPP).
			return nil, 0, false
		}
		if dataStart > cur {
			skipped += uint64(dataStart - cur)
		}

		holeStart, err := unix.Seek(fd, dataStart, unix.SEEK_HOLE)
		if err != nil || holeStart <= dataStart {
			holeStart = fileSize
		}

		extLen := uint64(holeStart - dataStart)
		remainingBudget := maxBytes - warmed
		if extLen > remainingBudget {
			extLen = remainingBudget
		}
		extents = append(extents, DataExtent{
			Offset: dataStart,
			Length: int64(extLen),
		})
		warmed += extLen
		cur = dataStart + int64(extLen)
		if cur < holeStart {
			// Budget exhausted inside a data extent.
			break
		}
	}

	// Reset file offset to 0 for cleanliness.
	_, _ = unix.Seek(fd, 0, 0)
	return extents, skipped, true
}

func platformAdviseWillNeed(f *os.File, offset int64, length int64) bool {
	if length <= 0 {
		return true
	}
	return unix.Fadvise(int(f.Fd()), offset, length, unix.FADV_WILLNEED) == nil
}

func platformDropPageCache(f *os.File, size int64) {
	if size <= 0 {
		return
	}
	_ = f.Sync()
	_ = unix.Fadvise(int(f.Fd()), 0, size, unix.FADV_DONTNEED)
}
