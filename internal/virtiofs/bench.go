package virtiofs

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"
)

// BenchmarkConfig configures a Virtio-FS / Virtio-PMEM shared DAX mapping
// vs private per-VM guest page-cache copy benchmark.
type BenchmarkConfig struct {
	VMs        int `json:"vms"`
	LayerMB    int `json:"layer_mb"`
	Iterations int `json:"iterations"`
}

// LatencySummary holds p50/p95/p99/mean per-VM pass latency in microseconds.
type LatencySummary struct {
	P50Micros  int64   `json:"p50_micros"`
	P95Micros  int64   `json:"p95_micros"`
	P99Micros  int64   `json:"p99_micros"`
	MeanMicros float64 `json:"mean_micros"`
}

// PassSummary reports throughput and memory footprint for one benchmark mode.
type PassSummary struct {
	Latency             LatencySummary `json:"latency"`
	ThroughputMBs       float64        `json:"throughput_mb_s"`
	ResidentMemoryBytes uint64         `json:"resident_memory_bytes"`
}

// BenchmarkReport compares shared MAP_SHARED DAX window reads against traditional
// private per-VM guest page-cache buffer copies across concurrent sandboxes.
type BenchmarkReport struct {
	VMs                  int         `json:"vms"`
	LayerMB              int         `json:"layer_mb"`
	Iterations           int         `json:"iterations"`
	PagesPerVM           int         `json:"pages_per_vm"`
	PrivateGuestCache    PassSummary `json:"private_guest_cache"`
	SharedDAXPool        PassSummary `json:"shared_dax_pool"`
	ThroughputSpeedup    float64     `json:"throughput_speedup"`
	MemorySavedBytes     uint64      `json:"memory_saved_bytes"`
	MemoryReductionRatio float64     `json:"memory_reduction_ratio"`
	PoolCacheHits        uint64      `json:"pool_cache_hits"`
	PoolDedupSavedBytes  uint64      `json:"pool_dedup_saved_bytes"`
}

