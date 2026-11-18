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
	"sync"
	"testing"
	"time"

	"github.com/SalyyS1/omnivm/internal/cgroup"
	"github.com/SalyyS1/omnivm/internal/client"
	"github.com/SalyyS1/omnivm/internal/config"
	"github.com/SalyyS1/omnivm/internal/lifecycle"
	"github.com/SalyyS1/omnivm/internal/logstore"
	"github.com/SalyyS1/omnivm/internal/registry"
	"github.com/SalyyS1/omnivm/internal/router"
	"github.com/SalyyS1/omnivm/internal/secrets"
	"github.com/SalyyS1/omnivm/internal/vmm"
)

type apiTestChannel struct {
	recvCh chan []byte
	closed chan struct{}
	once   sync.Once
}

func newAPITestChannel() *apiTestChannel {
	return &apiTestChannel{
		recvCh: make(chan []byte, 16),
		closed: make(chan struct{}),
	}
}

func (c *apiTestChannel) Send(_ context.Context, msg []byte) error {
	var req map[string]interface{}
	if json.Unmarshal(msg, &req) != nil {
		return nil
	}
	method, _ := req["method"].(string)
	var resp []byte
	switch method {
	case "run":
		resp, _ = json.Marshal(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      req["id"],
			"result":  map[string]interface{}{"pid": 77, "started_at": time.Now().Format(time.RFC3339)},
		})
	case "shutdown":
		resp, _ = json.Marshal(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      req["id"],
			"result":  map[string]interface{}{"status": "shutting_down"},
		})
	}
	if resp != nil {
		select {
		case c.recvCh <- resp:
		case <-c.closed:
		}
	}
	return nil
}

func (c *apiTestChannel) Recv(ctx context.Context) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.closed:
		return nil, io.EOF
	case msg := <-c.recvCh:
		return msg, nil
	}
}

func (c *apiTestChannel) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

type apiTestVMM struct{}

func (v *apiTestVMM) CreateVM(_ vmm.VMConfig) (vmm.Handle, error) {
	return vmm.Handle{ID: "vm-api"}, nil
}

func (v *apiTestVMM) StartVM(_ vmm.Handle) (vmm.ControlChannel, error) {
	return newAPITestChannel(), nil
}

func (v *apiTestVMM) PauseVM(_ vmm.Handle) error  { return nil }
func (v *apiTestVMM) ResumeVM(_ vmm.Handle) error { return nil }
func (v *apiTestVMM) StopVM(_ vmm.Handle) error   { return nil }
func (v *apiTestVMM) HostEndpoints(_ vmm.Handle) ([]vmm.HostEndpoint, error) {
	return []vmm.HostEndpoint{{GuestPort: 8080, HostPort: 18080, Protocol: "http", BackendAddr: "127.0.0.1"}}, nil
}

func (v *apiTestVMM) Capabilities() vmm.BackendCaps {
	return vmm.BackendCaps{
		Name:           "cloud-hypervisor",
		Pause:          true,
		RootFSType:     vmm.RootFSBlockImage,
		GuestArch:      "amd64",
		NetworkBackend: "tap",
	}
}

