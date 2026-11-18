// Package diskwarm implements sparse-extent-aware Linux page-cache pre-warming
// for microVM kernel images (vmlinux), ext4 root block disks, golden snapshot
// disks, and unpacked OCI rootfs trees ahead of cold boot.
//
// Sparse .ext4 block images routinely declare multi-gigabyte logical sizes
// while holding only tens of megabytes of allocated filesystem blocks. Warmer
// uses SEEK_DATA / SEEK_HOLE extent discovery to skip unallocated sparse holes
// in O(1) time and issues POSIX_FADV_WILLNEED (or chunked pread) exclusively
// against allocated extents within a bounded byte budget.
package diskwarm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Supported disk pre-warm modes.
const (
	ModeOff   = "off"
	ModeAuto  = "auto"
	ModeEager = "eager"
)

// Supported disk pre-warm strategies.
const (
	StrategyFadviseWillNeed = "fadvise-willneed"
	StrategyPreadExtents    = "pread-extents"
)

const (
	defaultMaxBytesPerFile uint64 = 64 * 1024 * 1024  // 64 MiB per artifact
	defaultBudgetBytes     uint64 = 256 * 1024 * 1024 // 256 MiB per warm pass
	defaultChunkBytes      int    = 256 * 1024        // 256 KiB pread buffer
	defaultCacheTTL               = 5 * time.Minute
	maxCacheEntries               = 256
)

var sinkChecksum uint64

// DataExtent represents a contiguous allocated byte range [Offset, Offset+Length) inside a file.
type DataExtent struct {
	Offset int64 `json:"offset"`
	Length int64 `json:"length"`
}

// Config configures a page-cache Warmer.
type Config struct {
	Mode            string
	Strategy        string
	MaxBytesPerFile uint64
	BudgetBytes     uint64
	CacheTTL        time.Duration
	ChunkBytes      int
}

// Metrics reports cumulative disk-cache pre-warming counters.
type Metrics struct {
	Mode                string `json:"mode"`
	Passes              uint64 `json:"passes"`
	FilesWarmed         uint64 `json:"files_warmed"`
	CacheHits           uint64 `json:"cache_hits"`
	WarmedBytes         uint64 `json:"warmed_bytes"`
	SparseSkippedBytes  uint64 `json:"sparse_skipped_bytes"`
	DurationMicrosTotal uint64 `json:"duration_micros_total"`
}

// WarmReport describes the result of warming a single artifact path (file or rootfs directory).
type WarmReport struct {
	Path               string `json:"path"`
	LogicalBytes       uint64 `json:"logical_bytes"`
	WarmedBytes        uint64 `json:"warmed_bytes"`
	SparseSkippedBytes uint64 `json:"sparse_skipped_bytes"`
	Extents            int    `json:"extents"`
	FilesWarmed        int    `json:"files_warmed"`
	CacheHit           bool   `json:"cache_hit"`
	UsedFadvise        bool   `json:"used_fadvise"`
	DurationMicros     int64  `json:"duration_micros"`
}

type cacheEntry struct {
	size      int64
	mtimeNano int64
	expiresAt time.Time
}

// Warmer coordinates deduplicated, budget-bounded page-cache pre-warming.
type Warmer struct {
	cfg Config

	mu    sync.Mutex
	cache map[string]cacheEntry

	passes              uint64
	filesWarmed         uint64
	cacheHits           uint64
	warmedBytes         uint64
	sparseSkippedBytes  uint64
	durationMicrosTotal uint64
}

// NormalizeMode canonicalizes a disk pre-warm mode string.
func NormalizeMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "":
		return ModeAuto
	case ModeOff:
		return ModeOff
	case ModeAuto:
		return ModeAuto
	case ModeEager:
		return ModeEager
	default:
		return strings.ToLower(strings.TrimSpace(mode))
	}
}

