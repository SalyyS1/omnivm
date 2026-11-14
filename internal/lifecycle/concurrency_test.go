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

// dynamicEndpointVMM models CloudHypervisorVMM's endpoint lifecycle: CreateVM populates
// endpoints from cfg.ExposePorts, and DynamicExposePort / DynamicUnexposePort mutate
// the live VM's endpoint table at runtime.
type dynamicEndpointVMM struct {
	*barrierVMM
	mu               sync.Mutex
	endpoints        map[string][]vmm.HostEndpoint
	guestIP          string
	dynExposeCalls   int64
	dynUnexposeCalls int64
}

func newDynamicEndpointVMM(guestIP string) *dynamicEndpointVMM {
	return &dynamicEndpointVMM{
		barrierVMM: newBarrierVMM(),
		endpoints:  make(map[string][]vmm.HostEndpoint),
		guestIP:    guestIP,
	}
}

func (d *dynamicEndpointVMM) CreateVM(cfg vmm.VMConfig) (vmm.Handle, error) {
	h, err := d.barrierVMM.CreateVM(cfg)
	if err != nil {
		return h, err
	}
	d.mu.Lock()
	var eps []vmm.HostEndpoint
	for _, ep := range cfg.ExposePorts {
		eps = append(eps, vmm.HostEndpoint{
			GuestPort:   ep.GuestPort,
			HostPort:    ep.GuestPort,
			Protocol:    ep.Protocol,
			BackendAddr: d.guestIP,
		})
	}
	d.endpoints[h.ID] = eps
	d.mu.Unlock()
	return h, nil
}

func (d *dynamicEndpointVMM) HostEndpoints(h vmm.Handle) ([]vmm.HostEndpoint, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	src := d.endpoints[h.ID]
	out := make([]vmm.HostEndpoint, len(src))
	copy(out, src)
	return out, nil
}

func (d *dynamicEndpointVMM) DynamicExposePort(h vmm.Handle, guestPort int) (int, error) {
	atomic.AddInt64(&d.dynExposeCalls, 1)
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, ep := range d.endpoints[h.ID] {
		if ep.GuestPort == guestPort {
			return ep.HostPort, nil
		}
	}
	d.endpoints[h.ID] = append(d.endpoints[h.ID], vmm.HostEndpoint{
		GuestPort:   guestPort,
		HostPort:    guestPort,
		Protocol:    "tcp",
		BackendAddr: d.guestIP,
	})
	return guestPort, nil
}

func (d *dynamicEndpointVMM) DynamicUnexposePort(h vmm.Handle, guestPort int) error {
	atomic.AddInt64(&d.dynUnexposeCalls, 1)
	d.mu.Lock()
	defer d.mu.Unlock()
	var kept []vmm.HostEndpoint
	for _, ep := range d.endpoints[h.ID] {
		if ep.GuestPort != guestPort {
			kept = append(kept, ep)
		}
	}
	d.endpoints[h.ID] = kept
	return nil
}

func TestBootInstance_ExposePortDuringStateStartingReconcilesEndpoint(t *testing.T) {
	dv := newDynamicEndpointVMM("172.16.0.42")
	startEntered := make(chan struct{})
	allowStart := make(chan struct{})
	var once sync.Once
	dv.onStart = func() {
		once.Do(func() { close(startEntered) })
		<-allowStart
	}

	m := newCustomManager(t, dv, 10*time.Minute, 20*time.Minute)
	m.OnExposePort(func(id string, guestPort, publicPort int, protocol string) (int, error) {
		if publicPort > 0 {
			return publicPort, nil
		}
		return 38080, nil
	})
	m.SetPublicPortsLookup(func(id string) map[int]int {
		return map[int]int{80: 38080}
	})

	// Create instance with NO initial exposed ports (matching POST /v1/instances followed by POST /expose).
	inst := m.CreateInstance("inst-midboot-expose", []string{"python3", "-m", "http.server", "80"}, nil)

	bootDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		bootDone <- m.EnsureInstance(ctx, "inst-midboot-expose")
	}()

	// Wait until CreateVM has already completed with ExposePorts=[] and StartVM is parked.
	<-startEntered
	if got := inst.GetState(); got != StateStarting {
		t.Fatalf("state during StartVM = %q, want %q", got, StateStarting)
	}

	// Expose guest port 80 while the VM is mid-boot (StateStarting).
	pubPort, err := m.ExposePort("inst-midboot-expose", 80, 0, "http")
	if err != nil {
		t.Fatalf("ExposePort during StateStarting failed: %v", err)
	}
	if pubPort != 38080 {
		t.Fatalf("allocated public port = %d, want 38080", pubPort)
	}

	close(allowStart)
	if err := <-bootDone; err != nil {
		t.Fatalf("EnsureInstance failed: %v", err)
	}

	// Immediately upon EnsureInstance returning, GetEndpoint MUST resolve the reconciled tap endpoint.
	ep, err := m.GetEndpoint("inst-midboot-expose", 80)
	if err != nil {
		t.Fatalf("GetEndpoint(80) after mid-boot ExposePort failed: %v", err)
	}
	if ep != "172.16.0.42:80" {
		t.Errorf("GetEndpoint(80) = %q, want %q", ep, "172.16.0.42:80")
	}
	if got := atomic.LoadInt64(&dv.dynExposeCalls); got != 1 {
		t.Errorf("DynamicExposePort calls = %d, want 1", got)
	}
}

func TestBootInstance_UnexposePortDuringStateStartingRemovesEndpoint(t *testing.T) {
	dv := newDynamicEndpointVMM("172.16.0.43")
	startEntered := make(chan struct{})
	allowStart := make(chan struct{})
	var once sync.Once
	dv.onStart = func() {
		once.Do(func() { close(startEntered) })
		<-allowStart
	}

	m := newCustomManager(t, dv, 10*time.Minute, 20*time.Minute)
	m.CreateInstance("inst-midboot-unexpose", []string{"python3", "-m", "http.server", "8080"}, []vmm.PortExpose{
		{GuestPort: 8080, Protocol: "http"},
	})

	bootDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		bootDone <- m.EnsureInstance(ctx, "inst-midboot-unexpose")
	}()

	<-startEntered
	if err := m.UnexposePort("inst-midboot-unexpose", 8080); err != nil {
		t.Fatalf("UnexposePort during StateStarting failed: %v", err)
	}

	close(allowStart)
	if err := <-bootDone; err != nil {
		t.Fatalf("EnsureInstance failed: %v", err)
	}

	if _, err := m.GetEndpoint("inst-midboot-unexpose", 8080); err == nil {
		t.Fatal("expected GetEndpoint(8080) to fail after mid-boot UnexposePort, but got nil error")
	}
	if got := atomic.LoadInt64(&dv.dynUnexposeCalls); got != 1 {
		t.Errorf("DynamicUnexposePort calls = %d, want 1", got)
	}
}
