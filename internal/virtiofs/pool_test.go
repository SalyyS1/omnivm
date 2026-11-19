package virtiofs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestAlign2MiBAndComputeDAXWindow(t *testing.T) {
	if got := Align2MiB(0); got != 0 {
		t.Fatalf("Align2MiB(0) = %d, want 0", got)
	}
	if got := Align2MiB(10 * 1024 * 1024); got != MinDAXWindowBytes {
		t.Fatalf("Align2MiB(10MiB) = %d, want %d", got, MinDAXWindowBytes)
	}
	unaligned := uint64(131*1024*1024 + 512)
	want := uint64(130 * 1024 * 1024)
	if got := Align2MiB(unaligned); got != want {
		t.Fatalf("Align2MiB(%d) = %d, want %d", unaligned, got, want)
	}
	if want%DAXAlignmentBytes != 0 {
		t.Fatalf("expected multiple of 2 MiB")
	}

	pool := NewPool(Config{Mode: ModeAuto, DefaultWindowBytes: 256 * 1024 * 1024})
	if got := pool.ComputeDAXWindow(128, 0); got != 0 {
		t.Fatalf("ComputeDAXWindow(128MB) in auto mode = %d, want 0", got)
	}
	if got := pool.ComputeDAXWindow(512, 0); got != 256*1024*1024 {
		t.Fatalf("ComputeDAXWindow(512MB) = %d, want 256 MiB", got)
	}
	if err := pool.SetMode(ModeOff); err != nil {
		t.Fatalf("SetMode(off): %v", err)
	}
	if got := pool.ComputeDAXWindow(1024, 0); got != 0 {
		t.Fatalf("ComputeDAXWindow in off mode = %d, want 0", got)
	}
}

func TestLayerCacheHitsDedupAndPinnedLRUEviction(t *testing.T) {
	tmp := t.TempDir()
	layerA := filepath.Join(tmp, "alpine.ext4")
	if err := os.WriteFile(layerA, []byte("alpine-base-rootfs-v1"), 0644); err != nil {
		t.Fatal(err)
	}

	const mb = uint64(1024 * 1024)
	pool := NewPool(Config{
		Mode:          ModeDAX,
		MaxCacheBytes: 300 * mb,
	})

	// 1. First acquire is a miss; second acquire is a hit and credits DedupSavedBytes.
	e1, hit, err := pool.AcquireLayer("", "alpine:3.20", layerA, 100*mb)
	if err != nil || hit {
		t.Fatalf("first AcquireLayer: hit=%v err=%v", hit, err)
	}
	e2, hit, err := pool.AcquireLayer(e1.Digest, "alpine:3.20", layerA, 100*mb)
	if err != nil || !hit {
		t.Fatalf("second AcquireLayer: hit=%v err=%v", hit, err)
	}
	if e2.ActiveRefs != 2 {
		t.Fatalf("ActiveRefs = %d, want 2", e2.ActiveRefs)
	}
	m := pool.Metrics()
	if m.CacheMisses != 1 || m.CacheHits != 1 || m.DedupSavedBytes != 100*mb {
		t.Fatalf("unexpected metrics after hit: %+v", m)
	}

	// Release one ref so e1 remains pinned with ActiveRefs=1.
	pool.ReleaseLayer(e1.Digest)

	// 2. Register two unpinned 100 MiB layers (fills cache to 300 MiB).
	l2, _, err := pool.RegisterLayer("sha256:bbbb", "node:22-alpine", "", 100*mb)
	if err != nil {
		t.Fatalf("RegisterLayer l2: %v", err)
	}
	l3, _, err := pool.RegisterLayer("sha256:cccc", "python:3.12-alpine", "", 100*mb)
	if err != nil {
		t.Fatalf("RegisterLayer l3: %v", err)
	}

	// Touch l2 so l3 is newer? Wait: l2 was registered before l3, so l2 has older lruSeq unless touched.
	// Let's acquire+release l2 so l3 becomes the oldest unpinned entry!
	if _, _, err := pool.AcquireLayer(l2.Digest, "node:22-alpine", "", 100*mb); err != nil {
		t.Fatal(err)
	}
	pool.ReleaseLayer(l2.Digest)

	// 3. Register a 4th 100 MiB layer -> must evict l3 (oldest unpinned) while sparing pinned e1 and newer l2!
	if _, _, err := pool.RegisterLayer("sha256:dddd", "ubuntu:24.04", "", 100*mb); err != nil {
		t.Fatalf("RegisterLayer l4 with LRU eviction: %v", err)
	}
	if _, delErr := pool.DeleteLayer(e1.Digest); delErr == nil {
		t.Fatalf("expected error deleting pinned layer %s", e1.Digest)
	}
	snap := pool.Snapshot()
	for _, le := range snap.Layers {
		if le.Digest == l3.Digest {
			t.Fatalf("expected oldest unpinned layer %s to be evicted", l3.Digest)
		}
	}
	if snap.Metrics.LayerEvictions != 1 {
		t.Fatalf("LayerEvictions = %d, want 1", snap.Metrics.LayerEvictions)
	}

	// 4. Pin all remaining capacity and verify ErrCachePoolExhausted when nothing is evictable.
	if _, _, err := pool.AcquireLayer("sha256:bbbb", "node:22-alpine", "", 100*mb); err != nil {
		t.Fatal(err)
	}
	if _, _, err := pool.AcquireLayer("sha256:dddd", "ubuntu:24.04", "", 100*mb); err != nil {
		t.Fatal(err)
	}
	if _, _, err := pool.AcquireLayer("sha256:eeee", "rust:1.85", "", 100*mb); !errors.Is(err, ErrCachePoolExhausted) {
		t.Fatalf("expected ErrCachePoolExhausted when all 300 MiB are pinned, got %v", err)
	}
}

