// Package vsockmux implements a binary-framed, credit-flow-controlled stream
// multiplexer over a single virtio-vsock or Unix domain socket carrier.
//
// In zero-network microVM sandboxes (where TAP/veth interfaces are omitted for
// airgapped isolation), ovmd and ovm-harness multiplex concurrent bidirectional
// RPC, stdio, blob transfer, and port-forwarded streams over one AF_VSOCK
// connection without head-of-line blocking across independent streams.
package vsockmux

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// HeaderSize is the fixed 10-byte wire header:
	//   [0..4)  StreamID uint32 (big-endian; 0 = session control)
	//   [4]     FrameType uint8
	//   [5]     Flags uint8
	//   [6..10) Length uint32 (big-endian payload byte length)
	HeaderSize = 10

	// DefaultStreamWindow is the per-stream initial receive credit window (256 KiB).
	DefaultStreamWindow uint32 = 256 * 1024

	// DefaultMaxFramePayload caps single DATA frame payloads (64 KiB) so large
	// writes interleave fairly across concurrent streams.
	DefaultMaxFramePayload uint32 = 64 * 1024

	// MaxControlPayload bounds control frame metadata (OPEN target, RESET reason).
	MaxControlPayload uint32 = 4096
)

// Frame types on the wire.
const (
	FrameOpen         uint8 = 0x01
	FrameOpenAck      uint8 = 0x02
	FrameData         uint8 = 0x03
	FrameWindowUpdate uint8 = 0x04
	FrameHalfClose    uint8 = 0x05
	FrameReset        uint8 = 0x06
	FramePing         uint8 = 0x07
	FramePong         uint8 = 0x08
	FrameGoAway       uint8 = 0x09
)

var (
	// ErrSessionClosed indicates the underlying mux session has shut down.
	ErrSessionClosed = errors.New("vsockmux: session closed")
	// ErrGoAway indicates the session is draining and rejecting new streams.
	ErrGoAway = errors.New("vsockmux: peer sent goaway")
	// ErrStreamClosed indicates writes were attempted after Close or CloseWrite.
	ErrStreamClosed = errors.New("vsockmux: stream closed for writes")
	// ErrProtocolViolation indicates a malformed frame or invalid stream state.
	ErrProtocolViolation = errors.New("vsockmux: protocol violation")
)

// StreamResetError is returned when a stream is aborted via FrameReset.
type StreamResetError struct {
	Reason string
}

func (e *StreamResetError) Error() string {
	if e.Reason == "" {
		return "vsockmux: stream reset by peer"
	}
	return "vsockmux: stream reset: " + e.Reason
}

// Config tunes per-stream flow-control windows and frame chunking.
type Config struct {
	InitialStreamWindow uint32
	MaxFramePayload     uint32
	AcceptBacklog       int
}

func (c Config) normalized() Config {
	if c.InitialStreamWindow < 4096 {
		c.InitialStreamWindow = DefaultStreamWindow
	}
	if c.MaxFramePayload == 0 || c.MaxFramePayload > 1024*1024 {
		c.MaxFramePayload = DefaultMaxFramePayload
	}
	if c.MaxFramePayload > c.InitialStreamWindow {
		c.MaxFramePayload = c.InitialStreamWindow
	}
	if c.AcceptBacklog <= 0 {
		c.AcceptBacklog = 64
	}
	return c
}

// Metrics snapshots session-level multiplexing and flow-control counters.
type Metrics struct {
	ActiveStreams    int    `json:"active_streams"`
	StreamsOpened    uint64 `json:"streams_opened"`
	StreamsAccepted  uint64 `json:"streams_accepted"`
	StreamsRejected  uint64 `json:"streams_rejected"`
	FramesSent       uint64 `json:"frames_sent"`
	FramesReceived   uint64 `json:"frames_received"`
	BytesSent        uint64 `json:"bytes_sent"`
	BytesReceived    uint64 `json:"bytes_received"`
	WindowStalls     uint64 `json:"window_stalls"`
	LastPingRTTMicro int64  `json:"last_ping_rtt_micros"`
}

type muxCounters struct {
	streamsOpened    uint64
	streamsAccepted  uint64
	streamsRejected  uint64
	framesSent       uint64
	framesReceived   uint64
	bytesSent        uint64
	bytesReceived    uint64
	windowStalls     uint64
	lastPingRTTMicro int64
}

