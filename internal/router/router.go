// Package router provides ingress for instances.
//
// Two ingress paths:
//   - Main HTTP router (:8099) for handle-based routing with HTTP reverse proxy
//   - Per-port TCP proxies for --expose ports with L4 relay and wake-on-connect
//
// Both paths share the same "ensure instance + dial backend + relay" core.
// Public ports (router-owned) are stable across pause/resume/stop/restart.
// They are freed only on instance delete or daemon shutdown.
package router

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/SalyyS1/omnivm/internal/lifecycle"
)

// PublicEndpoint describes a router-owned public port mapping.
type PublicEndpoint struct {
	GuestPort  int
	PublicPort int
	Protocol   string
}

// RouterMetrics exposes ingress traffic and readiness-probe counters.
type RouterMetrics struct {
	ActivePortProxies int    `json:"active_port_proxies"`
	HTTPRequests      uint64 `json:"http_requests"`
	TCPConnections    uint64 `json:"tcp_connections"`
	ProbeRetries      uint64 `json:"probe_retries"`
	BackendErrors     uint64 `json:"backend_errors"`
}

// portProxy represents a single per-port TCP listener owned by the router.
type portProxy struct {
	instanceID string
	guestPort  int
	publicPort int
	protocol   string
	listener   net.Listener
	cancel     context.CancelFunc
}

// Router is the ingress proxy for instances.
type Router struct {
	lm          *lifecycle.Manager
	addr        string
	server      *http.Server
	probeCfg    ProbeConfig
	mu          sync.Mutex
	portProxies map[string][]*portProxy // instanceID -> proxies

	httpRequests   uint64
	tcpConnections uint64
	probeRetries   uint64
	backendErrors  uint64
}

// New creates a new router.
func New(lm *lifecycle.Manager, addr string) *Router {
	r := &Router{
		lm:          lm,
		addr:        addr,
		probeCfg:    DefaultProbeConfig(),
		portProxies: make(map[string][]*portProxy),
	}
	r.server = &http.Server{
		Addr:    addr,
		Handler: http.HandlerFunc(r.handleRequest),
	}
	return r
}

// Metrics returns atomic ingress and readiness-probe telemetry.
func (r *Router) Metrics() RouterMetrics {
	r.mu.Lock()
	activeProxies := 0
	for _, list := range r.portProxies {
		activeProxies += len(list)
	}
	r.mu.Unlock()

	return RouterMetrics{
		ActivePortProxies: activeProxies,
		HTTPRequests:      atomic.LoadUint64(&r.httpRequests),
		TCPConnections:    atomic.LoadUint64(&r.tcpConnections),
		ProbeRetries:      atomic.LoadUint64(&r.probeRetries),
		BackendErrors:     atomic.LoadUint64(&r.backendErrors),
	}
}

// Start begins listening and serving the main HTTP router.
func (r *Router) Start() error {
	r.mu.Lock()
	listenAddr := r.addr
	r.mu.Unlock()

	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("router listen on %s: %w", listenAddr, err)
	}

	actualAddr := ln.Addr().String()
	r.mu.Lock()
	r.addr = actualAddr
	r.mu.Unlock()

	log.Printf("router listening on %s", actualAddr)

	go func() {
		if err := r.server.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("router error: %v", err)
		}
	}()

	return nil
}

// Stop gracefully shuts down the router and all port proxies.
func (r *Router) Stop(ctx context.Context) error {
	// Close all port proxy listeners
	r.mu.Lock()
	allIDs := make([]string, 0, len(r.portProxies))
	for id := range r.portProxies {
		allIDs = append(allIDs, id)
	}
	r.mu.Unlock()
	for _, id := range allIDs {
		r.FreeAllPorts(id)
	}

	return r.server.Shutdown(ctx)
}

// Addr returns the listen address.
func (r *Router) Addr() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.addr
}

// --- Per-port proxy management ---

