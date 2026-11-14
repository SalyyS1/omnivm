package lifecycle

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SalyyS1/omnivm/internal/config"
	"github.com/SalyyS1/omnivm/internal/logstore"
	"github.com/SalyyS1/omnivm/internal/vmm"
)

// barrierVMM wraps mockVMM with optional hooks and atomic call counters
// so tests can deterministically pause VMM calls mid-flight.
type barrierVMM struct {
	base *mockVMM

	createCalls int64
	startCalls  int64
	pauseCalls  int64
	resumeCalls int64
	stopCalls   int64

	onStart  func()
	onPause  func()
	onResume func()
	onStop   func()
}

func newBarrierVMM() *barrierVMM {
	return &barrierVMM{base: newMockVMM()}
}

func (b *barrierVMM) CreateVM(cfg vmm.VMConfig) (vmm.Handle, error) {
	atomic.AddInt64(&b.createCalls, 1)
	return b.base.CreateVM(cfg)
}

func (b *barrierVMM) StartVM(h vmm.Handle) (vmm.ControlChannel, error) {
	atomic.AddInt64(&b.startCalls, 1)
	if b.onStart != nil {
		b.onStart()
	}
	return b.base.StartVM(h)
}

func (b *barrierVMM) PauseVM(h vmm.Handle) error {
	atomic.AddInt64(&b.pauseCalls, 1)
	if b.onPause != nil {
		b.onPause()
	}
	return b.base.PauseVM(h)
}

func (b *barrierVMM) ResumeVM(h vmm.Handle) error {
	atomic.AddInt64(&b.resumeCalls, 1)
	if b.onResume != nil {
		b.onResume()
	}
	return b.base.ResumeVM(h)
}

func (b *barrierVMM) StopVM(h vmm.Handle) error {
	atomic.AddInt64(&b.stopCalls, 1)
	if b.onStop != nil {
		b.onStop()
	}
	return b.base.StopVM(h)
}

func (b *barrierVMM) HostEndpoints(h vmm.Handle) ([]vmm.HostEndpoint, error) {
	return b.base.HostEndpoints(h)
}

func (b *barrierVMM) Capabilities() vmm.BackendCaps {
	return b.base.Capabilities()
}

func newCustomManager(t *testing.T, v vmm.VMM, pauseAfter, stopAfter time.Duration) *Manager {
	t.Helper()
	cfg := &config.Config{
		DefaultMemoryMB: 256,
		DefaultVCPUs:    1,
		PauseAfterIdle:  pauseAfter,
		StopAfterIdle:   stopAfter,
	}
	ls := logstore.NewStore(t.TempDir())
	m := NewManager(v, cfg, ls, nil, nil)
	t.Cleanup(func() {
		m.Shutdown()
	})
	return m
}

func TestEnsureInstance_ConcurrentColdStartCoalescing(t *testing.T) {
	bv := newBarrierVMM()
	startEntered := make(chan struct{})
	allowStart := make(chan struct{})
	var once sync.Once
	bv.onStart = func() {
		once.Do(func() { close(startEntered) })
		<-allowStart
	}

	m := newCustomManager(t, bv, 10*time.Minute, 20*time.Minute)
	m.CreateInstance("inst-cold", []string{"echo", "ok"}, nil)

	const workers = 24
	errs := make(chan error, workers)
	var wg sync.WaitGroup

	// Launch first caller so it enters StartVM and holds StateStarting.
	wg.Add(1)
	go func() {
		defer wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		errs <- m.EnsureInstance(ctx, "inst-cold")
	}()

	<-startEntered

	// Launch remaining 23 callers while cold boot is mid-flight.
	for i := 1; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			errs <- m.EnsureInstance(ctx, "inst-cold")
		}()
	}

	// Give waiters a moment to park on transitionCh, then release StartVM.
	time.Sleep(15 * time.Millisecond)
	close(allowStart)
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("EnsureInstance returned error: %v", err)
		}
	}

	if got := atomic.LoadInt64(&bv.createCalls); got != 1 {
		t.Errorf("CreateVM calls = %d, want 1", got)
	}
	if got := atomic.LoadInt64(&bv.startCalls); got != 1 {
		t.Errorf("StartVM calls = %d, want 1", got)
	}

	metrics := m.Metrics()
	if metrics.ColdStarts != 1 {
		t.Errorf("ColdStarts = %d, want 1", metrics.ColdStarts)
	}
	if metrics.CoalescedWakes < 1 {
		t.Errorf("CoalescedWakes = %d, want >= 1", metrics.CoalescedWakes)
	}
}

