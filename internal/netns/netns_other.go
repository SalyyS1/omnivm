//go:build !linux

package netns

import "fmt"

func isAvailable() bool { return false }

func applyVethPair(cfg Config, vp *VethPair) error {
	return fmt.Errorf("netns: veth pair setup is only supported on Linux")
}

func teardownVethPair(vp *VethPair) error {
	return fmt.Errorf("netns: veth pair teardown is only supported on Linux")
}
