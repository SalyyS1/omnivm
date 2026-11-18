package ioqos

import (
	"context"
	"fmt"
	"math"
	"time"
)

// BenchmarkConfig parameterizes the multi-tenant noisy-neighbor I/O QoS benchmark.
type BenchmarkConfig struct {
	Tenants        int `json:"tenants"`
	NoisyVMs       int `json:"noisy_vms"`
	TenantBlkMBps  int `json:"tenant_blk_mbps"`
	TenantNetMBps  int `json:"tenant_net_mbps"`
	DurationMs     int `json:"duration_ms"`
}

// TenantBenchStats summarizes simulated I/O admission for one tenant.
type TenantBenchStats struct {
	TenantID           string  `json:"tenant_id"`
	Role               string  `json:"role"`
	ActiveVMs          int     `json:"active_vms"`
	RequestedBlkMB     float64 `json:"requested_blk_mb"`
	AdmittedBlkMB      float64 `json:"admitted_blk_mb"`
	RequestedNetMB     float64 `json:"requested_net_mb"`
	AdmittedNetMB      float64 `json:"admitted_net_mb"`
	ThrottledBursts    uint64  `json:"throttled_bursts"`
	MaxRetryWaitMicros int64   `json:"max_retry_wait_micros"`
	SLOAttainmentRatio float64 `json:"slo_attainment_ratio"`
}

// BenchmarkReport captures deterministic multi-tenant token-bucket isolation results.
type BenchmarkReport struct {
	Mode                       string             `json:"mode"`
	Tenants                    int                `json:"tenants"`
	NoisyVMs                   int                `json:"noisy_vms"`
	DurationMs                 int                `json:"duration_ms"`
	TenantBlkCeilingMBps       int                `json:"tenant_blk_ceiling_mbps"`
	TenantNetCeilingMBps       int                `json:"tenant_net_ceiling_mbps"`
	IntraTenantFairnessIndex   float64            `json:"intra_tenant_fairness_index"`
	InterTenantFairnessIndex   float64            `json:"inter_tenant_fairness_index"`
	QuietTenantZeroThrottles   bool               `json:"quiet_tenant_zero_throttles"`
	NoisyExcessShedPercent     float64            `json:"noisy_excess_shed_percent"`
	TenantResults              []TenantBenchStats `json:"tenant_results"`
}

