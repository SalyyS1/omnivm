// Package hibernate implements memory snapshot-to-disk sparse zero-page
// compaction with SHA-256 integrity verification, post-restore guest wall-clock
// resynchronization, and mandatory virtio-rng / VM-generation-ID cryptographic
// entropy reseeding.
package hibernate

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	ModeOff        = "off"
	ModeIdle       = "idle"
	ModeAggressive = "aggressive"

	PageSize      = 4096
	EntropyBytes  = 64
	StepThreshold = 500 * time.Millisecond
)

var imageMagic = [8]byte{'O', 'V', 'M', 'H', 'I', 'B', '0', '1'}

// Config configures the memory hibernation engine.
type Config struct {
	Mode string `json:"mode"`
}

// CompactionStats summarizes one memory snapshot-to-disk sparse compaction pass.
type CompactionStats struct {
	RawBytes         uint64  `json:"raw_bytes"`
	StoredBytes      uint64  `json:"stored_bytes"`
	SavedBytes       uint64  `json:"saved_bytes"`
	TotalPages       uint32  `json:"total_pages"`
	ZeroPages        uint32  `json:"zero_pages"`
	DirtyPages       uint32  `json:"dirty_pages"`
	CompressionRatio float64 `json:"compression_ratio"`
	RawSHA256        string  `json:"raw_sha256"`
}

// RestorePlan captures the mandatory guest clock-sync and cryptographic RNG
// reseeding frames dispatched to ovm-harness before traffic resumes after hibernation.
type RestorePlan struct {
	InstanceID           string          `json:"instance_id"`
	FrozenAt             time.Time       `json:"frozen_at"`
	ResumedAt            time.Time       `json:"resumed_at"`
	HibernatedDurationMs int64           `json:"hibernated_duration_ms"`
	ClockAdjustmentMode  string          `json:"clock_adjustment_mode"` // noop | slew | step
	EntropySeedBytes     int             `json:"entropy_seed_bytes"`
	EntropySeedSHA256    string          `json:"entropy_seed_sha256"`
	VMGenID              string          `json:"vm_gen_id"`
	ClockSyncRPC         json.RawMessage `json:"clock_sync_rpc"`
	RNGReseedRPC         json.RawMessage `json:"rng_reseed_rpc"`
}

// BenchmarkConfig parameterizes a synthetic memory hibernation compaction & restore benchmark.
type BenchmarkConfig struct {
	MemoryMB      int     `json:"memory_mb"`
	ZeroPageRatio float64 `json:"zero_page_ratio"`
}

// BenchmarkReport reports measured compaction, SHA-256 restore throughput, and space savings.
type BenchmarkReport struct {
	MemoryMB         int     `json:"memory_mb"`
	ZeroPageRatio    float64 `json:"zero_page_ratio"`
	RawBytes         uint64  `json:"raw_bytes"`
	StoredBytes      uint64  `json:"stored_bytes"`
	SavedBytes       uint64  `json:"saved_bytes"`
	ZeroPages        uint32  `json:"zero_pages"`
	DirtyPages       uint32  `json:"dirty_pages"`
	CompressionRatio float64 `json:"compression_ratio"`
	CompactDurationMs float64 `json:"compact_duration_ms"`
	RestoreDurationMs float64 `json:"restore_duration_ms"`
	CompactMBps      float64 `json:"compact_mbps"`
	RestoreMBps      float64 `json:"restore_mbps"`
	IntegrityVerified bool   `json:"integrity_verified"`
}

// Metrics exposes atomic counters for memory hibernation, compaction, and restore safety.
type Metrics struct {
	Mode               string `json:"mode"`
	ImagesCompacted    uint64 `json:"images_compacted"`
	ImagesRestored     uint64 `json:"images_restored"`
	ZeroPagesElided    uint64 `json:"zero_pages_elided"`
	RawBytesProcessed  uint64 `json:"raw_bytes_processed"`
	StoredBytesWritten uint64 `json:"stored_bytes_written"`
	SavedBytes         uint64 `json:"saved_bytes"`
	ClockSyncs         uint64 `json:"clock_syncs"`
	EntropyReseeds     uint64 `json:"entropy_reseeds"`
}

