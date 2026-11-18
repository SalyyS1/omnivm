package vsockmux

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func newPipePair(t *testing.T, cfg Config) (*Session, *Session) {
	t.Helper()
	c1, c2 := net.Pipe()
	client := NewClient(c1, cfg)
	server := NewServer(c2, cfg)
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})
	return client, server
}

func TestMux_ConcurrentBidirectionalStreamsIntegrity(t *testing.T) {
	client, server := newPipePair(t, Config{
		InitialStreamWindow: 32 * 1024,
		MaxFramePayload:     8 * 1024,
		AcceptBacklog:       64,
	})

	const numStreams = 24
	const payloadSize = 128 * 1024 // 4x initial window to exercise window updates

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Server echoes every accepted stream after reversing each 4 KiB block XOR mask.
	var srvWG sync.WaitGroup
	srvWG.Add(1)
	go func() {
		defer srvWG.Done()
		for i := 0; i < numStreams; i++ {
			st, err := server.AcceptStream(ctx)
			if err != nil {
				t.Errorf("AcceptStream(%d): %v", i, err)
				return
			}
			srvWG.Add(1)
			go func(s *Stream) {
				defer srvWG.Done()
				defer s.Close()
				data, err := io.ReadAll(s)
				if err != nil {
					t.Errorf("server ReadAll(%s): %v", s.Target(), err)
					return
				}
				for j := range data {
					data[j] ^= 0x5A
				}
				if _, err := s.Write(data); err != nil {
					t.Errorf("server Write(%s): %v", s.Target(), err)
					return
				}
				_ = s.CloseWrite()
			}(st)
		}
	}()

	var cliWG sync.WaitGroup
	for i := 0; i < numStreams; i++ {
		cliWG.Add(1)
		go func(idx int) {
			defer cliWG.Done()
			target := fmt.Sprintf("ipc:worker-%d", idx)
			st, err := client.OpenStream(ctx, target)
			if err != nil {
				t.Errorf("OpenStream(%s): %v", target, err)
				return
			}
			defer st.Close()

			sendBuf := make([]byte, payloadSize)
			for j := range sendBuf {
				sendBuf[j] = byte((idx*31 + j*17) & 0xff)
			}
			wantBuf := make([]byte, payloadSize)
			for j := range sendBuf {
				wantBuf[j] = sendBuf[j] ^ 0x5A
			}
			wantSum := sha256.Sum256(wantBuf)

			writeErrCh := make(chan error, 1)
			go func() {
				_, wErr := st.Write(sendBuf)
				if wErr == nil {
					wErr = st.CloseWrite()
				}
				writeErrCh <- wErr
			}()

			gotBuf, rErr := io.ReadAll(st)
			if rErr != nil {
				t.Errorf("client ReadAll(%s): %v", target, rErr)
				return
			}
			if wErr := <-writeErrCh; wErr != nil {
				t.Errorf("client Write(%s): %v", target, wErr)
				return
			}
			gotSum := sha256.Sum256(gotBuf)
			if gotSum != wantSum {
				t.Errorf("stream %s SHA-256 mismatch: len=%d", target, len(gotBuf))
			}
		}(i)
	}

	cliWG.Wait()
	srvWG.Wait()

	cm := client.Metrics()
	sm := server.Metrics()
	if cm.StreamsOpened != numStreams || sm.StreamsAccepted != numStreams {
		t.Fatalf("opened=%d accepted=%d, want %d", cm.StreamsOpened, sm.StreamsAccepted, numStreams)
	}
	wantBytes := uint64(numStreams * payloadSize)
	if cm.BytesSent != wantBytes || cm.BytesReceived != wantBytes {
		t.Fatalf("client bytes sent=%d recv=%d, want %d", cm.BytesSent, cm.BytesReceived, wantBytes)
	}
}

