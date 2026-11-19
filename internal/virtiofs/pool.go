// Package virtiofs manages the shared Virtio-FS and Virtio-PMEM DAX rootfs and
// workspace cache pool for Cloud Hypervisor microVMs.
//
// By mapping read-only base rootfs layers via virtio-pmem (discard_writes=true)
// and workspace directories via 2 MiB-aligned virtio-fs DAX windows, multiple
// concurrent microVMs share a single host page-cache copy instead of
// duplicating guest page caches inside each VM's anonymous RAM.
package virtiofs

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	ModeOff  = "off"
	ModeAuto = "auto"
	ModeDAX  = "dax"

	// DAXAlignmentBytes is the 2 MiB hugepage alignment required by Cloud Hypervisor
	// FsConfig.cache_size when DAX direct mapping is enabled.
	DAXAlignmentBytes uint64 = 2 * 1024 * 1024

	// MinDAXWindowBytes is the smallest per-VM DAX window allocated (64 MiB).
	MinDAXWindowBytes uint64 = 64 * 1024 * 1024

	// DefaultDAXWindowBytes is the default per-VM DAX window target (256 MiB).
	DefaultDAXWindowBytes uint64 = 256 * 1024 * 1024

	// DefaultMaxPoolBytes caps aggregate host DAX virtual address mappings across
	// active microVMs (4 GiB default).
	DefaultMaxPoolBytes uint64 = 4096 * 1024 * 1024

	// DefaultMaxCacheBytes caps total shared base rootfs layer bytes retained in
	// the content-addressed pool before unreferenced LRU eviction engages (8 GiB).
	DefaultMaxCacheBytes uint64 = 8192 * 1024 * 1024

	// MinGuestMemoryMBForAutoDAX is the guest RAM threshold at which ModeAuto
	// enables a DAX window automatically.
	MinGuestMemoryMBForAutoDAX = 256
)

// ErrCachePoolExhausted is returned when a new shared layer exceeds MaxCacheBytes
// even after evicting all unreferenced (ActiveRefs == 0) entries.
var ErrCachePoolExhausted = errors.New("virtiofs shared layer cache capacity exhausted by pinned active layers")

// Config configures a shared Virtio-FS and Virtio-PMEM DAX cache pool.
type Config struct {
	Mode               string
	DefaultWindowBytes uint64
	MaxPoolBytes       uint64
	MaxCacheBytes      uint64
}

// LayerEntry describes a content-addressed shared read-only rootfs or workspace base layer.
type LayerEntry struct {
	Digest        string    `json:"digest"`
	ImageRef      string    `json:"image_ref"`
	FilePath      string    `json:"file_path,omitempty"`
	SizeBytes     uint64    `json:"size_bytes"`
	ActiveRefs    int       `json:"active_refs"`
	TotalAcquires uint64    `json:"total_acquires"`
	ReadOnly      bool      `json:"read_only"`
	CreatedAt     time.Time `json:"created_at"`
	LastUsedAt    time.Time `json:"last_used_at"`

	lruSeq uint64
}

// MountAllocation tracks active per-instance DAX window and shared layer bindings.
type MountAllocation struct {
	InstanceID     string    `json:"instance_id"`
	Tag            string    `json:"tag"`
	DAXEnabled     bool      `json:"dax_enabled"`
	DAXWindowBytes uint64    `json:"dax_window_bytes"`
	NumQueues      int       `json:"num_queues"`
	QueueSize      int       `json:"queue_size"`
	CachePolicy    string    `json:"cache_policy"`
	LayerDigest    string    `json:"layer_digest,omitempty"`
	FallbackReason string    `json:"fallback_reason,omitempty"`
	AllocatedAt    time.Time `json:"allocated_at"`
}

// MountPlan contains synthesized virtiofsd CLI flags and Cloud Hypervisor REST
// payload structures for a single microVM mount.
type MountPlan struct {
	InstanceID     string                 `json:"instance_id"`
	Mode           string                 `json:"mode"`
	Tag            string                 `json:"tag"`
	CachePolicy    string                 `json:"cache_policy"`
	DAXEnabled     bool                   `json:"dax_enabled"`
	DAXWindowBytes uint64                 `json:"dax_window_bytes"`
	NumQueues      int                    `json:"num_queues"`
	QueueSize      int                    `json:"queue_size"`
	ThreadPoolSize int                    `json:"thread_pool_size"`
	SandboxMode    string                 `json:"sandbox_mode"`
	VirtiofsdArgs  []string               `json:"virtiofsd_args"`
	CHFsConfig     map[string]interface{} `json:"ch_fs_config"`
	CHPmemConfig   map[string]interface{} `json:"ch_pmem_config,omitempty"`
	SharedLayer    *LayerEntry            `json:"shared_layer,omitempty"`
	FallbackReason string                 `json:"fallback_reason,omitempty"`
}

