package overlay

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// CopyOverlay implements Overlay using Copy-on-Write reflinks (FICLONE on XFS,
// Btrfs, bcachefs) with automatic fallback to sparse extent cloning.
// Symlinks, directory modes, and busybox-style rootfs layouts are preserved
// without spawning external tar processes.
type CopyOverlay struct {
	baseDir string // directory containing all overlay copies

	overlaysCreated uint64
	diskClones      uint64
	reflinkFiles    uint64
	sparseFiles     uint64
	logicalBytes    uint64
	savedBytes      uint64
}

// NewCopyOverlay creates a CopyOverlay that stores copies under baseDir.
func NewCopyOverlay(baseDir string) *CopyOverlay {
	return &CopyOverlay{baseDir: baseDir}
}

func (o *CopyOverlay) Create(ctx context.Context, sourceDir, destID string) (string, error) {
	dest := filepath.Join(o.baseDir, destID)

	// Reuse existing overlay if present (e.g. after daemon restart)
	if _, err := os.Stat(dest); err == nil {
		return dest, nil
	}

	st, err := CloneTreeCoW(ctx, sourceDir, dest)
	if err != nil {
		return "", err
	}

	atomic.AddUint64(&o.overlaysCreated, 1)
	o.recordCloneStats(st)
	return dest, nil
}

// CloneDiskImage clones a raw .ext4 disk image into dstPath using FICLONE CoW
// or sparse hole-preserving extent copy and records the savings in OverlayStats.
func (o *CopyOverlay) CloneDiskImage(srcPath, dstPath string) (CloneStats, error) {
	st, err := CloneFileCoW(srcPath, dstPath, 0644)
	if err != nil {
		return st, err
	}
	atomic.AddUint64(&o.diskClones, 1)
	o.recordCloneStats(st)
	return st, nil
}

// RecordStats incorporates external CloneStats (e.g. workspace or snapshot clones)
// into the overlay manager's cumulative telemetry counters.
func (o *CopyOverlay) RecordStats(st CloneStats) {
	o.recordCloneStats(st)
}

func (o *CopyOverlay) recordCloneStats(st CloneStats) {
	atomic.AddUint64(&o.reflinkFiles, st.ReflinkFiles)
	atomic.AddUint64(&o.sparseFiles, st.SparseFiles)
	atomic.AddUint64(&o.logicalBytes, st.LogicalBytes)
	atomic.AddUint64(&o.savedBytes, st.SavedBytes)
}

// Stats returns cumulative Copy-on-Write and sparse storage telemetry.
func (o *CopyOverlay) Stats() OverlayStats {
	return OverlayStats{
		OverlaysCreated: atomic.LoadUint64(&o.overlaysCreated),
		DiskClones:      atomic.LoadUint64(&o.diskClones),
		ReflinkFiles:    atomic.LoadUint64(&o.reflinkFiles),
		SparseFiles:     atomic.LoadUint64(&o.sparseFiles),
		LogicalBytes:    atomic.LoadUint64(&o.logicalBytes),
		SavedBytes:      atomic.LoadUint64(&o.savedBytes),
	}
}

func (o *CopyOverlay) Remove(id string) error {
	base := filepath.Join(o.baseDir, id)
	_ = os.Remove(base + ".ext4")
	return os.RemoveAll(base)
}

func (o *CopyOverlay) Path(id string) string {
	return filepath.Join(o.baseDir, id)
}

// CleanStale removes stale overlay directories on daemon startup:
//   - task-* directories older than maxAge (crashed task cleanups)
//   - *.tmp / *.cow.tmp entries of any age (incomplete staging from crashed publishes)
func (o *CopyOverlay) CleanStale(maxAge time.Duration) {
	entries, err := os.ReadDir(o.baseDir)
	if err != nil {
		return
	}

	cutoff := time.Now().Add(-maxAge)
	for _, e := range entries {
		name := e.Name()
		path := filepath.Join(o.baseDir, name)

		// Always remove incomplete staging directories or files
		if strings.HasSuffix(name, ".tmp") {
			log.Printf("overlay GC: removing incomplete staging entry %s", name)
			os.RemoveAll(path)
			continue
		}

		if !e.IsDir() {
			continue
		}

		// Remove stale task overlays older than maxAge
		if strings.HasPrefix(name, "task-") {
			info, err := e.Info()
			if err != nil {
				continue
			}
			if info.ModTime().Before(cutoff) {
				log.Printf("overlay GC: removing stale task overlay %s (age=%v)", name, time.Since(info.ModTime()).Round(time.Minute))
				os.RemoveAll(path)
			}
		}
	}
}