func TestEnsureInstance_ConcurrentWarmResumeCoalescing(t *testing.T) {
	bv := newBarrierVMM()
	m := newCustomManager(t, bv, 10*time.Minute, 20*time.Minute)
	inst := m.CreateInstance("inst-warm", []string{"echo"}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.EnsureInstance(ctx, "inst-warm"); err != nil {
		t.Fatalf("initial boot: %v", err)
	}
	if err := m.PauseInstance("inst-warm"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if inst.GetState() != StatePaused {
		t.Fatalf("state = %q, want paused", inst.GetState())
	}

	resumeEntered := make(chan struct{})
	allowResume := make(chan struct{})
	var once sync.Once
	bv.onResume = func() {
		once.Do(func() { close(resumeEntered) })
		<-allowResume
	}

	const workers = 24
	errs := make(chan error, workers)
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		errs <- m.EnsureInstance(ctx, "inst-warm")
	}()

	<-resumeEntered

	for i := 1; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- m.EnsureInstance(ctx, "inst-warm")
		}()
	}

	time.Sleep(15 * time.Millisecond)
	close(allowResume)
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("EnsureInstance during warm resume: %v", err)
		}
	}

	if got := atomic.LoadInt64(&bv.resumeCalls); got != 1 {
		t.Errorf("ResumeVM calls = %d, want 1", got)
	}
	metrics := m.Metrics()
	if metrics.WarmResumes != 1 {
		t.Errorf("WarmResumes = %d, want 1", metrics.WarmResumes)
	}
	if metrics.CoalescedWakes < 1 {
		t.Errorf("CoalescedWakes = %d, want >= 1", metrics.CoalescedWakes)
	}
}

func TestEnsureInstance_ConcurrentWakeDuringReap(t *testing.T) {
	bv := newBarrierVMM()
	m := newCustomManager(t, bv, 10*time.Minute, 20*time.Minute)
	inst := m.CreateInstance("inst-reap", []string{"echo", "wake"}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.EnsureInstance(ctx, "inst-reap"); err != nil {
		t.Fatalf("initial boot: %v", err)
	}
	if err := m.PauseInstance("inst-reap"); err != nil {
		t.Fatalf("pause: %v", err)
	}

	stopEntered := make(chan struct{})
	allowStop := make(chan struct{})
	var stopOnce sync.Once
	bv.onStop = func() {
		stopOnce.Do(func() {
			close(stopEntered)
			<-allowStop
		})
	}

	// Trigger extended-idle reap in background; it blocks inside StopVM.
	reapDone := make(chan struct{})
	go func() {
		defer close(reapDone)
		m.stopIdleInstance(inst)
	}()

	<-stopEntered

	// Spawn 16 concurrent router-style wakes while StopVM is mid-execution.
	const workers = 16
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.ResetActivity("inst-reap")
			defer m.OnConnectionClose("inst-reap")
			errs <- m.EnsureInstance(ctx, "inst-reap")
		}()
	}

	time.Sleep(20 * time.Millisecond)

	// While StopVM is still blocked, no second VM may be created.
	if got := atomic.LoadInt64(&bv.createCalls); got != 1 {
		t.Fatalf("CreateVM called mid-reap before StopVM finished: got %d, want 1", got)
	}

	// Allow StopVM to complete; all 16 wakes must cleanly hand off to a single fresh cold boot.
	close(allowStop)
	<-reapDone
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("EnsureInstance wake-during-reap failed: %v", err)
		}
	}

	if inst.GetState() != StateRunning {
		t.Fatalf("final state = %q, want running", inst.GetState())
	}
	if got := atomic.LoadInt64(&bv.createCalls); got != 2 {
		t.Errorf("CreateVM total calls = %d, want 2 (initial + single post-reap boot)", got)
	}
	if got := atomic.LoadInt64(&bv.startCalls); got != 2 {
		t.Errorf("StartVM total calls = %d, want 2", got)
	}

	metrics := m.Metrics()
	if metrics.IdleStops != 1 {
		t.Errorf("IdleStops = %d, want 1", metrics.IdleStops)
	}
	if metrics.ReapWakeHandoffs < 1 {
		t.Errorf("ReapWakeHandoffs = %d, want >= 1", metrics.ReapWakeHandoffs)
	}
}