// StatusSnapshot reports current hibernation engine mode and telemetry.
type StatusSnapshot struct {
	Mode          string  `json:"mode"`
	PageSizeBytes int     `json:"page_size_bytes"`
	EntropyBytes  int     `json:"entropy_bytes"`
	Metrics       Metrics `json:"metrics"`
}

// Engine coordinates guest memory snapshot compaction and post-restore safety protocols.
type Engine struct {
	mu   sync.Mutex
	mode string

	imagesCompacted    uint64
	imagesRestored     uint64
	zeroPagesElided    uint64
	rawBytesProcessed  uint64
	storedBytesWritten uint64
	savedBytes         uint64
	clockSyncs         uint64
	entropyReseeds     uint64
}

// NormalizeMode validates and normalizes a hibernation mode string.
func NormalizeMode(raw string) (string, error) {
	m := strings.ToLower(strings.TrimSpace(raw))
	switch m {
	case "", ModeOff:
		return ModeOff, nil
	case ModeIdle:
		return ModeIdle, nil
	case ModeAggressive:
		return ModeAggressive, nil
	default:
		return "", fmt.Errorf("unsupported hibernate mode %q: expected off, idle, or aggressive", raw)
	}
}

// NewEngine creates a memory hibernation engine.
func NewEngine(cfg Config) *Engine {
	mode, err := NormalizeMode(cfg.Mode)
	if err != nil {
		mode = ModeOff
	}
	return &Engine{mode: mode}
}

// Mode returns the current operating mode.
func (e *Engine) Mode() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.mode
}

// SetMode dynamically updates the hibernation mode.
func (e *Engine) SetMode(raw string) error {
	norm, err := NormalizeMode(raw)
	if err != nil {
		return err
	}
	e.mu.Lock()
	e.mode = norm
	e.mu.Unlock()
	return nil
}

func isZeroPage(frame []byte) bool {
	i := 0
	for ; i+8 <= len(frame); i += 8 {
		if binary.LittleEndian.Uint64(frame[i:i+8]) != 0 {
			return false
		}
	}
	for ; i < len(frame); i++ {
		if frame[i] != 0 {
			return false
		}
	}
	return true
}