// Metrics reports cumulative and live Virtio-FS / Virtio-PMEM DAX pool telemetry.
type Metrics struct {
	Mode               string `json:"mode"`
	ActiveLayers       int    `json:"active_layers"`
	PinnedLayers       int    `json:"pinned_layers"`
	CachedBytes        uint64 `json:"cached_bytes"`
	MaxCacheBytes      uint64 `json:"max_cache_bytes"`
	CacheHits          uint64 `json:"cache_hits"`
	CacheMisses        uint64 `json:"cache_misses"`
	LayerEvictions     uint64 `json:"layer_evictions"`
	DedupSavedBytes    uint64 `json:"dedup_saved_bytes"`
	ActiveDAXMounts    int    `json:"active_dax_mounts"`
	AllocatedDAXBytes  uint64 `json:"allocated_dax_bytes"`
	MaxPoolBytes       uint64 `json:"max_pool_bytes"`
	DAXAllocations     uint64 `json:"dax_allocations"`
	DAXBudgetFallbacks uint64 `json:"dax_budget_fallbacks"`
}

// StatusSnapshot is a race-free point-in-time snapshot of the DAX cache pool.
type StatusSnapshot struct {
	Mode        string            `json:"mode"`
	Metrics     Metrics           `json:"metrics"`
	Layers      []LayerEntry      `json:"layers"`
	Allocations []MountAllocation `json:"allocations"`
}

// Pool coordinates content-addressed shared layers and host-wide DAX window quotas.
type Pool struct {
	mu                 sync.Mutex
	mode               string
	defaultWindowBytes uint64
	maxPoolBytes       uint64
	maxCacheBytes      uint64

	seq               uint64
	layers            map[string]*LayerEntry
	allocations       map[string]*MountAllocation
	cachedBytes       uint64
	allocatedDAXBytes uint64

	cacheHits          uint64
	cacheMisses        uint64
	layerEvictions     uint64
	dedupSavedBytes    uint64
	daxAllocations     uint64
	daxBudgetFallbacks uint64
}

// NormalizeMode validates and normalizes a pool mode string.
func NormalizeMode(raw string) (string, error) {
	m := strings.ToLower(strings.TrimSpace(raw))
	switch m {
	case "", ModeAuto:
		return ModeAuto, nil
	case ModeOff:
		return ModeOff, nil
	case ModeDAX:
		return ModeDAX, nil
	default:
		return "", fmt.Errorf("unsupported virtiofs pool mode %q: expected off, auto, or dax", raw)
	}
}

// Align2MiB rounds n down to the nearest 2 MiB boundary, or up to MinDAXWindowBytes
// when positive and smaller than MinDAXWindowBytes.
func Align2MiB(n uint64) uint64 {
	if n == 0 {
		return 0
	}
	if n < MinDAXWindowBytes {
		return MinDAXWindowBytes
	}
	return (n / DAXAlignmentBytes) * DAXAlignmentBytes
}

// NewPool initializes a Virtio-FS / Virtio-PMEM DAX shared cache pool.
func NewPool(cfg Config) *Pool {
	mode, err := NormalizeMode(cfg.Mode)
	if err != nil {
		mode = ModeAuto
	}
	win := Align2MiB(cfg.DefaultWindowBytes)
	if win == 0 {
		win = DefaultDAXWindowBytes
	}
	maxPool := cfg.MaxPoolBytes
	if maxPool == 0 {
		maxPool = DefaultMaxPoolBytes
	} else {
		maxPool = Align2MiB(maxPool)
	}
	maxCache := cfg.MaxCacheBytes
	if maxCache == 0 {
		maxCache = DefaultMaxCacheBytes
	}
	return &Pool{
		mode:               mode,
		defaultWindowBytes: win,
		maxPoolBytes:       maxPool,
		maxCacheBytes:      maxCache,
		layers:             make(map[string]*LayerEntry),
		allocations:        make(map[string]*MountAllocation),
	}
}

// Mode returns the active pool operating mode.
func (p *Pool) Mode() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.mode
}

