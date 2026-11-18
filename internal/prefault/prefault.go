// Package prefault implements parallel page-table pre-faulting for warm microVM
// memory pools and Copy-on-Write disk snapshots, plus an automated benchmark
// comparing demand-paged cold first-touch latency against pre-faulted warm
// pool access latency and kernel minor page fault counts.
package prefault

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Supported pre-faulting strategies.
const (
	StrategyMadvisePopulate = "madvise-populate"
	StrategyTouchRead       = "touch-read"
	StrategyTouchWrite      = "touch-write"
)

var sinkByte uint64

// NormalizeStrategy canonicalizes a pre-fault strategy name.
func NormalizeStrategy(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", StrategyMadvisePopulate, "populate", "madvise":
		return StrategyMadvisePopulate
	case StrategyTouchRead, "read":
		return StrategyTouchRead
	case StrategyTouchWrite, "write":
		return StrategyTouchWrite
	default:
		return strings.ToLower(strings.TrimSpace(s))
	}
}

// IsValidStrategy reports whether s names a supported pre-faulting strategy.
func IsValidStrategy(s string) bool {
	switch NormalizeStrategy(s) {
	case StrategyMadvisePopulate, StrategyTouchRead, StrategyTouchWrite:
		return true
	default:
		return false
	}
}

// PrefaultStats describes a single region or file pre-faulting pass.
type PrefaultStats struct {
	Strategy      string  `json:"strategy"`
	Bytes         uint64  `json:"bytes"`
	PagesTouched  uint64  `json:"pages_touched"`
	Workers       int     `json:"workers"`
	DurationMicro int64   `json:"duration_micros"`
	ThroughputMBs float64 `json:"throughput_mb_s"`
	UsedMadvise   bool    `json:"used_madvise"`
}

// Region pre-faults buf at os.Getpagesize() stride across workers goroutines.
func Region(ctx context.Context, buf []byte, strategy string, workers int) (PrefaultStats, error) {
	norm := NormalizeStrategy(strategy)
	if !IsValidStrategy(norm) {
		return PrefaultStats{}, fmt.Errorf("prefault: unsupported strategy %q", strategy)
	}
	if len(buf) == 0 {
		return PrefaultStats{Strategy: norm, Workers: 1}, nil
	}
	if err := ctx.Err(); err != nil {
		return PrefaultStats{}, err
	}

	pageSize := os.Getpagesize()
	if pageSize <= 0 {
		pageSize = 4096
	}
	totalPages := (len(buf) + pageSize - 1) / pageSize
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0)
	}
	if workers > totalPages {
		workers = totalPages
	}
	if workers < 1 {
		workers = 1
	}

	start := time.Now()
	usedMadvise := false

	if norm == StrategyMadvisePopulate {
		if ok := platformPopulate(buf, true); ok {
			usedMadvise = true
		} else {
			// Fallback to parallel write-touch when MADV_POPULATE_WRITE is unsupported.
			if err := parallelStrideTouch(ctx, buf, pageSize, workers, true); err != nil {
				return PrefaultStats{}, err
			}
		}
	} else {
		writable := norm == StrategyTouchWrite
		if err := parallelStrideTouch(ctx, buf, pageSize, workers, writable); err != nil {
			return PrefaultStats{}, err
		}
	}

	dur := time.Since(start)
	secs := dur.Seconds()
	var mbps float64
	if secs > 0 {
		mbps = (float64(len(buf)) / (1024.0 * 1024.0)) / secs
	}
	return PrefaultStats{
		Strategy:      norm,
		Bytes:         uint64(len(buf)),
		PagesTouched:  uint64(totalPages),
		Workers:       workers,
		DurationMicro: dur.Microseconds(),
		ThroughputMBs: math.Round(mbps*100) / 100,
		UsedMadvise:   usedMadvise,
	}, nil
}

func parallelStrideTouch(ctx context.Context, buf []byte, pageSize int, workers int, writable bool) error {
	totalPages := (len(buf) + pageSize - 1) / pageSize
	chunkPages := (totalPages + workers - 1) / workers

	var wg sync.WaitGroup
	var firstErr atomic.Value

	for w := 0; w < workers; w++ {
		startPage := w * chunkPages
		if startPage >= totalPages {
			break
		}
		endPage := startPage + chunkPages
		if endPage > totalPages {
			endPage = totalPages
		}

		wg.Add(1)
		go func(sp, ep int) {
			defer wg.Done()
			var localAcc uint64
			for p := sp; p < ep; p++ {
				if (p-sp)&255 == 0 {
					if err := ctx.Err(); err != nil {
						firstErr.CompareAndSwap(nil, err)
						return
					}
				}
				offset := p * pageSize
				b := buf[offset]
				if writable {
					buf[offset] = b ^ 1
					buf[offset] = b
				} else {
					localAcc += uint64(b)
				}
			}
			if localAcc != 0 {
				atomic.AddUint64(&sinkByte, localAcc)
			}
		}(startPage, endPage)
	}
	wg.Wait()

	if v := firstErr.Load(); v != nil {
		return v.(error)
	}
	return nil
}

