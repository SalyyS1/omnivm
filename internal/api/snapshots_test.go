package api

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SalyyS1/omnivm/internal/client"
	"github.com/SalyyS1/omnivm/internal/config"
	"github.com/SalyyS1/omnivm/internal/ksm"
	"github.com/SalyyS1/omnivm/internal/lifecycle"
	"github.com/SalyyS1/omnivm/internal/logstore"
	"github.com/SalyyS1/omnivm/internal/overlay"
	"github.com/SalyyS1/omnivm/internal/registry"
	"github.com/SalyyS1/omnivm/internal/router"
	"github.com/SalyyS1/omnivm/internal/secrets"
	"github.com/SalyyS1/omnivm/internal/vmm"
)

func TestSnapshotsAndBranchingHTTP_EndToEndWithKSMAndCoWMetrics(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "ovmd.sock")

	// Set up synthetic Linux KSM sysfs directory
	ksmDir := filepath.Join(dir, "ksm")
	if err := os.MkdirAll(ksmDir, 0755); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		"run":             "1",
		"pages_shared":    "512",
		"pages_sharing":   "4096", // 4096 * 4096B = 16 MiB saved
		"pages_unshared":  "128",
		"pages_volatile":  "16",
		"full_scans":      "3",
		"pages_to_scan":   "256",
		"sleep_millisecs": "200",
	} {
		if err := os.WriteFile(filepath.Join(ksmDir, k), []byte(v+"\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	cfg := &config.Config{
		DataDir:         dir,
		SocketPath:      sockPath,
		OverlaysDir:     filepath.Join(dir, "overlays"),
		WorkspacesDir:   filepath.Join(dir, "workspaces"),
		SnapshotsDir:    filepath.Join(dir, "snapshots"),
		LogsDir:         filepath.Join(dir, "logs"),
		DefaultMemoryMB: 256,
		DefaultVCPUs:    1,
		MinDiskMB:       256,
		MaxDiskMB:       2048,
		MaxActiveVMs:    2,
		PauseAfterIdle:  time.Hour,
		StopAfterIdle:   2 * time.Hour,
		RouterAddr:      "127.0.0.1:0",
		KSMGovernor:     "auto",
		MemoryDedup:     true,
	}
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	reg, err := registry.Open(filepath.Join(dir, "ovm.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()

	ss, err := secrets.NewStore(filepath.Join(dir, "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	ls := logstore.NewStore(cfg.LogsDir)
	ov := overlay.NewCopyOverlay(cfg.OverlaysDir)
	backend := &apiTestVMM{}
	lm := lifecycle.NewManager(backend, cfg, ls, nil, ov)
	lm.SetRegistry(reg)
	lm.SetSecretStore(ss)
	lm.SetKSMGovernor(ksm.NewGovernor(ksmDir, "auto", 4096))

	rtr := router.New(lm, "127.0.0.1:0")
	if err := rtr.Start(); err != nil {
		t.Fatal(err)
	}
	defer rtr.Stop(context.Background())

	srv := NewServer(cfg, backend, lm, reg, ss, ls, rtr)
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop(context.Background())

	// Seed a source instance with a sparse 256 KiB disk image and workspace file.
	srcWS := filepath.Join(cfg.WorkspacesDir, "seed")
	if err := os.MkdirAll(srcWS, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcWS, "state.txt"), []byte("baseline-v1"), 0644); err != nil {
		t.Fatal(err)
	}
	srcDisk := filepath.Join(cfg.OverlaysDir, "inst-seed.ext4")
	diskBuf := make([]byte, 256*1024)
	copy(diskBuf[:16], []byte("EXT4-SEED-HEADER"))
	if err := os.WriteFile(srcDisk, diskBuf, 0644); err != nil {
		t.Fatal(err)
	}

	lm.CreateInstance("inst-seed", []string{"python3", "/workspace/app.py"}, []vmm.PortExpose{{GuestPort: 8080, Protocol: "http"}},
		lifecycle.WithHandle("seed"),
		lifecycle.WithImageRef("python:3.12-alpine"),
		lifecycle.WithRootfs(srcDisk),
		lifecycle.WithWorkspace(srcWS),
	)
	if err := lm.EnsureInstance(context.Background(), "inst-seed"); err != nil {
		t.Fatalf("boot seed: %v", err)
	}

	cli := client.New(sockPath)
	ctx := context.Background()

	// 1. Create named snapshot via Go client over Unix socket
	snap, err := cli.CreateSnapshot(ctx, "seed", client.CreateSnapshotRequest{Name: "golden-v1"})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	if snap.Name != "golden-v1" || snap.SourceInstanceID != "inst-seed" {
		t.Fatalf("unexpected snapshot metadata: %+v", snap)
	}
	if snap.LogicalBytes == 0 || snap.CoWSavedBytes == 0 {
		t.Fatalf("expected non-zero logical and CoW saved bytes, got logical=%d cow=%d", snap.LogicalBytes, snap.CoWSavedBytes)
	}

	assertStatus := func(err error, want int) {
		t.Helper()
		var apiErr *client.APIError
		if !errors.As(err, &apiErr) || apiErr.StatusCode != want {
			t.Fatalf("expected HTTP %d APIError, got %v", want, err)
		}
	}

	// Duplicate snapshot name must fail with HTTP 409 Conflict
	_, err = cli.CreateSnapshot(ctx, "seed", client.CreateSnapshotRequest{Name: "golden-v1"})
	assertStatus(err, 409)

	// 2. List & Get snapshot
	listed, err := cli.ListSnapshots(ctx, "seed")
	if err != nil || len(listed) != 1 {
		t.Fatalf("ListSnapshots: len=%d err=%v", len(listed), err)
	}
	gotSnap, err := cli.GetSnapshot(ctx, "golden-v1")
	if err != nil || gotSnap.ID != snap.ID {
		t.Fatalf("GetSnapshot by name: got=%+v err=%v", gotSnap, err)
	}

	// 3. Branch from snapshot (booting immediately -> 2nd active VM within MaxActiveVMs=2)
	b1, err := cli.BranchSnapshot(ctx, "golden-v1", client.BranchInstanceRequest{
		Handle: "branch-a",
	})
	if err != nil {
		t.Fatalf("BranchSnapshot: %v", err)
	}
	if b1.State != lifecycle.StateRunning || b1.SourceSnapshotID != snap.ID || b1.ParentID != "inst-seed" {
		t.Fatalf("unexpected branch-a response: %+v", b1)
	}
	// Mutating branch-a's workspace must leave seed's workspace untouched
	if err := os.WriteFile(filepath.Join(b1.Workspace, "state.txt"), []byte("mutated-in-branch-a"), 0644); err != nil {
		t.Fatal(err)
	}
	seedBytes, err := os.ReadFile(filepath.Join(srcWS, "state.txt"))
	if err != nil || string(seedBytes) != "baseline-v1" {
		t.Fatalf("seed workspace corrupted by branch write: %q (err=%v)", string(seedBytes), err)
	}

	// 4. Attempting a 3rd active VM branch must hit HTTP 429 Too Many Requests (MaxActiveVMs=2)
	_, err = cli.BranchInstance(ctx, "seed", client.BranchInstanceRequest{Handle: "branch-over-quota"})
	assertStatus(err, 429)

	// 5. Branching with Start=false registers a scale-to-zero STOPPED branch without consuming active quota
	startFalse := false
	b2, err := cli.BranchInstance(ctx, "seed", client.BranchInstanceRequest{
		Handle: "branch-lazy",
		Start:  &startFalse,
	})
	if err != nil {
		t.Fatalf("BranchInstance(Start=false): %v", err)
	}
	if b2.State != lifecycle.StateStopped {
		t.Fatalf("expected stopped lazy branch, got %s", b2.State)
	}

	// 6. Verify KSM & CoW telemetry in /v1/status, /v1/metrics, and /metrics (Prometheus)
	st, err := cli.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Resources.KSMSavedMemoryMB != 16 || st.Resources.EffectiveResidentMemoryMB != (512-16) {
		t.Fatalf("unexpected KSM resource telemetry: %+v", st.Resources)
	}
	if st.Metrics.SnapshotsCreated != 1 || st.Metrics.SnapshotBranches < 2 || st.Metrics.CoWSavedBytes == 0 {
		t.Fatalf("unexpected snapshot/CoW lifecycle telemetry: %+v", st.Metrics)
	}

	promText, err := cli.MetricsPrometheus(ctx)
	if err != nil {
		t.Fatalf("MetricsPrometheus: %v", err)
	}
	for _, metricSubstr := range []string{
		"omnivm_host_effective_resident_memory_bytes ",
		"omnivm_ksm_saved_bytes 16777216",
		"omnivm_ksm_pages_sharing 4096",
		"omnivm_lifecycle_snapshots_created_total 1",
		"omnivm_storage_cow_saved_bytes ",
	} {
		if !strings.Contains(promText, metricSubstr) {
			t.Fatalf("missing %q in Prometheus exposition:\n%s", metricSubstr, promText)
		}
	}

	// 7. Delete snapshot and confirm HTTP 404 thereafter
	if err := cli.DeleteSnapshot(ctx, "golden-v1"); err != nil {
		t.Fatalf("DeleteSnapshot: %v", err)
	}
	_, err = cli.GetSnapshot(ctx, "golden-v1")
	assertStatus(err, 404)
}
