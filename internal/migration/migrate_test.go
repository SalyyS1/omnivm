package migration

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"
)

// stubDriver implements MigrateDriver for unit tests.
// It tracks page-send calls and simulates a converging dirty-set by halving
// dirty pages each round.
type stubDriver struct {
	mu sync.Mutex

	ramBytes   uint64
	dirtyPages int // current dirty page count (halved each call to DirtyPages)
	minDirty   int // floor before convergence

	pagesSentTotal  uint64
	bytesSentTotal  uint64
	pauseCalled     bool
	handoffCalled   bool
	handoffStartDst bool
}

func newStubDriver(ramMB int, initialDirty int) *stubDriver {
	return &stubDriver{
		ramBytes:   uint64(ramMB) * 1024 * 1024,
		dirtyPages: initialDirty,
		minDirty:   initialDirty / 32,
	}
}

func (d *stubDriver) RAMBytes(id string) (uint64, error) {
	return d.ramBytes, nil
}

func (d *stubDriver) DirtyPages(id string, dst *DirtyBitmap) (int, error) {
	d.mu.Lock()
	dirty := d.dirtyPages
	// Simulate convergence: halve dirty count each round (down to minDirty).
	if d.dirtyPages > d.minDirty {
		d.dirtyPages = d.dirtyPages / 2
		if d.dirtyPages < d.minDirty {
			d.dirtyPages = d.minDirty
		}
	}
	d.mu.Unlock()

	// Mark the pages dirty in the bitmap.
	for i := 0; i < dirty && i < dst.total; i++ {
		dst.MarkRange(i, 1)
	}
	return dirty, nil
}

func (d *stubDriver) SendPages(ctx context.Context, id string, pageNums []int, dst io.Writer) (uint64, error) {
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	// Simulate sending PageSize bytes per page.
	data := make([]byte, len(pageNums)*PageSize)
	n, err := dst.Write(data)
	if err != nil {
		return 0, err
	}
	d.mu.Lock()
	d.pagesSentTotal += uint64(len(pageNums))
	d.bytesSentTotal += uint64(n)
	d.mu.Unlock()
	return uint64(n), nil
}

func (d *stubDriver) ReceivePage(ctx context.Context, id string, pageNum int, data []byte) error {
	return nil
}

func (d *stubDriver) PauseForHandoff(id string) error {
	d.mu.Lock()
	d.pauseCalled = true
	d.mu.Unlock()
	return nil
}

func (d *stubDriver) ResumeOrHandoff(id string, startAtDest bool) error {
	d.mu.Lock()
	d.handoffCalled = true
	d.handoffStartDst = startAtDest
	d.mu.Unlock()
	return nil
}

// Tests