// Session multiplexes bidirectional streams over a single reliable transport.
// Client sessions allocate odd StreamIDs (1, 3, 5, ...); server sessions
// allocate even StreamIDs (2, 4, 6, ...).
type Session struct {
	conn     io.ReadWriteCloser
	isClient bool
	cfg      Config

	writeMu sync.Mutex

	mu        sync.Mutex
	nextID    uint32
	streams   map[uint32]*Stream
	pings     map[uint64]chan struct{}
	pingSeq   uint64
	goingAway bool
	closed    bool

	acceptCh chan *Stream
	closedCh chan struct{}
	doneCh   chan struct{}

	counters muxCounters
}

// NewClient constructs a client-side multiplexer session over conn.
func NewClient(conn io.ReadWriteCloser, cfg Config) *Session {
	return newSession(conn, true, cfg)
}

// NewServer constructs a server-side multiplexer session over conn.
func NewServer(conn io.ReadWriteCloser, cfg Config) *Session {
	return newSession(conn, false, cfg)
}

func newSession(conn io.ReadWriteCloser, isClient bool, cfg Config) *Session {
	norm := cfg.normalized()
	firstID := uint32(2)
	if isClient {
		firstID = 1
	}
	s := &Session{
		conn:     conn,
		isClient: isClient,
		cfg:      norm,
		nextID:   firstID,
		streams:  make(map[uint32]*Stream),
		pings:    make(map[uint64]chan struct{}),
		acceptCh: make(chan *Stream, norm.AcceptBacklog),
		closedCh: make(chan struct{}),
		doneCh:   make(chan struct{}),
	}
	go s.recvLoop()
	return s
}

// DialCloudHypervisorVsock connects to a Cloud Hypervisor host Unix vsock
// multiplexer socket, completes the single-byte-safe "CONNECT <port>\n" ->
// "OK <local_port>\n" handshake without buffering payload bytes, and starts a
// client multiplexing Session over the negotiated guest stream.
func DialCloudHypervisorVsock(ctx context.Context, udsPath string, port uint32, cfg Config) (*Session, uint32, error) {
	if port == 0 {
		return nil, 0, errors.New("vsockmux: guest vsock port must be > 0")
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", udsPath)
	if err != nil {
		return nil, 0, fmt.Errorf("vsockmux: dial %s: %w", udsPath, err)
	}

	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	reqLine := fmt.Sprintf("CONNECT %d\n", port)
	if _, err := io.WriteString(conn, reqLine); err != nil {
		_ = conn.Close()
		return nil, 0, fmt.Errorf("vsockmux: send CONNECT %d: %w", port, err)
	}

	// Read response header one byte at a time so zero binary mux frame bytes
	// following '\n' are ever consumed into an intermediate buffer.
	var lineBuf [64]byte
	lineLen := 0
	var one [1]byte
	for {
		if lineLen >= len(lineBuf) {
			_ = conn.Close()
			return nil, 0, fmt.Errorf("vsockmux: oversized vsock handshake response")
		}
		if _, err := io.ReadFull(conn, one[:]); err != nil {
			_ = conn.Close()
			return nil, 0, fmt.Errorf("vsockmux: read CONNECT ack: %w", err)
		}
		if one[0] == '\n' {
			break
		}
		lineBuf[lineLen] = one[0]
		lineLen++
	}
	_ = conn.SetDeadline(time.Time{})

	resp := strings.TrimSpace(string(lineBuf[:lineLen]))
	if !strings.HasPrefix(resp, "OK ") {
		_ = conn.Close()
		return nil, 0, fmt.Errorf("vsockmux: unexpected vsock handshake %q", resp)
	}
	localPort64, err := strconv.ParseUint(strings.TrimSpace(strings.TrimPrefix(resp, "OK ")), 10, 32)
	if err != nil {
		_ = conn.Close()
		return nil, 0, fmt.Errorf("vsockmux: invalid local port in %q: %w", resp, err)
	}
	return NewClient(conn, cfg), uint32(localPort64), nil
}

// OpenStream opens a new logical stream targeting target (e.g. "ipc:stdio" or
// "tcp:8080") and waits for the peer's FrameOpenAck or context cancellation.
func (s *Session) OpenStream(ctx context.Context, target string) (*Stream, error) {
	if uint32(len(target)) > MaxControlPayload {
		return nil, fmt.Errorf("vsockmux: target metadata exceeds %d bytes", MaxControlPayload)
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, ErrSessionClosed
	}
	if s.goingAway {
		s.mu.Unlock()
		return nil, ErrGoAway
	}
	id := s.nextID
	s.nextID += 2
	st := newStream(s, id, target, s.cfg.InitialStreamWindow, 0)
	s.streams[id] = st
	s.mu.Unlock()

	var openPayload []byte
	openPayload = make([]byte, 4+len(target))
	binary.BigEndian.PutUint32(openPayload[0:4], s.cfg.InitialStreamWindow)
	copy(openPayload[4:], target)

	if err := s.writeFrame(id, FrameOpen, 0, openPayload); err != nil {
		s.removeStream(id)
		return nil, err
	}

	select {
	case err := <-st.ackCh:
		if err != nil {
			s.removeStream(id)
			return nil, err
		}
		atomic.AddUint64(&s.counters.streamsOpened, 1)
		return st, nil
	case <-ctx.Done():
		_ = st.Reset("open cancelled")
		return nil, ctx.Err()
	case <-s.closedCh:
		return nil, ErrSessionClosed
	}
}

// AcceptStream waits for the next peer-initiated logical stream.
func (s *Session) AcceptStream(ctx context.Context) (*Stream, error) {
	select {
	case st := <-s.acceptCh:
		return st, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.closedCh:
		return nil, ErrSessionClosed
	}
}

// Ping sends an 8-byte nonce keepalive frame and measures round-trip latency.
func (s *Session) Ping(ctx context.Context) (time.Duration, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return 0, ErrSessionClosed
	}
	s.pingSeq++
	seq := s.pingSeq
	ch := make(chan struct{}, 1)
	s.pings[seq] = ch
	s.mu.Unlock()

	var payload [8]byte
	binary.BigEndian.PutUint64(payload[:], seq)
	start := time.Now()
	if err := s.writeFrame(0, FramePing, 0, payload[:]); err != nil {
		s.mu.Lock()
		delete(s.pings, seq)
		s.mu.Unlock()
		return 0, err
	}

	select {
	case <-ch:
		rtt := time.Since(start)
		atomic.StoreInt64(&s.counters.lastPingRTTMicro, rtt.Microseconds())
		return rtt, nil
	case <-ctx.Done():
		s.mu.Lock()
		delete(s.pings, seq)
		s.mu.Unlock()
		return 0, ctx.Err()
	case <-s.closedCh:
		return 0, ErrSessionClosed
	}
}

