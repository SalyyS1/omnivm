// Package telemetry provides low-overhead, concurrency-safe latency histograms
// with exact cumulative bucket accounting, sliding-window empirical quantiles,
// and Prometheus 0.0.4 / OpenMetrics 1.0.0 text exposition.
package telemetry

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const defaultRingCapacity = 256

// DefaultWakeBuckets defines histogram bucket upper bounds (in seconds) tuned
// for microVM wake transitions (sub-10ms warm SIGCONT resumes through multi-second cold boots).
var DefaultWakeBuckets = []float64{
	0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0, 10.0,
}

// DefaultProbeBuckets defines histogram bucket upper bounds (in seconds) tuned
// for post-wake TCP and virtio-vsock readiness probing.
var DefaultProbeBuckets = []float64{
	0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5,
}

// BucketCount represents a single cumulative histogram bucket (count of observations <= UpperBound).
type BucketCount struct {
	UpperBound float64 `json:"le"`
	Count      uint64  `json:"count"`
}

// HistogramSnapshot captures a point-in-time read of a Histogram for JSON and Prometheus export.
type HistogramSnapshot struct {
	Count      uint64        `json:"count"`
	SumMs      float64       `json:"sum_ms"`
	MinMs      float64       `json:"min_ms"`
	P50Ms      float64       `json:"p50_ms"`
	P95Ms      float64       `json:"p95_ms"`
	P99Ms      float64       `json:"p99_ms"`
	MaxMs      float64       `json:"max_ms"`
	MeanMs     float64       `json:"mean_ms"`
	SumSeconds float64       `json:"-"`
	Buckets    []BucketCount `json:"buckets,omitempty"`
}

// LabeledSnapshot binds a single label value (for example path="cold_boot") to a HistogramSnapshot.
type LabeledSnapshot struct {
	LabelValue string
	Snapshot   HistogramSnapshot
}

// Histogram records non-negative latency observations into cumulative upper-bound
// buckets and a bounded sliding ring buffer for empirical quantile estimation.
type Histogram struct {
	mu           sync.Mutex
	bounds       []float64
	bucketCounts []uint64 // non-cumulative counts per bound; len(bounds)
	count        uint64
	sumSeconds   float64
	minSeconds   float64
	maxSeconds   float64
	ring         []float64
	ringPos      int
	ringFull     bool
}

// NewHistogram constructs a Histogram with strictly increasing positive finite bucket bounds.
// If bounds is empty, DefaultWakeBuckets is used.
func NewHistogram(bounds []float64) *Histogram {
	src := bounds
	if len(src) == 0 {
		src = DefaultWakeBuckets
	}
	clean := make([]float64, 0, len(src))
	for _, b := range src {
		if !math.IsNaN(b) && !math.IsInf(b, 0) && b > 0 {
			clean = append(clean, b)
		}
	}
	if len(clean) == 0 {
		clean = append(clean, DefaultWakeBuckets...)
	}
	sort.Float64s(clean)
	dedup := clean[:0]
	for i, b := range clean {
		if i == 0 || b > dedup[len(dedup)-1] {
			dedup = append(dedup, b)
		}
	}

	return &Histogram{
		bounds:       dedup,
		bucketCounts: make([]uint64, len(dedup)),
		ring:         make([]float64, defaultRingCapacity),
	}
}

// ObserveDuration records a duration observation in seconds.
func (h *Histogram) ObserveDuration(d time.Duration) {
	if h == nil {
		return
	}
	if d < 0 {
		d = 0
	}
	h.ObserveSeconds(d.Seconds())
}

// ObserveSeconds records a latency observation in seconds.
func (h *Histogram) ObserveSeconds(sec float64) {
	if h == nil || math.IsNaN(sec) || math.IsInf(sec, 1) {
		return
	}
	if sec < 0 || math.IsInf(sec, -1) {
		sec = 0
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	if h.count == 0 {
		h.minSeconds = sec
		h.maxSeconds = sec
	} else {
		if sec < h.minSeconds {
			h.minSeconds = sec
		}
		if sec > h.maxSeconds {
			h.maxSeconds = sec
		}
	}
	h.count++
	h.sumSeconds += sec

	idx := sort.SearchFloat64s(h.bounds, sec)
	if idx < len(h.bucketCounts) {
		h.bucketCounts[idx]++
	}

	h.ring[h.ringPos] = sec
	h.ringPos++
	if h.ringPos >= len(h.ring) {
		h.ringPos = 0
		h.ringFull = true
	}
}

// Reset clears all recorded observations and bucket counts.
func (h *Histogram) Reset() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := range h.bucketCounts {
		h.bucketCounts[i] = 0
	}
	h.count = 0
	h.sumSeconds = 0
	h.minSeconds = 0
	h.maxSeconds = 0
	h.ringPos = 0
	h.ringFull = false
}

