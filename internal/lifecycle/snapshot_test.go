package lifecycle

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SalyyS1/omnivm/internal/config"
	"github.com/SalyyS1/omnivm/internal/ksm"
	"github.com/SalyyS1/omnivm/internal/logstore"
	"github.com/SalyyS1/omnivm/internal/overlay"
	"github.com/SalyyS1/omnivm/internal/registry"
	"github.com/SalyyS1/omnivm/internal/vmm"
)

type snapshotTrackingVMM struct {
	base      *barrierVMM
	pauses    int64
	resumes   int64
	snapshots int64

	mu      sync.Mutex
	configs []vmm.VMConfig
}

func newSnapshotTrackingVMM() *snapshotTrackingVMM {
	return &snapshotTrackingVMM{base: newBarrierVMM()}
}

func (s *snapshotTrackingVMM) CreateVM(cfg vmm.VMConfig) (vmm.Handle, error) {
	s.mu.Lock()
	s.configs = append(s.configs, cfg)
	s.mu.Unlock()
	return s.base.CreateVM(cfg)
}

func (s *snapshotTrackingVMM) StartVM(h vmm.Handle) (vmm.ControlChannel, error) {
	return s.base.StartVM(h)
}

func (s *snapshotTrackingVMM) PauseVM(h vmm.Handle) error {
	atomic.AddInt64(&s.pauses, 1)
	return s.base.PauseVM(h)
}

func (s *snapshotTrackingVMM) ResumeVM(h vmm.Handle) error {
	atomic.AddInt64(&s.resumes, 1)
	return s.base.ResumeVM(h)
}

func (s *snapshotTrackingVMM) StopVM(h vmm.Handle) error {
	return s.base.StopVM(h)
}

func (s *snapshotTrackingVMM) HostEndpoints(h vmm.Handle) ([]vmm.HostEndpoint, error) {
	return s.base.HostEndpoints(h)
}

func (s *snapshotTrackingVMM) Capabilities() vmm.BackendCaps {
	caps := s.base.Capabilities()
	caps.RootFSType = vmm.RootFSBlockImage
	return caps
}

func (s *snapshotTrackingVMM) SnapshotVM(h vmm.Handle, destDir string) error {
	atomic.AddInt64(&s.snapshots, 1)
	if err := os.MkdirAll(destDir, 0700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(destDir, "state.json"), []byte(`{"vm":"`+h.ID+`"}`), 0600)
}

func setupSnapshotTestManager(t *testing.T, v vmm.VMM, maxVMs int) (*Manager, *config.Config, *registry.DB) {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{
		DataDir:         dir,
		OverlaysDir:     filepath.Join(dir, "overlays"),
		WorkspacesDir:   filepath.Join(dir, "workspaces"),
		SnapshotsDir:    filepath.Join(dir, "snapshots"),
		LogsDir:         filepath.Join(dir, "logs"),
		DBPath:          filepath.Join(dir, "ovm.db"),
		SocketPath:      filepath.Join(dir, "ovmd.sock"),
		KernelPath:      filepath.Join(dir, "kernel", "vmlinux"),
		DefaultMemoryMB: 256,
		DefaultVCPUs:    1,
		MinDiskMB:       256,
		MaxDiskMB:       4096,
		MaxActiveVMs:    maxVMs,
		PauseAfterIdle:  10 * time.Minute,
		StopAfterIdle:   20 * time.Minute,
		MemoryDedup:     true,
	}
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	reg, err := registry.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })

	ls := logstore.NewStore(cfg.LogsDir)
	ov := overlay.NewCopyOverlay(cfg.OverlaysDir)
	m := NewManager(v, cfg, ls, nil, ov)
	m.SetRegistry(reg)
	t.Cleanup(func() { m.Shutdown() })
	return m, cfg, reg
}

