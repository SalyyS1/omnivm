// Package ioqos implements a hierarchical continuous-time token-bucket rate
// governor for multi-tenant virtio-blk and virtio-net I/O isolation.
//
// It enforces two-tier admission (per-instance child buckets nested beneath
// per-tenant aggregate buckets), computes weighted max-min water-filling fair
// shares across active tenant microVMs, synthesizes Cloud Hypervisor
// RateLimiterConfig payloads, and provides context-aware io.Reader/io.Writer
// stream shapers for L4/HTTP router traffic.
package ioqos

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Governor operating modes.
const (
	ModeOff       = "off"
	ModeStatic    = "static"
	ModeFairShare = "fairshare"

	DefaultTenantID       = "default"
	DefaultBurstRatio     = 1.5
	DefaultRefillWindowMs = 100
)

// Channel identifies a metered virtio I/O resource dimension.
type Channel string

const (
	ChannelBlkBandwidth Channel = "blk_bandwidth"
	ChannelBlkOps       Channel = "blk_ops"
	ChannelNetBandwidth Channel = "net_bandwidth"
	ChannelNetOps       Channel = "net_ops"
)

const bytesPerMiB = 1024 * 1024

// TenantPolicy defines aggregate token-bucket ceilings for a tenant across all
// of its resident/running microVMs. Zero values mean unlimited on that axis.
type TenantPolicy struct {
	TenantID   string  `json:"tenant_id"`
	MaxBlkMBps int     `json:"max_blk_mbps"`
	MaxBlkIOPS int     `json:"max_blk_iops"`
	MaxNetMBps int     `json:"max_net_mbps"`
	MaxNetPPS  int     `json:"max_net_pps"`
	BurstRatio float64 `json:"burst_ratio,omitempty"`
}

// Validate checks that a TenantPolicy contains non-negative limits.
func (p TenantPolicy) Validate() error {
	if strings.TrimSpace(p.TenantID) == "" {
		return errors.New("tenant_id must not be empty")
	}
	if p.MaxBlkMBps < 0 || p.MaxBlkIOPS < 0 || p.MaxNetMBps < 0 || p.MaxNetPPS < 0 {
		return errors.New("tenant QoS limits must be non-negative")
	}
	if p.BurstRatio < 0 || p.BurstRatio > 16.0 {
		return fmt.Errorf("burst_ratio must be between 0 and 16.0, got %v", p.BurstRatio)
	}
	return nil
}

func (p TenantPolicy) effectiveBurstRatio() float64 {
	if p.BurstRatio >= 1.0 {
		return p.BurstRatio
	}
	return DefaultBurstRatio
}

// InstancePolicy defines optional per-microVM I/O rate limits and proportional
// fair-share weight within its tenant.
type InstancePolicy struct {
	Weight     int     `json:"weight,omitempty"`
	MaxBlkMBps int     `json:"max_blk_mbps,omitempty"`
	MaxBlkIOPS int     `json:"max_blk_iops,omitempty"`
	MaxNetMBps int     `json:"max_net_mbps,omitempty"`
	MaxNetPPS  int     `json:"max_net_pps,omitempty"`
	BurstRatio float64 `json:"burst_ratio,omitempty"`
}

// Validate checks per-instance QoS invariants.
func (p *InstancePolicy) Validate() error {
	if p == nil {
		return nil
	}
	if p.Weight < 0 || p.Weight > 1024 {
		return fmt.Errorf("io_qos.weight must be between 0 and 1024, got %d", p.Weight)
	}
	if p.MaxBlkMBps < 0 || p.MaxBlkIOPS < 0 || p.MaxNetMBps < 0 || p.MaxNetPPS < 0 {
		return errors.New("io_qos rate ceilings must be non-negative")
	}
	if p.BurstRatio < 0 || p.BurstRatio > 16.0 {
		return fmt.Errorf("io_qos.burst_ratio must be between 0 and 16.0, got %v", p.BurstRatio)
	}
	return nil
}

// IsZero reports whether no custom per-instance limits or weight are set.
func (p *InstancePolicy) IsZero() bool {
	if p == nil {
		return true
	}
	return p.Weight == 0 && p.MaxBlkMBps == 0 && p.MaxBlkIOPS == 0 &&
		p.MaxNetMBps == 0 && p.MaxNetPPS == 0 && p.BurstRatio == 0
}

// InstanceDemand captures an active instance's tenant membership, weight, and per-VM caps.
type InstanceDemand struct {
	InstanceID string
	TenantID   string
	VCPUs      int
	Policy     InstancePolicy
}

// EffectiveAllocation holds the resolved steady-state and burst token-bucket
// parameters assigned to a single active microVM after tenant water-filling.
type EffectiveAllocation struct {
	InstanceID      string `json:"instance_id"`
	TenantID        string `json:"tenant_id"`
	Weight          int    `json:"weight"`
	BlkBytesPerSec  uint64 `json:"blk_bytes_per_sec"`
	BlkBurstBytes   uint64 `json:"blk_burst_bytes"`
	BlkIOPS         uint64 `json:"blk_iops"`
	BlkBurstIOPS    uint64 `json:"blk_burst_iops"`
	NetBytesPerSec  uint64 `json:"net_bytes_per_sec"`
	NetBurstBytes   uint64 `json:"net_burst_bytes"`
	NetPPS          uint64 `json:"net_pps"`
	NetBurstPPS     uint64 `json:"net_burst_pps"`
}