// SetMode dynamically updates the pool operating mode ("off", "auto", or "dax").
func (p *Pool) SetMode(mode string) error {
	norm, err := NormalizeMode(mode)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.mode = norm
	p.mu.Unlock()
	return nil
}

// ComputeDAXWindow calculates the target 2 MiB-aligned DAX window for a guest VM.
func (p *Pool) ComputeDAXWindow(memoryMB int, requestedBytes uint64) uint64 {
	p.mu.Lock()
	mode := p.mode
	defWin := p.defaultWindowBytes
	p.mu.Unlock()

	if mode == ModeOff {
		return 0
	}
	if mode == ModeAuto && memoryMB > 0 && memoryMB < MinGuestMemoryMBForAutoDAX && requestedBytes == 0 {
		return 0
	}
	if requestedBytes > 0 {
		return Align2MiB(requestedBytes)
	}
	if memoryMB <= 0 {
		return defWin
	}
	// Default heuristic: 50% of guest RAM aligned to 2 MiB, clamped to [64 MiB, defWin].
	proportional := Align2MiB(uint64(memoryMB) * 1024 * 1024 / 2)
	if proportional > defWin {
		proportional = defWin
	}
	if proportional < MinDAXWindowBytes {
		proportional = MinDAXWindowBytes
	}
	return proportional
}

// ComputeDigest computes a canonical sha256:<hex> digest from a file on disk,
// or from (imageRef, filePath, sizeBytes) metadata when filePath does not exist locally.
func ComputeDigest(imageRef, filePath string, sizeBytes uint64) (string, uint64, error) {
	if filePath != "" {
		info, err := os.Stat(filePath)
		if err == nil && info.Mode().IsRegular() {
			f, openErr := os.Open(filePath)
			if openErr != nil {
				return "", 0, openErr
			}
			defer f.Close()
			h := sha256.New()
			n, copyErr := io.Copy(h, f)
			if copyErr != nil {
				return "", 0, copyErr
			}
			actualSize := uint64(n)
			if sizeBytes > actualSize {
				actualSize = sizeBytes
			}
			return "sha256:" + hex.EncodeToString(h.Sum(nil)), actualSize, nil
		}
	}
	if strings.TrimSpace(imageRef) == "" && strings.TrimSpace(filePath) == "" {
		return "", 0, errors.New("either image_ref or file_path is required")
	}
	if sizeBytes == 0 {
		return "", 0, errors.New("size_bytes must be positive for virtual layer registration")
	}
	h := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%d", strings.TrimSpace(imageRef), strings.TrimSpace(filePath), sizeBytes)))
	return "sha256:" + hex.EncodeToString(h[:]), sizeBytes, nil
}

func normalizeDigest(digest string) string {
	d := strings.ToLower(strings.TrimSpace(digest))
	if d == "" {
		return ""
	}
	if !strings.HasPrefix(d, "sha256:") {
		return "sha256:" + d
	}
	return d
}

// AcquireLayer looks up or registers a content-addressed read-only base layer,
// increments its reference count, and evicts unreferenced LRU layers if needed.
// The returned boolean is true when served from an existing warm pool entry (cache hit).
func (p *Pool) AcquireLayer(digest, imageRef, filePath string, sizeBytes uint64) (LayerEntry, bool, error) {
	normDigest := normalizeDigest(digest)
	if normDigest == "" {
		computed, actualSize, err := ComputeDigest(imageRef, filePath, sizeBytes)
		if err != nil {
			return LayerEntry{}, false, err
		}
		normDigest = computed
		sizeBytes = actualSize
	} else if sizeBytes == 0 && filePath != "" {
		if st, err := os.Stat(filePath); err == nil {
			sizeBytes = uint64(st.Size())
		}
	}
	if sizeBytes == 0 {
		return LayerEntry{}, false, errors.New("layer size_bytes must be greater than zero")
	}

	now := time.Now().UTC()
	p.mu.Lock()
	defer p.mu.Unlock()

	if existing, ok := p.layers[normDigest]; ok {
		p.seq++
		existing.lruSeq = p.seq
		existing.ActiveRefs++
		existing.TotalAcquires++
		existing.LastUsedAt = now
		p.cacheHits++
		p.dedupSavedBytes += existing.SizeBytes
		return *existing, true, nil
	}

	if sizeBytes > p.maxCacheBytes {
		return LayerEntry{}, false, fmt.Errorf("%w: layer size %d exceeds max_cache_bytes %d", ErrCachePoolExhausted, sizeBytes, p.maxCacheBytes)
	}

	if p.cachedBytes+sizeBytes > p.maxCacheBytes {
		needed := (p.cachedBytes + sizeBytes) - p.maxCacheBytes
		p.evictLRULocked(needed)
		if p.cachedBytes+sizeBytes > p.maxCacheBytes {
			return LayerEntry{}, false, ErrCachePoolExhausted
		}
	}

	p.seq++
	entry := &LayerEntry{
		Digest:        normDigest,
		ImageRef:      strings.TrimSpace(imageRef),
		FilePath:      strings.TrimSpace(filePath),
		SizeBytes:     sizeBytes,
		ActiveRefs:    1,
		TotalAcquires: 1,
		ReadOnly:      true,
		CreatedAt:     now,
		LastUsedAt:    now,
		lruSeq:        p.seq,
	}
	p.layers[normDigest] = entry
	p.cachedBytes += sizeBytes
	p.cacheMisses++
	return *entry, false, nil
}