// GoAway signals graceful drain: no new streams will be opened or accepted,
// while existing streams continue transferring until closed.
func (s *Session) GoAway() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrSessionClosed
	}
	s.goingAway = true
	s.mu.Unlock()
	return s.writeFrame(0, FrameGoAway, 0, nil)
}

// Metrics returns a race-free snapshot of session multiplexing counters.
func (s *Session) Metrics() Metrics {
	s.mu.Lock()
	active := len(s.streams)
	s.mu.Unlock()
	return Metrics{
		ActiveStreams:    active,
		StreamsOpened:    atomic.LoadUint64(&s.counters.streamsOpened),
		StreamsAccepted:  atomic.LoadUint64(&s.counters.streamsAccepted),
		StreamsRejected:  atomic.LoadUint64(&s.counters.streamsRejected),
		FramesSent:       atomic.LoadUint64(&s.counters.framesSent),
		FramesReceived:   atomic.LoadUint64(&s.counters.framesReceived),
		BytesSent:        atomic.LoadUint64(&s.counters.bytesSent),
		BytesReceived:    atomic.LoadUint64(&s.counters.bytesReceived),
		WindowStalls:     atomic.LoadUint64(&s.counters.windowStalls),
		LastPingRTTMicro: atomic.LoadInt64(&s.counters.lastPingRTTMicro),
	}
}

// Close tears down the session and unblocks all waiting streams and callers.
func (s *Session) Close() error {
	err := s.shutdown()
	<-s.doneCh
	return err
}

// Done returns a channel closed when the session's receive loop exits.
func (s *Session) Done() <-chan struct{} {
	return s.doneCh
}

func (s *Session) isClosed() bool {
	select {
	case <-s.closedCh:
		return true
	default:
		return false
	}
}