// HasDiskLimits reports whether any virtio-blk rate ceiling applies.
func (a EffectiveAllocation) HasDiskLimits() bool {
	return a.BlkBytesPerSec > 0 || a.BlkIOPS > 0
}

// HasNetLimits reports whether any virtio-net rate ceiling applies.
func (a EffectiveAllocation) HasNetLimits() bool {
	return a.NetBytesPerSec > 0 || a.NetPPS > 0
}

// TokenBucket implements a continuous-time token bucket with separate steady-state
// refill rate (tokens/sec) and maximum burst capacity.
type TokenBucket struct {
	ratePerSec float64
	burstCap   float64
	tokens     float64
	lastRefill time.Time
}

// NewTokenBucket creates an initialized full TokenBucket. A zero ratePerSec
// represents an unlimited bucket.
func NewTokenBucket(ratePerSec, burstCap float64, now time.Time) *TokenBucket {
	if ratePerSec <= 0 {
		return &TokenBucket{}
	}
	if burstCap < ratePerSec {
		burstCap = ratePerSec
	}
	if now.IsZero() {
		now = time.Now()
	}
	return &TokenBucket{
		ratePerSec: ratePerSec,
		burstCap:   burstCap,
		tokens:     burstCap,
		lastRefill: now,
	}
}

// Reconfigure updates a bucket's steady-state rate and burst capacity while
// preserving accumulated token ratio and monotonic refill progress.
func (tb *TokenBucket) Reconfigure(ratePerSec, burstCap float64, now time.Time) {
	if now.IsZero() {
		now = time.Now()
	}
	tb.advance(now)
	if ratePerSec <= 0 {
		tb.ratePerSec = 0
		tb.burstCap = 0
		tb.tokens = 0
		tb.lastRefill = now
		return
	}
	if burstCap < ratePerSec {
		burstCap = ratePerSec
	}
	if tb.ratePerSec <= 0 {
		tb.tokens = burstCap
	} else if tb.tokens > burstCap {
		tb.tokens = burstCap
	}
	tb.ratePerSec = ratePerSec
	tb.burstCap = burstCap
}

func (tb *TokenBucket) advance(now time.Time) {
	if tb.ratePerSec <= 0 {
		return
	}
	if tb.lastRefill.IsZero() {
		tb.lastRefill = now
		tb.tokens = tb.burstCap
		return
	}
	elapsed := now.Sub(tb.lastRefill).Seconds()
	if elapsed > 0 {
		tb.tokens = math.Min(tb.burstCap, tb.tokens+elapsed*tb.ratePerSec)
		tb.lastRefill = now
	}
}

// Preview checks whether amount tokens are available at time now without
// mutating token balance (beyond lazy refill). When insufficient tokens remain
// (and amount <= burstCap), retryAfter reports the exact duration until refill.
func (tb *TokenBucket) Preview(amount uint64, now time.Time) (allowed bool, retryAfter time.Duration) {
	if tb == nil || tb.ratePerSec <= 0 || amount == 0 {
		return true, 0
	}
	tb.advance(now)
	need := float64(amount)
	if tb.tokens >= need {
		return true, 0
	}
	// If a single request exceeds burstCap, cap need at burstCap only if caller
	// chunks I/O; otherwise report wait time to accumulate need (or full bucket).
	deficit := need - tb.tokens
	waitSec := deficit / tb.ratePerSec
	waitDur := time.Duration(math.Ceil(waitSec * float64(time.Second)))
	if waitDur < time.Microsecond {
		waitDur = time.Microsecond
	}
	return false, waitDur
}

func (tb *TokenBucket) deduct(amount uint64) {
	if tb == nil || tb.ratePerSec <= 0 || amount == 0 {
		return
	}
	tb.tokens -= float64(amount)
	if tb.tokens < 0 {
		tb.tokens = 0
	}
}

// Decision describes the outcome of a hierarchical token-bucket admission check.
type Decision struct {
	Allowed    bool          `json:"allowed"`
	LimitedBy  string        `json:"limited_by,omitempty"` // "instance" or "tenant"
	RetryAfter time.Duration `json:"retry_after_ns,omitempty"`
}

type bucketSet struct {
	blkBW  *TokenBucket
	blkOps *TokenBucket
	netBW  *TokenBucket
	netOps *TokenBucket
}

func (bs *bucketSet) get(ch Channel) *TokenBucket {
	if bs == nil {
		return nil
	}
	switch ch {
	case ChannelBlkBandwidth:
		return bs.blkBW
	case ChannelBlkOps:
		return bs.blkOps
	case ChannelNetBandwidth:
		return bs.netBW
	case ChannelNetOps:
		return bs.netOps
	default:
		return nil
	}
}