// RunBenchmark executes a deterministic virtual-clock simulation of noisy-neighbor
// multi-tenant virtio-blk and virtio-net contention under fairshare QoS.
func RunBenchmark(ctx context.Context, cfg BenchmarkConfig) (*BenchmarkReport, error) {
	if cfg.Tenants == 0 {
		cfg.Tenants = 3
	}
	if cfg.Tenants < 2 || cfg.Tenants > 64 {
		return nil, fmt.Errorf("tenants must be between 2 and 64, got %d", cfg.Tenants)
	}
	if cfg.NoisyVMs == 0 {
		cfg.NoisyVMs = 4
	}
	if cfg.NoisyVMs < 1 || cfg.NoisyVMs > 64 {
		return nil, fmt.Errorf("noisy_vms must be between 1 and 64, got %d", cfg.NoisyVMs)
	}
	if cfg.TenantBlkMBps == 0 {
		cfg.TenantBlkMBps = 100
	}
	if cfg.TenantBlkMBps < 1 || cfg.TenantBlkMBps > 100000 {
		return nil, fmt.Errorf("tenant_blk_mbps must be between 1 and 100000, got %d", cfg.TenantBlkMBps)
	}
	if cfg.TenantNetMBps == 0 {
		cfg.TenantNetMBps = 50
	}
	if cfg.TenantNetMBps < 1 || cfg.TenantNetMBps > 100000 {
		return nil, fmt.Errorf("tenant_net_mbps must be between 1 and 100000, got %d", cfg.TenantNetMBps)
	}
	if cfg.DurationMs == 0 {
		cfg.DurationMs = 2000
	}
	if cfg.DurationMs < 100 || cfg.DurationMs > 60000 {
		return nil, fmt.Errorf("duration_ms must be between 100 and 60000, got %d", cfg.DurationMs)
	}

	gov := NewGovernor(Config{
		Mode: ModeFairShare,
		DefaultPolicy: TenantPolicy{
			TenantID:   DefaultTenantID,
			MaxBlkMBps: cfg.TenantBlkMBps,
			MaxBlkIOPS: 20000,
			MaxNetMBps: cfg.TenantNetMBps,
			MaxNetPPS:  50000,
			BurstRatio: 1.2,
		},
	})

	noisyTenant := "tenant-noisy"
	for i := 0; i < cfg.NoisyVMs; i++ {
		gov.RegisterInstance(InstanceDemand{
			InstanceID: fmt.Sprintf("noisy-vm-%d", i+1),
			TenantID:   noisyTenant,
			VCPUs:      2,
		})
	}

	quietTenants := make([]string, 0, cfg.Tenants-1)
	for t := 1; t < cfg.Tenants; t++ {
		tid := fmt.Sprintf("tenant-quiet-%d", t)
		quietTenants = append(quietTenants, tid)
		gov.RegisterInstance(InstanceDemand{
			InstanceID: fmt.Sprintf("quiet-vm-%d", t),
			TenantID:   tid,
			VCPUs:      2,
		})
	}

	const stepMs = 10
	steps := cfg.DurationMs / stepMs
	if steps < 1 {
		steps = 1
	}

	// Quiet tenants issue steady 80% of ceiling per step; each noisy VM attempts 100% of tenant ceiling per step.
	quietStepBlk := uint64(math.Round(float64(cfg.TenantBlkMBps*bytesPerMiB) * 0.80 * (float64(stepMs) / 1000.0)))
	quietStepNet := uint64(math.Round(float64(cfg.TenantNetMBps*bytesPerMiB) * 0.80 * (float64(stepMs) / 1000.0)))
	noisyPerVMStepBlk := uint64(math.Round(float64(cfg.TenantBlkMBps*bytesPerMiB) * (float64(stepMs) / 1000.0)))
	noisyPerVMStepNet := uint64(math.Round(float64(cfg.TenantNetMBps*bytesPerMiB) * (float64(stepMs) / 1000.0)))

	results := make([]TenantBenchStats, cfg.Tenants)
	results[0] = TenantBenchStats{
		TenantID:  noisyTenant,
		Role:      "noisy_neighbor",
		ActiveVMs: cfg.NoisyVMs,
	}
	for idx, tid := range quietTenants {
		results[idx+1] = TenantBenchStats{
			TenantID:  tid,
			Role:      "latency_sensitive",
			ActiveVMs: 1,
		}
	}

	noisyPerVMAdmitted := make([]float64, cfg.NoisyVMs)
	noisyWeights := make([]int, cfg.NoisyVMs)
	for i := range noisyWeights {
		noisyWeights[i] = 1
	}

	simClock := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	gov.ResetClocks(simClock)
	for step := 0; step < steps; step++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		now := simClock.Add(time.Duration(step*stepMs) * time.Millisecond)

		// Rotate starting VM index each tick so round-robin burst contention is symmetric.
		for k := 0; k < cfg.NoisyVMs; k++ {
			vmIdx := (step + k) % cfg.NoisyVMs
			vmID := fmt.Sprintf("noisy-vm-%d", vmIdx+1)

			results[0].RequestedBlkMB += float64(noisyPerVMStepBlk) / bytesPerMiB
			decBlk := gov.Admit(noisyTenant, vmID, ChannelBlkBandwidth, noisyPerVMStepBlk, now)
			if decBlk.Allowed {
				results[0].AdmittedBlkMB += float64(noisyPerVMStepBlk) / bytesPerMiB
				noisyPerVMAdmitted[vmIdx] += float64(noisyPerVMStepBlk)
			} else {
				results[0].ThrottledBursts++
				if waitUs := decBlk.RetryAfter.Microseconds(); waitUs > results[0].MaxRetryWaitMicros {
					results[0].MaxRetryWaitMicros = waitUs
				}
			}

			results[0].RequestedNetMB += float64(noisyPerVMStepNet) / bytesPerMiB
			decNet := gov.Admit(noisyTenant, vmID, ChannelNetBandwidth, noisyPerVMStepNet, now)
			if decNet.Allowed {
				results[0].AdmittedNetMB += float64(noisyPerVMStepNet) / bytesPerMiB
			} else {
				results[0].ThrottledBursts++
				if waitUs := decNet.RetryAfter.Microseconds(); waitUs > results[0].MaxRetryWaitMicros {
					results[0].MaxRetryWaitMicros = waitUs
				}
			}
		}

		// Quiet tenants issue steady-state within-budget traffic concurrently.
		for qIdx, tid := range quietTenants {
			vmID := fmt.Sprintf("quiet-vm-%d", qIdx+1)
			slot := &results[qIdx+1]

			slot.RequestedBlkMB += float64(quietStepBlk) / bytesPerMiB
			decBlk := gov.Admit(tid, vmID, ChannelBlkBandwidth, quietStepBlk, now)
			if decBlk.Allowed {
				slot.AdmittedBlkMB += float64(quietStepBlk) / bytesPerMiB
			} else {
				slot.ThrottledBursts++
				if waitUs := decBlk.RetryAfter.Microseconds(); waitUs > slot.MaxRetryWaitMicros {
					slot.MaxRetryWaitMicros = waitUs
				}
			}

			slot.RequestedNetMB += float64(quietStepNet) / bytesPerMiB
			decNet := gov.Admit(tid, vmID, ChannelNetBandwidth, quietStepNet, now)
			if decNet.Allowed {
				slot.AdmittedNetMB += float64(quietStepNet) / bytesPerMiB
			} else {
				slot.ThrottledBursts++
				if waitUs := decNet.RetryAfter.Microseconds(); waitUs > slot.MaxRetryWaitMicros {
					slot.MaxRetryWaitMicros = waitUs
				}
			}
		}
	}

	quietZeroThrottles := true
	tenantNormalized := make([]float64, len(results))
	tenantWeights := make([]int, len(results))
	for i := range results {
		r := &results[i]
		totalReq := r.RequestedBlkMB + r.RequestedNetMB
		totalAdm := r.AdmittedBlkMB + r.AdmittedNetMB
		if totalReq > 0 {
			r.SLOAttainmentRatio = math.Round((totalAdm/totalReq)*10000) / 10000
		} else {
			r.SLOAttainmentRatio = 1.0
		}
		r.RequestedBlkMB = math.Round(r.RequestedBlkMB*100) / 100
		r.AdmittedBlkMB = math.Round(r.AdmittedBlkMB*100) / 100
		r.RequestedNetMB = math.Round(r.RequestedNetMB*100) / 100
		r.AdmittedNetMB = math.Round(r.AdmittedNetMB*100) / 100
		if r.Role == "latency_sensitive" && r.ThrottledBursts > 0 {
			quietZeroThrottles = false
		}
		// Compare normalized SLO attainment up to each tenant's subscribed rate.
		subscribedBlk := float64(cfg.TenantBlkMBps) * (float64(cfg.DurationMs) / 1000.0)
		target := math.Min(r.RequestedBlkMB, subscribedBlk)
		if target > 0 {
			tenantNormalized[i] = math.Min(1.0, r.AdmittedBlkMB/target)
		} else {
			tenantNormalized[i] = 1.0
		}
		tenantWeights[i] = 1
	}

	var shedPct float64
	noisyReq := results[0].RequestedBlkMB + results[0].RequestedNetMB
	noisyAdm := results[0].AdmittedBlkMB + results[0].AdmittedNetMB
	if noisyReq > noisyAdm && noisyReq > 0 {
		shedPct = math.Round(((noisyReq-noisyAdm)/noisyReq)*10000) / 100
	}

	intraJain := math.Round(ComputeWeightedJainsIndex(noisyPerVMAdmitted, noisyWeights)*10000) / 10000
	interJain := math.Round(ComputeWeightedJainsIndex(tenantNormalized, tenantWeights)*10000) / 10000

	return &BenchmarkReport{
		Mode:                     ModeFairShare,
		Tenants:                  cfg.Tenants,
		NoisyVMs:                 cfg.NoisyVMs,
		DurationMs:               cfg.DurationMs,
		TenantBlkCeilingMBps:     cfg.TenantBlkMBps,
		TenantNetCeilingMBps:     cfg.TenantNetMBps,
		IntraTenantFairnessIndex: intraJain,
		InterTenantFairnessIndex: interJain,
		QuietTenantZeroThrottles: quietZeroThrottles,
		NoisyExcessShedPercent:   shedPct,
		TenantResults:            results,
	}, nil
}