// CompactMemoryImage scans 4 KiB guest RAM frames, eliding zero pages into a compact
// integrity-checked OVMHIB01 binary stream and returning the compacted bytes plus stats.
func (e *Engine) CompactMemoryImage(raw []byte) ([]byte, CompactionStats, error) {
	rawSum := sha256.Sum256(raw)
	fullPages := len(raw) / PageSize
	tailLen := len(raw) % PageSize
	totalPages := uint32(fullPages)
	if tailLen > 0 {
		totalPages++
	}

	dirtyIndices := make([]uint32, 0, totalPages/2+1)
	var zeroCount uint32
	for idx := uint32(0); idx < totalPages; idx++ {
		start := int(idx) * PageSize
		end := start + PageSize
		if end > len(raw) {
			end = len(raw)
		}
		if isZeroPage(raw[start:end]) {
			zeroCount++
		} else {
			dirtyIndices = append(dirtyIndices, idx)
		}
	}

	dirtyCount := uint32(len(dirtyIndices))
	// Header: 8 magic + 4 pageSize + 4 totalPages + 4 tailLen + 4 dirtyCount + 32 rawSHA256 = 56 bytes
	const headerLen = 56
	estCap := headerLen + int(dirtyCount)*(4+PageSize)
	out := bytes.NewBuffer(make([]byte, 0, estCap))
	out.Write(imageMagic[:])

	var hdr [16]byte
	binary.LittleEndian.PutUint32(hdr[0:4], PageSize)
	binary.LittleEndian.PutUint32(hdr[4:8], totalPages)
	binary.LittleEndian.PutUint32(hdr[8:12], uint32(tailLen))
	binary.LittleEndian.PutUint32(hdr[12:16], dirtyCount)
	out.Write(hdr[:])
	out.Write(rawSum[:])

	var idxBuf [4]byte
	for _, idx := range dirtyIndices {
		binary.LittleEndian.PutUint32(idxBuf[:], idx)
		out.Write(idxBuf[:])
		start := int(idx) * PageSize
		end := start + PageSize
		if end > len(raw) {
			end = len(raw)
		}
		out.Write(raw[start:end])
	}

	compacted := out.Bytes()
	rawBytes := uint64(len(raw))
	storedBytes := uint64(len(compacted))
	var saved uint64
	if rawBytes > storedBytes {
		saved = rawBytes - storedBytes
	}
	ratio := 1.0
	if storedBytes > 0 && rawBytes > 0 {
		ratio = float64(rawBytes) / float64(storedBytes)
	}

	if e != nil {
		atomic.AddUint64(&e.imagesCompacted, 1)
		atomic.AddUint64(&e.zeroPagesElided, uint64(zeroCount))
		atomic.AddUint64(&e.rawBytesProcessed, rawBytes)
		atomic.AddUint64(&e.storedBytesWritten, storedBytes)
		atomic.AddUint64(&e.savedBytes, saved)
	}

	return compacted, CompactionStats{
		RawBytes:         rawBytes,
		StoredBytes:      storedBytes,
		SavedBytes:       saved,
		TotalPages:       totalPages,
		ZeroPages:        zeroCount,
		DirtyPages:       dirtyCount,
		CompressionRatio: ratio,
		RawSHA256:        hex.EncodeToString(rawSum[:]),
	}, nil
}

// RestoreMemoryImage reconstructs the full guest RAM image from an OVMHIB01 stream
// and fails closed if any structural invariant or the SHA-256 digest fails verification.
func (e *Engine) RestoreMemoryImage(compacted []byte) ([]byte, error) {
	const headerLen = 56
	if len(compacted) < headerLen {
		return nil, errors.New("truncated hibernate image header")
	}
	if !bytes.Equal(compacted[:8], imageMagic[:]) {
		return nil, errors.New("invalid hibernate image magic")
	}
	pageSz := binary.LittleEndian.Uint32(compacted[8:12])
	if pageSz != PageSize {
		return nil, fmt.Errorf("unsupported hibernate page size %d", pageSz)
	}
	totalPages := binary.LittleEndian.Uint32(compacted[12:16])
	tailLen := binary.LittleEndian.Uint32(compacted[16:20])
	dirtyCount := binary.LittleEndian.Uint32(compacted[20:24])
	if tailLen >= PageSize {
		return nil, fmt.Errorf("invalid hibernate tail length %d", tailLen)
	}
	if dirtyCount > totalPages {
		return nil, fmt.Errorf("dirty page count (%d) exceeds total pages (%d)", dirtyCount, totalPages)
	}
	var expectedSum [32]byte
	copy(expectedSum[:], compacted[24:56])

	var rawLen uint64
	if totalPages > 0 {
		if tailLen > 0 {
			rawLen = uint64(totalPages-1)*PageSize + uint64(tailLen)
		} else {
			rawLen = uint64(totalPages) * PageSize
		}
	}
	if rawLen > 16*1024*1024*1024 {
		return nil, fmt.Errorf("hibernate image raw size exceeds 16 GiB safety cap (%d)", rawLen)
	}

	restored := make([]byte, int(rawLen))
	pos := headerLen
	prevIdx := -1
	for i := uint32(0); i < dirtyCount; i++ {
		if pos+4 > len(compacted) {
			return nil, errors.New("unexpected EOF reading dirty page index")
		}
		pageIdx := binary.LittleEndian.Uint32(compacted[pos : pos+4])
		pos += 4
		if pageIdx >= totalPages {
			return nil, fmt.Errorf("dirty page index %d out of range (%d)", pageIdx, totalPages)
		}
		if int(pageIdx) <= prevIdx {
			return nil, fmt.Errorf("non-monotonic dirty page index %d after %d", pageIdx, prevIdx)
		}
		prevIdx = int(pageIdx)

		frameLen := PageSize
		if tailLen > 0 && pageIdx == totalPages-1 {
			frameLen = int(tailLen)
		}
		if pos+frameLen > len(compacted) {
			return nil, errors.New("unexpected EOF reading dirty page payload")
		}
		dstOff := int(pageIdx) * PageSize
		copy(restored[dstOff:dstOff+frameLen], compacted[pos:pos+frameLen])
		pos += frameLen
	}
	if pos != len(compacted) {
		return nil, fmt.Errorf("trailing bytes in hibernate image (%d unconsumed)", len(compacted)-pos)
	}

	actualSum := sha256.Sum256(restored)
	if actualSum != expectedSum {
		return nil, errors.New("hibernate image SHA-256 integrity verification failed")
	}

	if e != nil {
		atomic.AddUint64(&e.imagesRestored, 1)
	}
	return restored, nil
}