// Metrics exposes atomic throughput, burst-throttle, and fairness counters.
type Metrics struct {
	Mode                   string  `json:"mode"`
	ActiveTenants          int     `json:"active_tenants"`
	ActiveInstances        int     `json:"active_instances"`
	FairnessIndex          float64 `json:"fairness_index"`
	BlkAdmittedBytes       uint64  `json:"blk_admitted_bytes"`
	BlkAdmittedOps         uint64  `json:"blk_admitted_ops"`
	NetAdmittedBytes       uint64  `json:"net_admitted_bytes"`
	NetAdmittedOps         uint64  `json:"net_admitted_ops"`
	InstanceBlkThrottles   uint64  `json:"instance_blk_throttles"`
	InstanceNetThrottles   uint64  `json:"instance_net_throttles"`
	TenantBlkThrottles     uint64  `json:"tenant_blk_throttles"`
	TenantNetThrottles     uint64  `json:"tenant_net_throttles"`
	ThrottleWaitMicros     uint64  `json:"throttle_wait_micros"`
	RebalancePasses        uint64  `json:"rebalance_passes"`
}

// StatusSnapshot provides a complete JSON-serializable view of the QoS governor.
type StatusSnapshot struct {
	Mode        string                `json:"mode"`
	DefaultPolicy TenantPolicy        `json:"default_policy"`
	Tenants     []TenantPolicy        `json:"tenants"`
	Allocations []EffectiveAllocation `json:"allocations"`
	Metrics     Metrics               `json:"metrics"`
}

// Config configures a new Governor.
type Config struct {
	Mode          string
	DefaultPolicy TenantPolicy
}

// Governor coordinates per-tenant and per-instance token-bucket QoS policies.
type Governor struct {
	mu            sync.Mutex
	mode          string
	defaultPolicy TenantPolicy
	tenants       map[string]TenantPolicy
	tenantBuckets map[string]*bucketSet
	demands       map[string]InstanceDemand
	allocations   map[string]EffectiveAllocation
	instBuckets   map[string]*bucketSet
	fairnessIndex float64

	blkAdmittedBytes     uint64
	blkAdmittedOps       uint64
	netAdmittedBytes     uint64
	netAdmittedOps       uint64
	instanceBlkThrottles uint64
	instanceNetThrottles uint64
	tenantBlkThrottles   uint64
	tenantNetThrottles   uint64
	throttleWaitMicros   uint64
	rebalancePasses      uint64
}

// NormalizeTenantID canonicalizes an optional tenant identifier.
func NormalizeTenantID(tenantID string) string {
	t := strings.TrimSpace(tenantID)
	if t == "" {
		return DefaultTenantID
	}
	return t
}

// NewGovernor constructs a multi-tenant I/O QoS governor.
func NewGovernor(cfg Config) *Governor {
	mode := strings.ToLower(strings.TrimSpace(cfg.Mode))
	switch mode {
	case ModeStatic, ModeFairShare:
	default:
		mode = ModeOff
	}
	defPol := cfg.DefaultPolicy
	defPol.TenantID = DefaultTenantID
	if defPol.BurstRatio == 0 {
		defPol.BurstRatio = DefaultBurstRatio
	}

	g := &Governor{
		mode:          mode,
		defaultPolicy: defPol,
		tenants:       make(map[string]TenantPolicy),
		tenantBuckets: make(map[string]*bucketSet),
		demands:       make(map[string]InstanceDemand),
		allocations:   make(map[string]EffectiveAllocation),
		instBuckets:   make(map[string]*bucketSet),
		fairnessIndex: 1.0,
	}
	if hasTenantLimits(defPol) {
		g.setTenantLocked(defPol, time.Now())
	}
	return g
}

func hasTenantLimits(p TenantPolicy) bool {
	return p.MaxBlkMBps > 0 || p.MaxBlkIOPS > 0 || p.MaxNetMBps > 0 || p.MaxNetPPS > 0
}