func TestPlanMountDAXPoolQuotaFallbackAndReclaim(t *testing.T) {
	tmp := t.TempDir()
	pmemPath := filepath.Join(tmp, "rootfs.raw")
	if err := os.WriteFile(pmemPath, []byte("rootfs-image"), 0644); err != nil {
		t.Fatal(err)
	}

	pool := NewPool(Config{
		Mode:               ModeDAX,
		DefaultWindowBytes: 128 * 1024 * 1024,
		MaxPoolBytes:       256 * 1024 * 1024,
		MaxCacheBytes:      1024 * 1024 * 1024,
	})
	layer, _, err := pool.RegisterLayer("", "python:3.12-alpine", pmemPath, 64*1024*1024)
	if err != nil {
		t.Fatal(err)
	}

	p1, err := pool.PlanMount("vm-1", "workspace", "/tmp/v1.sock", "/tmp/ws1", 512, 4, layer.Digest)
	if err != nil {
		t.Fatalf("PlanMount vm-1: %v", err)
	}
	if !p1.DAXEnabled || p1.DAXWindowBytes != 128*1024*1024 || p1.NumQueues != 4 {
		t.Fatalf("unexpected vm-1 plan: %+v", p1)
	}
	if p1.CHFsConfig["dax"] != true || p1.CHPmemConfig["discard_writes"] != true {
		t.Fatalf("expected CH dax=true and pmem discard_writes=true: fs=%v pmem=%v", p1.CHFsConfig, p1.CHPmemConfig)
	}

	p2, err := pool.PlanMount("vm-2", "workspace", "/tmp/v2.sock", "/tmp/ws2", 512, 2, layer.Digest)
	if err != nil || !p2.DAXEnabled {
		t.Fatalf("PlanMount vm-2: err=%v dax=%v", err, p2.DAXEnabled)
	}

	// Third VM exceeds 256 MiB MaxPoolBytes -> must gracefully fall back to cache=auto without failing boot.
	p3, err := pool.PlanMount("vm-3", "workspace", "/tmp/v3.sock", "/tmp/ws3", 512, 2, layer.Digest)
	if err != nil {
		t.Fatalf("PlanMount vm-3: %v", err)
	}
	if p3.DAXEnabled || p3.FallbackReason != "dax_pool_exhausted" || p3.CachePolicy != "auto" {
		t.Fatalf("expected dax_pool_exhausted fallback for vm-3, got %+v", p3)
	}

	// Releasing vm-1 frees 128 MiB so vm-4 acquires DAX cleanly.
	if !pool.ReleaseInstance("vm-1") {
		t.Fatalf("expected ReleaseInstance(vm-1) to return true")
	}
	p4, err := pool.PlanMount("vm-4", "workspace", "/tmp/v4.sock", "/tmp/ws4", 512, 2, layer.Digest)
	if err != nil || !p4.DAXEnabled {
		t.Fatalf("expected vm-4 to acquire reclaimed DAX window, got err=%v plan=%+v", err, p4)
	}
}

func TestPoolConcurrentRaceSafety(t *testing.T) {
	pool := NewPool(Config{
		Mode:               ModeAuto,
		DefaultWindowBytes: 64 * 1024 * 1024,
		MaxPoolBytes:       512 * 1024 * 1024,
		MaxCacheBytes:      1024 * 1024 * 1024,
	})
	base, _, err := pool.RegisterLayer("sha256:11112222", "python:3.12-alpine", "/var/lib/ovm/py.raw", 64*1024*1024)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			instID := fmt.Sprintf("race-vm-%d", id)
			for r := 0; r < 10; r++ {
				_, err := pool.PlanMount(instID, "workspace", "/tmp/s.sock", "/tmp/ws", 512, 2, base.Digest)
				if err != nil {
					t.Errorf("PlanMount(%s): %v", instID, err)
					return
				}
				_ = pool.Snapshot()
				pool.ReleaseInstance(instID)
			}
		}(i)
	}
	wg.Wait()

	m := pool.Metrics()
	if m.AllocatedDAXBytes != 0 || m.ActiveDAXMounts != 0 || m.PinnedLayers != 0 {
		t.Fatalf("expected zero leaked reservations after concurrent release, got %+v", m)
	}
}

func TestRunBenchmarkSharedDAXVsPrivateCache(t *testing.T) {
	rep, err := RunBenchmark(context.Background(), BenchmarkConfig{
		VMs:        4,
		LayerMB:    2,
		Iterations: 2,
	})
	if err != nil {
		t.Fatalf("RunBenchmark: %v", err)
	}
	if rep.MemoryReductionRatio != 5.0 {
		t.Fatalf("MemoryReductionRatio = %.2f, want 5.00", rep.MemoryReductionRatio)
	}
	if rep.PoolCacheHits != 4 {
		t.Fatalf("PoolCacheHits = %d, want 4", rep.PoolCacheHits)
	}
	if rep.SharedDAXPool.ThroughputMBs <= 0 || rep.PrivateGuestCache.ThroughputMBs <= 0 {
		t.Fatalf("expected positive benchmark throughput: %+v", rep)
	}
}
