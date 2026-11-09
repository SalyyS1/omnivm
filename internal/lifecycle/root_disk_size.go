package lifecycle

import (
	"fmt"
	"io/fs"
	"path/filepath"
)

const (
	// ext4BlockSize is the block size used when estimating how much space a
	// directory tree occupies once copied into an ext4 image.
	ext4BlockSize = 4096

	// rootDiskHeadroomMB is free space added on top of the image contents so
	// the workload can write files without an explicit disk size.
	rootDiskHeadroomMB = 256
)

// dirUsageBytes estimates the space a directory tree needs inside an ext4
// image: every regular file rounded up to whole blocks, plus one block per
// entry for directories, symlinks and inode/dirent overhead.
func dirUsageBytes(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		total += ext4BlockSize
		if d.Type().IsRegular() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			total += (info.Size() + ext4BlockSize - 1) / ext4BlockSize * ext4BlockSize
		}
		return nil
	})
	return total, err
}

// rootDiskSizeMB picks the size of a new instance root disk.
//
// usedMB is the estimated size of the image contents. requestedMB is the
// size the caller asked for (0 = automatic). The automatic size is the
// contents plus 30% for filesystem metadata plus fixed headroom, never below
// minMB. An explicit request must fit the contents and stay within maxMB.
func rootDiskSizeMB(usedMB, requestedMB, minMB, maxMB int) (int, error) {
	needed := usedMB + usedMB*3/10 + 1
	if requestedMB > 0 {
		if requestedMB > maxMB {
			return 0, fmt.Errorf("requested disk size %dMB exceeds the limit of %dMB", requestedMB, maxMB)
		}
		if requestedMB < needed {
			return 0, fmt.Errorf("requested disk size %dMB is too small for the image (needs at least %dMB)", requestedMB, needed)
		}
		return requestedMB, nil
	}
	size := needed + rootDiskHeadroomMB
	if size < minMB {
		size = minMB
	}
	if size > maxMB {
		if needed > maxMB {
			return 0, fmt.Errorf("image needs %dMB, more than the disk size limit of %dMB", needed, maxMB)
		}
		size = maxMB
	}
	return size, nil
}