// AllocatePort creates a public TCP listener for a guest port.
// If requestedPort is 0, a random port is allocated.
// If requestedPort is non-zero, that specific port is used.
// Idempotent: if instance+guestPort is already allocated, returns the existing public port.
// Returns the allocated host port.
func (r *Router) AllocatePort(instanceID string, guestPort int, requestedPort int, protocol string) (int, error) {
	// Dedup guard: if already allocated for this instance+guestPort, return existing
	r.mu.Lock()
	for _, pp := range r.portProxies[instanceID] {
		if pp.guestPort == guestPort {
			existing := pp.publicPort
			r.mu.Unlock()
			return existing, nil
		}
	}
	r.mu.Unlock()

	listenAddr := "127.0.0.1:0"
	if requestedPort > 0 {
		listenAddr = fmt.Sprintf("127.0.0.1:%d", requestedPort)
	}
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return 0, fmt.Errorf("allocate public port for guest %d: %w", guestPort, err)
	}
	publicPort := ln.Addr().(*net.TCPAddr).Port

	ctx, cancel := context.WithCancel(context.Background())
	pp := &portProxy{
		instanceID: instanceID,
		guestPort:  guestPort,
		publicPort: publicPort,
		protocol:   protocol,
		listener:   ln,
		cancel:     cancel,
	}

	r.mu.Lock()
	r.portProxies[instanceID] = append(r.portProxies[instanceID], pp)
	r.mu.Unlock()

	go r.acceptLoop(ctx, pp)

	log.Printf("router: public port :%d -> instance %s guest :%d (%s)",
		publicPort, instanceID, guestPort, protocol)
	return publicPort, nil
}

// FreeAllPorts closes all public port listeners for an instance.
// Called on instance delete (NOT on stop/pause; listeners survive state transitions).
func (r *Router) FreeAllPorts(instanceID string) {
	r.mu.Lock()
	proxies, ok := r.portProxies[instanceID]
	if ok {
		delete(r.portProxies, instanceID)
	}
	r.mu.Unlock()

	if !ok {
		return
	}

	for _, pp := range proxies {
		pp.cancel()
		pp.listener.Close()
		log.Printf("router: freed public port :%d for instance %s", pp.publicPort, instanceID)
	}
}

// FreePort closes the public port listener for a specific guest port on an instance.
func (r *Router) FreePort(instanceID string, guestPort int) {
	r.mu.Lock()
	proxies := r.portProxies[instanceID]
	for i, pp := range proxies {
		if pp.guestPort == guestPort {
			pp.cancel()
			pp.listener.Close()
			r.portProxies[instanceID] = append(proxies[:i], proxies[i+1:]...)
			r.mu.Unlock()
			log.Printf("router: freed public port :%d for instance %s guest :%d", pp.publicPort, instanceID, guestPort)
			return
		}
	}
	r.mu.Unlock()
}

// GetPublicPort returns the public host port for a guest port.
func (r *Router) GetPublicPort(instanceID string, guestPort int) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, pp := range r.portProxies[instanceID] {
		if pp.guestPort == guestPort {
			return pp.publicPort, nil
		}
	}
	return 0, fmt.Errorf("no public port for instance %s guest port %d", instanceID, guestPort)
}

// GetAllPublicPorts returns all public endpoints for an instance.
func (r *Router) GetAllPublicPorts(instanceID string) []PublicEndpoint {
	r.mu.Lock()
	defer r.mu.Unlock()
	proxies := r.portProxies[instanceID]
	eps := make([]PublicEndpoint, len(proxies))
	for i, pp := range proxies {
		eps[i] = PublicEndpoint{
			GuestPort:  pp.guestPort,
			PublicPort: pp.publicPort,
			Protocol:   pp.protocol,
		}
	}
	return eps
}

// --- Per-port TCP proxy ---

func (r *Router) acceptLoop(ctx context.Context, pp *portProxy) {
	for {
		conn, err := pp.listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return // clean shutdown
			default:
				// Listener closed
				return
			}
		}
		go r.handlePortConn(ctx, conn, pp)
	}
}

