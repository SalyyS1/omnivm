package ksm

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func setupSyntheticKSM(t *testing.T, initial map[string]uint64) string {
	t.Helper()
	dir := t.TempDir()
	defaults := map[string]uint64{
		"run":             0,
		"pages_shared":    0,
		"pages_sharing":   0,
		"pages_unshared":  0,
		"pages_volatile":  0,
		"full_scans":      0,
		"pages_to_scan":   100,
		"sleep_millisecs": 20,
	}
	for k, v := range initial {
		defaults[k] = v
	}
	for k, v := range defaults {
		if err := os.WriteFile(filepath.Join(dir, k), []byte(strconv.FormatUint(v, 10)+"\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func writeKSMFile(t *testing.T, dir, name string, val uint64) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(strconv.FormatUint(val, 10)+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestGovernor_OffModeNeverMutatesSysfs(t *testing.T) {
	dir := setupSyntheticKSM(t, map[string]uint64{
		"run":           1,
		"pages_shared":  1024,
		"pages_sharing": 4096, // 4096 * 4096B = 16 MiB saved
	})

	gov := NewGovernor(dir, ModeOff, 4096)
	res := gov.Reconcile(0)
	if res.Changed {
		t.Fatal("ModeOff must never mutate sysfs")
	}
	if !res.Metrics.Available || !res.Metrics.Active {
		t.Fatalf("unexpected metrics: %+v", res.Metrics)
	}
	if res.Metrics.SavedMB != 16 || res.Metrics.SavedBytes != 16*1024*1024 {
		t.Fatalf("SavedMB = %d (%d bytes), want 16 MiB", res.Metrics.SavedMB, res.Metrics.SavedBytes)
	}
	if res.Metrics.DedupRatio != 0.8 {
		t.Errorf("DedupRatio = %v, want 0.8 (4096 / 5120)", res.Metrics.DedupRatio)
	}
}

func TestGovernor_AutoZeroOverheadPauseAndConvergenceBackoff(t *testing.T) {
	dir := setupSyntheticKSM(t, map[string]uint64{
		"run":             0,
		"pages_sharing":   0,
		"full_scans":      1,
		"sleep_millisecs": 20,
	})

	gov := NewGovernor(dir, ModeAuto, 4096)

	// Single VM: ksmd must stay paused (run=0) for zero CPU scan overhead.
	r1 := gov.Reconcile(1)
	if r1.Run != 0 || r1.Metrics.Active {
		t.Fatalf("expected run=0 with 1 resident VM, got %+v", r1)
	}

	// Second branched VM arrives: ksmd activates (run=1, sleep=50ms, pages_to_scan=256).
	r2 := gov.Reconcile(2)
	if !r2.Changed || r2.Run != 1 || r2.SleepMillisecs != fastSleepMillisecs {
		t.Fatalf("expected active fast scan (50ms) on 2 VMs, got %+v", r2)
	}
	if r2.Metrics.PagesToScan != defaultPagesToScan {
		t.Errorf("PagesToScan = %d, want %d", r2.Metrics.PagesToScan, defaultPagesToScan)
	}

	// Simulate rapid initial deduplication (pages_sharing jumps 0 -> 8192 across scan #2).
	writeKSMFile(t, dir, "pages_sharing", 8192)
	writeKSMFile(t, dir, "full_scans", 2)
	r3 := gov.Reconcile(2)
	if r3.SleepMillisecs != fastSleepMillisecs {
		t.Fatalf("expected fast sleep while sharing is still growing >1%%, got %dms", r3.SleepMillisecs)
	}

	// Simulate convergence: full_scans advances to 3, pages_sharing grows by only 10 (<1% of 8192).
	writeKSMFile(t, dir, "pages_sharing", 8202)
	writeKSMFile(t, dir, "full_scans", 3)
	r4 := gov.Reconcile(2)
	if r4.SleepMillisecs != steadySleepMillisecs {
		t.Fatalf("expected convergence backoff to %dms, got %dms", steadySleepMillisecs, r4.SleepMillisecs)
	}

	// New 3rd VM boots: sleep_millisecs ramps back to fast 50ms to merge new guest pages quickly.
	r5 := gov.Reconcile(3)
	if r5.SleepMillisecs != fastSleepMillisecs {
		t.Fatalf("expected ramp back to %dms when residentVMs increases, got %dms", fastSleepMillisecs, r5.SleepMillisecs)
	}

	// Scale-to-zero drops resident VMs to 1: ksmd pauses (run=0) while merged pages remain counted.
	r6 := gov.Reconcile(1)
	if r6.Run != 0 || r6.Metrics.Active {
		t.Fatalf("expected ksmd pause (run=0) when residentVMs drops to 1, got %+v", r6)
	}
	if r6.Metrics.SavedMB != 32 {
		t.Errorf("SavedMB = %d, want 32 MiB retained", r6.Metrics.SavedMB)
	}
}

func TestGovernor_ProactiveBurstDensityScalingNUMAAndVolatileBackoff(t *testing.T) {
	dir := setupSyntheticKSM(t, map[string]uint64{
		"run":                0,
		"pages_shared":       256,
		"pages_sharing":      0,
		"pages_unshared":     512,
		"pages_volatile":     0,
		"full_scans":         1,
		"pages_to_scan":      100,
		"sleep_millisecs":    100,
		"merge_across_nodes": 1,
	})

	gov := NewGovernor(dir, ModeProactive, 4096)
	if !IsValidMode("proactive") || gov.Mode() != ModeProactive {
		t.Fatalf("expected ModeProactive, got %q", gov.Mode())
	}

	// 1. NotifySpawn(1) + 1st VM cold boot: pre-indexes base VM pages for 1 burst pass
	// and pins merge_across_nodes=0 while run==0.
	gov.NotifySpawn(1)
	r1 := gov.Reconcile(1)
	if r1.Run != 1 || r1.Phase != PhaseProactiveBurst || r1.SleepMillisecs != burstSleepMillisecs {
		t.Fatalf("expected 1st VM proactive pre-index burst, got %+v", r1)
	}
	if r1.Metrics.MergeAcrossNodes != 0 {
		t.Errorf("MergeAcrossNodes = %d, want 0 (NUMA-local pin)", r1.Metrics.MergeAcrossNodes)
	}
	if r1.PagesToScan != 512 {
		t.Errorf("PagesToScan = %d, want 512", r1.PagesToScan)
	}

	// If no 2nd VM arrives on the next pass, ksmd pauses back to run=0 (zero steady CPU overhead).
	r1Idle := gov.Reconcile(1)
	if r1Idle.Run != 0 || r1Idle.Phase != PhasePaused {
		t.Fatalf("expected pause on 2nd single-VM pass, got %+v", r1Idle)
	}

	// 2. Branch storm of 4 VMs arrives (total 5 resident VMs): density-scaled burst (5 * 256 = 1280 pages @ 20ms).
	gov.NotifySpawn(4)
	r2 := gov.Reconcile(5)
	if r2.Run != 1 || r2.Phase != PhaseProactiveBurst || r2.PagesToScan != 1280 || r2.SleepMillisecs != burstSleepMillisecs {
		t.Fatalf("expected density-scaled burst (1280 pages @ 20ms), got %+v", r2)
	}
	if r2.Metrics.ScanRatePagesPerSec != 64000 {
		t.Errorf("ScanRatePagesPerSec = %d, want 64000", r2.Metrics.ScanRatePagesPerSec)
	}

	// 3. Simulate volatile-page thrashing (pages_volatile dominates >50% while sharing stalls across scan #2).
	writeKSMFile(t, dir, "pages_shared", 500)
	writeKSMFile(t, dir, "pages_sharing", 2000)
	writeKSMFile(t, dir, "pages_unshared", 500)
	writeKSMFile(t, dir, "pages_volatile", 4500) // 4500 / 7500 = 0.60 volatile ratio
	writeKSMFile(t, dir, "full_scans", 2)
	// Record baseline at scan #2
	_ = gov.Reconcile(5)

	// Advance to scan #3 with zero sharing growth while volatile pages remain 60%.
	writeKSMFile(t, dir, "full_scans", 3)
	rBackoff := gov.Reconcile(5)
	if rBackoff.Phase != PhaseVolatileBackoff {
		t.Fatalf("expected PhaseVolatileBackoff, got %q (%+v)", rBackoff.Phase, rBackoff)
	}
	if rBackoff.SleepMillisecs != backoffSleepMillisecs || rBackoff.PagesToScan != volatilePagesToScan {
		t.Errorf("expected backoff sleep=%dms pages=%d, got sleep=%dms pages=%d",
			backoffSleepMillisecs, volatilePagesToScan, rBackoff.SleepMillisecs, rBackoff.PagesToScan)
	}
	if rBackoff.Metrics.VolatileBackoffs != 1 || rBackoff.Metrics.VolatileRatio != 0.6 {
		t.Errorf("unexpected volatile backoff telemetry: %+v", rBackoff.Metrics)
	}
}

func TestGovernor_UnavailableSysfsGraceful(t *testing.T) {
	gov := NewGovernor(filepath.Join(t.TempDir(), "missing-ksm"), ModeProactive, 4096)
	m := gov.ReadMetrics()
	if m.Available {
		t.Fatal("expected Available=false for missing sysfs directory")
	}
	res := gov.Reconcile(4)
	if res.Changed || res.Metrics.Available {
		t.Fatalf("expected graceful no-op on missing sysfs, got %+v", res)
	}
}
