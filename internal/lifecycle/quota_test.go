package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SalyyS1/omnivm/internal/config"
	"github.com/SalyyS1/omnivm/internal/logstore"
	"github.com/SalyyS1/omnivm/internal/vmm"
)

func newQuotaManager(t *testing.T, v vmm.VMM, maxMemMB, maxVCPUs, maxVMs int) *Manager {
	t.Helper()
	cfg := &config.Config{
		DefaultMemoryMB: 256,
		DefaultVCPUs:    1,
		PauseAfterIdle:  10 * time.Minute,
		StopAfterIdle:   20 * time.Minute,
		MaxHostMemoryMB: maxMemMB,
		MaxActiveVCPUs:  maxVCPUs,
		MaxActiveVMs:    maxVMs,
	}
	ls := logstore.NewStore(t.TempDir())
	m := NewManager(v, cfg, ls, nil, nil)
	t.Cleanup(func() {
		m.Shutdown()
	})
	return m
}

func TestAdmissionQuota_MaxHostMemoryAndLRUPausedEviction(t *testing.T) {
	bv := newBarrierVMM()
	m := newQuotaManager(t, bv, 512, 0, 0)

	inst1 := m.CreateInstance("inst-1", []string{"echo", "1"}, nil)
	inst2 := m.CreateInstance("inst-2", []string{"echo", "2"}, nil)
	inst3 := m.CreateInstance("inst-3", []string{"echo", "3"}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Boot inst-1 first, pause it; then boot inst-2 later and pause it.
	if err := m.EnsureInstance(ctx, "inst-1"); err != nil {
		t.Fatalf("boot inst-1: %v", err)
	}
	if err := m.PauseInstance("inst-1"); err != nil {
		t.Fatalf("pause inst-1: %v", err)
	}

	time.Sleep(15 * time.Millisecond)

	if err := m.EnsureInstance(ctx, "inst-2"); err != nil {
		t.Fatalf("boot inst-2: %v", err)
	}
	if err := m.PauseInstance("inst-2"); err != nil {
		t.Fatalf("pause inst-2: %v", err)
	}

	res := m.HostResources()
	if res.ResidentMemoryMB != 512 || res.PausedMemoryMB != 512 || res.ActiveMemoryMB != 0 {
		t.Fatalf("unexpected resources with 2 paused VMs: %+v", res)
	}

	// Waking inst-3 requires 256MB; host is at 512/512MB paused.
	// Manager must evict inst-1 (LRU) while keeping inst-2 warm in StatePaused.
	if err := m.EnsureInstance(ctx, "inst-3"); err != nil {
		t.Fatalf("boot inst-3 with LRU eviction: %v", err)
	}
	if inst1.GetState() != StateStopped {
		t.Errorf("inst-1 state = %q, want stopped (LRU evicted)", inst1.GetState())
	}
	if inst2.GetState() != StatePaused {
		t.Errorf("inst-2 state = %q, want paused (kept warm)", inst2.GetState())
	}
	if inst3.GetState() != StateRunning {
		t.Errorf("inst-3 state = %q, want running", inst3.GetState())
	}

	// Waking inst-1 now evicts inst-2 (the remaining paused candidate) so inst-1 + inst-3 run.
	if err := m.EnsureInstance(ctx, "inst-1"); err != nil {
		t.Fatalf("re-boot inst-1 with second eviction: %v", err)
	}
	if inst2.GetState() != StateStopped {
		t.Errorf("inst-2 state = %q, want stopped", inst2.GetState())
	}

	// Both 256MB slots are now actively RUNNING (512/512MB non-evictable).
	// Waking inst-2 must fail with ErrQuotaExceeded without disturbing inst-1 or inst-3.
	err := m.EnsureInstance(ctx, "inst-2")
	if !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("expected ErrQuotaExceeded when active memory is full, got %v", err)
	}
	var qe *QuotaError
	if !errors.As(err, &qe) || qe.Resource != "memory_mb" || qe.Limit != 512 {
		t.Fatalf("unexpected QuotaError details: %+v (%v)", qe, err)
	}

	metrics := m.Metrics()
	if metrics.PausedEvictions != 2 {
		t.Errorf("PausedEvictions = %d, want 2", metrics.PausedEvictions)
	}
	if metrics.QuotaRejections != 1 {
		t.Errorf("QuotaRejections = %d, want 1", metrics.QuotaRejections)
	}
}