func (s *Session) shutdown() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	close(s.closedCh)
	streams := make([]*Stream, 0, len(s.streams))
	for _, st := range s.streams {
		streams = append(streams, st)
	}
	s.streams = make(map[uint32]*Stream)
	s.pings = make(map[uint64]chan struct{})
	s.mu.Unlock()

	for _, st := range streams {
		st.abortOnSessionClose()
	}
	return s.conn.Close()
}

func (s *Session) writeFrame(streamID uint32, frameType uint8, flags uint8, payload []byte) error {
	if s.isClosed() {
		return ErrSessionClosed
	}
	var hdr [HeaderSize]byte
	binary.BigEndian.PutUint32(hdr[0:4], streamID)
	hdr[4] = frameType
	hdr[5] = flags
	binary.BigEndian.PutUint32(hdr[6:10], uint32(len(payload)))

	s.writeMu.Lock()
	_, err := s.conn.Write(hdr[:])
	if err == nil && len(payload) > 0 {
		_, err = s.conn.Write(payload)
	}
	s.writeMu.Unlock()

	if err != nil {
		_ = s.shutdown()
		return err
	}
	atomic.AddUint64(&s.counters.framesSent, 1)
	if frameType == FrameData {
		atomic.AddUint64(&s.counters.bytesSent, uint64(len(payload)))
	}
	return nil
}

func (s *Session) sendWindowUpdate(streamID uint32, increment uint32) error {
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], increment)
	return s.writeFrame(streamID, FrameWindowUpdate, 0, buf[:])
}

func (s *Session) removeStream(id uint32) {
	s.mu.Lock()
	delete(s.streams, id)
	s.mu.Unlock()
}

func (s *Session) getStream(id uint32) *Stream {
	s.mu.Lock()
	st := s.streams[id]
	s.mu.Unlock()
	return st
}

func (s *Session) recvLoop() {
	defer close(s.doneCh)
	defer func() { _ = s.shutdown() }()

	var hdr [HeaderSize]byte
	for {
		if _, err := io.ReadFull(s.conn, hdr[:]); err != nil {
			return
		}
		streamID := binary.BigEndian.Uint32(hdr[0:4])
		frameType := hdr[4]
		length := binary.BigEndian.Uint32(hdr[6:10])

		maxAllowed := s.cfg.InitialStreamWindow
		if maxAllowed < DefaultMaxFramePayload {
			maxAllowed = DefaultMaxFramePayload
		}
		if length > maxAllowed && length > MaxControlPayload {
			return
		}

		var payload []byte
		if length > 0 {
			payload = make([]byte, length)
			if _, err := io.ReadFull(s.conn, payload); err != nil {
				return
			}
		}

		atomic.AddUint64(&s.counters.framesReceived, 1)
		if frameType == FrameData {
			atomic.AddUint64(&s.counters.bytesReceived, uint64(length))
		}

		if err := s.dispatchFrame(streamID, frameType, payload); err != nil {
			return
		}
	}
}

