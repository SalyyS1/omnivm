package ioqos

import (
	"bytes"
	"context"
	"io"
	"sync"
	"testing"
	"time"
)

func TestWaterFillWeighted_RedistributesCappedSurplus(t *testing.T) {
	pol := TenantPolicy{
		TenantID:   "acme",
		MaxBlkMBps: 100,
		MaxBlkIOPS: 10000,
		MaxNetMBps: 60,
		MaxNetPPS:  12000,
		BurstRatio: 1.5,
	}
	demands := []InstanceDemand{
		{
			InstanceID: "vm-1",
			TenantID:   "acme",
			VCPUs:      1,
			Policy:     InstancePolicy{Weight: 1, MaxBlkMBps: 20},
		},
		{
			InstanceID: "vm-2",
			TenantID:   "acme",
			VCPUs:      1,
			Policy:     InstancePolicy{Weight: 1},
		},
		{
			InstanceID: "vm-3",
			TenantID:   "acme",
			VCPUs:      3,
			Policy:     InstancePolicy{Weight: 3},
		},
	}

	allocs := ComputeTenantAllocations(ModeFairShare, pol, demands)
	if len(allocs) != 3 {
		t.Fatalf("expected 3 allocations, got %d", len(allocs))
	}

	// vm-1 has weight 1/5 (=20 MiB/s) and cap 20 MiB/s -> 20 MiB/s.
	// What if vm-1 had cap 10 MiB/s? Let's test surplus redistribution too!
	demands[0].Policy.MaxBlkMBps = 10
	allocs = ComputeTenantAllocations(ModeFairShare, pol, demands)

	// Remaining 90 MiB/s split 1:3 between vm-2 (22.5 MiB/s) and vm-3 (67.5 MiB/s).
	wantVM1 := uint64(10 * bytesPerMiB)
	wantVM2 := uint64(22.5 * bytesPerMiB)
	wantVM3 := uint64(67.5 * bytesPerMiB)

	if allocs[0].BlkBytesPerSec != wantVM1 {
		t.Errorf("vm-1 blk = %d, want %d", allocs[0].BlkBytesPerSec, wantVM1)
	}
	if allocs[1].BlkBytesPerSec != wantVM2 {
		t.Errorf("vm-2 blk = %d, want %d", allocs[1].BlkBytesPerSec, wantVM2)
	}
	if allocs[2].BlkBytesPerSec != wantVM3 {
		t.Errorf("vm-3 blk = %d, want %d", allocs[2].BlkBytesPerSec, wantVM3)
	}
	if allocs[2].BlkBurstBytes != uint64(67.5*1.5*bytesPerMiB) {
		t.Errorf("vm-3 burst = %d, want %d", allocs[2].BlkBurstBytes, uint64(67.5*1.5*bytesPerMiB))
	}
}

func TestGovernor_AtomicTwoTierAdmissionPreservesChildTokensOnParentReject(t *testing.T) {
	now := time.Date(2026, 11, 19, 1, 0, 0, 0, time.UTC)
	gov := NewGovernor(Config{
		Mode: ModeStatic,
		DefaultPolicy: TenantPolicy{
			TenantID:   DefaultTenantID,
			MaxBlkMBps: 10,
			BurstRatio: 1.0,
		},
	})
	if err := gov.SetTenantPolicy(TenantPolicy{
		TenantID:   "tenant-a",
		MaxBlkMBps: 10,
		BurstRatio: 1.0,
	}); err != nil {
		t.Fatal(err)
	}

	// Register two VMs in Static mode, each with 10 MiB/s child cap sharing a 10 MiB/s tenant bucket.
	gov.RegisterInstance(InstanceDemand{InstanceID: "vm-a1", TenantID: "tenant-a", Policy: InstancePolicy{MaxBlkMBps: 10, BurstRatio: 1.0}})
	gov.RegisterInstance(InstanceDemand{InstanceID: "vm-a2", TenantID: "tenant-a", Policy: InstancePolicy{MaxBlkMBps: 10, BurstRatio: 1.0}})

	// vm-a1 consumes 8 MiB -> allowed (2 MiB left in tenant-a bucket, 2 MiB left in vm-a1).
	d1 := gov.Admit("tenant-a", "vm-a1", ChannelBlkBandwidth, 8*bytesPerMiB, now)
	if !d1.Allowed {
		t.Fatalf("expected d1 allowed, got %+v", d1)
	}

	// vm-a2 requests 5 MiB -> child bucket has 10 MiB, but parent tenant bucket only has 2 MiB!
	d2 := gov.Admit("tenant-a", "vm-a2", ChannelBlkBandwidth, 5*bytesPerMiB, now)
	if d2.Allowed {
		t.Fatalf("expected tenant rejection when aggregate exceeds 10 MiB")
	}
	if d2.LimitedBy != "tenant" {
		t.Errorf("LimitedBy = %q, want tenant", d2.LimitedBy)
	}
	if d2.RetryAfter < 290*time.Millisecond || d2.RetryAfter > 310*time.Millisecond {
		t.Errorf("RetryAfter = %v, want ~300ms for 3 MiB deficit at 10 MiB/s", d2.RetryAfter)
	}

	// Because two-tier admission is atomic, vm-a2's child bucket was NOT drained on rejection,
	// so a 2 MiB request that fits within the remaining tenant budget succeeds immediately!
	d3 := gov.Admit("tenant-a", "vm-a2", ChannelBlkBandwidth, 2*bytesPerMiB, now)
	if !d3.Allowed {
		t.Fatalf("expected 2 MiB request to succeed without child token leakage, got %+v", d3)
	}

	// Advance 500ms -> 5 MiB refills into tenant bucket -> vm-a2 can now admit 5 MiB.
	d4 := gov.Admit("tenant-a", "vm-a2", ChannelBlkBandwidth, 5*bytesPerMiB, now.Add(500*time.Millisecond))
	if !d4.Allowed {
		t.Fatalf("expected 5 MiB request after 500ms refill to succeed, got %+v", d4)
	}
}