// IsValidMode reports whether mode is a valid disk pre-warm setting.
func IsValidMode(mode string) bool {
	if strings.TrimSpace(mode) == "" {
		return true
	}
	switch NormalizeMode(mode) {
	case ModeOff, ModeAuto, ModeEager:
		return true
	default:
		return false
	}
}

// NormalizeStrategy canonicalizes a disk pre-warm strategy name.
func NormalizeStrategy(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", StrategyFadviseWillNeed, "fadvise", "willneed":
		return StrategyFadviseWillNeed
	case StrategyPreadExtents, "pread", "read":
		return StrategyPreadExtents
	default:
		return strings.ToLower(strings.TrimSpace(s))
	}
}

// IsValidStrategy reports whether s names a supported disk pre-warm strategy.
func IsValidStrategy(s string) bool {
	switch NormalizeStrategy(s) {
	case StrategyFadviseWillNeed, StrategyPreadExtents:
		return true
	default:
		return false
	}
}

// NewWarmer creates a new page-cache Warmer with normalized defaults.
func NewWarmer(cfg Config) *Warmer {
	cfg.Mode = NormalizeMode(cfg.Mode)
	if !IsValidMode(cfg.Mode) {
		cfg.Mode = ModeAuto
	}
	cfg.Strategy = NormalizeStrategy(cfg.Strategy)
	if !IsValidStrategy(cfg.Strategy) {
		cfg.Strategy = StrategyFadviseWillNeed
	}
	if cfg.MaxBytesPerFile == 0 {
		cfg.MaxBytesPerFile = defaultMaxBytesPerFile
	}
	if cfg.BudgetBytes == 0 {
		cfg.BudgetBytes = defaultBudgetBytes
	}
	if cfg.MaxBytesPerFile > cfg.BudgetBytes {
		cfg.MaxBytesPerFile = cfg.BudgetBytes
	}
	if cfg.CacheTTL <= 0 {
		cfg.CacheTTL = defaultCacheTTL
	}
	if cfg.ChunkBytes <= 0 {
		cfg.ChunkBytes = defaultChunkBytes
	}
	return &Warmer{
		cfg:   cfg,
		cache: make(map[string]cacheEntry),
	}
}

// Mode returns the configured disk pre-warm mode.
func (w *Warmer) Mode() string {
	if w == nil {
		return ModeOff
	}
	return w.cfg.Mode
}

// Metrics returns atomic page-cache pre-warming telemetry.
func (w *Warmer) Metrics() Metrics {
	if w == nil {
		return Metrics{Mode: ModeOff}
	}
	return Metrics{
		Mode:                w.cfg.Mode,
		Passes:              atomic.LoadUint64(&w.passes),
		FilesWarmed:         atomic.LoadUint64(&w.filesWarmed),
		CacheHits:           atomic.LoadUint64(&w.cacheHits),
		WarmedBytes:         atomic.LoadUint64(&w.warmedBytes),
		SparseSkippedBytes:  atomic.LoadUint64(&w.sparseSkippedBytes),
		DurationMicrosTotal: atomic.LoadUint64(&w.durationMicrosTotal),
	}
}

// WarmBootArtifacts pre-warms one or more boot artifacts (such as vmlinux and rootfs.ext4)
// within Warmer's configured BudgetBytes. Missing paths are silently skipped.
func (w *Warmer) WarmBootArtifacts(ctx context.Context, paths ...string) []WarmReport {
	if w == nil || w.cfg.Mode == ModeOff || len(paths) == 0 {
		return nil
	}
	atomic.AddUint64(&w.passes, 1)

	remaining := w.cfg.BudgetBytes
	reports := make([]WarmReport, 0, len(paths))
	seen := make(map[string]bool, len(paths))

	for _, rawPath := range paths {
		if err := ctx.Err(); err != nil || remaining == 0 {
			break
		}
		trimmed := strings.TrimSpace(rawPath)
		if trimmed == "" {
			continue
		}
		clean := filepath.Clean(trimmed)
		if seen[clean] {
			continue
		}
		seen[clean] = true

		maxForTarget := w.cfg.MaxBytesPerFile
		if maxForTarget > remaining {
			maxForTarget = remaining
		}
		rep, err := w.warmTarget(ctx, clean, maxForTarget)
		if err != nil {
			continue
		}
		reports = append(reports, rep)
		if rep.WarmedBytes >= remaining {
			remaining = 0
		} else {
			remaining -= rep.WarmedBytes
		}
	}
	return reports
}