// RegisterLayer inserts a warm unreferenced shared layer into the cache pool
// (ActiveRefs = 0) so subsequent microVM boots hit immediately.
func (p *Pool) RegisterLayer(digest, imageRef, filePath string, sizeBytes uint64) (LayerEntry, bool, error) {
	entry, hit, err := p.AcquireLayer(digest, imageRef, filePath, sizeBytes)
	if err != nil {
		return LayerEntry{}, false, err
	}
	p.ReleaseLayer(entry.Digest)
	p.mu.Lock()
	updated := *p.layers[entry.Digest]
	p.mu.Unlock()
	return updated, hit, nil
}

// ReleaseLayer decrements ActiveRefs on a shared layer when an instance stops.
func (p *Pool) ReleaseLayer(digest string) bool {
	norm := normalizeDigest(digest)
	p.mu.Lock()
	defer p.mu.Unlock()
	entry, ok := p.layers[norm]
	if !ok {
		return false
	}
	if entry.ActiveRefs > 0 {
		entry.ActiveRefs--
	}
	p.seq++
	entry.lruSeq = p.seq
	entry.LastUsedAt = time.Now().UTC()
	return true
}

// DeleteLayer removes an unreferenced layer from the cache pool.
// Returns an error if the layer is currently pinned by an active microVM.
func (p *Pool) DeleteLayer(digest string) (bool, error) {
	norm := normalizeDigest(digest)
	p.mu.Lock()
	defer p.mu.Unlock()
	entry, ok := p.layers[norm]
	if !ok {
		return false, nil
	}
	if entry.ActiveRefs > 0 {
		return false, fmt.Errorf("layer %s is pinned by %d active microVM(s)", norm, entry.ActiveRefs)
	}
	if p.cachedBytes >= entry.SizeBytes {
		p.cachedBytes -= entry.SizeBytes
	} else {
		p.cachedBytes = 0
	}
	delete(p.layers, norm)
	return true, nil
}

func (p *Pool) evictLRULocked(neededBytes uint64) uint64 {
	var candidates []*LayerEntry
	for _, e := range p.layers {
		if e.ActiveRefs == 0 {
			candidates = append(candidates, e)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].lruSeq < candidates[j].lruSeq
	})
	var freed uint64
	for _, c := range candidates {
		if freed >= neededBytes {
			break
		}
		delete(p.layers, c.Digest)
		if p.cachedBytes >= c.SizeBytes {
			p.cachedBytes -= c.SizeBytes
		} else {
			p.cachedBytes = 0
		}
		freed += c.SizeBytes
		p.layerEvictions++
	}
	return freed
}

