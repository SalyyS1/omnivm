package hibernate

import (
	"bytes"
	"context"
	"testing"
	"time"
)

func TestCompactAndRestoreRoundTripAndTamperRejection(t *testing.T) {
	eng := NewEngine(Config{Mode: ModeIdle})
	// 16 pages (64 KiB) + 123-byte partial tail; only 3 dirty pages
	raw := make([]byte, 16*PageSize+123)
	copy(raw[2*PageSize:], []byte("dirty-page-two"))
	copy(raw[9*PageSize:], []byte("dirty-page-nine"))
	copy(raw[16*PageSize:], []byte("tail-bytes"))

	compacted, stats, err := eng.CompactMemoryImage(raw)
	if err != nil {
		t.Fatalf("CompactMemoryImage: %v", err)
	}
	if stats.TotalPages != 17 || stats.DirtyPages != 3 || stats.ZeroPages != 14 {
		t.Fatalf("unexpected page counts: %+v", stats)
	}
	if stats.SavedBytes == 0 || stats.CompressionRatio <= 3.0 {
		t.Fatalf("expected substantial zero-page compression, got %+v", stats)
	}

	restored, err := eng.RestoreMemoryImage(compacted)
	if err != nil {
		t.Fatalf("RestoreMemoryImage: %v", err)
	}
	if !bytes.Equal(raw, restored) {
		t.Fatalf("restored image does not match original byte-for-byte")
	}

	// Flip one bit in dirty page payload -> fail closed on SHA-256 verification
	corrupted := append([]byte(nil), compacted...)
	corrupted[len(corrupted)-1] ^= 0x01
	if _, err := eng.RestoreMemoryImage(corrupted); err == nil {
		t.Fatalf("expected SHA-256 integrity failure on corrupted image")
	}
}

func TestBuildRestorePlanGeneratesUniqueEntropyAndClockStep(t *testing.T) {
	eng := NewEngine(Config{Mode: ModeAggressive})
	t0 := time.Date(2026, 11, 19, 6, 0, 0, 0, time.UTC)
	t1 := t0.Add(15 * time.Minute)

	p1, err := eng.BuildRestorePlan("vm-worker-1", t0, t1)
	if err != nil {
		t.Fatalf("BuildRestorePlan 1: %v", err)
	}
	p2, err := eng.BuildRestorePlan("vm-worker-1", t0, t1)
	if err != nil {
		t.Fatalf("BuildRestorePlan 2: %v", err)
	}

	if p1.ClockAdjustmentMode != "step" || p1.HibernatedDurationMs != 15*60*1000 {
		t.Fatalf("unexpected clock plan: %+v", p1)
	}
	if p1.EntropySeedSHA256 == p2.EntropySeedSHA256 || p1.VMGenID == p2.VMGenID {
		t.Fatalf("expected fresh CSPRNG seed and distinct VMGenID across restores")
	}

	// Sub-500ms pause uses gentle NTP slew instead of hard step
	slewPlan, err := eng.BuildRestorePlan("vm-worker-1", t0, t0.Add(120*time.Millisecond))
	if err != nil {
		t.Fatalf("BuildRestorePlan slew: %v", err)
	}
	if slewPlan.ClockAdjustmentMode != "slew" {
		t.Fatalf("expected slew mode, got %s", slewPlan.ClockAdjustmentMode)
	}
}

func TestRunBenchmarkVerifiesIntegrity(t *testing.T) {
	rep, err := RunBenchmark(context.Background(), BenchmarkConfig{
		MemoryMB:      4,
		ZeroPageRatio: 0.75,
	})
	if err != nil {
		t.Fatalf("RunBenchmark: %v", err)
	}
	if !rep.IntegrityVerified || rep.ZeroPages == 0 || rep.DirtyPages == 0 || rep.CompactMBps <= 0 {
		t.Fatalf("unexpected benchmark report: %+v", rep)
	}
}