// WarmPath pre-warms a single file or unpacked rootfs directory and returns its WarmReport.
func (w *Warmer) WarmPath(ctx context.Context, path string) (WarmReport, error) {
	if w == nil {
		return WarmReport{}, errors.New("warmer is nil")
	}
	if w.cfg.Mode == ModeOff {
		return WarmReport{Path: path}, nil
	}
	atomic.AddUint64(&w.passes, 1)
	return w.warmTarget(ctx, filepath.Clean(path), w.cfg.MaxBytesPerFile)
}

func (w *Warmer) warmTarget(ctx context.Context, cleanPath string, budget uint64) (WarmReport, error) {
	info, err := os.Stat(cleanPath)
	if err != nil {
		return WarmReport{}, err
	}
	if info.IsDir() {
		return w.warmRootfsDir(ctx, cleanPath, budget)
	}
	if !info.Mode().IsRegular() {
		return WarmReport{Path: cleanPath}, nil
	}
	return w.warmRegularFile(ctx, cleanPath, info, budget)
}

func (w *Warmer) checkCache(cleanPath string, size int64, mtimeNano int64, now time.Time) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	ent, ok := w.cache[cleanPath]
	if !ok {
		return false
	}
	if ent.size != size || ent.mtimeNano != mtimeNano || now.After(ent.expiresAt) {
		delete(w.cache, cleanPath)
		return false
	}
	return true
}

func (w *Warmer) recordCache(cleanPath string, size int64, mtimeNano int64, now time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.cache) >= maxCacheEntries {
		// Evict expired entries first; if still full, drop one arbitrary entry.
		for k, v := range w.cache {
			if now.After(v.expiresAt) {
				delete(w.cache, k)
			}
		}
		if len(w.cache) >= maxCacheEntries {
			for k := range w.cache {
				delete(w.cache, k)
				break
			}
		}
	}
	w.cache[cleanPath] = cacheEntry{
		size:      size,
		mtimeNano: mtimeNano,
		expiresAt: now.Add(w.cfg.CacheTTL),
	}
}

func (w *Warmer) warmRegularFile(ctx context.Context, cleanPath string, info os.FileInfo, budget uint64) (WarmReport, error) {
	if err := ctx.Err(); err != nil {
		return WarmReport{}, err
	}
	size := info.Size()
	if size <= 0 || budget == 0 {
		return WarmReport{Path: cleanPath}, nil
	}
	now := time.Now()
	mtime := info.ModTime().UnixNano()
	if w.checkCache(cleanPath, size, mtime, now) {
		atomic.AddUint64(&w.cacheHits, 1)
		return WarmReport{
			Path:         cleanPath,
			LogicalBytes: uint64(size),
			CacheHit:     true,
		}, nil
	}

	start := time.Now()
	f, err := os.Open(cleanPath)
	if err != nil {
		return WarmReport{}, err
	}
	defer f.Close()

	extents, skipped, ok := platformScanExtents(f, size, budget)
	if !ok {
		clamp := uint64(size)
		if clamp > budget {
			clamp = budget
		}
		extents = []DataExtent{{Offset: 0, Length: int64(clamp)}}
		skipped = 0
	}

	var warmed uint64
	usedFadvise := false
	buf := make([]byte, w.cfg.ChunkBytes)

	for _, ext := range extents {
		if err := ctx.Err(); err != nil {
			return WarmReport{}, err
		}
		if ext.Length <= 0 {
			continue
		}
		if w.cfg.Strategy == StrategyFadviseWillNeed && platformAdviseWillNeed(f, ext.Offset, ext.Length) {
			usedFadvise = true
			warmed += uint64(ext.Length)
			continue
		}
		nRead, rErr := preadExtent(ctx, f, ext.Offset, ext.Length, buf)
		warmed += nRead
		if rErr != nil {
			return WarmReport{}, rErr
		}
	}

	durMicros := time.Since(start).Microseconds()
	if durMicros < 0 {
		durMicros = 0
	}

	w.recordCache(cleanPath, size, mtime, time.Now())
	atomic.AddUint64(&w.filesWarmed, 1)
	atomic.AddUint64(&w.warmedBytes, warmed)
	atomic.AddUint64(&w.sparseSkippedBytes, skipped)
	atomic.AddUint64(&w.durationMicrosTotal, uint64(durMicros))

	return WarmReport{
		Path:               cleanPath,
		LogicalBytes:       uint64(size),
		WarmedBytes:        warmed,
		SparseSkippedBytes: skipped,
		Extents:            len(extents),
		FilesWarmed:        1,
		UsedFadvise:        usedFadvise,
		DurationMicros:     durMicros,
	}, nil
}