func generateVMGenID(seed []byte) string {
	var b [16]byte
	h := sha256.Sum256(seed)
	copy(b[:], h[:16])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// BuildRestorePlan constructs the post-hibernation guest wall-clock resync and
// cryptographic virtio-rng entropy reseed plan before releasing inbound requests.
func (e *Engine) BuildRestorePlan(instanceID string, frozenAt, resumedAt time.Time) (RestorePlan, error) {
	id := strings.TrimSpace(instanceID)
	if id == "" {
		return RestorePlan{}, errors.New("instance_id must not be empty")
	}
	if frozenAt.IsZero() {
		frozenAt = time.Now().UTC()
	}
	if resumedAt.IsZero() {
		resumedAt = time.Now().UTC()
	}

	delta := resumedAt.Sub(frozenAt)
	if delta < 0 {
		delta = 0
	}
	adjMode := "noop"
	if delta >= StepThreshold {
		adjMode = "step"
	} else if delta > 0 {
		adjMode = "slew"
	}

	seed := make([]byte, EntropyBytes)
	if _, err := rand.Read(seed); err != nil {
		return RestorePlan{}, fmt.Errorf("read cryptographic entropy: %w", err)
	}
	seedDigest := sha256.Sum256(seed)
	vmGenID := generateVMGenID(seed)

	clockRPC, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "clock.sync",
		"params": map[string]interface{}{
			"host_unix_ns":    resumedAt.UTC().UnixNano(),
			"skew_delta_ms":   delta.Milliseconds(),
			"adjustment_mode": adjMode,
		},
	})
	rngRPC, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "rng.reseed",
		"params": map[string]interface{}{
			"entropy_hex": hex.EncodeToString(seed),
			"vm_gen_id":   vmGenID,
			"credit_bits": EntropyBytes * 8,
		},
	})

	if e != nil {
		atomic.AddUint64(&e.clockSyncs, 1)
		atomic.AddUint64(&e.entropyReseeds, 1)
	}

	return RestorePlan{
		InstanceID:           id,
		FrozenAt:             frozenAt.UTC(),
		ResumedAt:            resumedAt.UTC(),
		HibernatedDurationMs: delta.Milliseconds(),
		ClockAdjustmentMode:  adjMode,
		EntropySeedBytes:     EntropyBytes,
		EntropySeedSHA256:    hex.EncodeToString(seedDigest[:]),
		VMGenID:              vmGenID,
		ClockSyncRPC:         clockRPC,
		RNGReseedRPC:         rngRPC,
	}, nil
}