func (s *Session) dispatchFrame(streamID uint32, frameType uint8, payload []byte) error {
	switch frameType {
	case FramePing:
		if streamID != 0 || len(payload) != 8 {
			return ErrProtocolViolation
		}
		_ = s.writeFrame(0, FramePong, 0, payload)
		return nil

	case FramePong:
		if streamID != 0 || len(payload) != 8 {
			return ErrProtocolViolation
		}
		seq := binary.BigEndian.Uint64(payload)
		s.mu.Lock()
		ch, ok := s.pings[seq]
		if ok {
			delete(s.pings, seq)
		}
		s.mu.Unlock()
		if ok {
			notify(ch)
		}
		return nil

	case FrameGoAway:
		s.mu.Lock()
		s.goingAway = true
		s.mu.Unlock()
		return nil

	case FrameOpen:
		if streamID == 0 || len(payload) < 4 {
			return ErrProtocolViolation
		}
		// Validate parity: client peers send odd IDs; server peers send even IDs.
		peerIsClient := (streamID % 2) == 1
		if peerIsClient == s.isClient {
			return ErrProtocolViolation
		}
		peerWindow := binary.BigEndian.Uint32(payload[0:4])
		if peerWindow == 0 {
			peerWindow = DefaultStreamWindow
		}
		target := string(payload[4:])

		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return nil
		}
		if s.goingAway {
			s.mu.Unlock()
			atomic.AddUint64(&s.counters.streamsRejected, 1)
			_ = s.writeFrame(streamID, FrameReset, 0, []byte("goaway"))
			return nil
		}
		if _, exists := s.streams[streamID]; exists {
			s.mu.Unlock()
			return ErrProtocolViolation
		}
		st := newStream(s, streamID, target, s.cfg.InitialStreamWindow, int64(peerWindow))
		st.established = true
		s.streams[streamID] = st
		s.mu.Unlock()

		select {
		case s.acceptCh <- st:
			var ack [4]byte
			binary.BigEndian.PutUint32(ack[:], s.cfg.InitialStreamWindow)
			if err := s.writeFrame(streamID, FrameOpenAck, 0, ack[:]); err != nil {
				return err
			}
			atomic.AddUint64(&s.counters.streamsAccepted, 1)
		default:
			s.removeStream(streamID)
			atomic.AddUint64(&s.counters.streamsRejected, 1)
			_ = s.writeFrame(streamID, FrameReset, 0, []byte("accept backlog full"))
		}
		return nil

	case FrameOpenAck:
		if streamID == 0 || len(payload) != 4 {
			return ErrProtocolViolation
		}
		st := s.getStream(streamID)
		if st == nil {
			return nil
		}
		peerWindow := binary.BigEndian.Uint32(payload)
		if peerWindow == 0 {
			peerWindow = DefaultStreamWindow
		}
		st.onOpenAck(int64(peerWindow))
		return nil

	case FrameData:
		if streamID == 0 {
			return ErrProtocolViolation
		}
		st := s.getStream(streamID)
		if st == nil {
			return nil
		}
		if err := st.onData(payload); err != nil {
			_ = st.Reset(err.Error())
		}
		return nil

	case FrameWindowUpdate:
		if streamID == 0 || len(payload) != 4 {
			return ErrProtocolViolation
		}
		inc := binary.BigEndian.Uint32(payload)
		if inc == 0 {
			return ErrProtocolViolation
		}
		if st := s.getStream(streamID); st != nil {
			st.onWindowUpdate(int64(inc))
		}
		return nil

	case FrameHalfClose:
		if streamID == 0 {
			return ErrProtocolViolation
		}
		if st := s.getStream(streamID); st != nil {
			st.onRemoteHalfClose()
		}
		return nil

	case FrameReset:
		if streamID == 0 {
			return ErrProtocolViolation
		}
		if st := s.getStream(streamID); st != nil {
			st.onRemoteReset(string(payload))
			s.removeStream(streamID)
		}
		return nil

	default:
		return ErrProtocolViolation
	}
}

// Stream is a full-duplex, flow-controlled logical channel implementing net.Conn.
type Stream struct {
	sess          *Session
	id            uint32
	target        string
	initialWindow uint32

	writeSerializeMu sync.Mutex

	mu              sync.Mutex
	established     bool
	recvBuf         bytes.Buffer
	recvAllowance   int64
	unackedConsumed uint32
	sendWindow      int64
	localFIN        bool
	remoteFIN       bool
	closed          bool
	resetErr        error
	readDeadline    time.Time
	writeDeadline   time.Time

	ackCh     chan error
	readWake  chan struct{}
	writeWake chan struct{}
}

func newStream(sess *Session, id uint32, target string, initialRecvWindow uint32, initialSendWindow int64) *Stream {
	return &Stream{
		sess:          sess,
		id:            id,
		target:        target,
		initialWindow: initialRecvWindow,
		recvAllowance: int64(initialRecvWindow),
		sendWindow:    initialSendWindow,
		ackCh:         make(chan error, 1),
		readWake:      make(chan struct{}, 1),
		writeWake:     make(chan struct{}, 1),
	}
}

// ID returns the stream's numeric identifier.
func (st *Stream) ID() uint32 { return st.id }

// Target returns the stream's service/port metadata string supplied on OpenStream.
func (st *Stream) Target() string { return st.target }

func (st *Stream) onOpenAck(peerWindow int64) {
	st.mu.Lock()
	if !st.established {
		st.established = true
		st.sendWindow += peerWindow
		notify(st.writeWake)
		select {
		case st.ackCh <- nil:
		default:
		}
	}
	st.mu.Unlock()
}