func createSparseTestDisk(t *testing.T, path string, sizeBytes int64, marker string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(sizeBytes); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte(marker), 0); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCreateSnapshotAndBranch_RunningVMQuiesceAndCoWIsolation(t *testing.T) {
	sv := newSnapshotTrackingVMM()
	m, cfg, _ := setupSnapshotTestManager(t, sv, 0)

	baseDisk := filepath.Join(cfg.OverlaysDir, "inst-base.ext4")
	createSparseTestDisk(t, baseDisk, 1024*1024, "GOLDEN_ROOTFS_V1")

	baseWs := filepath.Join(cfg.WorkspacesDir, "base-agent")
	if err := os.MkdirAll(baseWs, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(baseWs, "checkpoint.txt"), []byte("step=42"), 0644); err != nil {
		t.Fatal(err)
	}

	baseInst := m.CreateInstance(
		"inst-base",
		[]string{"python3", "/workspace/run.py"},
		[]vmm.PortExpose{{GuestPort: 8080, Protocol: "http"}},
		WithHandle("base-agent"),
		WithImageRef("python:3.12-alpine"),
		WithRootfs(baseDisk),
		WithWorkspace(baseWs),
		WithEnv(map[string]string{"STAGE": "seed"}),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := m.EnsureInstance(ctx, "inst-base"); err != nil {
		t.Fatalf("boot inst-base: %v", err)
	}

	snap, err := m.CreateSnapshot(ctx, "base-agent", SnapshotRequest{
		Name:            "step-42",
		IncludeVMMState: true,
	})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}

	// Running VM must have been transiently paused, snapshotted, and resumed back to RUNNING.
	if atomic.LoadInt64(&sv.pauses) != 1 || atomic.LoadInt64(&sv.resumes) != 1 || atomic.LoadInt64(&sv.snapshots) != 1 {
		t.Fatalf("expected 1 pause, 1 resume, 1 vmm snapshot; got pauses=%d resumes=%d snapshots=%d",
			sv.pauses, sv.resumes, sv.snapshots)
	}
	if baseInst.GetState() != StateRunning {
		t.Fatalf("baseInst state = %q, want running after snapshot", baseInst.GetState())
	}
	if snap.CoWSavedBytes < 900*1024 {
		t.Errorf("expected sparse/CoW savings >= 900 KiB on 1 MiB disk, got %d", snap.CoWSavedBytes)
	}
	if snap.VMMStateDir == "" {
		t.Error("expected non-empty VMMStateDir when IncludeVMMState=true")
	}

	// Duplicate snapshot name must return ErrSnapshotConflict.
	if _, dupErr := m.CreateSnapshot(ctx, "base-agent", SnapshotRequest{Name: "step-42"}); !errors.Is(dupErr, ErrSnapshotConflict) {
		t.Fatalf("expected ErrSnapshotConflict on duplicate name, got %v", dupErr)
	}

	// Branch 1: eager start with overridden env.
	b1, res1, err := m.BranchInstance(ctx, BranchRequest{
		SnapshotID: "step-42",
		Handle:     "rollout-1",
		Env:        map[string]string{"ROLLOUT": "1"},
		Start:      true,
	})
	if err != nil {
		t.Fatalf("BranchInstance rollout-1: %v", err)
	}
	if b1.GetState() != StateRunning || res1.SourceSnapshotID != snap.ID || res1.ParentID != "inst-base" {
		t.Fatalf("unexpected rollout-1 result: state=%s res=%+v", b1.GetState(), res1)
	}

	// Branch 2: lazy scale-to-zero branch (Start: false -> 0 MB RAM until woken).
	b2, res2, err := m.BranchInstance(ctx, BranchRequest{
		SnapshotID: snap.ID,
		Handle:     "rollout-2",
		Start:      false,
	})
	if err != nil {
		t.Fatalf("BranchInstance rollout-2: %v", err)
	}
	if b2.GetState() != StateStopped || res2.State != StateStopped {
		t.Fatalf("expected lazy branch rollout-2 in stopped state, got %s", b2.GetState())
	}

	// Verify workspace isolation across branches and parent.
	b1WsFile := filepath.Join(res1.Workspace, "checkpoint.txt")
	b2WsFile := filepath.Join(res2.Workspace, "checkpoint.txt")
	gotB1, err := os.ReadFile(b1WsFile)
	if err != nil || string(gotB1) != "step=42" {
		t.Fatalf("rollout-1 workspace file = %q (err=%v), want step=42", gotB1, err)
	}
	if err := os.WriteFile(b1WsFile, []byte("step=999-diverged"), 0644); err != nil {
		t.Fatal(err)
	}

	gotB2, _ := os.ReadFile(b2WsFile)
	gotBase, _ := os.ReadFile(filepath.Join(baseWs, "checkpoint.txt"))
	if string(gotB2) != "step=42" || string(gotBase) != "step=42" {
		t.Fatalf("CoW workspace isolation violated: b2=%q base=%q", gotB2, gotBase)
	}

	// Mutating rollout-1's .ext4 block image must not affect rollout-2's .ext4 image.
	b1Disk := b1.Snapshot().RootfsPath
	b2Disk := b2.Snapshot().RootfsPath
	fMut, err := os.OpenFile(b1Disk, os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fMut.WriteAt([]byte("BRANCH_1_MUTATED"), 0)
	_ = fMut.Close()

	b2Header := make([]byte, 16)
	fRead, err := os.Open(b2Disk)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fRead.ReadAt(b2Header, 0)
	_ = fRead.Close()
	if !bytes.Equal(b2Header, []byte("GOLDEN_ROOTFS_V1")) {
		t.Fatalf("CoW root disk isolation violated: rollout-2 header = %q", b2Header)
	}

	lm := m.Metrics()
	if lm.SnapshotsCreated != 1 || lm.SnapshotBranches != 2 || lm.CoWSavedBytes == 0 {
		t.Fatalf("unexpected lifecycle snapshot metrics: %+v", lm)
	}
}

func TestBranchInstance_DirectLiveBranchAndQuotaCleanup(t *testing.T) {
	sv := newSnapshotTrackingVMM()
	// Cap active VMs at 2.
	m, cfg, _ := setupSnapshotTestManager(t, sv, 2)

	baseDisk := filepath.Join(cfg.OverlaysDir, "inst-live.ext4")
	createSparseTestDisk(t, baseDisk, 512*1024, "LIVE_DISK")

	m.CreateInstance("inst-live", []string{"serve"}, nil, WithHandle("live-src"), WithRootfs(baseDisk))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := m.EnsureInstance(ctx, "inst-live"); err != nil {
		t.Fatal(err)
	}

	// Direct live branch #1 succeeds (2/2 active VMs).
	_, _, err := m.BranchInstance(ctx, BranchRequest{
		SourceInstanceID: "live-src",
		Handle:           "live-branch-1",
		Start:            true,
	})
	if err != nil {
		t.Fatalf("direct branch 1 failed: %v", err)
	}

	// Direct live branch #2 with Start: true exceeds MaxActiveVMs=2 -> ErrQuotaExceeded and rolls back registration.
	_, _, qErr := m.BranchInstance(ctx, BranchRequest{
		SourceInstanceID: "live-src",
		Handle:           "live-branch-rejected",
		Start:            true,
	})
	if !errors.Is(qErr, ErrQuotaExceeded) {
		t.Fatalf("expected ErrQuotaExceeded, got %v", qErr)
	}
	if ghost := m.GetInstanceByHandle("live-branch-rejected"); ghost != nil {
		t.Fatalf("failed eager branch should be cleaned up, found %+v", ghost)
	}

	// Lazy branch (Start: false) still succeeds at 0 MB RAM even when active VM quota is full!
	lazyInst, _, lErr := m.BranchInstance(ctx, BranchRequest{
		SourceInstanceID: "live-src",
		Handle:           "live-branch-lazy",
		Start:            false,
	})
	if lErr != nil || lazyInst.GetState() != StateStopped {
		t.Fatalf("expected lazy branch to succeed in stopped state, err=%v", lErr)
	}
}

func TestKSMGovernorIntegration_EffectiveMemoryTelemetry(t *testing.T) {
	sv := newSnapshotTrackingVMM()
	m, _, _ := setupSnapshotTestManager(t, sv, 0)

	ksmDir := t.TempDir()
	writeKSM := func(k string, v uint64) {
		_ = os.WriteFile(filepath.Join(ksmDir, k), []byte(strconv.FormatUint(v, 10)+"\n"), 0644)
	}
	for k, v := range map[string]uint64{
		"run":             0,
		"pages_shared":    512,
		"pages_sharing":   0,
		"pages_unshared":  100,
		"pages_volatile":  10,
		"full_scans":      1,
		"pages_to_scan":   100,
		"sleep_millisecs": 20,
	} {
		writeKSM(k, v)
	}

	gov := ksm.NewGovernor(ksmDir, ksm.ModeAuto, 4096)
	m.SetKSMGovernor(gov)

	m.CreateInstance("vm-1", []string{"w1"}, nil)
	m.CreateInstance("vm-2", []string{"w2"}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Boot 1st VM (256 MB): KSM stays paused (run=0) for zero CPU overhead.
	if err := m.EnsureInstance(ctx, "vm-1"); err != nil {
		t.Fatal(err)
	}
	if km := gov.ReadMetrics(); km.Active {
		t.Fatal("expected KSM inactive with only 1 resident VM")
	}

	// Boot 2nd VM (total 512 MB): KSM auto-enables (run=1).
	if err := m.EnsureInstance(ctx, "vm-2"); err != nil {
		t.Fatal(err)
	}
	if km := gov.ReadMetrics(); !km.Active {
		t.Fatal("expected KSM active when 2 microVMs are resident")
	}

	// Simulate kernel merging 49,152 identical 4 KiB pages (192 MiB deduplicated).
	writeKSM("pages_sharing", 49152)

	res := m.HostResources()
	if res.ResidentMemoryMB != 512 || res.KSMSavedMemoryMB != 192 || res.EffectiveResidentMemoryMB != 320 || !res.KSMActive {
		t.Fatalf("unexpected KSM host resource metrics: %+v", res)
	}
}

func TestConcurrentSnapshotAndBranchStorm_RaceSafety(t *testing.T) {
	sv := newSnapshotTrackingVMM()
	m, cfg, _ := setupSnapshotTestManager(t, sv, 0)

	baseDisk := filepath.Join(cfg.OverlaysDir, "inst-storm.ext4")
	createSparseTestDisk(t, baseDisk, 256*1024, "STORM")
	ws := filepath.Join(cfg.WorkspacesDir, "storm")
	_ = os.MkdirAll(ws, 0755)
	_ = os.WriteFile(filepath.Join(ws, "seed.txt"), []byte("seed"), 0644)

	m.CreateInstance("inst-storm", []string{"agent"}, nil, WithHandle("storm"), WithRootfs(baseDisk), WithWorkspace(ws))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := m.EnsureInstance(ctx, "inst-storm"); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			snap, err := m.CreateSnapshot(ctx, "storm", SnapshotRequest{
				Name: fmt.Sprintf("snap-%d", idx),
			})
			if err != nil {
				t.Errorf("CreateSnapshot %d: %v", idx, err)
				return
			}
			_, _, bErr := m.BranchInstance(ctx, BranchRequest{
				SnapshotID: snap.ID,
				Handle:     fmt.Sprintf("storm-branch-%d", idx),
				Start:      idx%2 == 0,
			})
			if bErr != nil {
				t.Errorf("BranchInstance %d: %v", idx, bErr)
			}
		}(i)
	}

	// Concurrent telemetry readers
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 25; i++ {
			_ = m.Metrics()
			_ = m.HostResources()
			_ = m.ListSnapshots("")
		}
	}()

	wg.Wait()

	if len(m.ListSnapshots("storm")) != 6 {
		t.Fatalf("ListSnapshots = %d, want 6", len(m.ListSnapshots("storm")))
	}
}
