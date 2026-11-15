package overlay

import (
	"bytes"
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCopyOverlayCreateAndRemove(t *testing.T) {
	sourceDir := t.TempDir()
	baseDir := t.TempDir()

	// Create a source tree with a regular file and a symlink
	if err := os.WriteFile(filepath.Join(sourceDir, "hello.txt"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(sourceDir, "subdir"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceDir, "subdir", "nested.txt"), []byte("nested"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("hello.txt", filepath.Join(sourceDir, "link.txt")); err != nil {
		t.Fatal(err)
	}

	ov := NewCopyOverlay(baseDir)

	// Create
	dest, err := ov.Create(context.Background(), sourceDir, "test-1")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Verify path
	if dest != ov.Path("test-1") {
		t.Errorf("dest = %q, want %q", dest, ov.Path("test-1"))
	}

	// Verify regular file
	data, err := os.ReadFile(filepath.Join(dest, "hello.txt"))
	if err != nil {
		t.Fatalf("read hello.txt: %v", err)
	}
	if string(data) != "hello" {
		t.Errorf("hello.txt = %q, want %q", data, "hello")
	}

	// Verify nested file
	data, err = os.ReadFile(filepath.Join(dest, "subdir", "nested.txt"))
	if err != nil {
		t.Fatalf("read nested.txt: %v", err)
	}
	if string(data) != "nested" {
		t.Errorf("nested.txt = %q, want %q", data, "nested")
	}

	// Verify symlink is preserved (not dereferenced)
	target, err := os.Readlink(filepath.Join(dest, "link.txt"))
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if target != "hello.txt" {
		t.Errorf("symlink target = %q, want %q", target, "hello.txt")
	}

	// Also create companion .ext4 file to verify Remove cleans both directory and raw disk image
	ext4File := dest + ".ext4"
	if err := os.WriteFile(ext4File, []byte("disk"), 0644); err != nil {
		t.Fatal(err)
	}

	// Remove
	if err := ov.Remove("test-1"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Error("directory still exists after remove")
	}
	if _, err := os.Stat(ext4File); !os.IsNotExist(err) {
		t.Error(".ext4 disk file still exists after remove")
	}
}

func TestCleanStale(t *testing.T) {
	baseDir := t.TempDir()
	ov := NewCopyOverlay(baseDir)

	// Create a stale task dir (fake old modtime)
	staleDir := filepath.Join(baseDir, "task-task-1")
	os.MkdirAll(staleDir, 0755)
	os.WriteFile(filepath.Join(staleDir, "file"), []byte("x"), 0644)
	oldTime := time.Now().Add(-3 * time.Hour)
	os.Chtimes(staleDir, oldTime, oldTime)

	// Create a fresh task dir
	freshDir := filepath.Join(baseDir, "task-task-2")
	os.MkdirAll(freshDir, 0755)
	os.WriteFile(filepath.Join(freshDir, "file"), []byte("x"), 0644)

	// Create a release dir (not a task - should not be cleaned)
	releaseDir := filepath.Join(baseDir, "rel-123")
	os.MkdirAll(releaseDir, 0755)
	oldTime2 := time.Now().Add(-5 * time.Hour)
	os.Chtimes(releaseDir, oldTime2, oldTime2)

	// Create an incomplete staging dir (.tmp - always removed)
	stagingDir := filepath.Join(baseDir, "rel-456.tmp")
	os.MkdirAll(stagingDir, 0755)
	os.WriteFile(filepath.Join(stagingDir, "file"), []byte("x"), 0644)

	// Run GC with 1 hour max age
	ov.CleanStale(1 * time.Hour)

	// Stale task dir should be removed
	if _, err := os.Stat(staleDir); !os.IsNotExist(err) {
		t.Error("stale task dir should have been removed")
	}

	// Fresh task dir should still exist
	if _, err := os.Stat(freshDir); err != nil {
		t.Error("fresh task dir should still exist")
	}

	// Release dir should still exist (not a task-*)
	if _, err := os.Stat(releaseDir); err != nil {
		t.Error("release dir should not be cleaned by task GC")
	}

	// Staging dir should always be removed regardless of age
	if _, err := os.Stat(stagingDir); !os.IsNotExist(err) {
		t.Error("staging .tmp dir should have been removed")
	}
}

func TestCreateUsesAtomicRename(t *testing.T) {
	sourceDir := t.TempDir()
	baseDir := t.TempDir()
	os.WriteFile(filepath.Join(sourceDir, "data.txt"), []byte("content"), 0644)

	ov := NewCopyOverlay(baseDir)

	dest, err := ov.Create(context.Background(), sourceDir, "rel-test")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Final dir should exist
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("final dir missing: %v", err)
	}

	// Staging dir should NOT exist (renamed away)
	staging := dest + ".tmp"
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Error("staging .tmp dir should not exist after successful create")
	}

	// Content should be correct
	data, err := os.ReadFile(filepath.Join(dest, "data.txt"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "content" {
		t.Errorf("data = %q, want %q", data, "content")
	}
}

func TestCopyOverlayRemoveNonexistent(t *testing.T) {
	baseDir := t.TempDir()
	ov := NewCopyOverlay(baseDir)

	// Should not error on nonexistent
	if err := ov.Remove("nonexistent"); err != nil {
		t.Fatalf("remove nonexistent: %v", err)
	}
}

func TestCloneFileCoW_SparseHolePreservationAndIsolation(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "golden.ext4")
	dstPath := filepath.Join(dir, "branch.ext4")

	const totalSize = 2 * 1024 * 1024 // 2 MiB
	f, err := os.OpenFile(srcPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0640)
	if err != nil {
		t.Fatal(err)
	}
	header := bytes.Repeat([]byte{0xAB}, sparseChunkSize)
	footer := bytes.Repeat([]byte{0xCD}, sparseChunkSize)
	if _, err := f.WriteAt(header, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(footer, totalSize-sparseChunkSize); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := CloneFileCoW(srcPath, dstPath, 0640)
	if err != nil {
		t.Fatalf("CloneFileCoW: %v", err)
	}
	if st.FilesCloned != 1 {
		t.Errorf("FilesCloned = %d, want 1", st.FilesCloned)
	}
	if st.LogicalBytes != totalSize {
		t.Errorf("LogicalBytes = %d, want %d", st.LogicalBytes, totalSize)
	}
	minExpectedSaved := uint64(totalSize - 2*sparseChunkSize)
	if st.SavedBytes < minExpectedSaved {
		t.Errorf("SavedBytes = %d, want >= %d", st.SavedBytes, minExpectedSaved)
	}

	srcBytes, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	dstBytes, err := os.ReadFile(dstPath)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(srcBytes) != sha256.Sum256(dstBytes) {
		t.Fatal("cloned file SHA-256 mismatch with source")
	}

	// Mutating the clone must never mutate the source file (CoW isolation).
	mutF, err := os.OpenFile(dstPath, os.O_WRONLY, 0640)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mutF.WriteAt([]byte("BRANCH_DELTA"), 16); err != nil {
		t.Fatal(err)
	}
	_ = mutF.Close()

	srcAfter, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(srcBytes, srcAfter) {
		t.Fatal("mutating cloned file corrupted source file")
	}
}

func TestCloneTreeCoW_ReadOnlyDirsAndSymlinks(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := filepath.Join(t.TempDir(), "cloned-tree")

	roSub := filepath.Join(srcDir, "etc-ro")
	if err := os.MkdirAll(roSub, 0755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(roSub, 0755) })

	if err := os.WriteFile(filepath.Join(roSub, "conf"), []byte("immutable=1"), 0444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(roSub, 0555); err != nil {
		t.Fatal(err)
	}
	binPath := filepath.Join(srcDir, "run.sh")
	if err := os.WriteFile(binPath, []byte("#!/bin/sh\necho ok\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("run.sh", filepath.Join(srcDir, "entrypoint")); err != nil {
		t.Fatal(err)
	}

	st, err := CloneTreeCoW(context.Background(), srcDir, dstDir)
	if err != nil {
		t.Fatalf("CloneTreeCoW: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dstDir, "etc-ro"), 0755) })

	if st.FilesCloned != 2 {
		t.Errorf("FilesCloned = %d, want 2", st.FilesCloned)
	}

	roInfo, err := os.Stat(filepath.Join(dstDir, "etc-ro"))
	if err != nil {
		t.Fatal(err)
	}
	if roInfo.Mode().Perm() != 0555 {
		t.Errorf("etc-ro mode = %o, want 0555", roInfo.Mode().Perm())
	}

	binInfo, err := os.Stat(filepath.Join(dstDir, "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if binInfo.Mode().Perm() != 0755 {
		t.Errorf("run.sh mode = %o, want 0755", binInfo.Mode().Perm())
	}

	linkTarget, err := os.Readlink(filepath.Join(dstDir, "entrypoint"))
	if err != nil {
		t.Fatal(err)
	}
	if linkTarget != "run.sh" {
		t.Errorf("entrypoint symlink = %q, want run.sh", linkTarget)
	}
}

func TestCopyOverlay_CloneDiskImageAndStats(t *testing.T) {
	baseDir := t.TempDir()
	ov := NewCopyOverlay(baseDir)

	srcDisk := filepath.Join(t.TempDir(), "snap.ext4")
	f, err := os.OpenFile(srcDisk, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(1024 * 1024); err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteAt([]byte("EXT4HDR"), 0)
	_ = f.Close()

	dstDisk := filepath.Join(baseDir, "inst-branch.ext4")
	if _, err := ov.CloneDiskImage(srcDisk, dstDisk); err != nil {
		t.Fatalf("CloneDiskImage: %v", err)
	}

	stats := ov.Stats()
	if stats.DiskClones != 1 {
		t.Errorf("DiskClones = %d, want 1", stats.DiskClones)
	}
	if stats.LogicalBytes != 1024*1024 {
		t.Errorf("LogicalBytes = %d, want 1048576", stats.LogicalBytes)
	}
	if stats.SavedBytes == 0 {
		t.Errorf("expected SavedBytes > 0 for sparse 1 MiB disk, got 0")
	}
}
