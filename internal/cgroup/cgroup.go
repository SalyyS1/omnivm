// Package cgroup implements Linux cgroup v2 memory telemetry (memory.current,
// memory.high, memory.max, memory.events, and memory.pressure PSI) alongside
// an adaptive memory.high soft-throttle watermark governor.
//
// In "off" mode (default), Governor only reads cgroup v2 counters and never
// writes to cgroupfs. In "static" or "adaptive" mode, Governor reconciles
// memory.high relative to cgroup memory.max (or ovmd MaxHostMemoryMB) so the
// kernel paces page reclaim before hard memory.max OOM kills trigger.
package cgroup

import (
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// DefaultMountRoot is the unified Linux cgroup v2 hierarchy mount point.
const DefaultMountRoot = "/sys/fs/cgroup"

// Governor modes.
const (
	ModeOff      = "off"
	ModeStatic   = "static"
	ModeAdaptive = "adaptive"
)

const (
	defaultBaseRatio = 0.85
	minAdaptiveRatio = 0.65
	maxAdaptiveRatio = 0.95
	pageAlignBytes   = uint64(4096)
	hysteresisBytes  = uint64(1024 * 1024) // 1 MiB deadband
)

// MemoryEvents captures cumulative counters from cgroup v2 memory.events.
type MemoryEvents struct {
	Low          uint64 `json:"low"`
	High         uint64 `json:"high"`
	Max          uint64 `json:"max"`
	OOM          uint64 `json:"oom"`
	OOMKill      uint64 `json:"oom_kill"`
	OOMGroupKill uint64 `json:"oom_group_kill"`
}

// PSILine captures one stall line ("some" or "full") from memory.pressure.
type PSILine struct {
	Avg10       float64 `json:"avg10"`
	Avg60       float64 `json:"avg60"`
	Avg300      float64 `json:"avg300"`
	TotalMicros uint64  `json:"total_micros"`
}

// Metrics represents a point-in-time snapshot of a cgroup v2 memory controller.
type Metrics struct {
	Available       bool         `json:"available"`
	CgroupDir       string       `json:"cgroup_dir,omitempty"`
	CurrentBytes    uint64       `json:"current_bytes"`
	SwapBytes       uint64       `json:"swap_bytes"`
	HighBytes       uint64       `json:"high_bytes"`
	HighUnlimited   bool         `json:"high_unlimited"`
	MaxBytes        uint64       `json:"max_bytes"`
	MaxUnlimited    bool         `json:"max_unlimited"`
	Utilization     float64      `json:"utilization"`
	Events          MemoryEvents `json:"events"`
	PSISome         PSILine      `json:"psi_some"`
	PSIFull         PSILine      `json:"psi_full"`
	EffectiveRatio  float64      `json:"effective_ratio"`
	ReconcileWrites uint64       `json:"reconcile_writes"`
}

// ReconcileResult reports the outcome of a Governor.Reconcile pass.
type ReconcileResult struct {
	Changed         bool
	TargetHighBytes uint64
	EffectiveRatio  float64
	Metrics         Metrics
}

// Governor reads cgroup v2 memory pressure/throttle telemetry and optionally
// reconciles the memory.high soft-reclaim watermark.
type Governor struct {
	mu        sync.Mutex
	cgroupDir string
	mode      string
	baseRatio float64

	effectiveRatio  float64
	prevHighEvents  uint64
	prevMaxEvents   uint64
	prevOOMEvents   uint64
	hasBaseline     bool
	reconcileWrites uint64
}

// NormalizeMode canonicalizes a cgroup governor mode string.
func NormalizeMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", ModeOff:
		return ModeOff
	case ModeStatic:
		return ModeStatic
	case ModeAdaptive:
		return ModeAdaptive
	default:
		return strings.ToLower(strings.TrimSpace(mode))
	}
}

// IsValidMode reports whether mode is a supported cgroup governor mode.
func IsValidMode(mode string) bool {
	switch NormalizeMode(mode) {
	case ModeOff, ModeStatic, ModeAdaptive:
		return true
	default:
		return false
	}
}

// ResolveSelfCgroupDir resolves the active cgroup v2 directory for the current
// process by combining mountRoot (default /sys/fs/cgroup) with the unified
// hierarchy entry ("0::<subpath>") in procCgroupFile (default /proc/self/cgroup).
// Falls back to mountRoot if procCgroupFile is unavailable or points to "/".
func ResolveSelfCgroupDir(mountRoot, procCgroupFile string) string {
	if mountRoot == "" {
		mountRoot = DefaultMountRoot
	}
	if procCgroupFile == "" {
		procCgroupFile = "/proc/self/cgroup"
	}
	data, err := os.ReadFile(procCgroupFile)
	if err != nil {
		return mountRoot
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "0::") {
			continue
		}
		rel := strings.TrimSpace(strings.TrimPrefix(line, "0::"))
		rel = strings.TrimPrefix(rel, "/")
		if rel == "" {
			return mountRoot
		}
		candidate := filepath.Join(mountRoot, filepath.Clean(rel))
		if _, statErr := os.Stat(filepath.Join(candidate, "memory.current")); statErr == nil {
			return candidate
		}
	}
	return mountRoot
}

