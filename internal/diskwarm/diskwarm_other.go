//go:build !linux

package diskwarm

import "os"

func platformScanExtents(_ *os.File, _ int64, _ uint64) ([]DataExtent, uint64, bool) {
	return nil, 0, false
}

func platformAdviseWillNeed(_ *os.File, _ int64, _ int64) bool {
	return false
}

func platformDropPageCache(f *os.File, _ int64) {
	_ = f.Sync()
}