// Mode returns the governor's active mode ("off", "static", or "fairshare").
func (g *Governor) Mode() string {
	if g == nil {
		return ModeOff
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.mode
}

// SetMode switches the governor mode and triggers a rebalance across active instances.
func (g *Governor) SetMode(mode string) error {
	norm := strings.ToLower(strings.TrimSpace(mode))
	switch norm {
	case "", ModeOff:
		norm = ModeOff
	case ModeStatic, ModeFairShare:
	default:
		return fmt.Errorf("unsupported io_qos_governor mode %q: expected off, static, or fairshare", mode)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.mode = norm
	g.rebalanceAllLocked(time.Now())
	return nil
}

// SetTenantPolicy configures or updates aggregate token-bucket ceilings for a tenant
// and immediately rebalances all active instances belonging to that tenant.
func (g *Governor) SetTenantPolicy(pol TenantPolicy) error {
	pol.TenantID = NormalizeTenantID(pol.TenantID)
	if err := pol.Validate(); err != nil {
		return err
	}
	now := time.Now()
	g.mu.Lock()
	defer g.mu.Unlock()
	g.setTenantLocked(pol, now)
	g.rebalanceAllLocked(now)
	return nil
}

// DeleteTenantPolicy removes an explicit tenant override (falling back to DefaultPolicy).
func (g *Governor) DeleteTenantPolicy(tenantID string) bool {
	tid := NormalizeTenantID(tenantID)
	now := time.Now()
	g.mu.Lock()
	defer g.mu.Unlock()
	_, existed := g.tenants[tid]
	delete(g.tenants, tid)
	delete(g.tenantBuckets, tid)
	g.rebalanceAllLocked(now)
	return existed
}

func (g *Governor) setTenantLocked(pol TenantPolicy, now time.Time) {
	if pol.BurstRatio == 0 {
		pol.BurstRatio = DefaultBurstRatio
	}
	g.tenants[pol.TenantID] = pol
	bs := g.tenantBuckets[pol.TenantID]
	if bs == nil {
		bs = &bucketSet{
			blkBW:  &TokenBucket{},
			blkOps: &TokenBucket{},
			netBW:  &TokenBucket{},
			netOps: &TokenBucket{},
		}
		g.tenantBuckets[pol.TenantID] = bs
	}
	br := pol.effectiveBurstRatio()
	blkBps := float64(pol.MaxBlkMBps) * bytesPerMiB
	blkOps := float64(pol.MaxBlkIOPS)
	netBps := float64(pol.MaxNetMBps) * bytesPerMiB
	netPps := float64(pol.MaxNetPPS)
	bs.blkBW.Reconfigure(blkBps, blkBps*br, now)
	bs.blkOps.Reconfigure(blkOps, blkOps*br, now)
	bs.netBW.Reconfigure(netBps, netBps*br, now)
	bs.netOps.Reconfigure(netPps, netPps*br, now)
}

func (g *Governor) effectiveTenantPolicyLocked(tenantID string) TenantPolicy {
	tid := NormalizeTenantID(tenantID)
	if p, ok := g.tenants[tid]; ok {
		return p
	}
	def := g.defaultPolicy
	def.TenantID = tid
	return def
}

func (g *Governor) ensureTenantBucketLocked(tenantID string, now time.Time) *bucketSet {
	tid := NormalizeTenantID(tenantID)
	if bs, ok := g.tenantBuckets[tid]; ok {
		return bs
	}
	pol := g.effectiveTenantPolicyLocked(tid)
	if !hasTenantLimits(pol) {
		return nil
	}
	g.setTenantLocked(pol, now)
	return g.tenantBuckets[tid]
}

// RegisterInstance registers or updates an active instance's demand and returns
// its resolved EffectiveAllocation.
func (g *Governor) RegisterInstance(d InstanceDemand) EffectiveAllocation {
	d.TenantID = NormalizeTenantID(d.TenantID)
	now := time.Now()
	g.mu.Lock()
	defer g.mu.Unlock()
	g.demands[d.InstanceID] = d
	g.ensureTenantBucketLocked(d.TenantID, now)
	g.rebalanceAllLocked(now)
	return g.allocations[d.InstanceID]
}

// UnregisterInstance removes an instance when it pauses or stops and redistributes
// tenant bandwidth across remaining active instances.
func (g *Governor) UnregisterInstance(instanceID string) {
	now := time.Now()
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.demands[instanceID]; !ok {
		return
	}
	delete(g.demands, instanceID)
	delete(g.allocations, instanceID)
	delete(g.instBuckets, instanceID)
	g.rebalanceAllLocked(now)
}

// AllocationFor returns the current resolved allocation for an instance, or
// computes a preview allocation if the instance is not yet registered.
func (g *Governor) AllocationFor(d InstanceDemand) EffectiveAllocation {
	d.TenantID = NormalizeTenantID(d.TenantID)
	g.mu.Lock()
	defer g.mu.Unlock()
	if alloc, ok := g.allocations[d.InstanceID]; ok {
		return alloc
	}
	tPol := g.effectiveTenantPolicyLocked(d.TenantID)
	var peers []InstanceDemand
	for _, existing := range g.demands {
		if existing.TenantID == d.TenantID && existing.InstanceID != d.InstanceID {
			peers = append(peers, existing)
		}
	}
	peers = append(peers, d)
	allocs := ComputeTenantAllocations(g.mode, tPol, peers)
	for _, a := range allocs {
		if a.InstanceID == d.InstanceID {
			return a
		}
	}
	return EffectiveAllocation{InstanceID: d.InstanceID, TenantID: d.TenantID, Weight: effectiveWeight(d)}
}

func effectiveWeight(d InstanceDemand) int {
	if d.Policy.Weight > 0 {
		return d.Policy.Weight
	}
	if d.VCPUs > 0 {
		return d.VCPUs
	}
	return 1
}

// ComputeTenantAllocations resolves per-instance steady-state and burst limits
// for all active instances in a tenant. In "fairshare" mode, tenant ceilings are
// partitioned via iterative weighted max-min water-filling; in "static" mode,
// each instance receives min(instance_cap, tenant_cap) while aggregate contention
// is still policed by the parent tenant bucket; in "off" mode, only explicit
// per-instance caps apply.
func ComputeTenantAllocations(mode string, tenant TenantPolicy, instances []InstanceDemand) []EffectiveAllocation {
	if len(instances) == 0 {
		return nil
	}
	sorted := make([]InstanceDemand, len(instances))
	copy(sorted, instances)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].InstanceID < sorted[j].InstanceID
	})

	weights := make([]int, len(sorted))
	blkCaps := make([]uint64, len(sorted))
	blkOpsCaps := make([]uint64, len(sorted))
	netCaps := make([]uint64, len(sorted))
	netPpsCaps := make([]uint64, len(sorted))

	for i, d := range sorted {
		weights[i] = effectiveWeight(d)
		if d.Policy.MaxBlkMBps > 0 {
			blkCaps[i] = uint64(d.Policy.MaxBlkMBps) * bytesPerMiB
		}
		if d.Policy.MaxBlkIOPS > 0 {
			blkOpsCaps[i] = uint64(d.Policy.MaxBlkIOPS)
		}
		if d.Policy.MaxNetMBps > 0 {
			netCaps[i] = uint64(d.Policy.MaxNetMBps) * bytesPerMiB
		}
		if d.Policy.MaxNetPPS > 0 {
			netPpsCaps[i] = uint64(d.Policy.MaxNetPPS)
		}
	}

	var tenantBlkBps, tenantBlkOps, tenantNetBps, tenantNetPps uint64
	if mode != ModeOff {
		if tenant.MaxBlkMBps > 0 {
			tenantBlkBps = uint64(tenant.MaxBlkMBps) * bytesPerMiB
		}
		if tenant.MaxBlkIOPS > 0 {
			tenantBlkOps = uint64(tenant.MaxBlkIOPS)
		}
		if tenant.MaxNetMBps > 0 {
			tenantNetBps = uint64(tenant.MaxNetMBps) * bytesPerMiB
		}
		if tenant.MaxNetPPS > 0 {
			tenantNetPps = uint64(tenant.MaxNetPPS)
		}
	}

	var blkShares, blkOpsShares, netShares, netPpsShares []uint64
	if mode == ModeFairShare {
		blkShares = waterFillWeighted(tenantBlkBps, weights, blkCaps)
		blkOpsShares = waterFillWeighted(tenantBlkOps, weights, blkOpsCaps)
		netShares = waterFillWeighted(tenantNetBps, weights, netCaps)
		netPpsShares = waterFillWeighted(tenantNetPps, weights, netPpsCaps)
	} else {
		blkShares = clampStatic(tenantBlkBps, blkCaps)
		blkOpsShares = clampStatic(tenantBlkOps, blkOpsCaps)
		netShares = clampStatic(tenantNetBps, netCaps)
		netPpsShares = clampStatic(tenantNetPps, netPpsCaps)
	}

	out := make([]EffectiveAllocation, len(sorted))
	tBurst := tenant.effectiveBurstRatio()
	for i, d := range sorted {
		br := tBurst
		if d.Policy.BurstRatio >= 1.0 {
			br = d.Policy.BurstRatio
		}
		out[i] = EffectiveAllocation{
			InstanceID:     d.InstanceID,
			TenantID:       NormalizeTenantID(d.TenantID),
			Weight:         weights[i],
			BlkBytesPerSec: blkShares[i],
			BlkBurstBytes:  scaleBurst(blkShares[i], br),
			BlkIOPS:        blkOpsShares[i],
			BlkBurstIOPS:   scaleBurst(blkOpsShares[i], br),
			NetBytesPerSec: netShares[i],
			NetBurstBytes:  scaleBurst(netShares[i], br),
			NetPPS:         netPpsShares[i],
			NetBurstPPS:    scaleBurst(netPpsShares[i], br),
		}
	}
	return out
}