// Snapshot returns a race-free cumulative bucket and quantile snapshot.
func (h *Histogram) Snapshot() HistogramSnapshot {
	if h == nil {
		return HistogramSnapshot{}
	}
	h.mu.Lock()
	count := h.count
	sumSec := h.sumSeconds
	minSec := h.minSeconds
	maxSec := h.maxSeconds
	buckets := make([]BucketCount, len(h.bounds))
	var cum uint64
	for i, ub := range h.bounds {
		cum += h.bucketCounts[i]
		buckets[i] = BucketCount{
			UpperBound: ub,
			Count:      cum,
		}
	}
	var samples []float64
	if count > 0 {
		if h.ringFull {
			samples = make([]float64, len(h.ring))
			copy(samples, h.ring)
		} else {
			samples = make([]float64, h.ringPos)
			copy(samples, h.ring[:h.ringPos])
		}
	}
	h.mu.Unlock()

	out := HistogramSnapshot{
		Count:      count,
		SumSeconds: sumSec,
		SumMs:      roundMs(sumSec * 1000.0),
		Buckets:    buckets,
	}
	if count == 0 {
		return out
	}

	out.MinMs = roundMs(minSec * 1000.0)
	out.MaxMs = roundMs(maxSec * 1000.0)
	out.MeanMs = roundMs((sumSec * 1000.0) / float64(count))

	if len(samples) > 0 {
		sort.Float64s(samples)
		pct := func(q float64) float64 {
			idx := int(math.Ceil(q*float64(len(samples)))) - 1
			if idx < 0 {
				idx = 0
			}
			if idx >= len(samples) {
				idx = len(samples) - 1
			}
			return roundMs(samples[idx] * 1000.0)
		}
		out.P50Ms = pct(0.50)
		out.P95Ms = pct(0.95)
		out.P99Ms = pct(0.99)
	}
	return out
}

func roundMs(v float64) float64 {
	return math.Round(v*1000) / 1000
}

// FormatPromBound formats a finite histogram upper bound according to Prometheus conventions.
func FormatPromBound(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

func escapeLabelValue(val string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(val)
}

// WritePrometheusHistogram writes a Prometheus 0.0.4 and OpenMetrics 1.0.0 compliant
// histogram metric family (labeled when labelKey != "", unlabeled when labelKey == "").
func WritePrometheusHistogram(b *strings.Builder, name, help, labelKey string, series []LabeledSnapshot) {
	if len(series) == 0 {
		return
	}
	fmt.Fprintf(b, "# HELP %s %s\n", name, help)
	fmt.Fprintf(b, "# TYPE %s histogram\n", name)

	for _, s := range series {
		snap := s.Snapshot
		buckets := snap.Buckets
		if len(buckets) == 0 {
			buckets = make([]BucketCount, len(DefaultWakeBuckets))
			for i, ub := range DefaultWakeBuckets {
				buckets[i] = BucketCount{UpperBound: ub, Count: 0}
			}
		}

		if labelKey == "" {
			for _, bk := range buckets {
				fmt.Fprintf(b, "%s_bucket{le=%q} %d\n", name, FormatPromBound(bk.UpperBound), bk.Count)
			}
			fmt.Fprintf(b, "%s_bucket{le=\"+Inf\"} %d\n", name, snap.Count)
			fmt.Fprintf(b, "%s_sum %.6f\n", name, snap.SumSeconds)
			fmt.Fprintf(b, "%s_count %d\n", name, snap.Count)
		} else {
			lv := escapeLabelValue(s.LabelValue)
			for _, bk := range buckets {
				fmt.Fprintf(b, "%s_bucket{%s=%q,le=%q} %d\n", name, labelKey, lv, FormatPromBound(bk.UpperBound), bk.Count)
			}
			fmt.Fprintf(b, "%s_bucket{%s=%q,le=\"+Inf\"} %d\n", name, labelKey, lv, snap.Count)
			fmt.Fprintf(b, "%s_sum{%s=%q} %.6f\n", name, labelKey, lv, snap.SumSeconds)
			fmt.Fprintf(b, "%s_count{%s=%q} %d\n", name, labelKey, lv, snap.Count)
		}
	}
}