func TestMux_CreditBackpressureDoesNotBlockSiblingStreams(t *testing.T) {
	client, server := newPipePair(t, Config{
		InitialStreamWindow: 8192,
		MaxFramePayload:     4096,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Accept two streams on the server:
	// - slowSrv never reads (exhausting stream 1's 8 KiB window).
	// - fastSrv echoes immediately (proving stream 3 is unaffected).
	accepted := make(chan *Stream, 2)
	go func() {
		for i := 0; i < 2; i++ {
			st, err := server.AcceptStream(ctx)
			if err == nil {
				accepted <- st
			}
		}
	}()

	slowCli, err := client.OpenStream(ctx, "ipc:slow")
	if err != nil {
		t.Fatalf("open slow: %v", err)
	}
	slowSrv := <-accepted
	defer slowSrv.Close()

	// Fill the entire 8 KiB initial window on slowCli; this must succeed immediately.
	if _, err := slowCli.Write(bytes.Repeat([]byte("A"), 8192)); err != nil {
		t.Fatalf("fill slow window: %v", err)
	}

	// Next write must stall on zero credit and time out via SetWriteDeadline.
	_ = slowCli.SetWriteDeadline(time.Now().Add(60 * time.Millisecond))
	_, err = slowCli.Write([]byte("overflow"))
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("expected deadline exceeded on exhausted window, got %v", err)
	}
	_ = slowCli.SetWriteDeadline(time.Time{})

	if client.Metrics().WindowStalls == 0 {
		t.Fatal("expected WindowStalls > 0 after credit exhaustion")
	}

	// While slowCli is stalled at 0 window, fastCli must still transfer 64 KiB cleanly.
	fastCli, err := client.OpenStream(ctx, "ipc:fast")
	if err != nil {
		t.Fatalf("open fast: %v", err)
	}
	fastSrv := <-accepted
	go func() {
		_, _ = io.Copy(fastSrv, fastSrv)
		_ = fastSrv.CloseWrite()
	}()

	payload := bytes.Repeat([]byte("fast-lane-"), 6400) // 64 KB
	go func() {
		_, _ = fastCli.Write(payload)
		_ = fastCli.CloseWrite()
	}()
	echoed, err := io.ReadAll(fastCli)
	if err != nil {
		t.Fatalf("fastCli ReadAll: %v", err)
	}
	if !bytes.Equal(echoed, payload) {
		t.Fatalf("fastCli echoed %d bytes, want %d", len(echoed), len(payload))
	}

	// Now drain slowSrv on the server; slowCli must regain credits and finish writing!
	doneDrain := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(slowSrv)
		doneDrain <- b
	}()
	if _, err := slowCli.Write(bytes.Repeat([]byte("B"), 4096)); err != nil {
		t.Fatalf("slowCli write after drain: %v", err)
	}
	_ = slowCli.CloseWrite()
	gotSlow := <-doneDrain
	if len(gotSlow) != 8192+4096 {
		t.Fatalf("slowSrv received %d bytes, want %d", len(gotSlow), 8192+4096)
	}
}

func TestMux_PingResetAndGoAway(t *testing.T) {
	client, server := newPipePair(t, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// 1. Ping RTT measurement
	rtt, err := client.Ping(ctx)
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if rtt < 0 {
		t.Fatalf("negative rtt: %v", rtt)
	}

	// 2. Peer stream reset propagates reason
	go func() {
		st, aErr := server.AcceptStream(ctx)
		if aErr == nil {
			_ = st.Reset("policy denied")
		}
	}()
	st, err := client.OpenStream(ctx, "tcp:22")
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	_, rErr := st.Read(make([]byte, 16))
	var resetErr *StreamResetError
	if !errors.As(rErr, &resetErr) || resetErr.Reason != "policy denied" {
		t.Fatalf("expected StreamResetError(policy denied), got %v", rErr)
	}

	// 3. GoAway rejects new streams cleanly
	if err := server.GoAway(); err != nil {
		t.Fatalf("server.GoAway: %v", err)
	}
	// Give recvLoop a moment or observe either local ErrGoAway or remote Reset("goaway").
	time.Sleep(20 * time.Millisecond)
	_, openErr := client.OpenStream(ctx, "ipc:after-goaway")
	if !errors.Is(openErr, ErrGoAway) {
		var re *StreamResetError
		if !errors.As(openErr, &re) {
			t.Fatalf("expected ErrGoAway or StreamResetError after GoAway, got %v", openErr)
		}
	}
}

func TestDialCloudHypervisorVsock_HandshakeAndMultiplex(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "ch-vsock.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	defer ln.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		conn, aErr := ln.Accept()
		if aErr != nil {
			errCh <- aErr
			return
		}
		// Read "CONNECT 5050\n" byte-by-byte like Cloud Hypervisor's vsock mux.
		var reqBuf []byte
		var b [1]byte
		for {
			if _, rErr := io.ReadFull(conn, b[:]); rErr != nil {
				errCh <- rErr
				return
			}
			if b[0] == '\n' {
				break
			}
			reqBuf = append(reqBuf, b[0])
		}
		if string(reqBuf) != "CONNECT 5050" {
			errCh <- fmt.Errorf("unexpected handshake %q", string(reqBuf))
			return
		}
		// Immediately send "OK 1073741824\n" followed by starting the server mux!
		if _, wErr := io.WriteString(conn, "OK 1073741824\n"); wErr != nil {
			errCh <- wErr
			return
		}
		srvMux := NewServer(conn, Config{})
		defer srvMux.Close()

		st, sErr := srvMux.AcceptStream(ctx)
		if sErr != nil {
			errCh <- sErr
			return
		}
		msg, _ := io.ReadAll(st)
		_, _ = st.Write(append([]byte("ack:"), msg...))
		_ = st.CloseWrite()
		<-srvMux.Done()
		errCh <- nil
	}()

	cliMux, localPort, err := DialCloudHypervisorVsock(ctx, sockPath, 5050, Config{})
	if err != nil {
		t.Fatalf("DialCloudHypervisorVsock: %v", err)
	}
	if localPort != 1073741824 {
		t.Fatalf("localPort = %d, want 1073741824", localPort)
	}

	st, err := cliMux.OpenStream(ctx, "ipc:guest-agent")
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	if _, err := st.Write([]byte("zero-net-rpc")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	_ = st.CloseWrite()
	reply, err := io.ReadAll(st)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(reply) != "ack:zero-net-rpc" {
		t.Fatalf("reply = %q, want ack:zero-net-rpc", string(reply))
	}
	_ = cliMux.Close()

	if srvErr := <-errCh; srvErr != nil {
		t.Fatalf("mock CH vsock server error: %v", srvErr)
	}
}