func scaleBurst(rate uint64, ratio float64) uint64 {
	if rate == 0 {
		return 0
	}
	if ratio < 1.0 {
		ratio = 1.0
	}
	v := uint64(math.Round(float64(rate) * ratio))
	if v < rate {
		return rate
	}
	return v
}

func clampStatic(tenantCap uint64, instCaps []uint64) []uint64 {
	out := make([]uint64, len(instCaps))
	for i, c := range instCaps {
		switch {
		case c > 0 && tenantCap > 0:
			if c < tenantCap {
				out[i] = c
			} else {
				out[i] = tenantCap
			}
		case c > 0:
			out[i] = c
		case tenantCap > 0:
			out[i] = tenantCap
		}
	}
	return out
}

// waterFillWeighted computes exact weighted max-min fair allocations across n
// consumers subject to an aggregate capacity and optional per-consumer ceilings.
func waterFillWeighted(capacity uint64, weights []int, caps []uint64) []uint64 {
	n := len(weights)
	out := make([]uint64, n)
	if capacity == 0 {
		copy(out, caps)
		return out
	}

	remaining := float64(capacity)
	settled := make([]bool, n)

	for {
		totalWeight := 0
		for i := 0; i < n; i++ {
			if !settled[i] {
				totalWeight += weights[i]
			}
		}
		if totalWeight == 0 || remaining <= 0 {
			break
		}

		clampedAny := false
		for i := 0; i < n; i++ {
			if settled[i] {
				continue
			}
			share := remaining * float64(weights[i]) / float64(totalWeight)
			if caps[i] > 0 && share >= float64(caps[i]) {
				out[i] = caps[i]
				remaining -= float64(caps[i])
				settled[i] = true
				clampedAny = true
			}
		}
		if !clampedAny {
			// Distribute remaining capacity deterministically without rounding loss.
			var distributed uint64
			lastUnsettled := -1
			for i := 0; i < n; i++ {
				if settled[i] {
					continue
				}
				lastUnsettled = i
				share := uint64(math.Floor(remaining * float64(weights[i]) / float64(totalWeight)))
				if share == 0 {
					share = 1
				}
				out[i] = share
				distributed += share
			}
			remInt := uint64(math.Max(0, math.Round(remaining)))
			if lastUnsettled >= 0 && distributed < remInt {
				out[lastUnsettled] += remInt - distributed
			}
			break
		}
	}
	return out
}