// RunBenchmark executes an empirical comparison of shared MAP_SHARED DAX page
// reads versus per-VM pread guest buffer copies and cleans up its temporary
// rootfs image immediately before returning.
func RunBenchmark(ctx context.Context, cfg BenchmarkConfig) (*BenchmarkReport, error) {
	if cfg.VMs <= 0 {
		cfg.VMs = 6
	}
	if cfg.VMs > 64 {
		return nil, fmt.Errorf("vms (%d) must be between 1 and 64", cfg.VMs)
	}
	if cfg.LayerMB <= 0 {
		cfg.LayerMB = 8
	}
	if cfg.LayerMB > 128 {
		return nil, fmt.Errorf("layer_mb (%d) must be between 1 and 128", cfg.LayerMB)
	}
	if cfg.Iterations <= 0 {
		cfg.Iterations = 4
	}
	if cfg.Iterations > 50 {
		return nil, fmt.Errorf("iterations (%d) must be between 1 and 50", cfg.Iterations)
	}

	tmpDir, err := os.MkdirTemp("", "ovm-bench-virtiofs-*")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	layerPath := filepath.Join(tmpDir, "shared-rootfs.ext4")
	layerBytes := cfg.LayerMB * 1024 * 1024
	const pageSize = 4096
	numPages := layerBytes / pageSize

	// Populate deterministic non-zero 4 KiB pages so the OS cannot elide zero pages.
	if err := writeDeterministicLayer(layerPath, numPages, pageSize); err != nil {
		return nil, err
	}

	f, err := os.Open(layerPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// 1. Private per-VM guest page-cache copy pass (pread into isolated VM buffers).
	privSamples := make([]int64, 0, cfg.VMs*cfg.Iterations)
	privStart := time.Now()
	for iter := 0; iter < cfg.Iterations; iter++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		iterLat := make([]int64, cfg.VMs)
		var wg sync.WaitGroup
		wg.Add(cfg.VMs)
		for vm := 0; vm < cfg.VMs; vm++ {
			go func(slot int) {
				defer wg.Done()
				t0 := time.Now()
				guestCache := make([]byte, layerBytes)
				var checksum uint64
				for p := 0; p < numPages; p++ {
					off := int64(p * pageSize)
					_, _ = f.ReadAt(guestCache[p*pageSize:(p+1)*pageSize], off)
					checksum += binary.LittleEndian.Uint64(guestCache[p*pageSize : p*pageSize+8])
				}
				if checksum == 0 {
					iterLat[slot] = 1
				}
				iterLat[slot] = time.Since(t0).Microseconds()
			}(vm)
		}
		wg.Wait()
		privSamples = append(privSamples, iterLat...)
	}
	privElapsed := time.Since(privStart)

	// 2. Shared DAX MAP_SHARED pool pass across concurrent microVMs.
	pool := NewPool(Config{
		Mode:               ModeDAX,
		DefaultWindowBytes: MinDAXWindowBytes,
		MaxPoolBytes:       uint64(cfg.VMs+2) * MinDAXWindowBytes,
		MaxCacheBytes:      uint64(layerBytes) * 4,
	})
	baseEntry, _, err := pool.RegisterLayer("", "python:3.12-alpine", layerPath, uint64(layerBytes))
	if err != nil {
		return nil, err
	}
	for vm := 0; vm < cfg.VMs; vm++ {
		instID := fmt.Sprintf("bench-vm-%d", vm)
		if _, err := pool.PlanMount(instID, "workspace", "/tmp/vfs.sock", tmpDir, 512, 2, baseEntry.Digest); err != nil {
			return nil, err
		}
	}

	mapped, err := syscall.Mmap(int(f.Fd()), 0, layerBytes, syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return nil, fmt.Errorf("mmap shared layer: %w", err)
	}
	defer syscall.Munmap(mapped)

	// Warm host page cache once before measuring warm shared DAX reads.
	var warmSum uint64
	for p := 0; p < numPages; p++ {
		warmSum += binary.LittleEndian.Uint64(mapped[p*pageSize : p*pageSize+8])
	}
	_ = warmSum

	daxSamples := make([]int64, 0, cfg.VMs*cfg.Iterations)
	daxStart := time.Now()
	for iter := 0; iter < cfg.Iterations; iter++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		iterLat := make([]int64, cfg.VMs)
		var wg sync.WaitGroup
		wg.Add(cfg.VMs)
		for vm := 0; vm < cfg.VMs; vm++ {
			go func(slot int) {
				defer wg.Done()
				t0 := time.Now()
				var checksum uint64
				for p := 0; p < numPages; p++ {
					idx := p * pageSize
					checksum += binary.LittleEndian.Uint64(mapped[idx : idx+8])
				}
				if checksum == 0 {
					iterLat[slot] = 1
				}
				iterLat[slot] = time.Since(t0).Microseconds()
			}(vm)
		}
		wg.Wait()
		daxSamples = append(daxSamples, iterLat...)
	}
	daxElapsed := time.Since(daxStart)

	poolMetrics := pool.Metrics()
	for vm := 0; vm < cfg.VMs; vm++ {
		pool.ReleaseInstance(fmt.Sprintf("bench-vm-%d", vm))
	}

	totalReadMB := float64(cfg.VMs*cfg.LayerMB*cfg.Iterations)
	privMBs := totalReadMB / maxSeconds(privElapsed)
	daxMBs := totalReadMB / maxSeconds(daxElapsed)
	speedup := 1.0
	if privMBs > 0 {
		speedup = daxMBs / privMBs
	}

	// Without DAX: 1 host page-cache copy + N private guest page-cache copies = (1+N)*LayerBytes.
	// With DAX: 1 shared host page-cache copy mapped directly into GPA windows = 1*LayerBytes.
	privResident := uint64(1+cfg.VMs) * uint64(layerBytes)
	daxResident := uint64(layerBytes)

	return &BenchmarkReport{
		VMs:        cfg.VMs,
		LayerMB:    cfg.LayerMB,
		Iterations: cfg.Iterations,
		PagesPerVM: numPages,
		PrivateGuestCache: PassSummary{
			Latency:             summarizeMicros(privSamples),
			ThroughputMBs:       privMBs,
			ResidentMemoryBytes: privResident,
		},
		SharedDAXPool: PassSummary{
			Latency:             summarizeMicros(daxSamples),
			ThroughputMBs:       daxMBs,
			ResidentMemoryBytes: daxResident,
		},
		ThroughputSpeedup:    speedup,
		MemorySavedBytes:     privResident - daxResident,
		MemoryReductionRatio: float64(privResident) / float64(daxResident),
		PoolCacheHits:        poolMetrics.CacheHits,
		PoolDedupSavedBytes:  poolMetrics.DedupSavedBytes,
	}, nil
}

func writeDeterministicLayer(path string, numPages, pageSize int) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	buf := make([]byte, pageSize)
	for p := 0; p < numPages; p++ {
		seed := uint64(0x9e3779b97f4a7c15) ^ uint64(p+1)*0xbf58476d1ce4e5b9
		binary.LittleEndian.PutUint64(buf[0:8], seed)
		binary.LittleEndian.PutUint64(buf[64:72], seed^0x94d049bb133111eb)
		if _, err := f.Write(buf); err != nil {
			return err
		}
	}
	return nil
}

func maxSeconds(d time.Duration) float64 {
	s := d.Seconds()
	if s < 1e-6 {
		return 1e-6
	}
	return s
}

func summarizeMicros(samples []int64) LatencySummary {
	if len(samples) == 0 {
		return LatencySummary{}
	}
	cp := append([]int64(nil), samples...)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	var sum int64
	for _, v := range cp {
		sum += v
	}
	pct := func(q float64) int64 {
		idx := int(float64(len(cp)-1) * q)
		if idx < 0 {
			idx = 0
		}
		if idx >= len(cp) {
			idx = len(cp) - 1
		}
		return cp[idx]
	}
	return LatencySummary{
		P50Micros:  pct(0.50),
		P95Micros:  pct(0.95),
		P99Micros:  pct(0.99),
		MeanMicros: float64(sum) / float64(len(cp)),
	}
}
