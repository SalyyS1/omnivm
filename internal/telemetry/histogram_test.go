package telemetry

import (
	"math"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestHistogram_CumulativeBucketsAndQuantiles(t *testing.T) {
	h := NewHistogram([]float64{0.01, 0.05, 0.1, 0.5, 1.0})

	// Observe 5 samples: 4ms, 20ms, 80ms, 300ms, 1500ms (overflow into +Inf only).
	h.ObserveDuration(4 * time.Millisecond)
	h.ObserveDuration(20 * time.Millisecond)
	h.ObserveDuration(80 * time.Millisecond)
	h.ObserveDuration(300 * time.Millisecond)
	h.ObserveDuration(1500 * time.Millisecond)

	snap := h.Snapshot()
	if snap.Count != 5 {
		t.Fatalf("Count = %d, want 5", snap.Count)
	}
	if snap.MinMs != 4.0 || snap.MaxMs != 1500.0 {
		t.Fatalf("MinMs=%v MaxMs=%v, want 4.0 and 1500.0", snap.MinMs, snap.MaxMs)
	}
	if snap.P50Ms != 80.0 {
		t.Errorf("P50Ms = %v, want 80.0", snap.P50Ms)
	}
	if snap.P95Ms != 1500.0 || snap.P99Ms != 1500.0 {
		t.Errorf("P95Ms=%v P99Ms=%v, want 1500.0", snap.P95Ms, snap.P99Ms)
	}

	wantCum := []uint64{1, 2, 3, 4, 4}
	if len(snap.Buckets) != len(wantCum) {
		t.Fatalf("len(Buckets) = %d, want %d", len(snap.Buckets), len(wantCum))
	}
	for i, w := range wantCum {
		if snap.Buckets[i].Count != w {
			t.Errorf("bucket[%d] (le=%v) = %d, want %d", i, snap.Buckets[i].UpperBound, snap.Buckets[i].Count, w)
		}
	}
}

func TestHistogram_PrometheusExpositionLabeledAndUnlabeled(t *testing.T) {
	cold := NewHistogram([]float64{0.1, 1.0})
	cold.ObserveDuration(450 * time.Millisecond)
	cold.ObserveDuration(1200 * time.Millisecond)

	warm := NewHistogram([]float64{0.1, 1.0})
	warm.ObserveDuration(12 * time.Millisecond)

	var b strings.Builder
	WritePrometheusHistogram(&b, "omnivm_wake_duration_seconds", "Wake latency.", "path", []LabeledSnapshot{
		{LabelValue: "cold_boot", Snapshot: cold.Snapshot()},
		{LabelValue: "warm_resume", Snapshot: warm.Snapshot()},
	})
	WritePrometheusHistogram(&b, "omnivm_router_probe_duration_seconds", "Probe latency.", "", []LabeledSnapshot{
		{Snapshot: warm.Snapshot()},
	})

	out := b.String()
	wantSnippets := []string{
		`# TYPE omnivm_wake_duration_seconds histogram`,
		`omnivm_wake_duration_seconds_bucket{path="cold_boot",le="0.1"} 0`,
		`omnivm_wake_duration_seconds_bucket{path="cold_boot",le="1"} 1`,
		`omnivm_wake_duration_seconds_bucket{path="cold_boot",le="+Inf"} 2`,
		`omnivm_wake_duration_seconds_sum{path="cold_boot"} 1.650000`,
		`omnivm_wake_duration_seconds_count{path="cold_boot"} 2`,
		`omnivm_wake_duration_seconds_bucket{path="warm_resume",le="0.1"} 1`,
		`omnivm_wake_duration_seconds_count{path="warm_resume"} 1`,
		`# TYPE omnivm_router_probe_duration_seconds histogram`,
		`omnivm_router_probe_duration_seconds_bucket{le="0.1"} 1`,
		`omnivm_router_probe_duration_seconds_bucket{le="+Inf"} 1`,
		`omnivm_router_probe_duration_seconds_count 1`,
	}
	for _, want := range wantSnippets {
		if !strings.Contains(out, want) {
			t.Errorf("exposition missing %q\nGot:\n%s", want, out)
		}
	}
}

func TestHistogram_ConcurrentObservationsAndSanitization(t *testing.T) {
	h := NewHistogram(nil)
	h.ObserveSeconds(math.NaN())
	h.ObserveSeconds(math.Inf(1))
	h.ObserveSeconds(-0.25) // clamped to 0

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				h.ObserveDuration(time.Duration((id*10)+i) * time.Millisecond)
			}
		}(g)
	}
	wg.Wait()

	snap := h.Snapshot()
	if snap.Count != 801 {
		t.Fatalf("Count = %d, want 801 (1 clamped + 800 concurrent)", snap.Count)
	}
	if snap.MinMs != 0 {
		t.Errorf("MinMs = %v, want 0", snap.MinMs)
	}
}
