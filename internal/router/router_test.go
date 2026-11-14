package router

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SalyyS1/omnivm/internal/config"
	"github.com/SalyyS1/omnivm/internal/lifecycle"
	"github.com/SalyyS1/omnivm/internal/logstore"
	"github.com/SalyyS1/omnivm/internal/vmm"
)

func TestProbeTCP_DelayedListenerAndImmediateCloseProxy(t *testing.T) {
	// Reserve an ephemeral port, then close so early probes see ECONNREFUSED.
	resLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := resLn.Addr().String()
	resLn.Close()

	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)

		// Stage 1: wait 35ms so initial dial attempts hit ECONNREFUSED.
		time.Sleep(35 * time.Millisecond)

		// Stage 2: simulate proxy accepting 2 connections and closing immediately (EOF).
		proxyLn, err := net.Listen("tcp", addr)
		if err != nil {
			return
		}
		for i := 0; i < 2; i++ {
			c, err := proxyLn.Accept()
			if err == nil {
				c.Close()
			}
		}
		proxyLn.Close()

		// Stage 3: real listener stays open and serves application bytes.
		realLn, err := net.Listen("tcp", addr)
		if err != nil {
			return
		}
		defer realLn.Close()
		c, err := realLn.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = c.Write([]byte("READY\n"))
		_, _ = io.Copy(io.Discard, c)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	cfg := ProbeConfig{
		DialTimeout:    150 * time.Millisecond,
		InitialBackoff: 10 * time.Millisecond,
		MaxBackoff:     40 * time.Millisecond,
		LivenessSettle: 15 * time.Millisecond,
	}

	conn, retries, err := ProbeTCPWithStats(ctx, addr, cfg)
	if err != nil {
		t.Fatalf("ProbeTCPWithStats failed: %v", err)
	}
	defer conn.Close()

	if retries < 2 {
		t.Fatalf("expected at least 2 retries across refused + immediate-close stages, got %d", retries)
	}

	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read payload from probed TCP conn: %v", err)
	}
	if line != "READY\n" {
		t.Fatalf("payload = %q, want %q", line, "READY\n")
	}
	conn.Close()
	<-serverDone
}

