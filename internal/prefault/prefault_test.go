package prefault

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestRegion_AllStrategiesAndValidation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	buf := make([]byte, 256*1024) // 64 4KiB pages
	for _, strat := range []string{StrategyMadvisePopulate, StrategyTouchRead, StrategyTouchWrite} {
		st, err := Region(ctx, buf, strat, 4)
		if err != nil {
			t.Fatalf("Region(%s): %v", strat, err)
		}
		if st.Bytes != uint64(len(buf)) || st.PagesTouched == 0 {
			t.Fatalf("unexpected stats for %s: %+v", strat, st)
		}
	}

	if _, err := Region(ctx, buf, "invalid-strategy", 2); err == nil {
		t.Fatal("expected error for unsupported strategy")
	}
}

func TestRunBenchmark_EliminatesMinorPageFaultsOnWarmPool(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	res, err := RunBenchmark(ctx, BenchmarkConfig{
		PoolSize:     3,
		SlotMemoryMB: 8,
		Workers:      4,
		Iterations:   3,
		Strategy:     StrategyTouchWrite,
	})
	if err != nil {
		t.Fatalf("RunBenchmark: %v", err)
	}
	if res.DemandPaged.Samples != 9 || res.WarmPrefaulted.Samples != 9 {
		t.Fatalf("unexpected sample counts: demand=%d warm=%d", res.DemandPaged.Samples, res.WarmPrefaulted.Samples)
	}
	if res.TotalPagesPerIteration == 0 {
		t.Fatal("expected positive TotalPagesPerIteration")
	}

	if runtime.GOOS == "linux" {
		// 3 slots * 8 MiB * 3 iterations = 18,432 4KiB pages.
		// Demand-paged phase faults every page on first touch, whereas pre-faulted
		// warm slots have already populated writable PTEs prior to WalkPages.
		if res.DemandMinorFaults < 10000 {
			t.Fatalf("expected substantial demand minor faults on Linux, got %d", res.DemandMinorFaults)
		}
		if res.WarmMinorFaults >= res.DemandMinorFaults/4 {
			t.Fatalf("expected warm minor faults (%d) << demand minor faults (%d)", res.WarmMinorFaults, res.DemandMinorFaults)
		}
	}
}

func TestFilePrefault_MmapSnapshotImage(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	path := filepath.Join(t.TempDir(), "snapshot.ext4")
	payload := make([]byte, 512*1024)
	for i := range payload {
		payload[i] = byte(i & 0xff)
	}
	if err := os.WriteFile(path, payload, 0644); err != nil {
		t.Fatal(err)
	}

	st, err := File(ctx, path, 2)
	if err != nil {
		t.Fatalf("File prefault: %v", err)
	}
	if st.Bytes != uint64(len(payload)) || st.PagesTouched == 0 {
		t.Fatalf("unexpected File stats: %+v", st)
	}
}