var priorityRootfsSubdirs = []string{
	"ovm-harness",
	"usr/local/bin/ovm-harness",
	"sbin/init",
	"bin",
	"sbin",
	"lib",
	"usr/bin",
	"usr/lib",
	"etc",
}

func (w *Warmer) warmRootfsDir(ctx context.Context, rootDir string, budget uint64) (WarmReport, error) {
	start := time.Now()
	var totalLogical uint64
	var totalWarmed uint64
	var totalSkipped uint64
	var totalExtents int
	var filesCount int
	usedFadviseAny := false
	allCacheHits := true
	seenFiles := 0

	for _, sub := range priorityRootfsSubdirs {
		if totalWarmed >= budget {
			break
		}
		target := filepath.Join(rootDir, sub)
		info, err := os.Lstat(target)
		if err != nil {
			continue
		}
		if info.Mode().IsRegular() {
			seenFiles++
			rep, wErr := w.warmRegularFile(ctx, target, info, budget-totalWarmed)
			if wErr == nil {
				totalLogical += rep.LogicalBytes
				totalWarmed += rep.WarmedBytes
				totalSkipped += rep.SparseSkippedBytes
				totalExtents += rep.Extents
				filesCount += rep.FilesWarmed
				if rep.UsedFadvise {
					usedFadviseAny = true
				}
				if !rep.CacheHit {
					allCacheHits = false
				}
			}
			continue
		}
		if !info.IsDir() {
			continue
		}
		_ = filepath.WalkDir(target, func(p string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return nil
			}
			if ctx.Err() != nil || totalWarmed >= budget {
				return fs.SkipAll
			}
			if !d.Type().IsRegular() {
				return nil
			}
			fi, sErr := d.Info()
			if sErr != nil || fi.Size() <= 0 {
				return nil
			}
			seenFiles++
			rep, wErr := w.warmRegularFile(ctx, p, fi, budget-totalWarmed)
			if wErr == nil {
				totalLogical += rep.LogicalBytes
				totalWarmed += rep.WarmedBytes
				totalSkipped += rep.SparseSkippedBytes
				totalExtents += rep.Extents
				filesCount += rep.FilesWarmed
				if rep.UsedFadvise {
					usedFadviseAny = true
				}
				if !rep.CacheHit {
					allCacheHits = false
				}
			}
			return nil
		})
	}

	return WarmReport{
		Path:               rootDir,
		LogicalBytes:       totalLogical,
		WarmedBytes:        totalWarmed,
		SparseSkippedBytes: totalSkipped,
		Extents:            totalExtents,
		FilesWarmed:        filesCount,
		CacheHit:           seenFiles > 0 && allCacheHits,
		UsedFadvise:        usedFadviseAny,
		DurationMicros:     time.Since(start).Microseconds(),
	}, nil
}