// ComputeWeightedJainsIndex computes Jain's fairness index over normalized
// weight-adjusted shares (x_i / w_i). Returns 1.0 when <= 1 active consumers exist.
func ComputeWeightedJainsIndex(shares []float64, weights []int) float64 {
	if len(shares) <= 1 || len(shares) != len(weights) {
		return 1.0
	}
	var sum float64
	var sumSq float64
	count := 0
	for i, x := range shares {
		if x <= 0 {
			continue
		}
		w := float64(weights[i])
		if w <= 0 {
			w = 1
		}
		norm := x / w
		sum += norm
		sumSq += norm * norm
		count++
	}
	if count <= 1 || sumSq == 0 {
		return 1.0
	}
	j := (sum * sum) / (float64(count) * sumSq)
	if j > 1.0 {
		return 1.0
	}
	return j
}

func (g *Governor) rebalanceAllLocked(now time.Time) {
	byTenant := make(map[string][]InstanceDemand)
	for _, d := range g.demands {
		tid := NormalizeTenantID(d.TenantID)
		byTenant[tid] = append(byTenant[tid], d)
	}

	var fairnessSum float64
	var fairnessTenants int

	for tid, list := range byTenant {
		tPol := g.effectiveTenantPolicyLocked(tid)
		g.ensureTenantBucketLocked(tid, now)
		allocs := ComputeTenantAllocations(g.mode, tPol, list)

		var shares []float64
		var weights []int
		for _, a := range allocs {
			g.allocations[a.InstanceID] = a
			bs := g.instBuckets[a.InstanceID]
			if bs == nil {
				bs = &bucketSet{
					blkBW:  &TokenBucket{},
					blkOps: &TokenBucket{},
					netBW:  &TokenBucket{},
					netOps: &TokenBucket{},
				}
				g.instBuckets[a.InstanceID] = bs
			}
			bs.blkBW.Reconfigure(float64(a.BlkBytesPerSec), float64(a.BlkBurstBytes), now)
			bs.blkOps.Reconfigure(float64(a.BlkIOPS), float64(a.BlkBurstIOPS), now)
			bs.netBW.Reconfigure(float64(a.NetBytesPerSec), float64(a.NetBurstBytes), now)
			bs.netOps.Reconfigure(float64(a.NetPPS), float64(a.NetBurstPPS), now)

			// Use primary constrained bandwidth channel for Jain's fairness index.
			switch {
			case a.BlkBytesPerSec > 0 && list[0].Policy.MaxBlkMBps == 0:
				shares = append(shares, float64(a.BlkBytesPerSec))
				weights = append(weights, a.Weight)
			case a.NetBytesPerSec > 0 && list[0].Policy.MaxNetMBps == 0:
				shares = append(shares, float64(a.NetBytesPerSec))
				weights = append(weights, a.Weight)
			}
		}
		if len(shares) > 1 {
			fairnessSum += ComputeWeightedJainsIndex(shares, weights)
			fairnessTenants++
		}
	}

	if fairnessTenants > 0 {
		g.fairnessIndex = fairnessSum / float64(fairnessTenants)
	} else {
		g.fairnessIndex = 1.0
	}
	atomic.AddUint64(&g.rebalancePasses, 1)
}

// Admit performs an atomic two-level token-bucket check across the instance's
// child bucket and its parent tenant's aggregate bucket. Tokens are deducted
// from both buckets if and only if both permit the I/O operation.
func (g *Governor) Admit(tenantID, instanceID string, ch Channel, amount uint64, now time.Time) Decision {
	if g == nil || amount == 0 {
		return Decision{Allowed: true}
	}
	if now.IsZero() {
		now = time.Now()
	}

	g.mu.Lock()
	if g.mode == ModeOff {
		// Even in "off" mode, honor explicit per-instance caps if registered.
		ibs := g.instBuckets[instanceID]
		if ib := ibs.get(ch); ib != nil && ib.ratePerSec > 0 {
			ok, wait := ib.Preview(amount, now)
			if !ok {
				g.mu.Unlock()
				g.recordThrottle("instance", ch, wait)
				return Decision{Allowed: false, LimitedBy: "instance", RetryAfter: wait}
			}
			ib.deduct(amount)
		}
		g.mu.Unlock()
		g.recordAdmitted(ch, amount)
		return Decision{Allowed: true}
	}

	tid := NormalizeTenantID(tenantID)
	ibs := g.instBuckets[instanceID]
	tbs := g.ensureTenantBucketLocked(tid, now)

	ib := ibs.get(ch)
	tb := tbs.get(ch)

	instOK, instWait := ib.Preview(amount, now)
	tenOK, tenWait := tb.Preview(amount, now)

	if !instOK || !tenOK {
		g.mu.Unlock()
		wait := instWait
		scope := "instance"
		if !tenOK && (tenWait >= instWait || instOK) {
			wait = tenWait
			scope = "tenant"
		}
		g.recordThrottle(scope, ch, wait)
		return Decision{Allowed: false, LimitedBy: scope, RetryAfter: wait}
	}

	ib.deduct(amount)
	tb.deduct(amount)
	g.mu.Unlock()

	g.recordAdmitted(ch, amount)
	return Decision{Allowed: true}
}

