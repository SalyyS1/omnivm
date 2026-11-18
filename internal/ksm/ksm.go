// Package ksm implements Linux Kernel Samepage Merging (KSM) sysfs telemetry
// and a proactive, churn-aware deduplication governor.
//
// Modes:
//   - "off" (default): Reads /sys/kernel/mm/ksm telemetry without mutating sysfs.
//   - "auto": Keeps ksmd paused (run=0, 0% CPU overhead while retaining merged
//     CoW pages) when fewer than 2 microVMs are resident, ramps at 50ms when
//     >=2 microVMs are resident, backs off to 200ms at steady state, and
//     throttles to 500ms when volatile-page churn exceeds 50%.
//   - "proactive": Pre-indexes mergeable guest pages on cold-boot/branch spawn
//     events (NotifySpawn), scales pages_to_scan proportionally to fleet density
//     (up to 2048 pages/20ms burst), enforces NUMA-local merging
//     (merge_across_nodes=0 when exposed by sysfs), and applies volatile-page
//     thrash backoff when mutating heap pages dominate candidate scans.
//   - "on": Keeps ksmd active whenever >=1 microVM holds resident RAM.
package ksm

import (
	"math"
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
	ModeOff       = "off"
	ModeAuto      = "auto"
	ModeProactive = "proactive"
	ModeOn        = "on"
)

// Governor scan phases exposed in telemetry.
const (
	PhaseDisabled        = "disabled"
	PhasePaused          = "paused"
	PhaseProactiveBurst  = "proactive_burst"
	PhaseRamp            = "ramp"
	PhaseSteady          = "steady"
	PhaseVolatileBackoff = "volatile_backoff"
)

const (
	defaultPagesToScan    uint64  = 256
	volatilePagesToScan   uint64  = 128
	maxProactivePagesScan uint64  = 2048
	maxRampPagesScan      uint64  = 1024
	burstSleepMillisecs   uint64  = 20
	fastSleepMillisecs    uint64  = 50
	steadySleepMillisecs  uint64  = 200
	backoffSleepMillisecs uint64  = 500
	volatileBackoffRatio  float64 = 0.50
	defaultBurstPasses    int     = 3
)

// Metrics captures live KSM deduplication counters and efficiency ratios from /sys/kernel/mm/ksm.
type Metrics struct {
	Available           bool    `json:"available"`
	Run                 int     `json:"run"`
	Active              bool    `json:"active"`
	Phase               string  `json:"phase,omitempty"`
	PagesShared         uint64  `json:"pages_shared"`
	PagesSharing        uint64  `json:"pages_sharing"`
	PagesUnshared       uint64  `json:"pages_unshared"`
	PagesVolatile       uint64  `json:"pages_volatile"`
	FullScans           uint64  `json:"full_scans"`
	PagesToScan         uint64  `json:"pages_to_scan"`
	SleepMillisecs      uint64  `json:"sleep_millisecs"`
	MergeAcrossNodes    int     `json:"merge_across_nodes"`
	ScanRatePagesPerSec uint64  `json:"scan_rate_pages_per_sec"`
	DedupRatio          float64 `json:"dedup_ratio"`
	VolatileRatio       float64 `json:"volatile_ratio"`
	SavedBytes          uint64  `json:"saved_bytes"`
	SavedMB             int     `json:"saved_mb"`
	BurstActivations    uint64  `json:"burst_activations"`
	VolatileBackoffs    uint64  `json:"volatile_backoffs"`
}

// ReconcileResult describes the outcome of a Governor.Reconcile pass.
type ReconcileResult struct {
	Changed        bool
	Phase          string
	Run            int
	PagesToScan    uint64
	SleepMillisecs uint64
	Metrics        Metrics
}

// Governor monitors /sys/kernel/mm/ksm and optionally reconciles ksmd state.
type Governor struct {
	mu       sync.Mutex
	sysfsDir string
	mode     string
	pageSize int

	lastResidentVMs  int
	lastFullScans    uint64
	lastSharing      uint64
	hasBaseline      bool
	lastPhase        string
	burstPasses      int
	preindexSingleVM bool
	burstActivations uint64
	volatileBackoffs uint64
}

// NormalizeMode canonicalizes a governor mode string.
func NormalizeMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", ModeOff:
		return ModeOff
	case ModeAuto:
		return ModeAuto
	case ModeProactive:
		return ModeProactive
	case ModeOn:
		return ModeOn
	default:
		return strings.ToLower(strings.TrimSpace(mode))
	}
}