func preadExtent(ctx context.Context, f *os.File, offset int64, length int64, buf []byte) (uint64, error) {
	var total uint64
	var acc uint64
	rem := length
	cur := offset
	step := 0

	for rem > 0 {
		if step&15 == 0 {
			if err := ctx.Err(); err != nil {
				return total, err
			}
		}
		step++
		toRead := len(buf)
		if int64(toRead) > rem {
			toRead = int(rem)
		}
		n, err := f.ReadAt(buf[:toRead], cur)
		if n > 0 {
			acc += uint64(buf[0]) + uint64(buf[n-1])
			total += uint64(n)
			cur += int64(n)
			rem -= int64(n)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return total, err
		}
	}
	if acc != 0 {
		atomic.AddUint64(&sinkChecksum, acc)
	}
	return total, nil
}

// BenchmarkConfig configures the sparse boot-disk pre-warming benchmark.
type BenchmarkConfig struct {
	LogicalMB  int    `json:"logical_mb"`
	DataMB     int    `json:"data_mb"`
	Iterations int    `json:"iterations"`
	Strategy   string `json:"strategy"`
}

func (c BenchmarkConfig) normalized() (BenchmarkConfig, error) {
	if c.LogicalMB <= 0 {
		c.LogicalMB = 64
	}
	if c.LogicalMB > 512 {
		return c, errors.New("logical_mb must be <= 512")
	}
	if c.DataMB <= 0 {
		c.DataMB = 8
	}
	if c.DataMB > c.LogicalMB {
		return c, fmt.Errorf("data_mb (%d) must be <= logical_mb (%d)", c.DataMB, c.LogicalMB)
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

// LatencySummary summarizes per-iteration pre-warm latencies in microseconds.
type LatencySummary struct {
	Samples    int     `json:"samples"`
	MinMicros  int64   `json:"min_micros"`
	P50Micros  int64   `json:"p50_micros"`
	P95Micros  int64   `json:"p95_micros"`
	P99Micros  int64   `json:"p99_micros"`
	MaxMicros  int64   `json:"max_micros"`
	MeanMicros float64 `json:"mean_micros"`
}

// BenchmarkResult compares naive dense full-disk read against sparse-extent-aware pre-warming.
type BenchmarkResult struct {
	Strategy               string         `json:"strategy"`
	LogicalMB              int            `json:"logical_mb"`
	DataMB                 int            `json:"data_mb"`
	Iterations             int            `json:"iterations"`
	ExtentsDetected        int            `json:"extents_detected"`
	WarmedBytesPerIter     uint64         `json:"warmed_bytes_per_iter"`
	SparseSkippedBytesIter uint64         `json:"sparse_skipped_bytes_per_iter"`
	HoleSkipRatio          float64        `json:"hole_skip_ratio"`
	UsedFadvise            bool           `json:"used_fadvise"`
	NaiveFullScan          LatencySummary `json:"naive_full_scan_latency"`
	ExtentPrewarm          LatencySummary `json:"extent_prewarm_latency"`
	SpeedupFactor          float64        `json:"speedup_factor"`
	EffectiveThroughputMBs float64        `json:"effective_throughput_mb_s"`
}

// RunBenchmark synthesizes a temporary sparse .ext4-shaped block image with 4 dispersed
// allocated extents (superblock, journal, system binaries, runtime libraries) separated
// by sparse holes, and compares naive full-image scanning against extent-aware pre-warming.
func RunBenchmark(ctx context.Context, cfg BenchmarkConfig) (BenchmarkResult, error) {
	norm, err := cfg.normalized()
	if err != nil {
		return BenchmarkResult{}, err
	}

	tmpDir, err := os.MkdirTemp("", "fame-work-diskwarm-bench-*")
	if err != nil {
		return BenchmarkResult{}, err
	}
	defer os.RemoveAll(tmpDir)

	imgPath := filepath.Join(tmpDir, "rootfs.ext4")
	f, err := os.OpenFile(imgPath, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0600)
	if err != nil {
		return BenchmarkResult{}, err
	}
	defer f.Close()

	logicalBytes := int64(norm.LogicalMB) * 1024 * 1024
	if err := f.Truncate(logicalBytes); err != nil {
		return BenchmarkResult{}, err
	}

	// Write 4 dispersed data extents totaling DataMB across the logical image.
	numZones := 4
	zoneBytes := (int64(norm.DataMB) * 1024 * 1024) / int64(numZones)
	if zoneBytes < 4096 {
		zoneBytes = 4096
	}
	stride := logicalBytes / int64(numZones)
	block := make([]byte, 64*1024)
	for i := range block {
		block[i] = byte((i*31 + 17) & 0xff)
	}
	for z := 0; z < numZones; z++ {
		baseOff := int64(z) * stride
		rem := zoneBytes
		off := baseOff
		for rem > 0 {
			n := int64(len(block))
			if n > rem {
				n = rem
			}
			if _, wErr := f.WriteAt(block[:n], off); wErr != nil {
				return BenchmarkResult{}, wErr
			}
			off += n
			rem -= n
		}
	}
	_ = f.Sync()

	naiveSamples := make([]int64, 0, norm.Iterations)
	extentSamples := make([]int64, 0, norm.Iterations)
	readBuf := make([]byte, defaultChunkBytes)

	var lastRep WarmReport
	for i := 0; i < norm.Iterations; i++ {
		if err := ctx.Err(); err != nil {
			return BenchmarkResult{}, err
		}

		platformDropPageCache(f, logicalBytes)
		t0 := time.Now()
		if _, rErr := preadExtent(ctx, f, 0, logicalBytes, readBuf); rErr != nil {
			return BenchmarkResult{}, rErr
		}
		naiveSamples = append(naiveSamples, time.Since(t0).Microseconds())

		platformDropPageCache(f, logicalBytes)
		warmer := NewWarmer(Config{
			Mode:            ModeAuto,
			Strategy:        norm.Strategy,
			MaxBytesPerFile: uint64(logicalBytes),
			BudgetBytes:     uint64(logicalBytes),
		})
		t1 := time.Now()
		rep, wErr := warmer.WarmPath(ctx, imgPath)
		if wErr != nil {
			return BenchmarkResult{}, wErr
		}
		extentSamples = append(extentSamples, time.Since(t1).Microseconds())
		lastRep = rep
	}

	naiveSum := summarizeMicros(naiveSamples)
	extSum := summarizeMicros(extentSamples)

	speedup := 1.0
	if extSum.MeanMicros > 0 && naiveSum.MeanMicros > 0 {
		speedup = math.Round((naiveSum.MeanMicros/extSum.MeanMicros)*100) / 100
	}
	var skipRatio float64
	if logicalBytes > 0 {
		skipRatio = math.Round((float64(lastRep.SparseSkippedBytes)/float64(logicalBytes))*10000) / 10000
	}
	var effThroughput float64
	if extSum.MeanMicros > 0 {
		secs := extSum.MeanMicros / 1e6
		effThroughput = math.Round((float64(norm.LogicalMB)/secs)*100) / 100
	}

	return BenchmarkResult{
		Strategy:               norm.Strategy,
		LogicalMB:              norm.LogicalMB,
		DataMB:                 norm.DataMB,
		Iterations:             norm.Iterations,
		ExtentsDetected:        lastRep.Extents,
		WarmedBytesPerIter:     lastRep.WarmedBytes,
		SparseSkippedBytesIter: lastRep.SparseSkippedBytes,
		HoleSkipRatio:          skipRatio,
		UsedFadvise:            lastRep.UsedFadvise,
		NaiveFullScan:          naiveSum,
		ExtentPrewarm:          extSum,
		SpeedupFactor:          speedup,
		EffectiveThroughputMBs: effThroughput,
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
