// Package ksm implements Linux Kernel Samepage Merging (KSM) sysfs telemetry
// and a zero-overhead adaptive deduplication governor.
//
// In "off" mode (default), Governor reads /sys/kernel/mm/ksm metrics without
// modifying host kernel parameters. In "auto" mode, Governor keeps ksmd paused
// (run=0, 0% background CPU overhead while retaining already-merged CoW pages)
// whenever fewer than 2 microVMs hold resident guest RAM, and activates ksmd
// (run=1) with adaptive scan-interval backoff when 2 or more microVMs are
// resident.
package ksm

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// DefaultSysfsDir is the standard Linux KSM sysfs mount path.
const DefaultSysfsDir = "/sys/kernel/mm/ksm"

// Supported governor modes.
const (
	ModeOff  = "off"
	ModeAuto = "auto"
	ModeOn   = "on"
)

const (
	defaultPagesToScan   uint64 = 256
	fastSleepMillisecs   uint64 = 50
	steadySleepMillisecs uint64 = 200
)

// Metrics captures live KSM deduplication counters from /sys/kernel/mm/ksm.
type Metrics struct {
	Available      bool   `json:"available"`
	Run            int    `json:"run"`
	Active         bool   `json:"active"`
	PagesShared    uint64 `json:"pages_shared"`
	PagesSharing   uint64 `json:"pages_sharing"`
	PagesUnshared  uint64 `json:"pages_unshared"`
	PagesVolatile  uint64 `json:"pages_volatile"`
	FullScans      uint64 `json:"full_scans"`
	PagesToScan    uint64 `json:"pages_to_scan"`
	SleepMillisecs uint64 `json:"sleep_millisecs"`
	SavedBytes     uint64 `json:"saved_bytes"`
	SavedMB        int    `json:"saved_mb"`
}

// ReconcileResult describes the outcome of a Governor.Reconcile pass.
type ReconcileResult struct {
	Changed        bool
	Run            int
	SleepMillisecs uint64
	Metrics        Metrics
}

// Governor monitors /sys/kernel/mm/ksm and optionally reconciles ksmd state.
type Governor struct {
	mu       sync.Mutex
	sysfsDir string
	mode     string
	pageSize int

	lastResidentVMs int
	lastFullScans   uint64
	lastSharing     uint64
	hasBaseline     bool
}

// NormalizeMode canonicalizes a governor mode string.
func NormalizeMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", ModeOff:
		return ModeOff
	case ModeAuto:
		return ModeAuto
	case ModeOn:
		return ModeOn
	default:
		return strings.ToLower(strings.TrimSpace(mode))
	}
}

// IsValidMode reports whether mode is a valid KSM governor setting.
func IsValidMode(mode string) bool {
	switch NormalizeMode(mode) {
	case ModeOff, ModeAuto, ModeOn:
		return true
	default:
		return false
	}
}

// NewGovernor constructs a KSM Governor reading sysfsDir.
// If sysfsDir is empty, DefaultSysfsDir is used. If pageSize <= 0, os.Getpagesize() is used.
func NewGovernor(sysfsDir, mode string, pageSize int) *Governor {
	if sysfsDir == "" {
		sysfsDir = DefaultSysfsDir
	}
	if pageSize <= 0 {
		pageSize = os.Getpagesize()
		if pageSize <= 0 {
			pageSize = 4096
		}
	}
	norm := NormalizeMode(mode)
	if !IsValidMode(norm) {
		norm = ModeOff
	}
	return &Governor{
		sysfsDir: sysfsDir,
		mode:     norm,
		pageSize: pageSize,
	}
}

// Mode returns the normalized governor mode ("off", "auto", or "on").
func (g *Governor) Mode() string {
	if g == nil {
		return ModeOff
	}
	return g.mode
}

// ReadMetrics reads a point-in-time snapshot of /sys/kernel/mm/ksm.
// Never mutates sysfs and never errors on non-Linux or KSM-disabled kernels.
func (g *Governor) ReadMetrics() Metrics {
	if g == nil {
		return Metrics{}
	}
	return g.readMetricsUnlocked()
}

