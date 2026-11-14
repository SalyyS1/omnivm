package router

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

// ProbeConfig controls readiness probing for guest TCP and virtio-vsock endpoints.
type ProbeConfig struct {
	DialTimeout    time.Duration
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	LivenessSettle time.Duration
}

// DefaultProbeConfig returns production defaults tuned for fast microVM wake paths.
func DefaultProbeConfig() ProbeConfig {
	return ProbeConfig{
		DialTimeout:    500 * time.Millisecond,
		InitialBackoff: 20 * time.Millisecond,
		MaxBackoff:     200 * time.Millisecond,
		LivenessSettle: 10 * time.Millisecond,
	}
}

func (c ProbeConfig) withDefaults() ProbeConfig {
	def := DefaultProbeConfig()
	if c.DialTimeout <= 0 {
		c.DialTimeout = def.DialTimeout
	}
	if c.InitialBackoff <= 0 {
		c.InitialBackoff = def.InitialBackoff
	}
	if c.MaxBackoff <= 0 {
		c.MaxBackoff = def.MaxBackoff
	}
	if c.MaxBackoff < c.InitialBackoff {
		c.MaxBackoff = c.InitialBackoff
	}
	if c.LivenessSettle < 0 {
		c.LivenessSettle = 0
	}
	return c
}

// ProbeTCP dials a TCP backend until a live connection is established or ctx expires.
func ProbeTCP(ctx context.Context, addr string, cfg ProbeConfig) (net.Conn, error) {
	conn, _, err := ProbeTCPWithStats(ctx, addr, cfg)
	return conn, err
}

// ProbeTCPWithStats dials a TCP backend with bounded exponential backoff and MSG_PEEK
// liveness verification, returning the live connection and retry count.
func ProbeTCPWithStats(ctx context.Context, addr string, cfg ProbeConfig) (net.Conn, uint64, error) {
	if strings.TrimSpace(addr) == "" {
		return nil, 0, errors.New("probe tcp: empty address")
	}
	cfg = cfg.withDefaults()
	backoff := cfg.InitialBackoff
	var retries uint64
	var lastErr error

	for {
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return nil, retries, fmt.Errorf("probe tcp %s: %w (last error: %v)", addr, err, lastErr)
			}
			return nil, retries, fmt.Errorf("probe tcp %s: %w", addr, err)
		}

		dialer := net.Dialer{Timeout: cfg.DialTimeout}
		conn, err := dialer.DialContext(ctx, "tcp", addr)
		if err != nil {
			lastErr = err
			if ctx.Err() != nil {
				return nil, retries, fmt.Errorf("probe tcp %s: %w (last error: %v)", addr, ctx.Err(), lastErr)
			}
			retries++
			if !sleepContext(ctx, backoff) {
				return nil, retries, fmt.Errorf("probe tcp %s: %w (last error: %v)", addr, ctx.Err(), lastErr)
			}
			backoff = nextBackoff(backoff, cfg.MaxBackoff)
			continue
		}

		if cfg.LivenessSettle > 0 {
			if !sleepContext(ctx, cfg.LivenessSettle) {
				conn.Close()
				return nil, retries, fmt.Errorf("probe tcp %s: %w", addr, ctx.Err())
			}
		}

		if tc, ok := conn.(*net.TCPConn); ok && !connAlive(tc) {
			conn.Close()
			lastErr = fmt.Errorf("backend %s accepted connection then closed immediately", addr)
			retries++
			if !sleepContext(ctx, backoff) {
				return nil, retries, fmt.Errorf("probe tcp %s: %w (last error: %v)", addr, ctx.Err(), lastErr)
			}
			backoff = nextBackoff(backoff, cfg.MaxBackoff)
			continue
		}

		return conn, retries, nil
	}
}

// CheckTCPReady verifies that a TCP endpoint accepts and sustains a connection.
func CheckTCPReady(ctx context.Context, addr string, cfg ProbeConfig) error {
	conn, err := ProbeTCP(ctx, addr, cfg)
	if err != nil {
		return err
	}
	return conn.Close()
}

