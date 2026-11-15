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

func TestGovernor_UnavailableSysfsGraceful(t *testing.T) {
	gov := NewGovernor(filepath.Join(t.TempDir(), "missing-ksm"), ModeAuto, 4096)
	m := gov.ReadMetrics()
	if m.Available {
		t.Fatal("expected Available=false for missing sysfs directory")
	}
	res := gov.Reconcile(4)
	if res.Changed || res.Metrics.Available {
		t.Fatalf("expected graceful no-op on missing sysfs, got %+v", res)
	}
}