// PlanMount reserves a DAX window (when enabled and within host quota), optionally
// attaches a shared virtio-pmem base layer, and synthesizes virtiofsd CLI flags
// plus Cloud Hypervisor FsConfig / PmemConfig payloads.
func (p *Pool) PlanMount(instanceID, tag, socketPath, sharedDir string, memoryMB, vcpus int, layerDigest string) (MountPlan, error) {
	instanceID = strings.TrimSpace(instanceID)
	if instanceID == "" {
		return MountPlan{}, errors.New("instance_id is required")
	}
	tag = strings.TrimSpace(tag)
	if tag == "" {
		tag = "workspace"
	}
	if vcpus <= 0 {
		vcpus = 1
	}
	numQueues := vcpus
	if numQueues > 8 {
		numQueues = 8
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// Release any stale allocation for the same instance before re-planning.
	p.releaseInstanceLocked(instanceID)

	mode := p.mode
	cachePolicy := "never"
	queueSize := 512
	var daxBytes uint64
	var fallbackReason string

	switch mode {
	case ModeOff:
		cachePolicy = "never"
		numQueues = 1
		queueSize = 512
	case ModeAuto:
		cachePolicy = "auto"
		queueSize = 1024
		if memoryMB < MinGuestMemoryMBForAutoDAX {
			fallbackReason = "guest_memory_below_threshold"
		} else {
			desired := Align2MiB(uint64(memoryMB) * 1024 * 1024 / 2)
			if desired > p.defaultWindowBytes {
				desired = p.defaultWindowBytes
			}
			if desired < MinDAXWindowBytes {
				desired = MinDAXWindowBytes
			}
			if p.allocatedDAXBytes+desired <= p.maxPoolBytes {
				daxBytes = desired
			} else {
				fallbackReason = "dax_pool_exhausted"
				p.daxBudgetFallbacks++
			}
		}
	case ModeDAX:
		cachePolicy = "always"
		queueSize = 1024
		desired := p.defaultWindowBytes
		if memoryMB > 0 {
			cand := Align2MiB(uint64(memoryMB) * 1024 * 1024 / 2)
			if cand > desired {
				cand = desired
			}
			if cand >= MinDAXWindowBytes {
				desired = cand
			}
		}
		if p.allocatedDAXBytes+desired <= p.maxPoolBytes {
			daxBytes = desired
		} else {
			cachePolicy = "auto"
			fallbackReason = "dax_pool_exhausted"
			p.daxBudgetFallbacks++
		}
	}

	var sharedLayerCopy *LayerEntry
	var pmemCfg map[string]interface{}
	normDigest := normalizeDigest(layerDigest)
	if normDigest != "" {
		entry, ok := p.layers[normDigest]
		if !ok {
			return MountPlan{}, fmt.Errorf("shared layer %s is not registered in virtiofs pool", normDigest)
		}
		p.seq++
		entry.lruSeq = p.seq
		entry.ActiveRefs++
		entry.TotalAcquires++
		entry.LastUsedAt = time.Now().UTC()
		if entry.TotalAcquires > 1 {
			p.cacheHits++
			p.dedupSavedBytes += entry.SizeBytes
		}
		cp := *entry
		sharedLayerCopy = &cp
		if entry.FilePath != "" && mode != ModeOff {
			pmemCfg = BuildCHPmemPayload("rootfs-dax", entry.FilePath, true)
		}
	}

	daxEnabled := daxBytes > 0
	if daxEnabled {
		p.allocatedDAXBytes += daxBytes
		p.daxAllocations++
	}

	threadPool := numQueues * 2
	if threadPool > 8 {
		threadPool = 8
	}

	alloc := &MountAllocation{
		InstanceID:     instanceID,
		Tag:            tag,
		DAXEnabled:     daxEnabled,
		DAXWindowBytes: daxBytes,
		NumQueues:      numQueues,
		QueueSize:      queueSize,
		CachePolicy:    cachePolicy,
		LayerDigest:    normDigest,
		FallbackReason: fallbackReason,
		AllocatedAt:    time.Now().UTC(),
	}
	p.allocations[instanceID] = alloc

	args := BuildVirtiofsdArgs(socketPath, sharedDir, cachePolicy, threadPool)
	fsCfg := BuildCHFsPayload(tag, socketPath, numQueues, queueSize, daxBytes)

	return MountPlan{
		InstanceID:     instanceID,
		Mode:           mode,
		Tag:            tag,
		CachePolicy:    cachePolicy,
		DAXEnabled:     daxEnabled,
		DAXWindowBytes: daxBytes,
		NumQueues:      numQueues,
		QueueSize:      queueSize,
		ThreadPoolSize: threadPool,
		SandboxMode:    "namespace",
		VirtiofsdArgs:  args,
		CHFsConfig:     fsCfg,
		CHPmemConfig:   pmemCfg,
		SharedLayer:    sharedLayerCopy,
		FallbackReason: fallbackReason,
	}, nil
}

// ReleaseInstance releases any DAX window reservation and shared layer pin held
// by instanceID. Safe to call multiple times.
func (p *Pool) ReleaseInstance(instanceID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.releaseInstanceLocked(strings.TrimSpace(instanceID))
}

func (p *Pool) releaseInstanceLocked(instanceID string) bool {
	alloc, ok := p.allocations[instanceID]
	if !ok {
		return false
	}
	if alloc.DAXEnabled && alloc.DAXWindowBytes > 0 {
		if p.allocatedDAXBytes >= alloc.DAXWindowBytes {
			p.allocatedDAXBytes -= alloc.DAXWindowBytes
		} else {
			p.allocatedDAXBytes = 0
		}
	}
	if alloc.LayerDigest != "" {
		if entry, exists := p.layers[alloc.LayerDigest]; exists && entry.ActiveRefs > 0 {
			entry.ActiveRefs--
			p.seq++
			entry.lruSeq = p.seq
			entry.LastUsedAt = time.Now().UTC()
		}
	}
	delete(p.allocations, instanceID)
	return true
}

// BuildVirtiofsdArgs constructs virtiofsd command-line flags for the given cache
// policy and worker thread pool size.
func BuildVirtiofsdArgs(socketPath, sharedDir, cachePolicy string, threadPoolSize int) []string {
	if cachePolicy == "" {
		cachePolicy = "never"
	}
	args := []string{
		"--socket-path=" + socketPath,
		"--shared-dir=" + sharedDir,
		"--cache=" + cachePolicy,
	}
	if cachePolicy != "never" && threadPoolSize > 0 {
		args = append(args, fmt.Sprintf("--thread-pool-size=%d", threadPoolSize))
	}
	return args
}

// BuildCHFsPayload constructs Cloud Hypervisor's FsConfig JSON object.
func BuildCHFsPayload(tag, socketPath string, numQueues, queueSize int, daxWindowBytes uint64) map[string]interface{} {
	if tag == "" {
		tag = "workspace"
	}
	if numQueues <= 0 {
		numQueues = 1
	}
	if queueSize <= 0 {
		queueSize = 512
	}
	cfg := map[string]interface{}{
		"tag":        tag,
		"socket":     socketPath,
		"num_queues": numQueues,
		"queue_size": queueSize,
	}
	aligned := Align2MiB(daxWindowBytes)
	if aligned > 0 {
		cfg["dax"] = true
		cfg["cache_size"] = aligned
	}
	return cfg
}

// BuildCHPmemPayload constructs Cloud Hypervisor's PmemConfig JSON object for
// direct-mapped shared base rootfs images.
func BuildCHPmemPayload(id, filePath string, discardWrites bool) map[string]interface{} {
	if id == "" {
		id = "rootfs-dax"
	}
	return map[string]interface{}{
		"id":             id,
		"file":           filePath,
		"discard_writes": discardWrites,
	}
}

// Metrics returns a thread-safe snapshot of pool telemetry counters and gauges.
func (p *Pool) Metrics() Metrics {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.metricsLocked()
}

func (p *Pool) metricsLocked() Metrics {
	pinned := 0
	for _, e := range p.layers {
		if e.ActiveRefs > 0 {
			pinned++
		}
	}
	activeDAX := 0
	for _, a := range p.allocations {
		if a.DAXEnabled {
			activeDAX++
		}
	}
	return Metrics{
		Mode:               p.mode,
		ActiveLayers:       len(p.layers),
		PinnedLayers:       pinned,
		CachedBytes:        p.cachedBytes,
		MaxCacheBytes:      p.maxCacheBytes,
		CacheHits:          p.cacheHits,
		CacheMisses:        p.cacheMisses,
		LayerEvictions:     p.layerEvictions,
		DedupSavedBytes:    p.dedupSavedBytes,
		ActiveDAXMounts:    activeDAX,
		AllocatedDAXBytes:  p.allocatedDAXBytes,
		MaxPoolBytes:       p.maxPoolBytes,
		DAXAllocations:     p.daxAllocations,
		DAXBudgetFallbacks: p.daxBudgetFallbacks,
	}
}

// Snapshot returns a race-free StatusSnapshot with sorted layers and allocations.
func (p *Pool) Snapshot() StatusSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()

	layers := make([]LayerEntry, 0, len(p.layers))
	for _, e := range p.layers {
		layers = append(layers, *e)
	}
	sort.Slice(layers, func(i, j int) bool {
		return layers[i].Digest < layers[j].Digest
	})

	allocs := make([]MountAllocation, 0, len(p.allocations))
	for _, a := range p.allocations {
		allocs = append(allocs, *a)
	}
	sort.Slice(allocs, func(i, j int) bool {
		return allocs[i].InstanceID < allocs[j].InstanceID
	})

	return StatusSnapshot{
		Mode:        p.mode,
		Metrics:     p.metricsLocked(),
		Layers:      layers,
		Allocations: allocs,
	}
}