func (r *Router) handlePortConn(ctx context.Context, clientConn net.Conn, pp *portProxy) {
	atomic.AddUint64(&r.tcpConnections, 1)
	defer clientConn.Close()
	start := time.Now()

	// Track connection BEFORE ensure to prevent idle timer race
	r.lm.ResetActivity(pp.instanceID)
	defer r.lm.OnConnectionClose(pp.instanceID)

	// Ensure instance is running (wake/boot). Separate generous timeout:
	// cold boot can take 10-15s (VM create + kernel + harness + run RPC).
	ensureCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if err := r.lm.EnsureInstance(ensureCtx, pp.instanceID); err != nil {
		atomic.AddUint64(&r.backendErrors, 1)
		if errors.Is(err, lifecycle.ErrInstanceDisabled) {
			log.Printf("router: instance %s disabled, rejecting connection on port :%d", pp.instanceID, pp.publicPort)
		} else {
			log.Printf("router: port :%d ensure instance %s failed after %v: %v", pp.publicPort, pp.instanceID, time.Since(start), err)
		}
		return
	}
	log.Printf("router: port :%d instance %s ready after %v", pp.publicPort, pp.instanceID, time.Since(start))

	// Get VMM backend endpoint
	backend, err := r.lm.GetEndpoint(pp.instanceID, pp.guestPort)
	if err != nil {
		atomic.AddUint64(&r.backendErrors, 1)
		log.Printf("router: port :%d no backend for guest :%d: %v", pp.publicPort, pp.guestPort, err)
		return
	}

	// Dial backend with readiness probing; app may still be binding its port.
	r.relay(ctx, clientConn, backend, 15*time.Second)
}

// dialBackend connects to the backend with retries using context.Background().
func (r *Router) dialBackend(backend string, timeout time.Duration) (net.Conn, error) {
	return r.dialBackendContext(context.Background(), backend, timeout)
}

// dialBackendContext connects to the backend with context-aware readiness probing.
// Handles two failure modes:
//  1. Dial refused: app has not bound its port yet (normal retry with backoff)
//  2. Dial succeeds but connection closes immediately: network proxy accepted the TCP
//     connection before the guest app was ready (detected via non-destructive MSG_PEEK)
func (r *Router) dialBackendContext(ctx context.Context, backend string, timeout time.Duration) (net.Conn, error) {
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	conn, retries, err := ProbeTCPWithStats(dialCtx, backend, r.probeCfg)
	if retries > 0 {
		atomic.AddUint64(&r.probeRetries, retries)
	}
	if err != nil {
		atomic.AddUint64(&r.backendErrors, 1)
		return nil, err
	}
	return conn, nil
}

// connAlive checks if a TCP connection is still open using a non-destructive
// peek (MSG_PEEK). Returns false if the remote end has already closed.
func connAlive(tc *net.TCPConn) bool {
	rc, err := tc.SyscallConn()
	if err != nil {
		return true // cannot check, assume alive
	}
	alive := true
	rc.Read(func(fd uintptr) bool {
		buf := make([]byte, 1)
		n, _, err := syscall.Recvfrom(int(fd), buf, syscall.MSG_PEEK|syscall.MSG_DONTWAIT)
		if err == syscall.EAGAIN || err == syscall.EWOULDBLOCK {
			alive = true // no data yet, connection open
		} else if err != nil {
			alive = false // ECONNRESET, etc.
		} else if n == 0 {
			alive = false // EOF: remote closed the connection
		}
		return true
	})
	return alive
}

// relay does bidirectional TCP copy between client and backend.
func (r *Router) relay(ctx context.Context, clientConn net.Conn, backend string, dialTimeout time.Duration) {
	backendConn, err := r.dialBackendContext(ctx, backend, dialTimeout)
	if err != nil {
		log.Printf("router: dial backend %s: %v", backend, err)
		return
	}
	defer backendConn.Close()

	done := make(chan struct{}, 2)
	go func() {
		io.Copy(backendConn, clientConn)
		done <- struct{}{}
	}()
	go func() {
		io.Copy(clientConn, backendConn)
		done <- struct{}{}
	}()
	<-done
}

// --- Main HTTP router (handle-based routing on :8099) ---

