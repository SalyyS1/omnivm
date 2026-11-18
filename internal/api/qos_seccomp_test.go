package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SalyyS1/omnivm/internal/config"
	"github.com/SalyyS1/omnivm/internal/ioqos"
	"github.com/SalyyS1/omnivm/internal/lifecycle"
	"github.com/SalyyS1/omnivm/internal/logstore"
	"github.com/SalyyS1/omnivm/internal/registry"
	"github.com/SalyyS1/omnivm/internal/seccomp"
	"github.com/SalyyS1/omnivm/internal/secrets"
	"github.com/SalyyS1/omnivm/internal/vmm"
)

type qosCapturingVMM struct {
	apiTestVMM
	lastCfg vmm.VMConfig
}

func (v *qosCapturingVMM) CreateVM(cfg vmm.VMConfig) (vmm.Handle, error) {
	v.lastCfg = cfg
	return vmm.Handle{ID: "vm-qos"}, nil
}

func TestQoSAndSeccompEndpoints_EndToEnd(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		SocketPath:      filepath.Join(dir, "ovmd.sock"),
		WorkspacesDir:   filepath.Join(dir, "workspaces"),
		DefaultMemoryMB: 256,
		DefaultVCPUs:    2,
		MinDiskMB:       256,
		MaxDiskMB:       4096,
		PauseAfterIdle:  5 * time.Minute,
		StopAfterIdle:   10 * time.Minute,
		IOQoSGovernor:   "fairshare",
		TenantBlkMBps:   120,
		TenantNetMBps:   60,
	}

	reg, err := registry.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()

	ss, err := secrets.NewStore(filepath.Join(dir, "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	ls := logstore.NewStore(filepath.Join(dir, "logs"))
	fv := &qosCapturingVMM{}
	lm := lifecycle.NewManager(fv, cfg, ls, nil, nil)
	defer lm.Shutdown()
	lm.SetRegistry(reg)

	srv := NewServer(cfg, fv, lm, reg, ss, ls, nil)
	ts := httptest.NewServer(srv.mux)
	defer ts.Close()

	// 1. Configure tenant policy via PUT /v1/qos/tenants/acme
	polBody, _ := json.Marshal(ioqos.TenantPolicy{
		MaxBlkMBps: 80,
		MaxBlkIOPS: 4000,
		MaxNetMBps: 40,
		MaxNetPPS:  8000,
		BurstRatio: 1.5,
	})
	putReq, _ := http.NewRequest("PUT", ts.URL+"/v1/qos/tenants/acme", bytes.NewReader(polBody))
	putReq.Header.Set("Content-Type", "application/json")
	putResp, err := http.DefaultClient.Do(putReq)
	if err != nil {
		t.Fatalf("PUT tenant policy: %v", err)
	}
	putResp.Body.Close()
	if putResp.StatusCode != http.StatusOK {
		t.Fatalf("PUT status = %d", putResp.StatusCode)
	}

	// 2. Boot an instance under tenant acme and verify Cloud Hypervisor rate_limiter_config synthesis.
	inst := lm.CreateInstance("inst-acme-1", []string{"serve"}, nil,
		lifecycle.WithHandle("acme-api"),
		lifecycle.WithTenantID("acme"),
		lifecycle.WithIOQoS(&ioqos.InstancePolicy{Weight: 2}),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := lm.EnsureInstance(ctx, inst.ID); err != nil {
		t.Fatalf("EnsureInstance: %v", err)
	}
	if fv.lastCfg.DiskRateLimiter == nil || fv.lastCfg.NetRateLimiter == nil {
		t.Fatalf("expected DiskRateLimiter and NetRateLimiter on VMConfig, got %+v", fv.lastCfg)
	}

	// 3. Verify GET /v1/qos exposes active allocation and fairness index.
	qosResp, err := http.Get(ts.URL + "/v1/qos")
	if err != nil {
		t.Fatalf("GET /v1/qos: %v", err)
	}
	var snap ioqos.StatusSnapshot
	if err := json.NewDecoder(qosResp.Body).Decode(&snap); err != nil {
		t.Fatalf("decode /v1/qos: %v", err)
	}
	qosResp.Body.Close()
	if snap.Mode != "fairshare" || len(snap.Allocations) != 1 || snap.Allocations[0].TenantID != "acme" {
		t.Fatalf("unexpected /v1/qos snapshot: %+v", snap)
	}

	// 4. Verify Prometheus /metrics includes omnivm_ioqos_* metrics.
	promResp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	promBytes, _ := io.ReadAll(promResp.Body)
	promResp.Body.Close()
	promStr := string(promBytes)
	for _, want := range []string{
		"omnivm_ioqos_active_instances 1",
		"omnivm_ioqos_fairness_index 1.0000",
		`omnivm_ioqos_admitted_bytes_total{channel="blk"}`,
		`omnivm_ioqos_throttles_total{scope="tenant",channel="blk"}`,
	} {
		if !strings.Contains(promStr, want) {
			t.Errorf("Prometheus missing %q\n%s", want, promStr)
		}
	}

	// 5. Verify POST /v1/bench/ioqos noisy-neighbor benchmark endpoint.
	benchResp, err := http.Post(ts.URL+"/v1/bench/ioqos", "application/json", strings.NewReader(`{"tenants":3,"noisy_vms":3,"duration_ms":500}`))
	if err != nil {
		t.Fatalf("POST /v1/bench/ioqos: %v", err)
	}
	if benchResp.StatusCode != http.StatusOK {
		t.Fatalf("bench ioqos status = %d", benchResp.StatusCode)
	}
	benchResp.Body.Close()

	// 6. Verify GET /v1/seccomp/baseline?target=vmm and POST /v1/seccomp/audit?format=sarif.
	baseResp, err := http.Get(ts.URL + "/v1/seccomp/baseline?target=vmm")
	if err != nil {
		t.Fatalf("GET /v1/seccomp/baseline: %v", err)
	}
	baseBytes, _ := io.ReadAll(baseResp.Body)
	baseResp.Body.Close()
	if baseResp.StatusCode != http.StatusOK {
		t.Fatalf("baseline status = %d: %s", baseResp.StatusCode, string(baseBytes))
	}

	auditResp, err := http.Post(ts.URL+"/v1/seccomp/audit?target=vmm", "application/json", bytes.NewReader(baseBytes))
	if err != nil {
		t.Fatalf("POST /v1/seccomp/audit: %v", err)
	}
	var auditRep seccomp.AuditReport
	_ = json.NewDecoder(auditResp.Body).Decode(&auditRep)
	auditResp.Body.Close()
	if !auditRep.Passed || auditRep.PostureScore != 100 {
		t.Fatalf("expected baseline audit score=100, got %+v", auditRep)
	}

	sarifResp, err := http.Post(ts.URL+"/v1/seccomp/audit?target=workload&format=sarif", "application/json",
		strings.NewReader(`{"defaultAction":"SCMP_ACT_ALLOW","syscalls":[{"names":["bpf"],"action":"SCMP_ACT_ALLOW"}]}`))
	if err != nil {
		t.Fatalf("POST /v1/seccomp/audit sarif: %v", err)
	}
	var sarifLog seccomp.SarifLog
	_ = json.NewDecoder(sarifResp.Body).Decode(&sarifLog)
	sarifResp.Body.Close()
	if sarifLog.Version != "2.1.0" || len(sarifLog.Runs[0].Results) < 2 {
		t.Fatalf("unexpected SARIF output: %+v", sarifLog)
	}
}
