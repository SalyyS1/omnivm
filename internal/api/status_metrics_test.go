package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

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