// WalkPages performs a single-threaded strided read+write touch across every
// page in buf to measure workload wake latency against either unfaulted or
// pre-faulted guest memory.
func WalkPages(buf []byte, pageSize int, writeTouch bool) uint64 {
	if pageSize <= 0 {
		pageSize = 4096
	}
	var acc uint64
	for off := 0; off < len(buf); off += pageSize {
		v := buf[off]
		if writeTouch {
			buf[off] = v ^ 1
		}
		acc += uint64(v)
	}
	atomic.AddUint64(&sinkByte, acc)
	return uint64((len(buf) + pageSize - 1) / pageSize)
}

// BenchmarkConfig parameterizes the automated warm-pool pre-faulting benchmark.
type BenchmarkConfig struct {
	PoolSize     int    `json:"pool_size"`
	SlotMemoryMB int    `json:"slot_memory_mb"`
	Workers      int    `json:"workers"`
	Iterations   int    `json:"iterations"`
	Strategy     string `json:"strategy"`
}

func (c BenchmarkConfig) normalized() (BenchmarkConfig, error) {
	if c.PoolSize <= 0 {
		c.PoolSize = 4
	}
	if c.PoolSize > 64 {
		return c, errors.New("pool_size must be <= 64")
	}
	if c.SlotMemoryMB <= 0 {
		c.SlotMemoryMB = 16
	}
	if c.SlotMemoryMB > 512 {
		return c, errors.New("slot_memory_mb must be <= 512")
	}
	if c.Workers <= 0 {
		c.Workers = runtime.GOMAXPROCS(0)
		if c.Workers > 8 {
			c.Workers = 8
		}
		if c.Workers < 1 {
			c.Workers = 1
		}
	}
	if c.Iterations <= 0 {
		c.Iterations = 5
	}
	if c.Iterations > 100 {
		return c, errors.New("iterations must be <= 100")
	}
	c.Strategy = NormalizeStrategy(c.Strategy)
	if !IsValidStrategy(c.Strategy) {
		return c, fmt.Errorf("unsupported strategy %q", c.Strategy)
	}
	return c, nil
}

// LatencySummary summarizes per-slot wake access latencies in microseconds.
type LatencySummary struct {
	Samples    int     `json:"samples"`
	MinMicros  int64   `json:"min_micros"`
	P50Micros  int64   `json:"p50_micros"`
	P95Micros  int64   `json:"p95_micros"`
	P99Micros  int64   `json:"p99_micros"`
	MaxMicros  int64   `json:"max_micros"`
	MeanMicros float64 `json:"mean_micros"`
}

// BenchmarkResult captures cold demand-paged vs warm pre-faulted pool metrics.
type BenchmarkResult struct {
	Strategy               string         `json:"strategy"`
	PoolSize               int            `json:"pool_size"`
	SlotMemoryMB           int            `json:"slot_memory_mb"`
	Workers                int            `json:"workers"`
	Iterations             int            `json:"iterations"`
	PageSizeBytes          int            `json:"page_size_bytes"`
	TotalPagesPerIteration uint64         `json:"total_pages_per_iteration"`
	DemandPaged            LatencySummary `json:"demand_paged_latency"`
	WarmPrefaulted         LatencySummary `json:"warm_prefaulted_latency"`
	PrefaultPassMeanMicros float64        `json:"prefault_pass_mean_micros"`
	PrefaultThroughputMBs  float64        `json:"prefault_throughput_mb_s"`
	DemandMinorFaults      uint64         `json:"demand_minor_faults"`
	WarmMinorFaults        uint64         `json:"warm_minor_faults"`
	SpeedupFactor          float64        `json:"speedup_factor"`
	UsedMadvise            bool           `json:"used_madvise"`
}

