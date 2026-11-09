package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// Config holds ovmd runtime configuration.
type Config struct {
	// DataDir is the base directory for ovm runtime data.
	DataDir string

	// BinDir is the directory containing ovm binaries.
	BinDir string

	// SocketPath is the unix socket path for the ovmd API.
	SocketPath string

	// DefaultMemoryMB is the default VM memory in megabytes.
	DefaultMemoryMB int

	// DefaultVCPUs is the default number of virtual CPUs.
	DefaultVCPUs int

	// RouterAddr is the address for the HTTP router (serve mode).
	RouterAddr string

	// DBPath is the path to the SQLite database.
	DBPath string

	// ImageCacheDir is the directory for cached OCI image rootfs directories.
	ImageCacheDir string

	// OverlaysDir is the directory for instance rootfs overlays.
	OverlaysDir string

	// WorkspacesDir is the directory for workspace volumes.
	WorkspacesDir string

	// LogsDir is the directory for per-instance log files.
	LogsDir string

	// MasterKeyPath is the path to the AES-256 master key for secret encryption.
	MasterKeyPath string

	// PauseAfterIdle is the duration after which an idle instance is paused (SIGSTOP).
	PauseAfterIdle time.Duration

	// StopAfterIdle is the duration after which a paused instance is stopped.
	StopAfterIdle time.Duration

	// NetworkBackend selects the data-plane networking mode.
	// Only "tap" (tap device + NAT) is supported; "auto" resolves to it.
	NetworkBackend string

	// KernelPath is the path to the vmlinux kernel image.
	KernelPath string

	// CloudHypervisorBin is the path to the cloud-hypervisor binary.
	// Empty means search PATH.
	CloudHypervisorBin string

	// VirtiofsdBin is the path to the virtiofsd binary.
	// Empty means search PATH.
	VirtiofsdBin string
}

// DefaultConfig returns the default configuration.
func DefaultConfig() *Config {
	homeDir, _ := os.UserHomeDir()
	ovmDir := filepath.Join(homeDir, ".omnivm")
	execDir := executableDir()

	// Kernel path: prefer user-local, fall back to system package path
	kernelPath := filepath.Join(ovmDir, "kernel", "vmlinux")
	if _, err := os.Stat(kernelPath); err != nil {
		sysKernel := "/usr/share/omnivm/kernel/vmlinux"
		if _, err := os.Stat(sysKernel); err == nil {
			kernelPath = sysKernel
		}
	}

	return &Config{
		DataDir:         filepath.Join(ovmDir, "data"),
		BinDir:          execDir,
		SocketPath:      filepath.Join(ovmDir, "ovmd.sock"),
		DefaultMemoryMB: 512,
		DefaultVCPUs:    1,
		RouterAddr:      "127.0.0.1:8099",
		DBPath:          filepath.Join(ovmDir, "data", "ovm.db"),
		ImageCacheDir:   filepath.Join(ovmDir, "data", "images"),
		OverlaysDir:     filepath.Join(ovmDir, "data", "overlays"),
		WorkspacesDir:   filepath.Join(ovmDir, "data", "workspaces"),
		LogsDir:         filepath.Join(ovmDir, "data", "logs"),
		MasterKeyPath:   filepath.Join(ovmDir, "master.key"),
		PauseAfterIdle:  60 * time.Second,
		StopAfterIdle:   5 * time.Minute,
		NetworkBackend:  "auto",
		KernelPath:      kernelPath,
	}
}

// EnsureDirs creates all required directories.
func (c *Config) EnsureDirs() error {
	dirs := []string{
		c.DataDir,
		filepath.Join(c.DataDir, "sockets"),
		filepath.Dir(c.SocketPath),
		c.ImageCacheDir,
		c.OverlaysDir,
		c.WorkspacesDir,
		c.LogsDir,
		filepath.Dir(c.KernelPath),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0700); err != nil {
			return err
		}
	}
	return nil
}

// ResolveNetworkBackend resolves "auto" (or unset) to the only supported
// backend: tap + NAT for Cloud Hypervisor.
func (c *Config) ResolveNetworkBackend() {
	c.NetworkBackend = "tap"
}

// ResolveBinaries eagerly resolves CloudHypervisorBin and VirtiofsdBin
// if they are empty. Called once at startup so the backend and doctor
// share the same discovery result.
func (c *Config) ResolveBinaries() {
	if c.CloudHypervisorBin == "" {
		c.CloudHypervisorBin = FindBinary("cloud-hypervisor", c.BinDir)
	}
	if c.VirtiofsdBin == "" {
		c.VirtiofsdBin = FindBinary("virtiofsd", c.BinDir)
	}
}

// FindBinary locates a companion binary by name. Search order:
//  1. BinDir — directory of the running executable (dev builds)
//  2. PATH (exec.LookPath)
//  3. Known system paths (deb/rpm installs)
//
// BinDir is checked first so dev builds (./bin/ovmd) always find freshly
// built siblings (./bin/ovm-harness) instead of stale system-installed copies.
//
// Returns the absolute path, or "" if not found.
func FindBinary(name string, binDir string) string {
	// 1. Sibling of the running executable
	if binDir != "" {
		p := filepath.Join(binDir, name)
		if _, err := os.Stat(p); err == nil {
			abs, _ := filepath.Abs(p)
			return abs
		}
	}

	// 2. PATH
	if p, err := exec.LookPath(name); err == nil {
		return p
	}

	// 3. Known system paths
	for _, dir := range []string{"/usr/lib/omnivm", "/usr/libexec", "/usr/local/bin"} {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}

	return ""
}

// executableDir returns the directory containing the current executable.
func executableDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(exe)
}