// RunBenchmark executes an in-memory sparse compaction and verified restore benchmark.
func RunBenchmark(ctx context.Context, cfg BenchmarkConfig) (BenchmarkReport, error) {
	memMB := cfg.MemoryMB
	if memMB == 0 {
		memMB = 32
	}
	if memMB < 1 || memMB > 512 {
		return BenchmarkReport{}, fmt.Errorf("memory_mb must be between 1 and 512, got %d", memMB)
	}
	ratio := cfg.ZeroPageRatio
	if ratio == 0 {
		ratio = 0.75
	}
	if ratio < 0 || ratio > 1.0 {
		return BenchmarkReport{}, fmt.Errorf("zero_page_ratio must be between 0.0 and 1.0, got %v", ratio)
	}

	select {
	case <-ctx.Done():
		return BenchmarkReport{}, ctx.Err()
	default:
	}

	totalBytes := memMB * 1024 * 1024
	totalPages := totalBytes / PageSize
	zeroTarget := int(float64(totalPages) * ratio)
	raw := make([]byte, totalBytes)
	for p := zeroTarget; p < totalPages; p++ {
		off := p * PageSize
		binary.LittleEndian.PutUint64(raw[off:off+8], uint64(p+1)*0x9e3779b97f4a7c15)
		binary.LittleEndian.PutUint64(raw[off+PageSize-8:off+PageSize], uint64(p+7))
	}

	eng := NewEngine(Config{Mode: ModeIdle})
	t0 := time.Now()
	compacted, stats, err := eng.CompactMemoryImage(raw)
	if err != nil {
		return BenchmarkReport{}, err
	}
	compactDur := time.Since(t0)

	t1 := time.Now()
	restored, err := eng.RestoreMemoryImage(compacted)
	if err != nil {
		return BenchmarkReport{}, err
	}
	restoreDur := time.Since(t1)

	verified := bytes.Equal(raw, restored)
	compactSec := compactDur.Seconds()
	if compactSec <= 0 {
		compactSec = 1e-6
	}
	restoreSec := restoreDur.Seconds()
	if restoreSec <= 0 {
		restoreSec = 1e-6
	}

	return BenchmarkReport{
		MemoryMB:          memMB,
		ZeroPageRatio:     ratio,
		RawBytes:          stats.RawBytes,
		StoredBytes:       stats.StoredBytes,
		SavedBytes:        stats.SavedBytes,
		ZeroPages:         stats.ZeroPages,
		DirtyPages:        stats.DirtyPages,
		CompressionRatio:  stats.CompressionRatio,
		CompactDurationMs: float64(compactDur.Microseconds()) / 1000.0,
		RestoreDurationMs: float64(restoreDur.Microseconds()) / 1000.0,
		CompactMBps:       float64(memMB) / compactSec,
		RestoreMBps:       float64(memMB) / restoreSec,
		IntegrityVerified: verified,
	}, nil
}

// Metrics returns atomic counters for the hibernation engine.
func (e *Engine) Metrics() Metrics {
	e.mu.Lock()
	mode := e.mode
	e.mu.Unlock()
	return Metrics{
		Mode:               mode,
		ImagesCompacted:    atomic.LoadUint64(&e.imagesCompacted),
		ImagesRestored:     atomic.LoadUint64(&e.imagesRestored),
		ZeroPagesElided:    atomic.LoadUint64(&e.zeroPagesElided),
		RawBytesProcessed:  atomic.LoadUint64(&e.rawBytesProcessed),
		StoredBytesWritten: atomic.LoadUint64(&e.storedBytesWritten),
		SavedBytes:         atomic.LoadUint64(&e.savedBytes),
		ClockSyncs:         atomic.LoadUint64(&e.clockSyncs),
		EntropyReseeds:     atomic.LoadUint64(&e.entropyReseeds),
	}
}

// Snapshot returns full hibernation status and telemetry.
func (e *Engine) Snapshot() StatusSnapshot {
	return StatusSnapshot{
		Mode:          e.Mode(),
		PageSizeBytes: PageSize,
		EntropyBytes:  EntropyBytes,
		Metrics:       e.Metrics(),
	}
}
