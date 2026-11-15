package overlay

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

const sparseChunkSize = 64 * 1024

// CloneStats summarizes Copy-on-Write and sparse extent savings for a clone operation.
type CloneStats struct {
	FilesCloned   uint64 `json:"files_cloned"`
	ReflinkFiles  uint64 `json:"reflink_files"`
	SparseFiles   uint64 `json:"sparse_files"`
	LogicalBytes  uint64 `json:"logical_bytes"`
	SavedBytes    uint64 `json:"saved_bytes"`
}

// OverlayStats reports cumulative overlay and disk-clone telemetry for the daemon.
type OverlayStats struct {
	OverlaysCreated uint64 `json:"overlays_created"`
	DiskClones      uint64 `json:"disk_clones"`
	ReflinkFiles    uint64 `json:"reflink_files"`
	SparseFiles     uint64 `json:"sparse_files"`
	LogicalBytes    uint64 `json:"logical_bytes"`
	SavedBytes      uint64 `json:"saved_bytes"`
}

func isAllZero(buf []byte) bool {
	for _, b := range buf {
		if b != 0 {
			return false
		}
	}
	return true
}

// CloneFileCoW clones srcPath to dstPath preserving permissions.
// On reflink-capable Linux filesystems (XFS, Btrfs, bcachefs), it executes an
// O(1) FICLONE ioctl sharing all data extents Copy-on-Write. On filesystems
// without reflink support (such as ext4 or tmpfs), it falls back to a sparse
// extent copy that skips zero-filled 64 KiB blocks so sparse .ext4 disk images
// retain thin physical allocation.
func CloneFileCoW(srcPath, dstPath string, perm os.FileMode) (CloneStats, error) {
	var stats CloneStats

	src, err := os.Open(srcPath)
	if err != nil {
		return stats, fmt.Errorf("open source %s: %w", srcPath, err)
	}
	defer src.Close()

	info, err := src.Stat()
	if err != nil {
		return stats, fmt.Errorf("stat source %s: %w", srcPath, err)
	}
	size := info.Size()
	if perm == 0 {
		perm = info.Mode().Perm()
	}

	if err := os.MkdirAll(filepath.Dir(dstPath), 0755); err != nil {
		return stats, fmt.Errorf("mkdir parent %s: %w", dstPath, err)
	}

	tmpPath := dstPath + ".cow.tmp"
	_ = os.Remove(tmpPath)

	dst, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return stats, fmt.Errorf("create staging %s: %w", tmpPath, err)
	}

	cleanupTmp := func() {
		dst.Close()
		_ = os.Remove(tmpPath)
	}

	stats.FilesCloned = 1
	stats.LogicalBytes = uint64(size)

	if size > 0 {
		reflinked, rErr := tryReflinkClone(dst, src)
		if rErr != nil {
			cleanupTmp()
			return stats, fmt.Errorf("reflink clone %s: %w", srcPath, rErr)
		}
		if reflinked {
			stats.ReflinkFiles = 1
			stats.SavedBytes = uint64(size)
		} else {
			sparseSaved, cErr := copySparseExtents(dst, src, size)
			if cErr != nil {
				cleanupTmp()
				return stats, fmt.Errorf("sparse copy %s: %w", srcPath, cErr)
			}
			stats.SparseFiles = 1
			stats.SavedBytes = sparseSaved
		}
	}

	if err := dst.Chmod(perm); err != nil {
		cleanupTmp()
		return stats, fmt.Errorf("chmod %s: %w", tmpPath, err)
	}
	if err := dst.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return stats, fmt.Errorf("close %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, dstPath); err != nil {
		_ = os.Remove(tmpPath)
		return stats, fmt.Errorf("rename %s -> %s: %w", tmpPath, dstPath, err)
	}
	return stats, nil
}

func copySparseExtents(dst, src *os.File, totalSize int64) (uint64, error) {
	buf := make([]byte, sparseChunkSize)
	var offset int64
	var holeBytes uint64
	trailingHole := false

	for {
		n, rErr := src.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if isAllZero(chunk) {
				if _, sErr := dst.Seek(int64(n), io.SeekCurrent); sErr != nil {
					return 0, sErr
				}
				holeBytes += uint64(n)
				trailingHole = true
			} else {
				if _, wErr := dst.Write(chunk); wErr != nil {
					return 0, wErr
				}
				trailingHole = false
			}
			offset += int64(n)
		}
		if rErr == io.EOF {
			break
		}
		if rErr != nil {
			return 0, rErr
		}
	}

	if trailingHole || offset != totalSize {
		if err := dst.Truncate(totalSize); err != nil {
			return 0, err
		}
	}
	return holeBytes, nil
}

// CloneTreeCoW atomically clones directory tree srcDir into dstDir preserving
// directory permissions, symlinks (without dereferencing), and regular files
// using Copy-on-Write reflinks or sparse extent copies.
func CloneTreeCoW(ctx context.Context, srcDir, dstDir string) (CloneStats, error) {
	var total CloneStats

	srcInfo, err := os.Stat(srcDir)
	if err != nil {
		return total, fmt.Errorf("stat source dir %s: %w", srcDir, err)
	}
	if !srcInfo.IsDir() {
		return total, fmt.Errorf("source %s is not a directory", srcDir)
	}

	if err := os.MkdirAll(filepath.Dir(dstDir), 0755); err != nil {
		return total, fmt.Errorf("create parent dir: %w", err)
	}

	staging := dstDir + ".tmp"
	_ = os.RemoveAll(staging)
	if err := os.MkdirAll(staging, srcInfo.Mode().Perm()); err != nil {
		return total, fmt.Errorf("create staging dir: %w", err)
	}

	type dirPerm struct {
		path string
		perm os.FileMode
	}
	var dirs []dirPerm

	walkErr := filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, wErr error) error {
		if wErr != nil {
			return wErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}

		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(staging, rel)

		info, err := d.Info()
		if err != nil {
			return err
		}
		mode := info.Mode()

		switch {
		case mode.IsDir():
			// Ensure owner write during population, then restore exact mode bits at the end.
			if err := os.MkdirAll(target, mode.Perm()|0700); err != nil {
				return err
			}
			dirs = append(dirs, dirPerm{path: target, perm: mode.Perm()})
		case mode&os.ModeSymlink != 0:
			linkTarget, err := os.Readlink(path)
			if err != nil {
				return fmt.Errorf("readlink %s: %w", path, err)
			}
			if err := os.Symlink(linkTarget, target); err != nil {
				return fmt.Errorf("symlink %s -> %s: %w", target, linkTarget, err)
			}
		case mode.IsRegular():
			st, err := CloneFileCoW(path, target, mode.Perm())
			if err != nil {
				return err
			}
			total.FilesCloned += st.FilesCloned
			total.ReflinkFiles += st.ReflinkFiles
			total.SparseFiles += st.SparseFiles
			total.LogicalBytes += st.LogicalBytes
			total.SavedBytes += st.SavedBytes
		default:
			// Skip special device/socket nodes in rootfs trees if any.
		}
		return nil
	})
	if walkErr != nil {
		_ = os.RemoveAll(staging)
		return total, walkErr
	}

	// Restore exact directory permissions bottom-up.
	for i := len(dirs) - 1; i >= 0; i-- {
		_ = os.Chmod(dirs[i].path, dirs[i].perm)
	}

	if err := os.Rename(staging, dstDir); err != nil {
		_ = os.RemoveAll(staging)
		return total, fmt.Errorf("rename staging to final: %w", err)
	}
	return total, nil
}
