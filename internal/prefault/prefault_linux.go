//go:build linux

package prefault

import (
	"context"
	"os"
	"syscall"
)

// Linux 5.14+ MADV_POPULATE_READ / MADV_POPULATE_WRITE constants.
const (
	madvPopulateRead  = 22
	madvPopulateWrite = 23
)

func platformPopulate(buf []byte, writable bool) bool {
	if len(buf) == 0 {
		return true
	}
	advice := madvPopulateRead
	if writable {
		advice = madvPopulateWrite
	}
	return syscall.Madvise(buf, advice) == nil
}

func allocAnonPool(count, slotBytes int) ([][]byte, func(), error) {
	slots := make([][]byte, 0, count)
	release := func() {
		for _, s := range slots {
			_ = syscall.Munmap(s)
		}
	}
	for i := 0; i < count; i++ {
		b, err := syscall.Mmap(
			-1,
			0,
			slotBytes,
			syscall.PROT_READ|syscall.PROT_WRITE,
			syscall.MAP_PRIVATE|syscall.MAP_ANONYMOUS,
		)
		if err != nil {
			release()
			return nil, nil, err
		}
		slots = append(slots, b)
	}
	return slots, release, nil
}

func readMinorFaults() uint64 {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	if ru.Minflt < 0 {
		return 0
	}
	return uint64(ru.Minflt)
}

// File pre-faults a snapshot or rootfs image file into the host kernel page
// cache in bounded 64 MiB mmap windows without modifying the file.
func File(ctx context.Context, path string, workers int) (PrefaultStats, error) {
	f, err := os.Open(path)
	if err != nil {
		return PrefaultStats{}, err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return PrefaultStats{}, err
	}
	size := fi.Size()
	if size <= 0 {
		return PrefaultStats{Strategy: StrategyTouchRead, Workers: 1}, nil
	}

	pageSize := int64(os.Getpagesize())
	if pageSize <= 0 {
		pageSize = 4096
	}
	const windowBytes = int64(64 * 1024 * 1024)

	var agg PrefaultStats
	agg.Strategy = StrategyTouchRead
	agg.Workers = workers

	for off := int64(0); off < size; off += windowBytes {
		if err := ctx.Err(); err != nil {
			return agg, err
		}
		mapLen := size - off
		if mapLen > windowBytes {
			mapLen = windowBytes
		}
		mapped, err := syscall.Mmap(int(f.Fd()), off, int(mapLen), syscall.PROT_READ, syscall.MAP_SHARED)
		if err != nil {
			return agg, err
		}
		_ = syscall.Madvise(mapped, syscall.MADV_WILLNEED)
		st, pErr := Region(ctx, mapped, StrategyTouchRead, workers)
		_ = syscall.Munmap(mapped)
		if pErr != nil {
			return agg, pErr
		}
		agg.Bytes += st.Bytes
		agg.PagesTouched += st.PagesTouched
		agg.DurationMicro += st.DurationMicro
		agg.Workers = st.Workers
	}

	if agg.DurationMicro > 0 {
		secs := float64(agg.DurationMicro) / 1e6
		agg.ThroughputMBs = float64(int(((float64(agg.Bytes)/(1024*1024))/secs)*100)) / 100
	}
	return agg, nil
}