func TestAdmissionQuota_WarmResumeRespectsActiveVCPUAndVMCeilings(t *testing.T) {
	bv := newBarrierVMM()
	// Allow 1024MB resident memory (fits 2 VMs) but cap active VMs to 1 and active vCPUs to 2.
	m := newQuotaManager(t, bv, 1024, 2, 1)

	instA := m.CreateInstance("inst-a", []string{"echo", "a"}, nil, WithVCPUs(2))
	m.CreateInstance("inst-b", []string{"echo", "b"}, nil, WithVCPUs(2))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := m.EnsureInstance(ctx, "inst-a"); err != nil {
		t.Fatalf("boot inst-a: %v", err)
	}
	if err := m.PauseInstance("inst-a"); err != nil {
		t.Fatalf("pause inst-a: %v", err)
	}

	// inst-a is PAUSED (0 active vCPUs, 256MB resident), so inst-b can cold-boot.
	if err := m.EnsureInstance(ctx, "inst-b"); err != nil {
		t.Fatalf("boot inst-b while inst-a paused: %v", err)
	}

	// Resuming inst-a while inst-b is RUNNING would exceed MaxActiveVMs=1 and MaxActiveVCPUs=2.
	err := m.EnsureInstance(ctx, "inst-a")
	if !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("expected ErrQuotaExceeded on warm resume over active ceiling, got %v", err)
	}
	if instA.GetState() != StatePaused {
		t.Fatalf("inst-a state = %q, want paused after rejected resume", instA.GetState())
	}

	// Pausing inst-b frees the active slot; inst-a can now warm-resume.
	if err := m.PauseInstance("inst-b"); err != nil {
		t.Fatalf("pause inst-b: %v", err)
	}
	if err := m.EnsureInstance(ctx, "inst-a"); err != nil {
		t.Fatalf("warm resume inst-a after freeing slot: %v", err)
	}
	if instA.GetState() != StateRunning {
		t.Errorf("inst-a state = %q, want running", instA.GetState())
	}
}

type peakTrackingVMM struct {
	base    *mockVMM
	liveVMs int64
	peakVMs int64
}

func newPeakTrackingVMM() *peakTrackingVMM {
	return &peakTrackingVMM{base: newMockVMM()}
}

func (p *peakTrackingVMM) CreateVM(cfg vmm.VMConfig) (vmm.Handle, error) {
	cur := atomic.AddInt64(&p.liveVMs, 1)
	for {
		prev := atomic.LoadInt64(&p.peakVMs)
		if cur <= prev || atomic.CompareAndSwapInt64(&p.peakVMs, prev, cur) {
			break
		}
	}
	time.Sleep(5 * time.Millisecond)
	return p.base.CreateVM(cfg)
}

func (p *peakTrackingVMM) StartVM(h vmm.Handle) (vmm.ControlChannel, error) {
	time.Sleep(5 * time.Millisecond)
	return p.base.StartVM(h)
}

func (p *peakTrackingVMM) PauseVM(h vmm.Handle) error  { return p.base.PauseVM(h) }
func (p *peakTrackingVMM) ResumeVM(h vmm.Handle) error { return p.base.ResumeVM(h) }
func (p *peakTrackingVMM) StopVM(h vmm.Handle) error {
	atomic.AddInt64(&p.liveVMs, -1)
	return p.base.StopVM(h)
}
func (p *peakTrackingVMM) HostEndpoints(h vmm.Handle) ([]vmm.HostEndpoint, error) {
	return p.base.HostEndpoints(h)
}
func (p *peakTrackingVMM) Capabilities() vmm.BackendCaps {
	return p.base.Capabilities()
}

func TestAdmissionQuota_ConcurrentWakeStormNeverOvercommitsUnderRace(t *testing.T) {
	pv := newPeakTrackingVMM()
	// Allow at most 3 VMs (3 * 256MB = 768MB, 3 vCPUs).
	m := newQuotaManager(t, pv, 768, 3, 3)

	const numInstances = 10
	for i := 0; i < numInstances; i++ {
		m.CreateInstance(fmt.Sprintf("inst-%d", i), []string{"echo", "storm"}, nil)
	}

	// Launch 30 concurrent callers (3 callers per instance across 10 instances).
	const callersPerInstance = 3
	var wg sync.WaitGroup
	var succeeded int64
	var rejected int64

	for i := 0; i < numInstances; i++ {
		instID := fmt.Sprintf("inst-%d", i)
		for c := 0; c < callersPerInstance; c++ {
			wg.Add(1)
			go func(id string) {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				err := m.EnsureInstance(ctx, id)
				if err == nil {
					atomic.AddInt64(&succeeded, 1)
				} else if errors.Is(err, ErrQuotaExceeded) {
					atomic.AddInt64(&rejected, 1)
				} else {
					t.Errorf("unexpected error for %s: %v", id, err)
				}
			}(instID)
		}
	}
	wg.Wait()

	if peak := atomic.LoadInt64(&pv.peakVMs); peak > 3 {
		t.Fatalf("peak concurrent VMs = %d, exceeded MaxActiveVMs = 3", peak)
	}

	counts := m.InstanceCounts()
	if counts.Running != 3 || counts.Stopped != 7 {
		t.Fatalf("unexpected final counts: %+v (want 3 running, 7 stopped)", counts)
	}
	// All 3 callers hitting each of the 3 admitted instances must succeed via coalescing (3 * 3 = 9).
	if got := atomic.LoadInt64(&succeeded); got != 9 {
		t.Errorf("succeeded callers = %d, want 9 (3 admitted instances * 3 coalesced callers)", got)
	}
	if got := atomic.LoadInt64(&rejected); got != 21 {
		t.Errorf("rejected callers = %d, want 21", got)
	}

	res := m.HostResources()
	if res.ResidentMemoryMB != 768 || res.ActiveVCPUs != 3 || res.ActiveVMs != 3 {
		t.Errorf("unexpected HostResources after storm: %+v", res)
	}
}