// ProbeVsock connects to a guest vsock listener via Cloud Hypervisor's host-side
// virtio-vsock Unix multiplexer socket ("CONNECT <port>\n" -> "OK <local_port>\n").
// The handshake response is read one byte at a time so zero guest payload bytes are consumed.
func ProbeVsock(ctx context.Context, muxSocketPath string, guestPort int, cfg ProbeConfig) (net.Conn, error) {
	if strings.TrimSpace(muxSocketPath) == "" {
		return nil, errors.New("probe vsock: empty mux socket path")
	}
	if guestPort <= 0 || guestPort > 65535 {
		return nil, fmt.Errorf("probe vsock: invalid guest port %d", guestPort)
	}
	cfg = cfg.withDefaults()
	backoff := cfg.InitialBackoff
	var lastErr error

	for {
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return nil, fmt.Errorf("probe vsock %s:%d: %w (last error: %v)", muxSocketPath, guestPort, err, lastErr)
			}
			return nil, fmt.Errorf("probe vsock %s:%d: %w", muxSocketPath, guestPort, err)
		}

		conn, err := tryVsockHandshake(ctx, muxSocketPath, guestPort, cfg.DialTimeout)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, fmt.Errorf("probe vsock %s:%d: %w (last error: %v)", muxSocketPath, guestPort, ctx.Err(), lastErr)
		}
		if !sleepContext(ctx, backoff) {
			return nil, fmt.Errorf("probe vsock %s:%d: %w (last error: %v)", muxSocketPath, guestPort, ctx.Err(), lastErr)
		}
		backoff = nextBackoff(backoff, cfg.MaxBackoff)
	}
}

// CheckVsockReady verifies that a guest vsock port completes the Cloud Hypervisor handshake.
func CheckVsockReady(ctx context.Context, muxSocketPath string, guestPort int, cfg ProbeConfig) error {
	conn, err := ProbeVsock(ctx, muxSocketPath, guestPort, cfg)
	if err != nil {
		return err
	}
	return conn.Close()
}

func tryVsockHandshake(ctx context.Context, muxSocketPath string, guestPort int, timeout time.Duration) (net.Conn, error) {
	dialer := net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "unix", muxSocketPath)
	if err != nil {
		return nil, err
	}

	deadline := time.Now().Add(timeout)
	if ctxDeadline, ok := ctx.Done(), false; ctxDeadline != nil {
		if d, has := ctx.Deadline(); has && d.Before(deadline) {
			deadline = d
		} else {
			_ = ok
		}
	}
	if err := conn.SetDeadline(deadline); err != nil {
		conn.Close()
		return nil, err
	}

	stopCancelWatch := make(chan struct{})
	defer close(stopCancelWatch)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.SetDeadline(time.Now())
		case <-stopCancelWatch:
		}
	}()

	cmd := fmt.Sprintf("CONNECT %d\n", guestPort)
	if _, err := conn.Write([]byte(cmd)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("vsock write handshake: %w", err)
	}

	line, err := readLineSingleByte(conn, 128)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("vsock read handshake: %w", err)
	}
	if !strings.HasPrefix(line, "OK ") && line != "OK" {
		conn.Close()
		return nil, fmt.Errorf("vsock handshake rejected: %q", line)
	}

	if err := conn.SetDeadline(time.Time{}); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

func readLineSingleByte(conn net.Conn, maxLen int) (string, error) {
	var buf [1]byte
	out := make([]byte, 0, 32)
	for len(out) < maxLen {
		n, err := conn.Read(buf[:])
		if n > 0 {
			if buf[0] == '\n' {
				return strings.TrimSuffix(string(out), "\r"), nil
			}
			out = append(out, buf[0])
		}
		if err != nil {
			return "", err
		}
	}
	return "", errors.New("vsock handshake response exceeded max line length")
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func nextBackoff(cur, max time.Duration) time.Duration {
	next := cur * 2
	if next > max {
		return max
	}
	return next
}