func TestMigrate_PreCopyConvergence(t *testing.T) {
	// 128 MB RAM, 4096 initial dirty pages -> converges in ~log2(4096/256)=4 rounds.
	driver := newStubDriver(128, 4096)
	m := New(driver)

	req := MigrateRequest{
		SourceInstanceID:    "inst-abc",
		DestAddr:            "/tmp/test-dest.sock",
		DirtyThresholdPages: 256,
		MaxRounds:           8,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := m.Migrate(ctx, req)
	if err != nil {
		t.Fatalf("Migrate returned error: %v", err)
	}

	if res.Rounds == 0 {
		t.Error("expected at least one pre-copy round")
	}
	if res.Rounds > req.MaxRounds+1 {
		t.Errorf("rounds %d exceeded MaxRounds %d", res.Rounds, req.MaxRounds)
	}
	if res.PagesSent == 0 {
		t.Error("expected pages to be sent")
	}
	if res.BytesSent == 0 {
		t.Error("expected bytes to be sent")
	}
	if !driver.pauseCalled {
		t.Error("PauseForHandoff was not called")
	}
	if !driver.handoffCalled {
		t.Error("ResumeOrHandoff was not called")
	}
	if !driver.handoffStartDst {
		t.Error("expected startAtDest=true for successful migration")
	}
	if res.TotalMs < 0 {
		t.Error("TotalMs should be non-negative")
	}
	t.Logf("migration: rounds=%d pages=%d bytes=%d stop_ms=%d total_ms=%d",
		res.Rounds, res.PagesSent, res.BytesSent, res.StopAndCopyMs, res.TotalMs)
}

func TestMigrate_PostCopyFaults(t *testing.T) {
	driver := newStubDriver(64, 2048)
	m := New(driver)

	req := MigrateRequest{
		SourceInstanceID:    "inst-postcopy",
		DestAddr:            "/tmp/test-dest-postcopy.sock",
		PostCopy:            true,
		DirtyThresholdPages: 128,
		MaxRounds:           4,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := m.Migrate(ctx, req)
	if err != nil {
		t.Fatalf("Migrate (post-copy) returned error: %v", err)
	}
	if res.PostCopyFaults == 0 {
		t.Error("expected post-copy faults to be serviced")
	}
	t.Logf("post-copy faults serviced: %d", res.PostCopyFaults)
}

func TestMigrate_NilDriver_ReturnsNotSupported(t *testing.T) {
	m := New(nil)
	_, err := m.Migrate(context.Background(), MigrateRequest{SourceInstanceID: "x"})
	if err == nil {
		t.Fatal("expected error for nil driver")
	}
	if err.Error() != ErrNotSupported.Error() {
		t.Errorf("expected ErrNotSupported, got: %v", err)
	}
}

func TestMigrate_ContextCancellation(t *testing.T) {
	// Very high dirty pages to trigger many rounds; cancel context early.
	driver := newStubDriver(256, 65536)
	m := New(driver)

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel immediately.
	cancel()

	req := MigrateRequest{
		SourceInstanceID:    "inst-cancel",
		DestAddr:            "/tmp/test-cancel.sock",
		DirtyThresholdPages: 1,
		MaxRounds:           100,
	}

	_, err := m.Migrate(ctx, req)
	if err == nil {
		t.Fatal("expected error on cancelled context")
	}
}

func TestDirtyBitmap_MarkAndCollect(t *testing.T) {
	bm := NewDirtyBitmap(64 * 1024) // 16 pages

	bm.MarkRange(0, 3)
	bm.MarkRange(10, 2)

	if got := bm.CountDirty(); got != 5 {
		t.Errorf("CountDirty: got %d, want 5", got)
	}

	pages := bm.CollectDirtyPages()
	if len(pages) != 5 {
		t.Fatalf("CollectDirtyPages: got %d pages, want 5", len(pages))
	}
	// After collect the bitmap should be clear.
	if got := bm.CountDirty(); got != 0 {
		t.Errorf("CountDirty after collect: got %d, want 0", got)
	}
}

func TestDirtyBitmap_ClearAll(t *testing.T) {
	bm := NewDirtyBitmap(1024 * 1024) // 256 pages
	bm.MarkRange(0, 100)
	if got := bm.ClearAll(); got != 100 {
		t.Errorf("ClearAll: got %d dirty, want 100", got)
	}
	if got := bm.CountDirty(); got != 0 {
		t.Errorf("CountDirty after ClearAll: got %d, want 0", got)
	}
}

func TestMigrate_MaxRoundsExceeded_StillHandsOff(t *testing.T) {
	// Dirty pages never converge below threshold within MaxRounds.
	driver := &stubDriver{
		ramBytes:   32 * 1024 * 1024,
		dirtyPages: 10000,
		minDirty:   9000, // stays above threshold=256
	}
	m := New(driver)

	req := MigrateRequest{
		SourceInstanceID:    "inst-noconverge",
		DestAddr:            "/tmp/test-noconverge.sock",
		DirtyThresholdPages: 256,
		MaxRounds:           3,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := m.Migrate(ctx, req)
	if err != nil {
		t.Fatalf("Migrate (no-converge) returned error: %v", err)
	}
	// Even without convergence, handoff must complete.
	if !driver.handoffCalled {
		t.Error("ResumeOrHandoff must be called even when MaxRounds exceeded")
	}
	if !driver.handoffStartDst {
		t.Error("startAtDest must be true for successful migration")
	}
	if res.Rounds == 0 {
		t.Error("expected rounds > 0")
	}
	t.Logf("no-converge migration: rounds=%d finalDirty=%d", res.Rounds, res.FinalDirtyPages)
}