func (st *Stream) onData(payload []byte) error {
	if len(payload) == 0 {
		return nil
	}
	st.mu.Lock()
	if st.remoteFIN {
		st.mu.Unlock()
		return errors.New("data after half-close")
	}
	if int64(len(payload)) > st.recvAllowance {
		st.mu.Unlock()
		return errors.New("receive window exceeded")
	}
	if st.closed || st.resetErr != nil {
		st.mu.Unlock()
		return nil
	}
	st.recvAllowance -= int64(len(payload))
	_, _ = st.recvBuf.Write(payload)
	notify(st.readWake)
	st.mu.Unlock()
	return nil
}

func (st *Stream) onWindowUpdate(inc int64) {
	st.mu.Lock()
	st.sendWindow += inc
	notify(st.writeWake)
	st.mu.Unlock()
}

func (st *Stream) onRemoteHalfClose() {
	st.mu.Lock()
	st.remoteFIN = true
	shouldRemove := st.localFIN
	notify(st.readWake)
	st.mu.Unlock()
	if shouldRemove {
		st.sess.removeStream(st.id)
	}
}

func (st *Stream) onRemoteReset(reason string) {
	err := &StreamResetError{Reason: reason}
	st.mu.Lock()
	if st.resetErr == nil {
		st.resetErr = err
	}
	if !st.established {
		select {
		case st.ackCh <- err:
		default:
		}
	}
	notify(st.readWake)
	notify(st.writeWake)
	st.mu.Unlock()
}

func (st *Stream) abortOnSessionClose() {
	st.mu.Lock()
	if st.resetErr == nil {
		st.resetErr = ErrSessionClosed
	}
	notify(st.readWake)
	notify(st.writeWake)
	st.mu.Unlock()
}

// Read reads from the stream's flow-controlled buffer and sends batched
// FrameWindowUpdate credits once at least half of the window is consumed.
func (st *Stream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		st.mu.Lock()
		if st.recvBuf.Len() > 0 {
			n, _ := st.recvBuf.Read(p)
			st.unackedConsumed += uint32(n)
			var grant uint32
			if !st.remoteFIN && st.resetErr == nil && !st.closed && st.unackedConsumed >= st.initialWindow/2 {
				grant = st.unackedConsumed
				st.unackedConsumed = 0
				st.recvAllowance += int64(grant)
			}
			if st.recvBuf.Len() > 0 || st.remoteFIN || st.resetErr != nil {
				notify(st.readWake)
			}
			st.mu.Unlock()

			if grant > 0 {
				_ = st.sess.sendWindowUpdate(st.id, grant)
			}
			return n, nil
		}
		if st.resetErr != nil {
			err := st.resetErr
			st.mu.Unlock()
			return 0, err
		}
		if st.closed {
			st.mu.Unlock()
			return 0, io.ErrClosedPipe
		}
		if st.remoteFIN {
			st.mu.Unlock()
			return 0, io.EOF
		}
		dl := st.readDeadline
		st.mu.Unlock()

		if err := waitWake(st.readWake, st.sess.closedCh, dl); err != nil {
			return 0, err
		}
	}
}

// Write chunks p into MaxFramePayload frames while respecting the peer's
// advertised credit window.
func (st *Stream) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	st.writeSerializeMu.Lock()
	defer st.writeSerializeMu.Unlock()

	written := 0
	maxChunk := int(st.sess.cfg.MaxFramePayload)

	for written < len(p) {
		var chunk []byte
		for {
			st.mu.Lock()
			if st.resetErr != nil {
				err := st.resetErr
				st.mu.Unlock()
				return written, err
			}
			if st.localFIN || st.closed {
				st.mu.Unlock()
				return written, ErrStreamClosed
			}
			if st.sendWindow > 0 {
				take := len(p) - written
				if take > maxChunk {
					take = maxChunk
				}
				if int64(take) > st.sendWindow {
					take = int(st.sendWindow)
				}
				st.sendWindow -= int64(take)
				if st.sendWindow > 0 {
					notify(st.writeWake)
				}
				chunk = p[written : written+take]
				st.mu.Unlock()
				break
			}
			dl := st.writeDeadline
			st.mu.Unlock()

			atomic.AddUint64(&st.sess.counters.windowStalls, 1)
			if err := waitWake(st.writeWake, st.sess.closedCh, dl); err != nil {
				return written, err
			}
		}

		if err := st.sess.writeFrame(st.id, FrameData, 0, chunk); err != nil {
			return written, err
		}
		written += len(chunk)
	}
	return written, nil
}