func TestStatusAndMetricsEndpoints_StructuredJSON(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "ovmd.sock")
	cfg := &config.Config{
		SocketPath:      sockPath,
		WorkspacesDir:   filepath.Join(dir, "workspaces"),
		DefaultMemoryMB: 256,
		DefaultVCPUs:    1,
		MinDiskMB:       256,
		MaxDiskMB:       4096,
		PauseAfterIdle:  5 * time.Minute,
		StopAfterIdle:   10 * time.Minute,
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
	fv := &apiTestVMM{}
	lm := lifecycle.NewManager(fv, cfg, ls, nil, nil)
	defer lm.Shutdown()
	lm.SetRegistry(reg)

	rtr := router.New(lm, "127.0.0.1:0")
	if err := rtr.Start(); err != nil {
		t.Fatal(err)
	}
	defer rtr.Stop(context.Background())

	lm.CreateInstance("inst-run", []string{"serve"}, []vmm.PortExpose{{GuestPort: 8080, Protocol: "http"}}, lifecycle.WithHandle("run-svc"))
	lm.CreateInstance("inst-stop", []string{"worker"}, nil, lifecycle.WithHandle("stop-svc"))
	if _, err := rtr.AllocatePort("inst-run", 8080, 0, "http"); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := lm.EnsureInstance(ctx, "inst-run"); err != nil {
		t.Fatalf("EnsureInstance: %v", err)
	}
	if err := lm.PauseInstance("inst-run"); err != nil {
		t.Fatalf("PauseInstance: %v", err)
	}
	if err := lm.ResumeInstance("inst-run"); err != nil {
		t.Fatalf("ResumeInstance: %v", err)
	}

	srv := NewServer(cfg, fv, lm, reg, ss, ls, rtr)
	if err := srv.Start(); err != nil {
		t.Fatalf("srv.Start: %v", err)
	}
	defer srv.Stop(context.Background())

	cli := client.New(sockPath)
	st, err := cli.Status(ctx)
	if err != nil {
		t.Fatalf("cli.Status: %v", err)
	}
	if st.Status != "running" {
		t.Errorf("Status = %q, want running", st.Status)
	}
	if st.Backend != "cloud-hypervisor" {
		t.Errorf("Backend = %q, want cloud-hypervisor", st.Backend)
	}
	if st.Instances.Total != 2 || st.Instances.Running != 1 || st.Instances.Stopped != 1 {
		t.Errorf("unexpected status instance counts: %+v", st.Instances)
	}
	if st.Metrics.ColdStarts != 1 || st.Metrics.WarmResumes != 1 {
		t.Errorf("unexpected status lifecycle metrics: %+v", st.Metrics)
	}

	dm, err := cli.Metrics(ctx)
	if err != nil {
		t.Fatalf("cli.Metrics: %v", err)
	}
	if dm.Timestamp == "" {
		t.Error("expected non-empty RFC3339 timestamp in /v1/metrics")
	}
	if dm.Instances.Total != 2 || dm.Lifecycle.ColdStarts != 1 || dm.Router.ActivePortProxies != 1 {
		t.Errorf("unexpected /v1/metrics payload: %+v", dm)
	}
}

func TestAPIHandlers_ConcurrentReadWriteRaceSafety(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		WorkspacesDir:   filepath.Join(dir, "workspaces"),
		DefaultMemoryMB: 256,
		DefaultVCPUs:    1,
		MinDiskMB:       256,
		MaxDiskMB:       4096,
		PauseAfterIdle:  5 * time.Minute,
		StopAfterIdle:   10 * time.Minute,
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
	fv := &apiTestVMM{}
	lm := lifecycle.NewManager(fv, cfg, ls, nil, nil)
	defer lm.Shutdown()
	lm.SetRegistry(reg)

	rtr := router.New(lm, "127.0.0.1:0")
	if err := rtr.Start(); err != nil {
		t.Fatal(err)
	}
	defer rtr.Stop(context.Background())

	lm.CreateInstance("inst-race", []string{"serve"}, []vmm.PortExpose{{GuestPort: 8080, Protocol: "http"}},
		lifecycle.WithHandle("race-svc"),
		lifecycle.WithSecretKeys([]string{"KEY_A"}),
	)

	srv := NewServer(cfg, fv, lm, reg, ss, ls, rtr)
	ts := httptest.NewServer(srv.mux)
	defer ts.Close()

	stopCh := make(chan struct{})
	var wg sync.WaitGroup

	// Mutator goroutine cycling instance lifecycle states.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 15; i++ {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = lm.StartInstance(ctx, "inst-race")
			cancel()
			_ = lm.PauseInstance("inst-race")
			_ = lm.ResumeInstance("inst-race")
			_ = lm.DisableInstance("inst-race")
		}
		close(stopCh)
	}()

	// Concurrent HTTP readers and secret-key updaters.
	endpoints := []string{
		"/v1/status",
		"/v1/metrics",
		"/metrics",
		"/v1/metrics?format=prometheus",
		"/v1/instances",
		"/v1/instances/race-svc",
	}
	for _, ep := range endpoints {
		wg.Add(1)
		go func(path string) {
			defer wg.Done()
			for {
				select {
				case <-stopCh:
					return
				default:
					resp, err := http.Get(ts.URL + path)
					if err == nil {
						_, _ = io.Copy(io.Discard, resp.Body)
						resp.Body.Close()
					}
				}
			}
		}(ep)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stopCh:
				return
			default:
				payload, _ := json.Marshal(map[string]interface{}{
					"secrets": []string{"KEY_A", "KEY_B"},
				})
				req, _ := http.NewRequest("PUT", ts.URL+"/v1/instances/race-svc/secrets", bytes.NewReader(payload))
				req.Header.Set("Content-Type", "application/json")
				resp, err := http.DefaultClient.Do(req)
				if err == nil {
					_, _ = io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}
			}
		}
	}()

	wg.Wait()
}