func (r *Router) handleRequest(w http.ResponseWriter, req *http.Request) {
	atomic.AddUint64(&r.httpRequests, 1)

	inst := r.resolveInstance(req)
	if inst == nil {
		http.Error(w, "No active instance", http.StatusServiceUnavailable)
		return
	}

	// Track connection BEFORE ensure to prevent idle timer race
	r.lm.ResetActivity(inst.ID)
	defer r.lm.OnConnectionClose(inst.ID)

	// Ensure instance is running (wake/boot)
	ensureCtx, cancel := context.WithTimeout(req.Context(), 30*time.Second)
	defer cancel()

	if err := r.lm.EnsureInstance(ensureCtx, inst.ID); err != nil {
		atomic.AddUint64(&r.backendErrors, 1)
		if errors.Is(err, lifecycle.ErrInstanceDisabled) {
			http.Error(w, "instance disabled", http.StatusServiceUnavailable)
		} else {
			log.Printf("router: ensure instance %s: %v", inst.ID, err)
			w.Header().Set("Retry-After", "3")
			http.Error(w, fmt.Sprintf("Service unavailable: %v", err), http.StatusServiceUnavailable)
		}
		return
	}

	// Get the host endpoint for the first exposed port
	guestPort := inst.FirstGuestPort()
	if guestPort == 0 {
		http.Error(w, "No exposed ports", http.StatusServiceUnavailable)
		return
	}

	target, err := r.lm.GetEndpoint(inst.ID, guestPort)
	if err != nil {
		atomic.AddUint64(&r.backendErrors, 1)
		http.Error(w, fmt.Sprintf("endpoint not found: %v", err), http.StatusBadGateway)
		return
	}

	// Check for WebSocket upgrade
	if isWebSocketUpgrade(req) {
		r.handleWebSocket(w, req, target, 15*time.Second)
		return
	}

	// Standard HTTP reverse proxy with context-aware readiness probe
	targetURL, _ := url.Parse("http://" + target)
	proxy := &httputil.ReverseProxy{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return r.dialBackendContext(ctx, addr, 15*time.Second)
			},
		},
		Director: func(r *http.Request) {
			r.URL.Scheme = targetURL.Scheme
			r.URL.Host = targetURL.Host
			r.Host = targetURL.Host
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("router proxy error: %v", err)
			w.Header().Set("Retry-After", "3")
			http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
		},
	}

	proxy.ServeHTTP(w, req)
}

func (r *Router) handleWebSocket(w http.ResponseWriter, req *http.Request, target string, dialTimeout time.Duration) {
	// Dial backend with retries (app may not have bound port yet after wake)
	backendConn, err := r.dialBackendContext(req.Context(), target, dialTimeout)
	if err != nil {
		http.Error(w, "WebSocket backend connection failed", http.StatusBadGateway)
		return
	}
	defer backendConn.Close()

	// Hijack the client connection
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "WebSocket hijack not supported", http.StatusInternalServerError)
		return
	}
	clientConn, clientBuf, err := hj.Hijack()
	if err != nil {
		http.Error(w, "WebSocket hijack failed", http.StatusInternalServerError)
		return
	}
	defer clientConn.Close()

	// Forward the original upgrade request to the backend
	if err := req.Write(backendConn); err != nil {
		return
	}

	// Flush any buffered data from the client
	if clientBuf.Reader.Buffered() > 0 {
		buffered := make([]byte, clientBuf.Reader.Buffered())
		clientBuf.Read(buffered)
		backendConn.Write(buffered)
	}

	// Bidirectional copy
	done := make(chan struct{}, 2)
	go func() {
		io.Copy(backendConn, clientConn)
		done <- struct{}{}
	}()
	go func() {
		io.Copy(clientConn, backendConn)
		done <- struct{}{}
	}()
	<-done
}

// resolveInstance finds an instance for a request.
// Resolution order:
//  1. X-OmniVM-Instance header -> route to instance by ID or handle
//  2. Fall back to default instance if only one exists
func (r *Router) resolveInstance(req *http.Request) *lifecycle.Instance {
	// 1. Header-based routing: X-OmniVM-Instance (ID or handle)
	if instRef := req.Header.Get("X-OmniVM-Instance"); instRef != "" {
		if inst := r.lm.GetInstance(instRef); inst != nil {
			return inst
		}
		return r.lm.GetInstanceByHandle(instRef)
	}

	// 2. Default instance fallback
	count := r.lm.InstanceCount()
	if count == 1 {
		return r.lm.GetDefaultInstance()
	}
	if count > 1 {
		log.Printf("router: ambiguous request to %s with %d instances; use X-OmniVM-Instance header or per-port endpoints", req.URL.Path, count)
	}
	return nil
}

func isWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") &&
		strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade")
}