// CloseWrite sends a half-close FIN on the stream while leaving Read open.
func (st *Stream) CloseWrite() error {
	st.mu.Lock()
	if st.localFIN {
		st.mu.Unlock()
		return nil
	}
	st.localFIN = true
	shouldRemove := st.remoteFIN
	notify(st.writeWake)
	st.mu.Unlock()

	err := st.sess.writeFrame(st.id, FrameHalfClose, 0, nil)
	if shouldRemove {
		st.sess.removeStream(st.id)
	}
	return err
}

// Reset immediately aborts both directions of the stream and sends FrameReset.
func (st *Stream) Reset(reason string) error {
	if uint32(len(reason)) > MaxControlPayload {
		reason = reason[:MaxControlPayload]
	}
	st.mu.Lock()
	if st.resetErr == nil {
		st.resetErr = &StreamResetError{Reason: reason}
	}
	st.closed = true
	st.localFIN = true
	notify(st.readWake)
	notify(st.writeWake)
	st.mu.Unlock()

	st.sess.removeStream(st.id)
	return st.sess.writeFrame(st.id, FrameReset, 0, []byte(reason))
}

// Close closes both directions of the stream. If unread bytes remain or remote
// has not yet closed, it sends HalfClose (and Reset if unread data was discarded).
func (st *Stream) Close() error {
	st.mu.Lock()
	if st.closed {
		st.mu.Unlock()
		return nil
	}
	st.closed = true
	wasFIN := st.localFIN
	st.localFIN = true
	hadUnread := st.recvBuf.Len() > 0
	st.recvBuf.Reset()
	remoteDone := st.remoteFIN || st.resetErr != nil
	notify(st.readWake)
	notify(st.writeWake)
	st.mu.Unlock()

	st.sess.removeStream(st.id)
	if hadUnread {
		return st.sess.writeFrame(st.id, FrameReset, 0, []byte("closed with unread data"))
	}
	if !wasFIN && !remoteDone {
		return st.sess.writeFrame(st.id, FrameHalfClose, 0, nil)
	}
	return nil
}

// LocalAddr satisfies net.Conn.
func (st *Stream) LocalAddr() net.Addr {
	side := "server"
	if st.sess.isClient {
		side = "client"
	}
	return muxAddr{label: fmt.Sprintf("vsockmux:%s:%d", side, st.id)}
}

// RemoteAddr satisfies net.Conn.
func (st *Stream) RemoteAddr() net.Addr {
	return muxAddr{label: fmt.Sprintf("vsockmux:peer:%d:%s", st.id, st.target)}
}

// SetDeadline sets both read and write deadlines on the stream.
func (st *Stream) SetDeadline(t time.Time) error {
	st.mu.Lock()
	st.readDeadline = t
	st.writeDeadline = t
	notify(st.readWake)
	notify(st.writeWake)
	st.mu.Unlock()
	return nil
}

// SetReadDeadline sets the deadline for blocked and future Read calls.
func (st *Stream) SetReadDeadline(t time.Time) error {
	st.mu.Lock()
	st.readDeadline = t
	notify(st.readWake)
	st.mu.Unlock()
	return nil
}

// SetWriteDeadline sets the deadline for blocked and future Write calls.
func (st *Stream) SetWriteDeadline(t time.Time) error {
	st.mu.Lock()
	st.writeDeadline = t
	notify(st.writeWake)
	st.mu.Unlock()
	return nil
}

type muxAddr struct{ label string }

func (a muxAddr) Network() string { return "vsockmux" }
func (a muxAddr) String() string  { return a.label }

func notify(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func waitWake(wakeCh <-chan struct{}, closedCh <-chan struct{}, deadline time.Time) error {
	if !deadline.IsZero() {
		rem := time.Until(deadline)
		if rem <= 0 {
			return os.ErrDeadlineExceeded
		}
		timer := time.NewTimer(rem)
		defer timer.Stop()
		select {
		case <-wakeCh:
			return nil
		case <-closedCh:
			return ErrSessionClosed
		case <-timer.C:
			return os.ErrDeadlineExceeded
		}
	}
	select {
	case <-wakeCh:
		return nil
	case <-closedCh:
		return ErrSessionClosed
	}
}
