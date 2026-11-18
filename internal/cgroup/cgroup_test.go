package cgroup

import (
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
)

func writeFakeCgroup(t *testing.T, dir string, current, high, maxVal, events, pressure string) {
	t.Helper()
	mustWrite := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	mustWrite("memory.current", current+"\n")
	mustWrite("memory.swap.current", "0\n")
	mustWrite("memory.high", high+"\n")
	mustWrite("memory.max", maxVal+"\n")
	mustWrite("memory.events", events)
	mustWrite("memory.pressure", pressure)
}

func TestReadMetrics_EventsAndPSI(t *testing.T) {
	dir := t.TempDir()
	const gib = 1024 * 1024 * 1024
	writeFakeCgroup(
		t,
		dir,
		strconv.FormatUint(768*1024*1024, 10),
		strconv.FormatUint(896*1024*1024, 10),
		strconv.FormatUint(gib, 10),
		"low 4\nhigh 39\nmax 2\noom 1\noom_kill 0\noom_group_kill 0\n",
		"some avg10=2.75 avg60=1.10 avg300=0.45 total=184500\nfull avg10=0.35 avg60=0.12 avg300=0.04 total=19200\n",
	)

	gov := NewGovernor(dir, ModeOff, 0.85)
	m := gov.ReadMetrics()
	if !m.Available {
		t.Fatal("expected Available=true")
	}
	if m.CurrentBytes != 768*1024*1024 {
		t.Fatalf("CurrentBytes = %d", m.CurrentBytes)
	}
	if m.HighBytes != 896*1024*1024 || m.HighUnlimited {
		t.Fatalf("HighBytes = %d, unlimited=%v", m.HighBytes, m.HighUnlimited)
	}
	if m.MaxBytes != gib || m.MaxUnlimited {
		t.Fatalf("MaxBytes = %d, unlimited=%v", m.MaxBytes, m.MaxUnlimited)
	}
	if m.Utilization != 0.75 {
		t.Fatalf("Utilization = %v, want 0.75", m.Utilization)
	}
	if m.Events.High != 39 || m.Events.Max != 2 || m.Events.OOM != 1 {
		t.Fatalf("unexpected events: %+v", m.Events)
	}
	if m.PSISome.Avg10 != 2.75 || m.PSISome.TotalMicros != 184500 {
		t.Fatalf("unexpected PSISome: %+v", m.PSISome)
	}
	if m.PSIFull.Avg10 != 0.35 || m.PSIFull.TotalMicros != 19200 {
		t.Fatalf("unexpected PSIFull: %+v", m.PSIFull)
	}

	// ModeOff must never mutate memory.high even if Reconcile is called.
	res := gov.Reconcile(gib)
	if res.Changed {
		t.Fatal("ModeOff must never mutate memory.high")
	}
}

func TestGovernor_AdaptiveWatermarkTightenRelaxAndRecover(t *testing.T) {
	dir := t.TempDir()
	const ceiling = uint64(1024 * 1024 * 1024) // 1 GiB

	writeFakeCgroup(
		t,
		dir,
		strconv.FormatUint(600*1024*1024, 10),
		"max",
		"max",
		"low 0\nhigh 10\nmax 0\noom 0\noom_kill 0\n",
		"some avg10=0.20 avg60=0.10 avg300=0.05 total=1000\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n",
	)

	gov := NewGovernor(dir, ModeAdaptive, 0.85)

	// 1. Initial pass sets memory.high = floor(0.85 * 1 GiB) aligned to 4 KiB.
	r1 := gov.Reconcile(ceiling)
	if !r1.Changed {
		t.Fatal("expected initial Reconcile to replace 'max' with 85% watermark")
	}
	ratio := 0.85
	wantInitial := (uint64(float64(ceiling)*ratio) / 4096) * 4096
	if r1.TargetHighBytes != wantInitial || r1.Metrics.HighBytes != wantInitial {
		t.Fatalf("r1 high = %d, want %d", r1.Metrics.HighBytes, wantInitial)
	}

	// 2. Simulate hard-limit breach (max events jumps from 0 -> 3): ratio must tighten 0.85 -> 0.80.
	if err := os.WriteFile(filepath.Join(dir, "memory.events"), []byte("low 0\nhigh 25\nmax 3\noom 0\noom_kill 0\n"), 0644); err != nil {
		t.Fatal(err)
	}
	r2 := gov.Reconcile(ceiling)
	if !r2.Changed || r2.EffectiveRatio != 0.80 {
		t.Fatalf("expected ratio 0.80 and write after max breach, got changed=%v ratio=%v", r2.Changed, r2.EffectiveRatio)
	}

	// 3. Simulate severe full-stall thrashing (full avg10=7.5%) while usage is 600 MiB (<92%):
	// ratio must relax 0.80 -> 0.83.
	if err := os.WriteFile(filepath.Join(dir, "memory.pressure"), []byte("some avg10=12.0 avg60=4.0 avg300=1.0 total=5000\nfull avg10=7.50 avg60=2.0 avg300=0.5 total=2500\n"), 0644); err != nil {
		t.Fatal(err)
	}
	r3 := gov.Reconcile(ceiling)
	if !r3.Changed || r3.EffectiveRatio != 0.83 {
		t.Fatalf("expected ratio 0.83 after PSI full stall, got changed=%v ratio=%v", r3.Changed, r3.EffectiveRatio)
	}

	// 4. Calm pressure returns: ratio steps +0.01 toward baseRatio (0.83 -> 0.84 -> 0.85).
	if err := os.WriteFile(filepath.Join(dir, "memory.pressure"), []byte("some avg10=0.10 avg60=0.10 avg300=0.05 total=5100\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=2500\n"), 0644); err != nil {
		t.Fatal(err)
	}
	r4 := gov.Reconcile(ceiling)
	if r4.EffectiveRatio != 0.84 {
		t.Fatalf("expected ratio 0.84 on calm step 1, got %v", r4.EffectiveRatio)
	}
	r5 := gov.Reconcile(ceiling)
	if r5.EffectiveRatio != 0.85 {
		t.Fatalf("expected ratio 0.85 on calm step 2, got %v", r5.EffectiveRatio)
	}
}

func TestResolveSelfCgroupDir_AndConcurrentSafety(t *testing.T) {
	mountRoot := t.TempDir()
	subDir := filepath.Join(mountRoot, "system.slice", "ovmd.service")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatal(err)
	}
	writeFakeCgroup(t, subDir, "104857600", "max", "536870912", "high 7\n", "")

	procFile := filepath.Join(mountRoot, "proc-self-cgroup")
	if err := os.WriteFile(procFile, []byte("0::/system.slice/ovmd.service\n"), 0644); err != nil {
		t.Fatal(err)
	}

	resolved := ResolveSelfCgroupDir(mountRoot, procFile)
	if resolved != subDir {
		t.Fatalf("resolved = %q, want %q", resolved, subDir)
	}

	gov := NewGovernor(resolved, ModeAdaptive, 0.85)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				_ = gov.ReadMetrics()
				_ = gov.Reconcile(536870912)
			}
		}()
	}
	wg.Wait()
}
