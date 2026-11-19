package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SalyyS1/omnivm/internal/config"
	"github.com/SalyyS1/omnivm/internal/lifecycle"
	"github.com/SalyyS1/omnivm/internal/logstore"
	"github.com/SalyyS1/omnivm/internal/netpolicy"
	"github.com/SalyyS1/omnivm/internal/registry"
	"github.com/SalyyS1/omnivm/internal/secrets"
	"github.com/SalyyS1/omnivm/internal/virtiofs"
)

func TestVirtioFSAndNetPolicyEndpoints_EndToEnd(t *testing.T) {
	dir := t.TempDir()
	wsDir := filepath.Join(dir, "workspace-vm1")
	if err := os.MkdirAll(wsDir, 0755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		SocketPath:          filepath.Join(dir, "ovmd.sock"),
		WorkspacesDir:       filepath.Join(dir, "workspaces"),
		DefaultMemoryMB:     512,
		DefaultVCPUs:        2,
		MinDiskMB:           256,
		MaxDiskMB:           4096,
		PauseAfterIdle:      5 * time.Minute,
		StopAfterIdle:       10 * time.Minute,
		VirtioFSPool:        "dax",
		VirtioFSDAXWindowMB: 128,
		VirtioFSMaxPoolMB:   512,
		VirtioFSCacheMB:     1024,
		NetPolicyMode:       "enforce",
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

	// 1. Register a shared base rootfs layer via POST /v1/virtiofs/layers.
	regResp, err := http.Post(ts.URL+"/v1/virtiofs/layers", "application/json",
		strings.NewReader(`{"digest":"sha256:abcd1234","image_ref":"python:3.12-alpine","size_bytes":67108864}`))
	if err != nil {
		t.Fatalf("POST /v1/virtiofs/layers: %v", err)
	}
	regResp.Body.Close()
	if regResp.StatusCode != http.StatusOK {
		t.Fatalf("register layer status = %d", regResp.StatusCode)
	}

	// 2. Boot an instance with a workspace and verify VMConfig receives 2 MiB-aligned DAX VirtioFS options.
	inst := lm.CreateInstance("inst-vfs-1", []string{"serve"}, nil,
		lifecycle.WithHandle("dax-worker"),
		lifecycle.WithWorkspace(wsDir),
		lifecycle.WithTenantID("acme"),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := lm.EnsureInstance(ctx, inst.ID); err != nil {
		t.Fatalf("EnsureInstance: %v", err)
	}
	if fv.lastCfg.VirtioFS == nil || fv.lastCfg.VirtioFS.DAXWindowBytes != 128*1024*1024 || fv.lastCfg.VirtioFS.CachePolicy != "always" {
		t.Fatalf("expected VirtioFS DAX options on VMConfig, got %+v", fv.lastCfg.VirtioFS)
	}

	// 3. Verify GET /v1/virtiofs exposes active DAX mount and layer cache.
	vfsResp, err := http.Get(ts.URL + "/v1/virtiofs")
	if err != nil {
		t.Fatalf("GET /v1/virtiofs: %v", err)
	}
	var vfsSnap virtiofs.StatusSnapshot
	if err := json.NewDecoder(vfsResp.Body).Decode(&vfsSnap); err != nil {
		t.Fatalf("decode /v1/virtiofs: %v", err)
	}
	vfsResp.Body.Close()
	if vfsSnap.Metrics.ActiveDAXMounts != 1 || vfsSnap.Metrics.ActiveLayers != 1 {
		t.Fatalf("unexpected /v1/virtiofs snapshot: %+v", vfsSnap)
	}

	// 4. Verify POST /v1/bench/virtiofs endpoint.
	benchResp, err := http.Post(ts.URL+"/v1/bench/virtiofs", "application/json",
		strings.NewReader(`{"vms":3,"layer_mb":2,"iterations":2}`))
	if err != nil {
		t.Fatalf("POST /v1/bench/virtiofs: %v", err)
	}
	if benchResp.StatusCode != http.StatusOK {
		t.Fatalf("bench virtiofs status = %d", benchResp.StatusCode)
	}
	benchResp.Body.Close()

	// 5. Generate baseline policy via GET /v1/netpolicy/baseline, install via PUT /v1/netpolicy/tenants/acme, and evaluate traffic.
	baseResp, err := http.Get(ts.URL + "/v1/netpolicy/baseline?tenant=acme&preset=strict-ai-sandbox")
	if err != nil {
		t.Fatalf("GET /v1/netpolicy/baseline: %v", err)
	}
	baseBytes, _ := io.ReadAll(baseResp.Body)
	baseResp.Body.Close()
	if baseResp.StatusCode != http.StatusOK {
		t.Fatalf("baseline status = %d: %s", baseResp.StatusCode, string(baseBytes))
	}

	putReq, _ := http.NewRequest("PUT", ts.URL+"/v1/netpolicy/tenants/acme", bytes.NewReader(baseBytes))
	putReq.Header.Set("Content-Type", "application/json")
	putResp, err := http.DefaultClient.Do(putReq)
	if err != nil {
		t.Fatalf("PUT tenant netpolicy: %v", err)
	}
	putResp.Body.Close()
	if putResp.StatusCode != http.StatusOK {
		t.Fatalf("PUT netpolicy status = %d", putResp.StatusCode)
	}

	// Evaluate IMDS SSRF block via POST /v1/netpolicy/evaluate.
	evalResp, err := http.Post(ts.URL+"/v1/netpolicy/evaluate", "application/json",
		strings.NewReader(`{"packet":{"tenant_id":"acme","dst_ip":"169.254.169.254","protocol":"tcp","dst_port":80}}`))
	if err != nil {
		t.Fatalf("POST /v1/netpolicy/evaluate: %v", err)
	}
	var dec netpolicy.Decision
	_ = json.NewDecoder(evalResp.Body).Decode(&dec)
	evalResp.Body.Close()
	if dec.Allowed || dec.Reason != "cloud_metadata_blocked" {
		t.Fatalf("expected cloud_metadata_blocked decision, got %+v", dec)
	}

	// Compile nftables ruleset via POST /v1/netpolicy/compile.
	compBody := `{"tap_name":"ovm0","guest_ip":"172.16.0.2","policy":` + string(baseBytes) + `}`
	compResp, err := http.Post(ts.URL+"/v1/netpolicy/compile", "application/json", strings.NewReader(compBody))
	if err != nil {
		t.Fatalf("POST /v1/netpolicy/compile: %v", err)
	}
	var compOut map[string]string
	_ = json.NewDecoder(compResp.Body).Decode(&compOut)
	compResp.Body.Close()
	if !strings.Contains(compOut["nftables"], "table inet ovm_ovm0") {
		t.Fatalf("unexpected compiled nftables output: %+v", compOut)
	}

	// Audit unsafe policy in SARIF format via POST /v1/netpolicy/audit?format=sarif.
	sarifResp, err := http.Post(ts.URL+"/v1/netpolicy/audit?format=sarif", "application/json",
		strings.NewReader(`{"tenant_id":"bad","default_egress":"allow","block_metadata":false,"block_private_networks":false,"anti_spoofing":false}`))
	if err != nil {
		t.Fatalf("POST /v1/netpolicy/audit sarif: %v", err)
	}
	var sarifLog netpolicy.SarifLog
	_ = json.NewDecoder(sarifResp.Body).Decode(&sarifLog)
	sarifResp.Body.Close()
	if sarifLog.Version != "2.1.0" || len(sarifLog.Runs[0].Results) < 4 {
		t.Fatalf("unexpected netpolicy SARIF output: %+v", sarifLog)
	}

	// 6. Verify Prometheus /metrics and OpenMetrics include omnivm_virtiofs_* and omnivm_netpolicy_*.
	promResp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	promBytes, _ := io.ReadAll(promResp.Body)
	promResp.Body.Close()
	promStr := string(promBytes)
	for _, want := range []string{
		"omnivm_virtiofs_active_layers 1",
		"omnivm_virtiofs_active_dax_mounts 1",
		"omnivm_netpolicy_active_policies 1",
		"omnivm_netpolicy_imds_blocks_total 1",
		"omnivm_netpolicy_compiled_rulesets_total 1",
	} {
		if !strings.Contains(promStr, want) {
			t.Errorf("Prometheus output missing %q\n%s", want, promStr)
		}
	}
}