func TestPrometheusAndOpenMetricsExposition_AndQuotaHTTP429(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "ovmd.sock")
	cfg := &config.Config{
		SocketPath:      sockPath,
		WorkspacesDir:   filepath.Join(dir, "workspaces"),
		DefaultMemoryMB: 256,
		DefaultVCPUs:    2,
		MinDiskMB:       256,
		MaxDiskMB:       4096,
		MaxHostMemoryMB: 512,
		MaxActiveVCPUs:  2,
		MaxActiveVMs:    1,
		PauseAfterIdle:  5 * time.Minute,
		StopAfterIdle:   10 * time.Minute,
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
	fv := &apiTestVMM{}
	lm := lifecycle.NewManager(fv, cfg, ls, nil, nil)
	defer lm.Shutdown()
	lm.SetRegistry(reg)

	rtr := router.New(lm, "127.0.0.1:0")
	if err := rtr.Start(); err != nil {
		t.Fatal(err)
	}
	defer rtr.Stop(context.Background())

	lm.CreateInstance("inst-1", []string{"serve"}, []vmm.PortExpose{{GuestPort: 8080, Protocol: "http"}}, lifecycle.WithHandle("svc-1"))
	lm.CreateInstance("inst-2", []string{"serve"}, []vmm.PortExpose{{GuestPort: 8080, Protocol: "http"}}, lifecycle.WithHandle("svc-2"))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := lm.EnsureInstance(ctx, "inst-1"); err != nil {
		t.Fatalf("boot inst-1: %v", err)
	}

	srv := NewServer(cfg, fv, lm, reg, ss, ls, rtr)
	if err := srv.Start(); err != nil {
		t.Fatalf("srv.Start: %v", err)
	}
	defer srv.Stop(context.Background())

	// 1. Trigger quota rejection over HTTP ingress router (svc-1 already holds MaxActiveVMs=1).
	rtrReq, _ := http.NewRequest("GET", "http://"+rtr.Addr()+"/", nil)
	rtrReq.Header.Set("X-OmniVM-Instance", "svc-2")
	rtrResp, err := http.DefaultClient.Do(rtrReq)
	if err != nil {
		t.Fatalf("router request: %v", err)
	}
	rtrResp.Body.Close()
	if rtrResp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("router status = %d, want 429 Too Many Requests", rtrResp.StatusCode)
	}

	// 2. Trigger quota rejection over control-plane wake-on-exec API.
	ts := httptest.NewServer(srv.mux)
	defer ts.Close()

	execBody, _ := json.Marshal(map[string]interface{}{"command": []string{"echo", "hi"}})
	execResp, err := http.Post(ts.URL+"/v1/instances/svc-2/exec", "application/json", bytes.NewReader(execBody))
	if err != nil {
		t.Fatalf("exec request: %v", err)
	}
	execResp.Body.Close()
	if execResp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("exec status = %d, want 429 Too Many Requests", execResp.StatusCode)
	}

	// 3. Verify Prometheus 0.0.4 exposition via client and HTTP.
	cli := client.New(sockPath)
	promText, err := cli.MetricsPrometheus(ctx)
	if err != nil {
		t.Fatalf("cli.MetricsPrometheus: %v", err)
	}
	requiredSnippets := []string{
		`# TYPE omnivm_up gauge`,
		`omnivm_up 1`,
		`omnivm_instances{state="running"} 1`,
		`omnivm_instances{state="stopped"} 1`,
		`omnivm_host_resident_memory_bytes 268435456`,
		`omnivm_host_quota_memory_bytes 536870912`,
		`omnivm_host_active_vcpus 2`,
		`omnivm_host_quota_vcpus 2`,
		`omnivm_host_active_vms 1`,
		`omnivm_host_quota_active_vms 1`,
		`omnivm_lifecycle_cold_starts_total 1`,
		`omnivm_lifecycle_quota_rejections_total 2`,
	}
	for _, s := range requiredSnippets {
		if !strings.Contains(promText, s) {
			t.Errorf("Prometheus output missing %q\nFull output:\n%s", s, promText)
		}
	}

	// 4. Verify OpenMetrics 1.0.0 content negotiation (# EOF and base counter TYPE).
	omReq, _ := http.NewRequest("GET", ts.URL+"/v1/metrics", nil)
	omReq.Header.Set("Accept", "application/openmetrics-text; version=1.0.0")
	omResp, err := http.DefaultClient.Do(omReq)
	if err != nil {
		t.Fatalf("openmetrics request: %v", err)
	}
	omBytes, _ := io.ReadAll(omResp.Body)
	omResp.Body.Close()
	if !strings.HasPrefix(omResp.Header.Get("Content-Type"), "application/openmetrics-text") {
		t.Errorf("OpenMetrics Content-Type = %q", omResp.Header.Get("Content-Type"))
	}
	omBody := string(omBytes)
	if !strings.HasSuffix(omBody, "# EOF\n") {
		t.Errorf("OpenMetrics output missing trailing # EOF")
	}
	if !strings.Contains(omBody, "# TYPE omnivm_lifecycle_quota_rejections counter\nomnivm_lifecycle_quota_rejections_total 2\n") {
		t.Errorf("OpenMetrics counter header/sample mismatch:\n%s", omBody)
	}

	// 5. Verify structured JSON resources block on /v1/status and /v1/metrics.
	dm, err := cli.Metrics(ctx)
	if err != nil {
		t.Fatalf("cli.Metrics: %v", err)
	}
	if dm.Resources.ResidentMemoryMB != 256 || dm.Resources.MaxHostMemoryMB != 512 || dm.Lifecycle.QuotaRejections != 2 {
		t.Errorf("unexpected JSON metrics resources/quotas: %+v / %+v", dm.Resources, dm.Lifecycle)
	}
}