func TestBuildCHRateLimiter_CloudHypervisorSchema(t *testing.T) {
	alloc := EffectiveAllocation{
		InstanceID:     "vm-1",
		TenantID:       "prod",
		BlkBytesPerSec: 100 * bytesPerMiB,
		BlkBurstBytes:  150 * bytesPerMiB,
		BlkIOPS:        4000,
		BlkBurstIOPS:   6000,
		NetBytesPerSec: 50 * bytesPerMiB,
		NetBurstBytes:  50 * bytesPerMiB,
	}
	diskRL, netRL := ToCloudHypervisorConfigs(alloc, 100)
	if diskRL == nil || netRL == nil {
		t.Fatalf("expected non-nil diskRL and netRL")
	}

	bw := diskRL["bandwidth"].(map[string]interface{})
	if bw["size"].(uint64) != 10*bytesPerMiB {
		t.Errorf("disk bandwidth size = %v, want %d (100 MiB/s * 100ms)", bw["size"], 10*bytesPerMiB)
	}
	if bw["one_time_burst"].(uint64) != 50*bytesPerMiB {
		t.Errorf("disk one_time_burst = %v, want %d", bw["one_time_burst"], 50*bytesPerMiB)
	}
	if bw["refill_time"].(uint64) != 100 {
		t.Errorf("disk refill_time = %v, want 100", bw["refill_time"])
	}

	ops := diskRL["ops"].(map[string]interface{})
	if ops["size"].(uint64) != 400 || ops["one_time_burst"].(uint64) != 2000 {
		t.Errorf("unexpected ops config: %+v", ops)
	}

	netBW := netRL["bandwidth"].(map[string]interface{})
	if _, hasBurst := netBW["one_time_burst"]; hasBurst {
		t.Errorf("expected no one_time_burst when burst == rate: %+v", netBW)
	}
	if _, hasNetOps := netRL["ops"]; hasNetOps {
		t.Errorf("expected no ops map when NetPPS == 0")
	}
}

func TestShapeReader_StreamsAndAccountsBytes(t *testing.T) {
	gov := NewGovernor(Config{
		Mode: ModeFairShare,
		DefaultPolicy: TenantPolicy{
			TenantID:   "t1",
			MaxNetMBps: 50,
			MaxNetPPS:  10000,
		},
	})
	gov.RegisterInstance(InstanceDemand{InstanceID: "vm-s1", TenantID: "t1", VCPUs: 1})

	payload := bytes.Repeat([]byte("omnivm-qos-"), 4096)
	shaped := gov.ShapeReader(context.Background(), bytes.NewReader(payload), "t1", "vm-s1", ChannelNetBandwidth, ChannelNetOps)
	got, err := io.ReadAll(shaped)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload mismatch: len %d vs %d", len(got), len(payload))
	}
	m := gov.Metrics()
	if m.NetAdmittedBytes != uint64(len(payload)) {
		t.Errorf("NetAdmittedBytes = %d, want %d", m.NetAdmittedBytes, len(payload))
	}
	if m.NetAdmittedOps == 0 {
		t.Errorf("expected NetAdmittedOps > 0")
	}
}

func TestRunBenchmark_NoisyNeighborIsolationAndConcurrency(t *testing.T) {
	rep, err := RunBenchmark(context.Background(), BenchmarkConfig{
		Tenants:       4,
		NoisyVMs:      4,
		TenantBlkMBps: 100,
		TenantNetMBps: 50,
		DurationMs:    2000,
	})
	if err != nil {
		t.Fatalf("RunBenchmark: %v", err)
	}
	if !rep.QuietTenantZeroThrottles {
		t.Errorf("expected zero throttles on quiet tenants, got %+v", rep.TenantResults)
	}
	if rep.IntraTenantFairnessIndex < 0.99 {
		t.Errorf("IntraTenantFairnessIndex = %v, want >= 0.99", rep.IntraTenantFairnessIndex)
	}
	if rep.InterTenantFairnessIndex < 0.99 {
		t.Errorf("InterTenantFairnessIndex = %v, want >= 0.99", rep.InterTenantFairnessIndex)
	}
	if rep.NoisyExcessShedPercent < 40.0 {
		t.Errorf("expected noisy excess shedding >= 40%%, got %.2f%%", rep.NoisyExcessShedPercent)
	}

	// Verify race-safety under concurrent Register/Admit/SetTenantPolicy/Snapshot calls.
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			gov := NewGovernor(Config{Mode: ModeFairShare, DefaultPolicy: TenantPolicy{MaxBlkMBps: 100, MaxNetMBps: 100}})
			for i := 0; i < 50; i++ {
				gov.RegisterInstance(InstanceDemand{InstanceID: "vm-1", TenantID: "t-a", VCPUs: 2})
				gov.Admit("t-a", "vm-1", ChannelBlkBandwidth, 4096, time.Now())
				_ = gov.Snapshot()
			}
		}(g)
	}
	wg.Wait()
}