// NewGovernor creates a cgroup v2 memory watermark governor rooted at cgroupDir.
// When cgroupDir is empty, ResolveSelfCgroupDir(DefaultMountRoot, "") is used.
// When baseRatio <= 0, 0.85 is used.
func NewGovernor(cgroupDir, mode string, baseRatio float64) *Governor {
	if cgroupDir == "" {
		cgroupDir = ResolveSelfCgroupDir(DefaultMountRoot, "")
	}
	norm := NormalizeMode(mode)
	if !IsValidMode(norm) {
		norm = ModeOff
	}
	if baseRatio <= 0 {
		baseRatio = defaultBaseRatio
	}
	if baseRatio < 0.50 {
		baseRatio = 0.50
	}
	if baseRatio > 0.98 {
		baseRatio = 0.98
	}
	return &Governor{
		cgroupDir:      cgroupDir,
		mode:           norm,
		baseRatio:      baseRatio,
		effectiveRatio: baseRatio,
	}
}

// Mode returns the governor's normalized operating mode.
func (g *Governor) Mode() string {
	if g == nil {
		return ModeOff
	}
	return g.mode
}

// ReadMetrics snapshots cgroup v2 memory usage, watermarks, events, and PSI.
// Never mutates cgroupfs and safely returns Available=false when cgroup v2
// memory accounting is unavailable.
func (g *Governor) ReadMetrics() Metrics {
	if g == nil {
		return Metrics{}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.readMetricsUnlocked()
}

func (g *Governor) readMetricsUnlocked() Metrics {
	currentBytes, ok := readUintFile(filepath.Join(g.cgroupDir, "memory.current"))
	if !ok {
		return Metrics{Available: false}
	}

	swapBytes, _ := readUintFile(filepath.Join(g.cgroupDir, "memory.swap.current"))
	highBytes, highUnlimited, _ := readLimitFile(filepath.Join(g.cgroupDir, "memory.high"))
	maxBytes, maxUnlimited, _ := readLimitFile(filepath.Join(g.cgroupDir, "memory.max"))
	events := parseMemoryEventsFile(filepath.Join(g.cgroupDir, "memory.events"))
	psiSome, psiFull := parseMemoryPressureFile(filepath.Join(g.cgroupDir, "memory.pressure"))

	var util float64
	if !maxUnlimited && maxBytes > 0 {
		util = float64(currentBytes) / float64(maxBytes)
	} else if !highUnlimited && highBytes > 0 {
		util = float64(currentBytes) / float64(highBytes)
	}

	return Metrics{
		Available:       true,
		CgroupDir:       g.cgroupDir,
		CurrentBytes:    currentBytes,
		SwapBytes:       swapBytes,
		HighBytes:       highBytes,
		HighUnlimited:   highUnlimited,
		MaxBytes:        maxBytes,
		MaxUnlimited:    maxUnlimited,
		Utilization:     math.Round(util*10000) / 10000,
		Events:          events,
		PSISome:         psiSome,
		PSIFull:         psiFull,
		EffectiveRatio:  math.Round(g.effectiveRatio*10000) / 10000,
		ReconcileWrites: g.reconcileWrites,
	}
}

// Reconcile evaluates memory.events and PSI stall signals and updates
// memory.high when mode is "static" or "adaptive".
// fallbackCeilingBytes (e.g. MaxHostMemoryMB * 1 MiB) is used as the reference
// ceiling when cgroup memory.max is "max" (unlimited).
func (g *Governor) Reconcile(fallbackCeilingBytes uint64) ReconcileResult {
	if g == nil {
		return ReconcileResult{}
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	cur := g.readMetricsUnlocked()
	if !cur.Available || g.mode == ModeOff {
		return ReconcileResult{
			EffectiveRatio: g.effectiveRatio,
			Metrics:        cur,
		}
	}

	ceiling := cur.MaxBytes
	if (cur.MaxUnlimited || ceiling == 0) && fallbackCeilingBytes > 0 {
		ceiling = fallbackCeilingBytes
	} else if !cur.MaxUnlimited && fallbackCeilingBytes > 0 && fallbackCeilingBytes < ceiling {
		ceiling = fallbackCeilingBytes
	}
	if ceiling < pageAlignBytes*16 {
		return ReconcileResult{
			EffectiveRatio: g.effectiveRatio,
			Metrics:        cur,
		}
	}

	if g.mode == ModeStatic {
		g.effectiveRatio = g.baseRatio
	} else if g.mode == ModeAdaptive {
		if g.hasBaseline {
			deltaMax := diffUint64(cur.Events.Max, g.prevMaxEvents)
			totalOOM := cur.Events.OOM + cur.Events.OOMKill
			deltaOOM := diffUint64(totalOOM, g.prevOOMEvents)

			switch {
			case deltaMax > 0 || deltaOOM > 0:
				// Hard limit hit before soft reclaim paced allocations: lower memory.high
				// so direct reclaim engages earlier and prevents OOM kills.
				g.effectiveRatio -= 0.05
			case cur.PSIFull.Avg10 >= 5.0 && float64(cur.CurrentBytes) < 0.92*float64(ceiling):
				// Severe full-stall thrashing while still comfortably below ceiling:
				// relax memory.high slightly to reduce premature reclaim stalls.
				g.effectiveRatio += 0.03
			case cur.PSISome.Avg10 < 1.0 && deltaMax == 0:
				// Calm pressure: step gradually back toward configured baseRatio.
				if g.effectiveRatio < g.baseRatio {
					g.effectiveRatio = math.Min(g.baseRatio, g.effectiveRatio+0.01)
				} else if g.effectiveRatio > g.baseRatio {
					g.effectiveRatio = math.Max(g.baseRatio, g.effectiveRatio-0.01)
				}
			}
			if g.effectiveRatio < minAdaptiveRatio {
				g.effectiveRatio = minAdaptiveRatio
			}
			if g.effectiveRatio > maxAdaptiveRatio {
				g.effectiveRatio = maxAdaptiveRatio
			}
		}
		g.prevHighEvents = cur.Events.High
		g.prevMaxEvents = cur.Events.Max
		g.prevOOMEvents = cur.Events.OOM + cur.Events.OOMKill
		g.hasBaseline = true
	}

	target := (uint64(float64(ceiling)*g.effectiveRatio) / pageAlignBytes) * pageAlignBytes
	if target < pageAlignBytes {
		target = pageAlignBytes
	}

	changed := false
	if cur.HighUnlimited || absDiffUint64(cur.HighBytes, target) >= hysteresisBytes {
		if err := os.WriteFile(filepath.Join(g.cgroupDir, "memory.high"), []byte(strconv.FormatUint(target, 10)), 0644); err == nil {
			changed = true
			g.reconcileWrites++
		}
	}

	after := g.readMetricsUnlocked()
	return ReconcileResult{
		Changed:         changed,
		TargetHighBytes: target,
		EffectiveRatio:  after.EffectiveRatio,
		Metrics:         after,
	}
}

func readUintFile(path string) (uint64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

func readLimitFile(path string) (val uint64, unlimited bool, ok bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false, false
	}
	s := strings.TrimSpace(string(b))
	if s == "max" {
		return 0, true, true
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, false, false
	}
	return v, false, true
}

// ParseMemoryEvents parses the contents of a cgroup v2 memory.events file.
func ParseMemoryEvents(raw string) MemoryEvents {
	var ev MemoryEvents
	for _, line := range strings.Split(raw, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		v, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "low":
			ev.Low = v
		case "high":
			ev.High = v
		case "max":
			ev.Max = v
		case "oom":
			ev.OOM = v
		case "oom_kill":
			ev.OOMKill = v
		case "oom_group_kill":
			ev.OOMGroupKill = v
		}
	}
	return ev
}

func parseMemoryEventsFile(path string) MemoryEvents {
	b, err := os.ReadFile(path)
	if err != nil {
		return MemoryEvents{}
	}
	return ParseMemoryEvents(string(b))
}

// ParseMemoryPressure parses the contents of a cgroup v2 memory.pressure (PSI) file.
func ParseMemoryPressure(raw string) (some PSILine, full PSILine) {
	for _, line := range strings.Split(raw, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		var parsed PSILine
		for _, kv := range fields[1:] {
			parts := strings.SplitN(kv, "=", 2)
			if len(parts) != 2 {
				continue
			}
			switch parts[0] {
			case "avg10":
				parsed.Avg10, _ = strconv.ParseFloat(parts[1], 64)
			case "avg60":
				parsed.Avg60, _ = strconv.ParseFloat(parts[1], 64)
			case "avg300":
				parsed.Avg300, _ = strconv.ParseFloat(parts[1], 64)
			case "total":
				parsed.TotalMicros, _ = strconv.ParseUint(parts[1], 10, 64)
			}
		}
		switch fields[0] {
		case "some":
			some = parsed
		case "full":
			full = parsed
		}
	}
	return some, full
}

func parseMemoryPressureFile(path string) (PSILine, PSILine) {
	b, err := os.ReadFile(path)
	if err != nil {
		return PSILine{}, PSILine{}
	}
	return ParseMemoryPressure(string(b))
}

func diffUint64(cur, prev uint64) uint64 {
	if cur > prev {
		return cur - prev
	}
	return 0
}

func absDiffUint64(a, b uint64) uint64 {
	if a >= b {
		return a - b
	}
	return b - a
}