func TestCgroupV2MetricsExposition_AndPrefaultBenchEndpoint(t *testing.T) {
	dir := t.TempDir()
	cgDir := filepath.Join(dir, "cgroup")
	if err := os.MkdirAll(cgDir, 0755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(cgDir, "memory.current"), []byte("314572800\n"), 0644)
	_ = os.WriteFile(filepath.Join(cgDir, "memory.swap.current"), []byte("0\n"), 0644)
	_ = os.WriteFile(filepath.Join(cgDir, "memory.high"), []byte("max\n"), 0644)
	_ = os.WriteFile(filepath.Join(cgDir, "memory.max"), []byte("536870912\n"), 0644)
	_ = os.WriteFile(filepath.Join(cgDir, "memory.events"), []byte("low 0\nhigh 17\nmax 1\noom 0\noom_kill 0\n"), 0644)
	_ = os.WriteFile(filepath.Join(cgDir, "memory.pressure"), []byte("some avg10=1.50 avg60=0.40 avg300=0.10 total=9200\nfull avg10=0.25 avg60=0.05 avg300=0.00 total=1400\n"), 0644)

	sockPath := filepath.Join(dir, "ovmd.sock")
	cfg := &config.Config{
		SocketPath:      sockPath,
		WorkspacesDir:   filepath.Join(dir, "workspaces"),
		DefaultMemoryMB: 256,
		DefaultVCPUs:    1,
		MinDiskMB:       256,
		MaxDiskMB:       4096,
		MaxHostMemoryMB: 512,
		PauseAfterIdle:  5 * time.Minute,
		StopAfterIdle:   10 * time.Minute,
		CgroupGovernor:  "adaptive",
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
	fv := &apiTestVMM{}
	lm := lifecycle.NewManager(fv, cfg, ls, nil, nil)
	defer lm.Shutdown()
	lm.SetRegistry(reg)
	lm.SetCgroupGovernor(cgroup.NewGovernor(cgDir, cgroup.ModeAdaptive, 0.85))

	lm.CreateInstance("inst-cg", []string{"serve"}, nil, lifecycle.WithHandle("cg-svc"))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := lm.EnsureInstance(ctx, "inst-cg"); err != nil {
		t.Fatalf("EnsureInstance: %v", err)
	}

	srv := NewServer(cfg, fv, lm, reg, ss, ls, nil)
	if err := srv.Start(); err != nil {
		t.Fatalf("srv.Start: %v", err)
	}
	defer srv.Stop(context.Background())

	cli := client.New(sockPath)
	dm, err := cli.Metrics(ctx)
	if err != nil {
		t.Fatalf("cli.Metrics: %v", err)
	}
	if !dm.Resources.CgroupAvailable || dm.Resources.CgroupHighThrottleEvents != 17 || dm.Resources.CgroupMemoryHighBytes == 0 {
		t.Fatalf("unexpected cgroup v2 JSON metrics: %+v", dm.Resources)
	}

	promText, err := cli.MetricsPrometheus(ctx)
	if err != nil {
		t.Fatalf("MetricsPrometheus: %v", err)
	}
	for _, want := range []string{
		`omnivm_cgroup_available 1`,
		`omnivm_cgroup_memory_current_bytes 314572800`,
		`omnivm_cgroup_memory_high_throttles_total 17`,
		`omnivm_cgroup_memory_psi_avg10_percent{level="some"} 1.5000`,
		`omnivm_cgroup_memory_psi_stall_microseconds_total{level="full"} 1400`,
	} {
		if !strings.Contains(promText, want) {
			t.Errorf("Prometheus output missing %q\n%s", want, promText)
		}
	}

	// Verify POST /v1/bench/prefault endpoint
	ts := httptest.NewServer(srv.mux)
	defer ts.Close()
	benchPayload, _ := json.Marshal(map[string]interface{}{
		"pool_size":      2,
		"slot_memory_mb": 4,
		"workers":        2,
		"iterations":     2,
		"strategy":       "touch-write",
	})
	resp, err := http.Post(ts.URL+"/v1/bench/prefault", "application/json", bytes.NewReader(benchPayload))
	if err != nil {
		t.Fatalf("POST /v1/bench/prefault: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("bench status = %d: %s", resp.StatusCode, string(b))
	}
	var benchRes map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&benchRes); err != nil {
		t.Fatalf("decode bench response: %v", err)
	}
	if benchRes["strategy"] != "touch-write" {
		t.Fatalf("unexpected bench response: %+v", benchRes)
	}
}