func TestProbeTCP_ContextCancellationAndEmptyAddr(t *testing.T) {
	if _, err := ProbeTCP(context.Background(), "   ", DefaultProbeConfig()); err == nil {
		t.Fatal("expected error for empty TCP probe address")
	}

	// Reserve and close a port so nothing is listening.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()

	err = CheckTCPReady(ctx, addr, ProbeConfig{
		DialTimeout:    25 * time.Millisecond,
		InitialBackoff: 10 * time.Millisecond,
		MaxBackoff:     20 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("expected CheckTCPReady to fail on closed port when context expires")
	}
}

func TestProbeVsock_HandshakeRetryAndZeroOverRead(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "ch-vsock.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	var accepts int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			n := atomic.AddInt32(&accepts, 1)
			switch n {
			case 1:
				// Early EOF before guest vsock listener binds.
				c.Close()
			case 2:
				// Handshake rejected.
				_, _ = readLineSingleByte(c, 128)
				_, _ = c.Write([]byte("REJECT\n"))
				c.Close()
			default:
				// Valid Cloud Hypervisor mux handshake + immediate guest payload in same write!
				cmd, err := readLineSingleByte(c, 128)
				if err != nil || cmd != "CONNECT 1024" {
					c.Close()
					return
				}
				_, _ = c.Write([]byte("OK 1073741824\nGUEST_STREAM_BYTES"))
				c.Close()
				return
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	conn, err := ProbeVsock(ctx, sockPath, 1024, ProbeConfig{
		DialTimeout:    200 * time.Millisecond,
		InitialBackoff: 10 * time.Millisecond,
		MaxBackoff:     30 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("ProbeVsock failed: %v", err)
	}
	defer conn.Close()

	payload, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read guest payload after vsock handshake: %v", err)
	}
	if string(payload) != "GUEST_STREAM_BYTES" {
		t.Fatalf("vsock payload = %q, want %q (handshake must not over-read)", string(payload), "GUEST_STREAM_BYTES")
	}
	if got := atomic.LoadInt32(&accepts); got != 3 {
		t.Fatalf("accept count = %d, want 3", got)
	}
}

func TestProbeVsock_ValidationAndTimeout(t *testing.T) {
	if err := CheckVsockReady(context.Background(), "", 1024, DefaultProbeConfig()); err == nil {
		t.Fatal("expected error for empty mux socket path")
	}
	if err := CheckVsockReady(context.Background(), "/tmp/nonexistent.sock", 0, DefaultProbeConfig()); err == nil {
		t.Fatal("expected error for invalid guest port 0")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := CheckVsockReady(ctx, filepath.Join(t.TempDir(), "missing.sock"), 5000, ProbeConfig{
		DialTimeout:    20 * time.Millisecond,
		InitialBackoff: 10 * time.Millisecond,
		MaxBackoff:     20 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("expected timeout error for missing vsock mux socket")
	}
}

// --- Fake VMM wired to local HTTP + TCP backends for Router wake-on-connect test ---

type routerTestChannel struct {
	recvCh chan []byte
	closed chan struct{}
	once   sync.Once
}

func newRouterTestChannel() *routerTestChannel {
	return &routerTestChannel{
		recvCh: make(chan []byte, 16),
		closed: make(chan struct{}),
	}
}

func (c *routerTestChannel) Send(_ context.Context, msg []byte) error {
	var req map[string]interface{}
	if err := json.Unmarshal(msg, &req); err != nil {
		return nil
	}
	method, _ := req["method"].(string)
	var resp []byte
	switch method {
	case "run":
		resp, _ = json.Marshal(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      req["id"],
			"result":  map[string]interface{}{"pid": 100, "started_at": time.Now().Format(time.RFC3339)},
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

func (c *routerTestChannel) Recv(ctx context.Context) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.closed:
		return nil, io.EOF
	case msg := <-c.recvCh:
		return msg, nil
	}
}

func (c *routerTestChannel) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

type routerTestVMM struct {
	httpHostPort int
	tcpHostPort  int
	startDelay   time.Duration
	startCalls   int64
}

func (v *routerTestVMM) CreateVM(_ vmm.VMConfig) (vmm.Handle, error) {
	return vmm.Handle{ID: "vm-router-test"}, nil
}

func (v *routerTestVMM) StartVM(_ vmm.Handle) (vmm.ControlChannel, error) {
	atomic.AddInt64(&v.startCalls, 1)
	if v.startDelay > 0 {
		time.Sleep(v.startDelay)
	}
	return newRouterTestChannel(), nil
}

func (v *routerTestVMM) PauseVM(_ vmm.Handle) error  { return nil }
func (v *routerTestVMM) ResumeVM(_ vmm.Handle) error { return nil }
func (v *routerTestVMM) StopVM(_ vmm.Handle) error   { return nil }

func (v *routerTestVMM) HostEndpoints(_ vmm.Handle) ([]vmm.HostEndpoint, error) {
	return []vmm.HostEndpoint{
		{GuestPort: 8080, HostPort: v.httpHostPort, Protocol: "http", BackendAddr: "127.0.0.1"},
		{GuestPort: 9000, HostPort: v.tcpHostPort, Protocol: "tcp", BackendAddr: "127.0.0.1"},
	}, nil
}

func (v *routerTestVMM) Capabilities() vmm.BackendCaps {
	return vmm.BackendCaps{
		Name:           "cloud-hypervisor",
		Pause:          true,
		RootFSType:     vmm.RootFSBlockImage,
		GuestArch:      "amd64",
		NetworkBackend: "tap",
	}
}

func TestRouter_ConcurrentHTTPAndTCPWakeCoalescing(t *testing.T) {
	// Guest HTTP backend (guest port 8080)
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok-from-guest"))
	}))
	defer httpSrv.Close()
	httpPort := httpSrv.Listener.Addr().(*net.TCPAddr).Port

	// Guest TCP echo backend (guest port 9000)
	tcpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tcpLn.Close()
	tcpPort := tcpLn.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			c, err := tcpLn.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				line, err := bufio.NewReader(conn).ReadString('\n')
				if err == nil {
					_, _ = conn.Write([]byte("echo:" + line))
				}
			}(c)
		}
	}()

	fakeVMM := &routerTestVMM{
		httpHostPort: httpPort,
		tcpHostPort:  tcpPort,
		startDelay:   40 * time.Millisecond,
	}
	cfg := &config.Config{
		DefaultMemoryMB: 256,
		DefaultVCPUs:    1,
		PauseAfterIdle:  5 * time.Minute,
		StopAfterIdle:   10 * time.Minute,
	}
	ls := logstore.NewStore(t.TempDir())
	lm := lifecycle.NewManager(fakeVMM, cfg, ls, nil, nil)
	defer lm.Shutdown()

	rtr := New(lm, "127.0.0.1:0")
	if err := rtr.Start(); err != nil {
		t.Fatalf("router Start: %v", err)
	}
	defer rtr.Stop(context.Background())

	lm.CreateInstance("inst-web", []string{"serve"}, []vmm.PortExpose{
		{GuestPort: 8080, Protocol: "http"},
		{GuestPort: 9000, Protocol: "tcp"},
	}, lifecycle.WithHandle("web"))

	pubTCPPort, err := rtr.AllocatePort("inst-web", 9000, 0, "tcp")
	if err != nil {
		t.Fatalf("AllocatePort: %v", err)
	}

	const httpClients = 16
	const tcpClients = 4
	errCh := make(chan error, httpClients+tcpClients)
	var wg sync.WaitGroup

	// Fire 16 HTTP requests + 4 TCP connections concurrently against the stopped instance.
	for i := 0; i < httpClients; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, err := http.NewRequest("GET", "http://"+rtr.Addr()+"/health", nil)
			if err != nil {
				errCh <- err
				return
			}
			req.Header.Set("X-OmniVM-Instance", "web")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				errCh <- err
				return
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusOK || string(body) != "ok-from-guest" {
				errCh <- fmt.Errorf("HTTP got %d %q", resp.StatusCode, string(body))
			}
		}()
	}

	for i := 0; i < tcpClients; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", pubTCPPort), 3*time.Second)
			if err != nil {
				errCh <- err
				return
			}
			defer conn.Close()
			msg := fmt.Sprintf("ping-%d\n", idx)
			if _, err := conn.Write([]byte(msg)); err != nil {
				errCh <- err
				return
			}
			reply, err := bufio.NewReader(conn).ReadString('\n')
			if err != nil {
				errCh <- err
				return
			}
			if strings.TrimSpace(reply) != "echo:"+strings.TrimSpace(msg) {
				errCh <- fmt.Errorf("TCP reply = %q, want %q", reply, "echo:"+msg)
			}
		}(i)
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("concurrent wake client error: %v", err)
	}

	if got := atomic.LoadInt64(&fakeVMM.startCalls); got != 1 {
		t.Fatalf("StartVM calls = %d, want 1 (all 20 requests must coalesce into 1 cold boot)", got)
	}

	lmMetrics := lm.Metrics()
	if lmMetrics.ColdStarts != 1 {
		t.Errorf("LifecycleMetrics.ColdStarts = %d, want 1", lmMetrics.ColdStarts)
	}
	if lmMetrics.CoalescedWakes == 0 {
		t.Errorf("LifecycleMetrics.CoalescedWakes = %d, want > 0", lmMetrics.CoalescedWakes)
	}

	rm := rtr.Metrics()
	if rm.ActivePortProxies != 1 {
		t.Errorf("RouterMetrics.ActivePortProxies = %d, want 1", rm.ActivePortProxies)
	}
	if rm.HTTPRequests != httpClients {
		t.Errorf("RouterMetrics.HTTPRequests = %d, want %d", rm.HTTPRequests, httpClients)
	}
	if rm.TCPConnections != tcpClients {
		t.Errorf("RouterMetrics.TCPConnections = %d, want %d", rm.TCPConnections, tcpClients)
	}
	if rm.BackendErrors != 0 {
		t.Errorf("RouterMetrics.BackendErrors = %d, want 0", rm.BackendErrors)
	}
}