func TestPauseInstance_AbortsWhenActivityArrivesMidPause(t *testing.T) {
	bv := newBarrierVMM()
	m := newCustomManager(t, bv, 10*time.Minute, 20*time.Minute)
	inst := m.CreateInstance("inst-midpause", []string{"echo"}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.EnsureInstance(ctx, "inst-midpause"); err != nil {
		t.Fatalf("boot: %v", err)
	}

	pauseEntered := make(chan struct{})
	allowPause := make(chan struct{})
	var once sync.Once
	bv.onPause = func() {
		once.Do(func() { close(pauseEntered) })
		<-allowPause
	}

	pauseDone := make(chan struct{})
	go func() {
		defer close(pauseDone)
		m.pauseInstance(inst)
	}()

	<-pauseEntered

	// Simulate an inbound router connection arriving while PauseVM is executing.
	m.ResetActivity("inst-midpause")
	defer m.OnConnectionClose("inst-midpause")

	close(allowPause)
	<-pauseDone

	if inst.GetState() != StateRunning {
		t.Fatalf("state = %q, want running after aborted pause", inst.GetState())
	}
	if got := atomic.LoadInt64(&bv.resumeCalls); got != 1 {
		t.Errorf("ResumeVM calls = %d, want 1 immediate undo-pause", got)
	}

	metrics := m.Metrics()
	if metrics.AbortedPauses != 1 {
		t.Errorf("AbortedPauses = %d, want 1", metrics.AbortedPauses)
	}
	if metrics.IdlePauses != 0 {
		t.Errorf("IdlePauses = %d, want 0", metrics.IdlePauses)
	}
}

func TestStopIdleInstance_SkipsWhenConnectionActive(t *testing.T) {
	bv := newBarrierVMM()
	m := newCustomManager(t, bv, 10*time.Minute, 20*time.Minute)
	inst := m.CreateInstance("inst-active", []string{"echo"}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.EnsureInstance(ctx, "inst-active"); err != nil {
		t.Fatalf("boot: %v", err)
	}
	if err := m.PauseInstance("inst-active"); err != nil {
		t.Fatalf("pause: %v", err)
	}

	// Connection arrives right before stopTimer callback runs.
	m.ResetActivity("inst-active")
	defer m.OnConnectionClose("inst-active")

	m.stopIdleInstance(inst)

	if got := atomic.LoadInt64(&bv.stopCalls); got != 0 {
		t.Fatalf("StopVM should not be called while activeConns > 0, got %d", got)
	}
	if err := m.EnsureInstance(ctx, "inst-active"); err != nil {
		t.Fatalf("EnsureInstance warm resume: %v", err)
	}
	if got := atomic.LoadInt64(&bv.resumeCalls); got != 1 {
		t.Errorf("ResumeVM calls = %d, want 1", got)
	}
}

func TestScaleToZero_FullIdleTransitionAndSnapshotRaceSafety(t *testing.T) {
	bv := newBarrierVMM()
	m := newCustomManager(t, bv, 35*time.Millisecond, 45*time.Millisecond)
	inst := m.CreateInstance("inst-idle", []string{"python3", "server.py"}, []vmm.PortExpose{
		{GuestPort: 8080, Protocol: "http"},
	}, WithHandle("idle-web"))

	// Continuously read Snapshot(), InstanceCounts(), and Metrics() from a concurrent goroutine
	// to verify zero data races across all timer-driven transitions under go test -race.
	stopReaders := make(chan struct{})
	var readerWG sync.WaitGroup
	readerWG.Add(1)
	go func() {
		defer readerWG.Done()
		for {
			select {
			case <-stopReaders:
				return
			default:
				_ = inst.Snapshot()
				_ = m.InstanceCounts()
				_ = m.Metrics()
				time.Sleep(2 * time.Millisecond)
			}
		}
	}()
	defer func() {
		close(stopReaders)
		readerWG.Wait()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.EnsureInstance(ctx, "inst-idle"); err != nil {
		t.Fatalf("boot: %v", err)
	}

	// Wait for automatic transition to StateStopped (running -> paused -> stopped).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if inst.GetState() == StateStopped {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if inst.GetState() != StateStopped {
		t.Fatalf("instance did not scale to zero within timeout, state = %q", inst.GetState())
	}

	counts := m.InstanceCounts()
	if counts.Total != 1 || counts.Stopped != 1 || counts.Running != 0 {
		t.Errorf("unexpected InstanceCounts after scale-to-zero: %+v", counts)
	}

	metrics := m.Metrics()
	if metrics.ColdStarts != 1 || metrics.IdlePauses != 1 || metrics.IdleStops != 1 {
		t.Errorf("unexpected metrics after scale-to-zero: %+v", metrics)
	}

	// Wake again from zero.
	if err := m.EnsureInstance(ctx, "inst-idle"); err != nil {
		t.Fatalf("wake from zero: %v", err)
	}
	if inst.GetState() != StateRunning {
		t.Fatalf("state after wake = %q, want running", inst.GetState())
	}
	if got := m.Metrics().ColdStarts; got != 2 {
		t.Errorf("ColdStarts after wake = %d, want 2", got)
	}
}