func (g *Governor) recordAdmitted(ch Channel, amount uint64) {
	switch ch {
	case ChannelBlkBandwidth:
		atomic.AddUint64(&g.blkAdmittedBytes, amount)
	case ChannelBlkOps:
		atomic.AddUint64(&g.blkAdmittedOps, amount)
	case ChannelNetBandwidth:
		atomic.AddUint64(&g.netAdmittedBytes, amount)
	case ChannelNetOps:
		atomic.AddUint64(&g.netAdmittedOps, amount)
	}
}

func (g *Governor) recordThrottle(scope string, ch Channel, wait time.Duration) {
	isBlk := ch == ChannelBlkBandwidth || ch == ChannelBlkOps
	if scope == "tenant" {
		if isBlk {
			atomic.AddUint64(&g.tenantBlkThrottles, 1)
		} else {
			atomic.AddUint64(&g.tenantNetThrottles, 1)
		}
	} else {
		if isBlk {
			atomic.AddUint64(&g.instanceBlkThrottles, 1)
		} else {
			atomic.AddUint64(&g.instanceNetThrottles, 1)
		}
	}
	if us := uint64(wait / time.Microsecond); us > 0 {
		atomic.AddUint64(&g.throttleWaitMicros, us)
	}
}

// Wait blocks until amount tokens are admitted on ch or ctx is canceled.
func (g *Governor) Wait(ctx context.Context, tenantID, instanceID string, ch Channel, amount uint64) error {
	for {
		dec := g.Admit(tenantID, instanceID, ch, amount, time.Now())
		if dec.Allowed {
			return nil
		}
		timer := time.NewTimer(dec.RetryAfter)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

type shapedReader struct {
	ctx        context.Context
	r          io.Reader
	gov        *Governor
	tenantID   string
	instanceID string
	ch         Channel
	maxChunk   int
}

// ShapeReader wraps r with hierarchical token-bucket bandwidth pacing and packet/op accounting.
func (g *Governor) ShapeReader(ctx context.Context, r io.Reader, tenantID, instanceID string, bwCh, opsCh Channel) io.Reader {
	if g == nil || r == nil {
		return r
	}
	return &shapedReader{
		ctx:        ctx,
		r:          r,
		gov:        g,
		tenantID:   NormalizeTenantID(tenantID),
		instanceID: instanceID,
		ch:         bwCh,
		maxChunk:   32 * 1024,
	}
}

func (sr *shapedReader) Read(p []byte) (int, error) {
	if len(p) > sr.maxChunk {
		p = p[:sr.maxChunk]
	}
	n, err := sr.r.Read(p)
	if n > 0 {
		if wErr := sr.gov.Wait(sr.ctx, sr.tenantID, sr.instanceID, sr.ch, uint64(n)); wErr != nil {
			return n, wErr
		}
		if sr.ch == ChannelNetBandwidth {
			_ = sr.gov.Wait(sr.ctx, sr.tenantID, sr.instanceID, ChannelNetOps, 1)
		} else if sr.ch == ChannelBlkBandwidth {
			_ = sr.gov.Wait(sr.ctx, sr.tenantID, sr.instanceID, ChannelBlkOps, 1)
		}
	}
	return n, err
}

// BuildCHRateLimiter constructs Cloud Hypervisor's RateLimiterConfig JSON map
// (bandwidth and/or ops TokenBucket with refill_time in milliseconds).
// Returns nil when both bytesPerSec and opsPerSec are zero.
func BuildCHRateLimiter(bytesPerSec, burstBytes, opsPerSec, burstOps, refillMs uint64) map[string]interface{} {
	if bytesPerSec == 0 && opsPerSec == 0 {
		return nil
	}
	if refillMs == 0 {
		refillMs = DefaultRefillWindowMs
	}
	out := make(map[string]interface{}, 2)
	if bytesPerSec > 0 {
		size := (bytesPerSec * refillMs) / 1000
		if size == 0 {
			size = 1
		}
		var oneTimeBurst uint64
		if burstBytes > bytesPerSec {
			oneTimeBurst = burstBytes - bytesPerSec
		}
		bw := map[string]interface{}{
			"size":        size,
			"refill_time": refillMs,
		}
		if oneTimeBurst > 0 {
			bw["one_time_burst"] = oneTimeBurst
		}
		out["bandwidth"] = bw
	}
	if opsPerSec > 0 {
		size := (opsPerSec * refillMs) / 1000
		if size == 0 {
			size = 1
		}
		var oneTimeBurst uint64
		if burstOps > opsPerSec {
			oneTimeBurst = burstOps - opsPerSec
		}
		ops := map[string]interface{}{
			"size":        size,
			"refill_time": refillMs,
		}
		if oneTimeBurst > 0 {
			ops["one_time_burst"] = oneTimeBurst
		}
		out["ops"] = ops
	}
	return out
}

// ToCloudHypervisorConfigs converts an EffectiveAllocation into disk and net
// RateLimiterConfig maps suitable for Cloud Hypervisor vm.create payloads.
func ToCloudHypervisorConfigs(alloc EffectiveAllocation, refillMs uint64) (diskRL, netRL map[string]interface{}) {
	diskRL = BuildCHRateLimiter(alloc.BlkBytesPerSec, alloc.BlkBurstBytes, alloc.BlkIOPS, alloc.BlkBurstIOPS, refillMs)
	netRL = BuildCHRateLimiter(alloc.NetBytesPerSec, alloc.NetBurstBytes, alloc.NetPPS, alloc.NetBurstPPS, refillMs)
	return diskRL, netRL
}

// Metrics returns a race-free snapshot of QoS governor counters and gauges.
func (g *Governor) Metrics() Metrics {
	if g == nil {
		return Metrics{Mode: ModeOff, FairnessIndex: 1.0}
	}
	g.mu.Lock()
	mode := g.mode
	activeInsts := len(g.demands)
	tenantSet := make(map[string]struct{}, len(g.tenants)+len(g.demands))
	for tid := range g.tenants {
		tenantSet[tid] = struct{}{}
	}
	for _, d := range g.demands {
		tenantSet[NormalizeTenantID(d.TenantID)] = struct{}{}
	}
	fairness := g.fairnessIndex
	g.mu.Unlock()

	return Metrics{
		Mode:                 mode,
		ActiveTenants:        len(tenantSet),
		ActiveInstances:      activeInsts,
		FairnessIndex:        fairness,
		BlkAdmittedBytes:     atomic.LoadUint64(&g.blkAdmittedBytes),
		BlkAdmittedOps:       atomic.LoadUint64(&g.blkAdmittedOps),
		NetAdmittedBytes:     atomic.LoadUint64(&g.netAdmittedBytes),
		NetAdmittedOps:       atomic.LoadUint64(&g.netAdmittedOps),
		InstanceBlkThrottles: atomic.LoadUint64(&g.instanceBlkThrottles),
		InstanceNetThrottles: atomic.LoadUint64(&g.instanceNetThrottles),
		TenantBlkThrottles:   atomic.LoadUint64(&g.tenantBlkThrottles),
		TenantNetThrottles:   atomic.LoadUint64(&g.tenantNetThrottles),
		ThrottleWaitMicros:   atomic.LoadUint64(&g.throttleWaitMicros),
		RebalancePasses:      atomic.LoadUint64(&g.rebalancePasses),
	}
}

// ResetClocks synchronizes all active tenant and instance token buckets to now
// at full burst capacity (used by deterministic virtual-clock benchmarks and tests).
func (g *Governor) ResetClocks(now time.Time) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	resetSet := func(bs *bucketSet) {
		if bs == nil {
			return
		}
		for _, tb := range []*TokenBucket{bs.blkBW, bs.blkOps, bs.netBW, bs.netOps} {
			if tb != nil && tb.ratePerSec > 0 {
				tb.tokens = tb.burstCap
				tb.lastRefill = now
			}
		}
	}
	for _, bs := range g.tenantBuckets {
		resetSet(bs)
	}
	for _, bs := range g.instBuckets {
		resetSet(bs)
	}
}

// Snapshot returns full tenant policies, resolved per-VM allocations, and metrics.
func (g *Governor) Snapshot() StatusSnapshot {
	if g == nil {
		return StatusSnapshot{Mode: ModeOff, Metrics: Metrics{Mode: ModeOff, FairnessIndex: 1.0}}
	}
	m := g.Metrics()
	g.mu.Lock()
	defer g.mu.Unlock()

	tenants := make([]TenantPolicy, 0, len(g.tenants))
	for _, p := range g.tenants {
		tenants = append(tenants, p)
	}
	sort.Slice(tenants, func(i, j int) bool {
		return tenants[i].TenantID < tenants[j].TenantID
	})

	allocs := make([]EffectiveAllocation, 0, len(g.allocations))
	for _, a := range g.allocations {
		allocs = append(allocs, a)
	}
	sort.Slice(allocs, func(i, j int) bool {
		if allocs[i].TenantID != allocs[j].TenantID {
			return allocs[i].TenantID < allocs[j].TenantID
		}
		return allocs[i].InstanceID < allocs[j].InstanceID
	})

	return StatusSnapshot{
		Mode:          g.mode,
		DefaultPolicy: g.defaultPolicy,
		Tenants:       tenants,
		Allocations:   allocs,
		Metrics:       m,
	}
}
