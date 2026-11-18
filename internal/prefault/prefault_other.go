//go:build !linux

package prefault

import (
	"context"
	"io"
	"os"
	"time"
)

func platformPopulate(_ []byte, _ bool) bool {
	return false
}

func allocAnonPool(count, slotBytes int) ([][]byte, func(), error) {
	slots := make([][]byte, count)
	for i := 0; i < count; i++ {
		slots[i] = make([]byte, slotBytes)
	}
	return slots, func() {}, nil
}

func readMinorFaults() uint64 {
	return 0
}

// File reads path sequentially on non-Linux hosts to warm the OS file cache.
func File(ctx context.Context, path string, workers int) (PrefaultStats, error) {
	f, err := os.Open(path)
	if err != nil {
		return PrefaultStats{}, err
	}
	defer f.Close()

	start := time.Now()
	pageSize := os.Getpagesize()
	if pageSize <= 0 {
		pageSize = 4096
	}
	buf := make([]byte, 64*1024)
	var total uint64
	for {
		if err := ctx.Err(); err != nil {
			return PrefaultStats{}, err
		}
		n, rErr := f.Read(buf)
		total += uint64(n)
		if rErr == io.EOF {
			break
		}
		if rErr != nil {
			return PrefaultStats{}, rErr
		}
	}
	dur := time.Since(start)
	pages := (total + uint64(pageSize) - 1) / uint64(pageSize)
	return PrefaultStats{
		Strategy:      StrategyTouchRead,
		Bytes:         total,
		PagesTouched:  pages,
		Workers:       1,
		DurationMicro: dur.Microseconds(),
	}, nil
}