// RunBenchmark executes Iterations rounds comparing first-touch demand-paged
// access across PoolSize anonymous VM memory slots against pre-faulted warm
// pool slots, releasing all anonymous mappings immediately after each phase.
func RunBenchmark(ctx context.Context, cfg BenchmarkConfig) (BenchmarkResult, error) {
	norm, err := cfg.normalized()
	if err != nil {
		return BenchmarkResult{}, err
	}

	pageSize := os.Getpagesize()
	if pageSize <= 0 {
		pageSize = 4096
	}
	slotBytes := norm.SlotMemoryMB * 1024 * 1024
	totalSamples := norm.Iterations * norm.PoolSize

	demandSamples := make([]int64, 0, totalSamples)
	warmSamples := make([]int64, 0, totalSamples)
	var totalDemandMinflt uint64
	var totalWarmMinflt uint64
	var prefaultMicrosSum int64
	var prefaultBytesSum uint64
	usedMadviseAny := false

	for iter := 0; iter < norm.Iterations; iter++ {
		if err := ctx.Err(); err != nil {
			return BenchmarkResult{}, err
		}

		// Phase A: Demand-paged cold slots (fresh unmapped anonymous pages).
		coldSlots, releaseCold, err := allocAnonPool(norm.PoolSize, slotBytes)
		if err != nil {
			return BenchmarkResult{}, err
		}
		fltBeforeCold := readMinorFaults()
		for _, slot := range coldSlots {
			t0 := time.Now()
			WalkPages(slot, pageSize, true)
			demandSamples = append(demandSamples, time.Since(t0).Microseconds())
		}
		fltAfterCold := readMinorFaults()
		if fltAfterCold >= fltBeforeCold {
			totalDemandMinflt += fltAfterCold - fltBeforeCold
		}
		releaseCold()

		// Phase B: Warm-pool slots pre-faulted ahead of workload wake.
		warmSlots, releaseWarm, err := allocAnonPool(norm.PoolSize, slotBytes)
		if err != nil {
			return BenchmarkResult{}, err
		}
		for _, slot := range warmSlots {
			st, pErr := Region(ctx, slot, norm.Strategy, norm.Workers)
			if pErr != nil {
				releaseWarm()
				return BenchmarkResult{}, pErr
			}
			prefaultMicrosSum += st.DurationMicro
			prefaultBytesSum += st.Bytes
			if st.UsedMadvise {
				usedMadviseAny = true
			}
		}

		fltBeforeWarm := readMinorFaults()
		for _, slot := range warmSlots {
			t0 := time.Now()
			WalkPages(slot, pageSize, true)
			warmSamples = append(warmSamples, time.Since(t0).Microseconds())
		}
		fltAfterWarm := readMinorFaults()
		if fltAfterWarm >= fltBeforeWarm {
			totalWarmMinflt += fltAfterWarm - fltBeforeWarm
		}
		releaseWarm()
	}

	demandSummary := summarizeMicros(demandSamples)
	warmSummary := summarizeMicros(warmSamples)

	speedup := 1.0
	if warmSummary.MeanMicros > 0 && demandSummary.MeanMicros > 0 {
		speedup = math.Round((demandSummary.MeanMicros/warmSummary.MeanMicros)*100) / 100
	}

	var prefaultMean float64
	if totalSamples > 0 {
		prefaultMean = math.Round((float64(prefaultMicrosSum)/float64(totalSamples))*100) / 100
	}
	var throughputMBs float64
	if prefaultMicrosSum > 0 {
		secs := float64(prefaultMicrosSum) / 1e6
		throughputMBs = math.Round(((float64(prefaultBytesSum)/(1024*1024))/secs)*100) / 100
	}

	pagesPerSlot := uint64((slotBytes + pageSize - 1) / pageSize)
	return BenchmarkResult{
		Strategy:               norm.Strategy,
		PoolSize:               norm.PoolSize,
		SlotMemoryMB:           norm.SlotMemoryMB,
		Workers:                norm.Workers,
		Iterations:             norm.Iterations,
		PageSizeBytes:          pageSize,
		TotalPagesPerIteration: pagesPerSlot * uint64(norm.PoolSize),
		DemandPaged:            demandSummary,
		WarmPrefaulted:         warmSummary,
		PrefaultPassMeanMicros: prefaultMean,
		PrefaultThroughputMBs:  throughputMBs,
		DemandMinorFaults:      totalDemandMinflt,
		WarmMinorFaults:        totalWarmMinflt,
		SpeedupFactor:          speedup,
		UsedMadvise:            usedMadviseAny,
	}, nil
}

func summarizeMicros(samples []int64) LatencySummary {
	if len(samples) == 0 {
		return LatencySummary{}
	}
	sorted := make([]int64, len(samples))
	copy(sorted, samples)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	var sum int64
	for _, v := range sorted {
		sum += v
	}
	pct := func(q float64) int64 {
		idx := int(math.Ceil(q*float64(len(sorted)))) - 1
		if idx < 0 {
			idx = 0
		}
		if idx >= len(sorted) {
			idx = len(sorted) - 1
		}
		return sorted[idx]
	}

	return LatencySummary{
		Samples:    len(sorted),
		MinMicros:  sorted[0],
		P50Micros:  pct(0.50),
		P95Micros:  pct(0.95),
		P99Micros:  pct(0.99),
		MaxMicros:  sorted[len(sorted)-1],
		MeanMicros: math.Round((float64(sum)/float64(len(sorted)))*100) / 100,
	}
}
