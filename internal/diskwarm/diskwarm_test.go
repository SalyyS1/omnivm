package diskwarm

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestWarmer_SparseExt4HoleSkippingAndStatCache(t *testing.T) {
	dir := t.TempDir()
	imgPath := filepath.Join(dir, "sparse-rootfs.ext4")

	f, err := os.OpenFile(imgPath, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0600)
	if err != nil {
		t.Fatal(err)
	}
	const logicalSize = 16 * 1024 * 1024 // 16 MiB logical
	if err := f.Truncate(logicalSize); err != nil {
		f.Close()
		t.Fatal(err)
	}
	payload := make([]byte, 64*1024)
	for i := range payload {
		payload[i] = 0x5a
	}
	// Write two 64 KiB extents at 0 and 8 MiB; ~15.875 MiB remains sparse holes.
	if _, err := f.WriteAt(payload, 0); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if _, err := f.WriteAt(payload, 8*1024*1024); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()

	w := NewWarmer(Config{
		Mode:            ModeAuto,
		Strategy:        StrategyPreadExtents,
		MaxBytesPerFile: 32 * 1024 * 1024,
		BudgetBytes:     64 * 1024 * 1024,
		CacheTTL:        time.Minute,
	})

	ctx := context.Background()
	rep1, err := w.WarmPath(ctx, imgPath)
	if err != nil {
		t.Fatalf("WarmPath #1: %v", err)
	}
	if rep1.CacheHit {
		t.Fatal("expected cold miss on first WarmPath")
	}
	if rep1.LogicalBytes != logicalSize {
		t.Errorf("LogicalBytes = %d, want %d", rep1.LogicalBytes, logicalSize)
	}
	// On Linux ext4/tmpfs supporting SEEK_DATA, sparse holes should be skipped (>14 MiB skipped).
	if runtime.GOOS == "linux" && rep1.SparseSkippedBytes > 0 {
		if rep1.WarmedBytes > 2*1024*1024 {
			t.Errorf("expected sparse extent warming <= 2 MiB, got %d bytes", rep1.WarmedBytes)
		}
	}

	// Second warm of identical unchanged file hits stat+TTL cache in O(1).
	rep2, err := w.WarmPath(ctx, imgPath)
	if err != nil {
		t.Fatalf("WarmPath #2: %v", err)
	}
	if !rep2.CacheHit {
		t.Fatal("expected CacheHit=true on repeated WarmPath within TTL")
	}

	// Mutating file mtime/size invalidates cache entry automatically.
	if err := os.Truncate(imgPath, logicalSize+4096); err != nil {
		t.Fatal(err)
	}
	rep3, err := w.WarmPath(ctx, imgPath)
	if err != nil {
		t.Fatalf("WarmPath #3 after truncate: %v", err)
	}
	if rep3.CacheHit {
		t.Fatal("expected cache invalidation after file size mutation")
	}

	m := w.Metrics()
	if m.Passes != 3 || m.FilesWarmed != 2 || m.CacheHits != 1 {
		t.Fatalf("unexpected Warmer metrics: %+v", m)
	}
}

func TestWarmer_RootfsDirectoryAndBootArtifactsBudget(t *testing.T) {
	dir := t.TempDir()
	kernelPath := filepath.Join(dir, "vmlinux")
	rootfsDir := filepath.Join(dir, "rootfs")

	if err := os.MkdirAll(filepath.Join(rootfsDir, "bin"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rootfsDir, "var", "log"), 0755); err != nil {
		t.Fatal(err)
	}

	kernelBytes := make([]byte, 128*1024)
	harnessBytes := make([]byte, 64*1024)
	busyboxBytes := make([]byte, 96*1024)
	ignoredLog := make([]byte, 512*1024)

	_ = os.WriteFile(kernelPath, kernelBytes, 0644)
	_ = os.WriteFile(filepath.Join(rootfsDir, "ovm-harness"), harnessBytes, 0755)
	_ = os.WriteFile(filepath.Join(rootfsDir, "bin", "busybox"), busyboxBytes, 0755)
	_ = os.WriteFile(filepath.Join(rootfsDir, "var", "log", "huge.log"), ignoredLog, 0644)

	w := NewWarmer(Config{
		Mode:            ModeEager,
		Strategy:        StrategyFadviseWillNeed,
		MaxBytesPerFile: 1024 * 1024,
		BudgetBytes:     2 * 1024 * 1024,
	})

	reps := w.WarmBootArtifacts(context.Background(), kernelPath, rootfsDir, filepath.Join(dir, "nonexistent.ext4"))
	if len(reps) != 2 {
		t.Fatalf("len(reps) = %d, want 2", len(reps))
	}
	// Only kernel (128 KiB) + priority rootfs binaries (64 + 96 = 160 KiB) are warmed; var/log is skipped.
	wantWarmed := uint64(len(kernelBytes) + len(harnessBytes) + len(busyboxBytes))
	if m := w.Metrics(); m.WarmedBytes != wantWarmed {
		t.Errorf("WarmedBytes = %d, want %d (var/log must not be warmed)", m.WarmedBytes, wantWarmed)
	}
}

func TestWarmer_OffModeAndBenchmark(t *testing.T) {
	off := NewWarmer(Config{Mode: ModeOff})
	if reps := off.WarmBootArtifacts(context.Background(), "/etc/hosts"); len(reps) != 0 {
		t.Fatalf("ModeOff must be a no-op, got %+v", reps)
	}

	res, err := RunBenchmark(context.Background(), BenchmarkConfig{
		LogicalMB:  16,
		DataMB:     2,
		Iterations: 2,
		Strategy:   StrategyFadviseWillNeed,
	})
	if err != nil {
		t.Fatalf("RunBenchmark: %v", err)
	}
	if res.LogicalMB != 16 || res.DataMB != 2 || res.NaiveFullScan.Samples != 2 || res.ExtentPrewarm.Samples != 2 {
		t.Fatalf("unexpected benchmark result: %+v", res)
	}
}
