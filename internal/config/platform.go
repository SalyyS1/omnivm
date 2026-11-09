package config

import (
	"fmt"
	"runtime"
)

// Platform describes the detected host platform.
type Platform struct {
	OS   string // always "linux"
	Arch string // "arm64" or "amd64"

	// VMM backend to use
	Backend string // "cloud-hypervisor"

	// NeedsRoot indicates the backend requires root/sudo at startup.
	NeedsRoot bool
}

// DetectPlatform detects the host platform and selects the VMM backend.
func DetectPlatform() (*Platform, error) {
	p := &Platform{
		OS:   runtime.GOOS,
		Arch: runtime.GOARCH,
	}

	switch {
	case p.OS == "linux":
		p.Backend = "cloud-hypervisor"
		p.NeedsRoot = true // tap + iptables networking
	default:
		return nil, fmt.Errorf(
			"unsupported platform: %s/%s. Aegis requires Linux with KVM (Cloud Hypervisor)",
			p.OS, p.Arch,
		)
	}

	return p, nil
}