func (g *Governor) readMetricsUnlocked() Metrics {
	runVal, ok := g.readUint("run")
	if !ok {
		return Metrics{Available: false}
	}
	sharing, _ := g.readUint("pages_sharing")
	shared, _ := g.readUint("pages_shared")
	unshared, _ := g.readUint("pages_unshared")
	volatilePages, _ := g.readUint("pages_volatile")
	fullScans, _ := g.readUint("full_scans")
	pagesToScan, _ := g.readUint("pages_to_scan")
	sleepMs, _ := g.readUint("sleep_millisecs")

	savedBytes := sharing * uint64(g.pageSize)
	return Metrics{
		Available:      true,
		Run:            int(runVal),
		Active:         runVal == 1,
		PagesShared:    shared,
		PagesSharing:   sharing,
		PagesUnshared:  unshared,
		PagesVolatile:  volatilePages,
		FullScans:      fullScans,
		PagesToScan:    pagesToScan,
		SleepMillisecs: sleepMs,
		SavedBytes:     savedBytes,
		SavedMB:        int(savedBytes / (1024 * 1024)),
	}
}

// Reconcile adjusts ksmd parameters according to residentVMs (RUNNING + PAUSED)
// when mode is "auto" or "on". In "off" mode it only reads telemetry.
func (g *Governor) Reconcile(residentVMs int) ReconcileResult {
	if g == nil {
		return ReconcileResult{}
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	cur := g.readMetricsUnlocked()
	if !cur.Available || g.mode == ModeOff {
		return ReconcileResult{
			Run:            cur.Run,
			SleepMillisecs: cur.SleepMillisecs,
			Metrics:        cur,
		}
	}

	minVMs := 2
	if g.mode == ModeOn {
		minVMs = 1
	}

	changed := false
	if residentVMs < minVMs {
		// Pause ksmd scanning for zero background CPU overhead while keeping
		// already-merged pages shared Copy-on-Write in the kernel page tables.
		if cur.Run != 0 {
			if g.writeUint("run", 0) == nil {
				changed = true
			}
		}
		g.lastResidentVMs = residentVMs
		g.hasBaseline = false
		after := g.readMetricsUnlocked()
		return ReconcileResult{
			Changed:        changed,
			Run:            after.Run,
			SleepMillisecs: after.SleepMillisecs,
			Metrics:        after,
		}
	}

	// Determine target sleep interval: ramp quickly (50ms) when new VMs arrive
	// or before a full scan completes; back off to steady-state (200ms) once
	// full_scans advances with <1% growth in pages_sharing.
	targetSleep := fastSleepMillisecs
	if g.hasBaseline && residentVMs <= g.lastResidentVMs && cur.FullScans > g.lastFullScans {
		var growth uint64
		if cur.PagesSharing > g.lastSharing {
			growth = cur.PagesSharing - g.lastSharing
		}
		threshold := g.lastSharing / 100
		if threshold == 0 {
			threshold = 1
		}
		if growth < threshold {
			targetSleep = steadySleepMillisecs
		}
	}

	if cur.PagesToScan != defaultPagesToScan {
		if g.writeUint("pages_to_scan", defaultPagesToScan) == nil {
			changed = true
		}
	}
	if cur.SleepMillisecs != targetSleep {
		if g.writeUint("sleep_millisecs", targetSleep) == nil {
			changed = true
		}
	}
	if cur.Run != 1 {
		if g.writeUint("run", 1) == nil {
			changed = true
		}
	}

	after := g.readMetricsUnlocked()
	g.lastResidentVMs = residentVMs
	g.lastFullScans = after.FullScans
	g.lastSharing = after.PagesSharing
	g.hasBaseline = true

	return ReconcileResult{
		Changed:        changed,
		Run:            after.Run,
		SleepMillisecs: after.SleepMillisecs,
		Metrics:        after,
	}
}

func (g *Governor) readUint(name string) (uint64, bool) {
	data, err := os.ReadFile(filepath.Join(g.sysfsDir, name))
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

func (g *Governor) writeUint(name string, val uint64) error {
	return os.WriteFile(filepath.Join(g.sysfsDir, name), []byte(strconv.FormatUint(val, 10)), 0644)
}