// IsValidMode reports whether mode is a valid KSM governor setting.
func IsValidMode(mode string) bool {
	switch NormalizeMode(mode) {
	case ModeOff, ModeAuto, ModeProactive, ModeOn:
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
	initialPhase := PhaseDisabled
	if norm != ModeOff {
		initialPhase = PhasePaused
	}
	return &Governor{
		sysfsDir:  sysfsDir,
		mode:      norm,
		pageSize:  pageSize,
		lastPhase: initialPhase,
	}
}

// Mode returns the normalized governor mode ("off", "auto", "proactive", or "on").
func (g *Governor) Mode() string {
	if g == nil {
		return ModeOff
	}
	return g.mode
}

// NotifySpawn signals that newVMs (>= 1) microVMs have cold-booted or branched,
// arming a bounded proactive KSM merge burst before the next Reconcile pass.
func (g *Governor) NotifySpawn(newVMs int) {
	if g == nil || newVMs <= 0 {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.mode != ModeProactive {
		return
	}
	passes := defaultBurstPasses
	if newVMs > 2 {
		passes = defaultBurstPasses + newVMs - 2
		if passes > 8 {
			passes = 8
		}
	}
	if passes > g.burstPasses {
		g.burstPasses = passes
	}
	g.preindexSingleVM = true
	g.burstActivations++
}

// ReadMetrics reads a point-in-time snapshot of /sys/kernel/mm/ksm.
// Never mutates sysfs and never errors on non-Linux or KSM-disabled kernels.
func (g *Governor) ReadMetrics() Metrics {
	if g == nil {
		return Metrics{}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.readMetricsUnlocked()
}

func (g *Governor) readMetricsUnlocked() Metrics {
	runVal, ok := g.readUint("run")
	if !ok {
		return Metrics{Available: false, MergeAcrossNodes: -1}
	}
	sharing, _ := g.readUint("pages_sharing")
	shared, _ := g.readUint("pages_shared")
	unshared, _ := g.readUint("pages_unshared")
	volatilePages, _ := g.readUint("pages_volatile")
	fullScans, _ := g.readUint("full_scans")
	pagesToScan, _ := g.readUint("pages_to_scan")
	sleepMs, _ := g.readUint("sleep_millisecs")

	mergeAcross := -1
	if ma, hasMA := g.readUint("merge_across_nodes"); hasMA {
		mergeAcross = int(ma)
	}

	active := runVal == 1
	var scanRate uint64
	if active {
		if sleepMs > 0 {
			scanRate = (pagesToScan * 1000) / sleepMs
		} else {
			scanRate = pagesToScan * 1000
		}
	}

	var dedupRatio float64
	stableCandidates := shared + sharing + unshared
	if stableCandidates > 0 {
		dedupRatio = round4(float64(sharing) / float64(stableCandidates))
	}

	var volRatio float64
	allTracked := stableCandidates + volatilePages
	if allTracked > 0 {
		volRatio = round4(float64(volatilePages) / float64(allTracked))
	}

	phase := g.lastPhase
	if g.mode == ModeOff {
		if active {
			phase = PhaseSteady
		} else {
			phase = PhaseDisabled
		}
	} else if !active {
		phase = PhasePaused
	} else if phase == "" || phase == PhasePaused || phase == PhaseDisabled {
		phase = PhaseSteady
	}

	savedBytes := sharing * uint64(g.pageSize)
	return Metrics{
		Available:           true,
		Run:                 int(runVal),
		Active:              active,
		Phase:               phase,
		PagesShared:         shared,
		PagesSharing:        sharing,
		PagesUnshared:       unshared,
		PagesVolatile:       volatilePages,
		FullScans:           fullScans,
		PagesToScan:         pagesToScan,
		SleepMillisecs:      sleepMs,
		MergeAcrossNodes:    mergeAcross,
		ScanRatePagesPerSec: scanRate,
		DedupRatio:          dedupRatio,
		VolatileRatio:       volRatio,
		SavedBytes:          savedBytes,
		SavedMB:             int(savedBytes / (1024 * 1024)),
		BurstActivations:    g.burstActivations,
		VolatileBackoffs:    g.volatileBackoffs,
	}
}

func round4(v float64) float64 {
	return math.Round(v*10000) / 10000
}

// Reconcile adjusts ksmd parameters according to residentVMs (STARTING + RUNNING + PAUSED)
// when mode is "auto", "proactive", or "on". In "off" mode it only reads telemetry.
func (g *Governor) Reconcile(residentVMs int) ReconcileResult {
	if g == nil {
		return ReconcileResult{}
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	cur := g.readMetricsUnlocked()
	if !cur.Available || g.mode == ModeOff {
		return ReconcileResult{
			Phase:          cur.Phase,
			Run:            cur.Run,
			PagesToScan:    cur.PagesToScan,
			SleepMillisecs: cur.SleepMillisecs,
			Metrics:        cur,
		}
	}

	changed := false

	// In proactive mode, pin merge_across_nodes=0 when sysfs exposes it and ksmd is stopped
	// so page deduplication stays NUMA-socket-local on multi-socket hosts.
	if g.mode == ModeProactive && cur.MergeAcrossNodes > 0 && cur.Run == 0 {
		if g.writeUint("merge_across_nodes", 0) == nil {
			changed = true
		}
	}

	minVMs := 2
	switch g.mode {
	case ModeOn:
		minVMs = 1
	case ModeProactive:
		// Allow a single pre-indexing pass when the 1st template VM spawns via NotifySpawn,
		// then return to run=0 if no 2nd VM joins.
		if residentVMs == 1 && g.preindexSingleVM {
			minVMs = 1
			g.preindexSingleVM = false
		}
	}

	if residentVMs < minVMs {
		if cur.Run != 0 {
			if g.writeUint("run", 0) == nil {
				changed = true
			}
		}
		g.lastResidentVMs = residentVMs
		g.hasBaseline = false
		g.lastPhase = PhasePaused
		if residentVMs == 0 {
			g.burstPasses = 0
			g.preindexSingleVM = false
		}
		after := g.readMetricsUnlocked()
		return ReconcileResult{
			Changed:        changed,
			Phase:          after.Phase,
			Run:            after.Run,
			PagesToScan:    after.PagesToScan,
			SleepMillisecs: after.SleepMillisecs,
			Metrics:        after,
		}
	}

	// Determine whether marginal sharing growth converged across the latest full scan.
	converged := false
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
			converged = true
		}
	}

	// If fleet grew since last pass in proactive mode, trigger a fresh burst window.
	if g.mode == ModeProactive && residentVMs > g.lastResidentVMs {
		if g.burstPasses < 2 {
			g.burstPasses = 2
			g.burstActivations++
		}
	}

	targetPages := defaultPagesToScan
	targetSleep := fastSleepMillisecs
	targetPhase := PhaseRamp

	switch {
	case converged && cur.VolatileRatio >= volatileBackoffRatio:
		// High-churn workload (JIT/GC mutating pages faster than KSM stabilizes them):
		// throttle scan batch and lengthen sleep to protect host CPU.
		targetPages = volatilePagesToScan
		targetSleep = backoffSleepMillisecs
		targetPhase = PhaseVolatileBackoff
		g.burstPasses = 0
		if g.lastPhase != PhaseVolatileBackoff {
			g.volatileBackoffs++
		}

	case g.mode == ModeProactive && g.burstPasses > 0 && !converged:
		g.burstPasses--
		effVMs := uint64(residentVMs)
		if effVMs < 2 {
			effVMs = 2
		}
		targetPages = effVMs * 256
		if targetPages > maxProactivePagesScan {
			targetPages = maxProactivePagesScan
		}
		targetSleep = burstSleepMillisecs
		targetPhase = PhaseProactiveBurst

	case converged:
		targetPages = defaultPagesToScan
		targetSleep = steadySleepMillisecs
		targetPhase = PhaseSteady

	default:
		if g.mode == ModeProactive {
			scaled := uint64(residentVMs) * 128
			if scaled < defaultPagesToScan {
				scaled = defaultPagesToScan
			}
			if scaled > maxRampPagesScan {
				scaled = maxRampPagesScan
			}
			targetPages = scaled
		}
		targetSleep = fastSleepMillisecs
		targetPhase = PhaseRamp
	}

	if cur.PagesToScan != targetPages {
		if g.writeUint("pages_to_scan", targetPages) == nil {
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

	g.lastPhase = targetPhase
	after := g.readMetricsUnlocked()
	g.lastResidentVMs = residentVMs
	g.lastFullScans = after.FullScans
	g.lastSharing = after.PagesSharing
	g.hasBaseline = true

	return ReconcileResult{
		Changed:        changed,
		Phase:          after.Phase,
		Run:            after.Run,
		PagesToScan:    after.PagesToScan,
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
