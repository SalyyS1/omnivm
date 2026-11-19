// Package vmmjail implements unprivileged Cloud Hypervisor sandboxing (UID/GID
// subordinate pool isolation, chroot layout, capability bounding-set zeroing,
// RLIMIT ceilings, FD handoff) and daemonless microVM survival leases with
// PID start-time tick verification across ovmd restarts.
package vmmjail

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	ModeOff    = "off"
	ModeAudit  = "audit"
	ModeStrict = "strict"

	DefaultUIDBase      uint32 = 100000
	DefaultPoolSize     uint32 = 65536
	DefaultOverheadMB   int    = 256
	DefaultNoFileLimit  uint64 = 4096
	DefaultNProcLimit   uint64 = 64
	LeaseSchemaVersion  int    = 1
)

// Config configures the unprivileged VMM jail manager and daemonless lease store.
type Config struct {
	Mode       string `json:"mode"`
	RootDir    string `json:"root_dir"`
	LeaseDir   string `json:"lease_dir"`
	UIDBase    uint32 `json:"uid_base"`
	PoolSize   uint32 `json:"pool_size"`
	OverheadMB int    `json:"overhead_mb"`
}

// BindMount describes a host-to-jail filesystem mount entry.
type BindMount struct {
	Source   string `json:"source"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only"`
}

// JailSpec defines the complete OS isolation envelope for a single microVM VMM process.
type JailSpec struct {
	InstanceID        string      `json:"instance_id"`
	TenantID          string      `json:"tenant_id,omitempty"`
	Mode              string      `json:"mode"`
	UID               uint32      `json:"uid"`
	GID               uint32      `json:"gid"`
	ChrootDir         string      `json:"chroot_dir"`
	APISocketRel      string      `json:"api_socket_rel"`
	VsockSocketRel    string      `json:"vsock_socket_rel"`
	NoNewPrivs        bool        `json:"no_new_privs"`
	ClearBoundingCaps bool        `json:"clear_bounding_caps"`
	RetainedCaps      []string    `json:"retained_caps,omitempty"`
	KVMFDHandoff      bool        `json:"kvm_fd_handoff"`
	TAPFDHandoff      bool        `json:"tap_fd_handoff"`
	SeccompProfile    string      `json:"seccomp_profile"`
	NetNamespace      string      `json:"net_namespace"`
	CgroupScope       string      `json:"cgroup_scope"`
	MemoryMB          int         `json:"memory_mb"`
	VCPUs             int         `json:"vcpus"`
	RlimitASBytes     uint64      `json:"rlimit_as_bytes"`
	RlimitNoFile      uint64      `json:"rlimit_nofile"`
	RlimitNProc       uint64      `json:"rlimit_nproc"`
	BindMounts        []BindMount `json:"bind_mounts,omitempty"`
}

// SurvivalLease persists live hypervisor attachment coordinates across ovmd upgrades.
type SurvivalLease struct {
	SchemaVersion  int       `json:"schema_version"`
	InstanceID     string    `json:"instance_id"`
	TenantID       string    `json:"tenant_id,omitempty"`
	PID            int       `json:"pid"`
	ProcStartTicks uint64    `json:"proc_start_ticks"`
	UID            uint32    `json:"uid"`
	GID            uint32    `json:"gid"`
	ChrootDir      string    `json:"chroot_dir"`
	APISocket      string    `json:"api_socket"`
	VsockSocket    string    `json:"vsock_socket"`
	GuestCID       uint32    `json:"guest_cid"`
	MemoryMB       int       `json:"memory_mb"`
	VCPUs          int       `json:"vcpus"`
	LastState      string    `json:"last_state"`
	IssuedAt       time.Time `json:"issued_at"`
	Digest         string    `json:"digest"`
}

// ReconcileOutcome classifies one persisted survival lease during daemon startup recovery.
type ReconcileOutcome struct {
	InstanceID string `json:"instance_id"`
	PID        int    `json:"pid"`
	Action     string `json:"action"` // reattached | stale_pid_reused | orphaned_cleaned | tampered_rejected
	Reason     string `json:"reason"`
}

// ReconcileReport summarizes a daemonless VM survival pass across all persisted leases.
type ReconcileReport struct {
	CheckedLeases    int                `json:"checked_leases"`
	Reattached       int                `json:"reattached"`
	StalePIDReused   int                `json:"stale_pid_reused"`
	OrphanedCleaned  int                `json:"orphaned_cleaned"`
	TamperedRejected int                `json:"tampered_rejected"`
	Outcomes         []ReconcileOutcome `json:"outcomes"`
}

// Metrics exposes atomic telemetry for VMM jail allocation and daemonless survival.
type Metrics struct {
	Mode              string `json:"mode"`
	ActiveJails       int    `json:"active_jails"`
	ActiveLeases      int    `json:"active_leases"`
	JailsPlanned      uint64 `json:"jails_planned"`
	LeasesPersisted   uint64 `json:"leases_persisted"`
	ReattachedVMs     uint64 `json:"reattached_vms"`
	StalePIDPrevented uint64 `json:"stale_pid_prevented"`
	OrphanedCleaned   uint64 `json:"orphaned_cleaned"`
	AuditViolations   uint64 `json:"audit_violations"`
}

// StatusSnapshot reports current jail manager configuration, active slots, and telemetry.
type StatusSnapshot struct {
	Mode       string     `json:"mode"`
	RootDir    string     `json:"root_dir"`
	LeaseDir   string     `json:"lease_dir"`
	UIDBase    uint32     `json:"uid_base"`
	PoolSize   uint32     `json:"pool_size"`
	Metrics    Metrics    `json:"metrics"`
	ActiveSpec []JailSpec `json:"active_jails,omitempty"`
}

// ProcInspector queries whether a PID is alive and returns its /proc/<pid>/stat field 22 start ticks.
type ProcInspector func(pid int) (alive bool, startTicks uint64)

// SocketProber checks whether a Cloud Hypervisor Unix API socket responds to connections.
type SocketProber func(sockPath string) bool

// Manager coordinates subordinate UID/GID allocation, jail specs, and daemonless survival leases.
type Manager struct {
	mu          sync.Mutex
	mode        string
	rootDir     string
	leaseDir    string
	uidBase     uint32
	poolSize    uint32
	overheadMB  int
	nextCursor  uint32
	byInstance  map[string]JailSpec
	usedSlots   map[uint32]string
	procInspect ProcInspector
	sockProbe   SocketProber

	jailsPlanned      uint64
	leasesPersisted   uint64
	reattachedVMs     uint64
	stalePIDPrevented uint64
	orphanedCleaned   uint64
	auditViolations   uint64
}

// NormalizeMode validates and normalizes a jail mode string.
func NormalizeMode(raw string) (string, error) {
	m := strings.ToLower(strings.TrimSpace(raw))
	switch m {
	case "", ModeOff:
		return ModeOff, nil
	case ModeAudit:
		return ModeAudit, nil
	case ModeStrict:
		return ModeStrict, nil
	default:
		return "", fmt.Errorf("unsupported jail mode %q: expected off, audit, or strict", raw)
	}
}

// NewManager constructs a VMM jail and survival lease manager.
func NewManager(cfg Config) *Manager {
	mode, err := NormalizeMode(cfg.Mode)
	if err != nil {
		mode = ModeOff
	}
	rootDir := strings.TrimSpace(cfg.RootDir)
	if rootDir == "" {
		rootDir = "/var/run/omnivm/jails"
	}
	leaseDir := strings.TrimSpace(cfg.LeaseDir)
	if leaseDir == "" {
		leaseDir = filepath.Join(rootDir, "leases")
	}
	uidBase := cfg.UIDBase
	if uidBase < 65536 {
		uidBase = DefaultUIDBase
	}
	poolSize := cfg.PoolSize
	if poolSize == 0 {
		poolSize = DefaultPoolSize
	}
	overhead := cfg.OverheadMB
	if overhead <= 0 {
		overhead = DefaultOverheadMB
	}
	return &Manager{
		mode:        mode,
		rootDir:     rootDir,
		leaseDir:    leaseDir,
		uidBase:     uidBase,
		poolSize:    poolSize,
		overheadMB:  overhead,
		byInstance:  make(map[string]JailSpec),
		usedSlots:   make(map[uint32]string),
		procInspect: DefaultProcInspector,
		sockProbe:   DefaultSocketProber,
	}
}

// SetProbes overrides OS process and Unix socket probes (used by unit tests).
func (m *Manager) SetProbes(procFn ProcInspector, sockFn SocketProber) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if procFn != nil {
		m.procInspect = procFn
	}
	if sockFn != nil {
		m.sockProbe = sockFn
	}
}

// Mode returns the current operating mode.
func (m *Manager) Mode() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.mode
}

// SetMode updates the operating mode dynamically.
func (m *Manager) SetMode(raw string) error {
	norm, err := NormalizeMode(raw)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.mode = norm
	m.mu.Unlock()
	return nil
}

func sanitizeID(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "default"
	}
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		return "inst"
	}
	return out
}

// PlanJail allocates a dedicated subordinate UID/GID and constructs a hardened JailSpec.
func (m *Manager) PlanJail(instanceID, tenantID string, memoryMB, vcpus int, rootfsPath string, dryRun bool) (JailSpec, error) {
	id := strings.TrimSpace(instanceID)
	if id == "" {
		return JailSpec{}, errors.New("instance_id must not be empty")
	}
	if memoryMB <= 0 {
		memoryMB = 512
	}
	if vcpus <= 0 {
		vcpus = 1
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if existing, ok := m.byInstance[id]; ok {
		return existing, nil
	}

	var chosenUID uint32
	found := false
	for i := uint32(0); i < m.poolSize; i++ {
		slot := (m.nextCursor + i) % m.poolSize
		uid := m.uidBase + slot
		if _, busy := m.usedSlots[uid]; !busy {
			chosenUID = uid
			if !dryRun {
				m.nextCursor = (slot + 1) % m.poolSize
			}
			found = true
			break
		}
	}
	if !found {
		return JailSpec{}, fmt.Errorf("subordinate jail UID pool exhausted (%d active slots)", len(m.usedSlots))
	}

	safeID := sanitizeID(id)
	chrootDir := filepath.Join(m.rootDir, safeID, "root")
	rlimitAS := uint64(memoryMB+m.overheadMB) * 1024 * 1024
	nprocs := DefaultNProcLimit + uint64(vcpus*8)

	var mounts []BindMount
	if strings.TrimSpace(rootfsPath) != "" {
		mounts = append(mounts, BindMount{
			Source:   filepath.Clean(rootfsPath),
			Target:   "/rootfs.ext4",
			ReadOnly: false,
		})
	}

	mode := m.mode
	if mode == ModeOff && dryRun {
		mode = ModeStrict
	}

	spec := JailSpec{
		InstanceID:        id,
		TenantID:          strings.TrimSpace(tenantID),
		Mode:              mode,
		UID:               chosenUID,
		GID:               chosenUID,
		ChrootDir:         chrootDir,
		APISocketRel:      "/run/ch-api.sock",
		VsockSocketRel:    "/run/ch-vsock.sock",
		NoNewPrivs:        true,
		ClearBoundingCaps: true,
		RetainedCaps:      []string{},
		KVMFDHandoff:      true,
		TAPFDHandoff:      true,
		SeccompProfile:    "builtin:cloud-hypervisor-vmm",
		NetNamespace:      "ovm-" + safeID,
		CgroupScope:       "ovm.slice/ovm-" + safeID + ".scope",
		MemoryMB:          memoryMB,
		VCPUs:             vcpus,
		RlimitASBytes:     rlimitAS,
		RlimitNoFile:      DefaultNoFileLimit,
		RlimitNProc:       nprocs,
		BindMounts:        mounts,
	}

	if !dryRun {
		m.byInstance[id] = spec
		m.usedSlots[chosenUID] = id
		atomic.AddUint64(&m.jailsPlanned, 1)
	}
	return spec, nil
}

// ReleaseJail frees the subordinate UID/GID slot reserved for an instance.
func (m *Manager) ReleaseJail(instanceID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	spec, ok := m.byInstance[instanceID]
	if !ok {
		return false
	}
	delete(m.byInstance, instanceID)
	delete(m.usedSlots, spec.UID)
	return true
}

// ComputeLeaseDigest computes the canonical SHA-256 integrity digest over a SurvivalLease.
func ComputeLeaseDigest(l SurvivalLease) string {
	payload := fmt.Sprintf("v=%d|id=%s|tenant=%s|pid=%d|ticks=%d|uid=%d|gid=%d|chroot=%s|api=%s|vsock=%s|cid=%d|mem=%d|vcpus=%d",
		l.SchemaVersion, l.InstanceID, l.TenantID, l.PID, l.ProcStartTicks,
		l.UID, l.GID, l.ChrootDir, l.APISocket, l.VsockSocket, l.GuestCID, l.MemoryMB, l.VCPUs)
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}

func (m *Manager) leasePathLocked(instanceID string) string {
	return filepath.Join(m.leaseDir, sanitizeID(instanceID)+".lease.json")
}

// PersistLease writes an atomic, digest-protected survival lease to disk.
func (m *Manager) PersistLease(l SurvivalLease) (SurvivalLease, error) {
	if strings.TrimSpace(l.InstanceID) == "" {
		return SurvivalLease{}, errors.New("instance_id is required")
	}
	if l.PID <= 0 {
		return SurvivalLease{}, errors.New("pid must be positive")
	}
	l.SchemaVersion = LeaseSchemaVersion
	if l.IssuedAt.IsZero() {
		l.IssuedAt = time.Now().UTC()
	}
	if l.ProcStartTicks == 0 {
		m.mu.Lock()
		inspect := m.procInspect
		m.mu.Unlock()
		if alive, ticks := inspect(l.PID); alive {
			l.ProcStartTicks = ticks
		}
	}
	l.Digest = ComputeLeaseDigest(l)

	m.mu.Lock()
	leaseDir := m.leaseDir
	dstPath := m.leasePathLocked(l.InstanceID)
	m.mu.Unlock()

	if err := os.MkdirAll(leaseDir, 0700); err != nil {
		return SurvivalLease{}, fmt.Errorf("mkdir lease dir: %w", err)
	}
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return SurvivalLease{}, err
	}
	tmpPath := dstPath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0600); err != nil {
		return SurvivalLease{}, err
	}
	if err := os.Rename(tmpPath, dstPath); err != nil {
		_ = os.Remove(tmpPath)
		return SurvivalLease{}, err
	}
	atomic.AddUint64(&m.leasesPersisted, 1)
	return l, nil
}

// RemoveLease deletes a persisted survival lease file if present.
func (m *Manager) RemoveLease(instanceID string) error {
	m.mu.Lock()
	p := m.leasePathLocked(instanceID)
	m.mu.Unlock()
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// ReconcileLeases inspects all persisted survival leases on ovmd startup, re-adopting
// live hypervisors whose PID and /proc/<pid>/stat start ticks match while safely sweeping
// dead sockets and refusing recycled PIDs.
func (m *Manager) ReconcileLeases() (ReconcileReport, error) {
	m.mu.Lock()
	leaseDir := m.leaseDir
	procFn := m.procInspect
	sockFn := m.sockProbe
	m.mu.Unlock()

	entries, err := os.ReadDir(leaseDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ReconcileReport{Outcomes: []ReconcileOutcome{}}, nil
		}
		return ReconcileReport{}, err
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	rep := ReconcileReport{Outcomes: make([]ReconcileOutcome, 0, len(entries))}
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".lease.json") {
			continue
		}
		rep.CheckedLeases++
		fullPath := filepath.Join(leaseDir, ent.Name())
		raw, err := os.ReadFile(fullPath)
		if err != nil {
			continue
		}
		var lease SurvivalLease
		if err := json.Unmarshal(raw, &lease); err != nil {
			_ = os.Remove(fullPath)
			rep.TamperedRejected++
			rep.Outcomes = append(rep.Outcomes, ReconcileOutcome{
				InstanceID: strings.TrimSuffix(ent.Name(), ".lease.json"),
				Action:     "tampered_rejected",
				Reason:     "malformed survival lease JSON",
			})
			continue
		}
		if ComputeLeaseDigest(lease) != lease.Digest {
			_ = os.Remove(fullPath)
			rep.TamperedRejected++
			rep.Outcomes = append(rep.Outcomes, ReconcileOutcome{
				InstanceID: lease.InstanceID,
				PID:        lease.PID,
				Action:     "tampered_rejected",
				Reason:     "SHA-256 lease digest mismatch",
			})
			continue
		}

		alive, liveTicks := procFn(lease.PID)
		if !alive {
			_ = os.Remove(fullPath)
			if lease.APISocket != "" {
				_ = os.Remove(lease.APISocket)
			}
			rep.OrphanedCleaned++
			atomic.AddUint64(&m.orphanedCleaned, 1)
			rep.Outcomes = append(rep.Outcomes, ReconcileOutcome{
				InstanceID: lease.InstanceID,
				PID:        lease.PID,
				Action:     "orphaned_cleaned",
				Reason:     "VMM process exited while ovmd was offline",
			})
			continue
		}

		if lease.ProcStartTicks > 0 && liveTicks > 0 && lease.ProcStartTicks != liveTicks {
			_ = os.Remove(fullPath)
			rep.StalePIDReused++
			atomic.AddUint64(&m.stalePIDPrevented, 1)
			rep.Outcomes = append(rep.Outcomes, ReconcileOutcome{
				InstanceID: lease.InstanceID,
				PID:        lease.PID,
				Action:     "stale_pid_reused",
				Reason:     fmt.Sprintf("PID %d start ticks changed (%d -> %d); prevented signalling recycled host process", lease.PID, lease.ProcStartTicks, liveTicks),
			})
			continue
		}

		if lease.APISocket != "" && !sockFn(lease.APISocket) {
			_ = os.Remove(fullPath)
			rep.OrphanedCleaned++
			atomic.AddUint64(&m.orphanedCleaned, 1)
			rep.Outcomes = append(rep.Outcomes, ReconcileOutcome{
				InstanceID: lease.InstanceID,
				PID:        lease.PID,
				Action:     "orphaned_cleaned",
				Reason:     "VMM API socket unresponsive",
			})
			continue
		}

		m.mu.Lock()
		if lease.UID >= m.uidBase && lease.UID < m.uidBase+m.poolSize {
			m.usedSlots[lease.UID] = lease.InstanceID
			m.byInstance[lease.InstanceID] = JailSpec{
				InstanceID:        lease.InstanceID,
				TenantID:          lease.TenantID,
				Mode:              m.mode,
				UID:               lease.UID,
				GID:               lease.GID,
				ChrootDir:         lease.ChrootDir,
				NoNewPrivs:        true,
				ClearBoundingCaps: true,
				KVMFDHandoff:      true,
				TAPFDHandoff:      true,
				SeccompProfile:    "builtin:cloud-hypervisor-vmm",
				MemoryMB:          lease.MemoryMB,
				VCPUs:             lease.VCPUs,
				RlimitASBytes:     uint64(lease.MemoryMB+m.overheadMB) * 1024 * 1024,
				RlimitNoFile:      DefaultNoFileLimit,
				RlimitNProc:       DefaultNProcLimit + uint64(lease.VCPUs*8),
			}
		}
		m.mu.Unlock()

		rep.Reattached++
		atomic.AddUint64(&m.reattachedVMs, 1)
		rep.Outcomes = append(rep.Outcomes, ReconcileOutcome{
			InstanceID: lease.InstanceID,
			PID:        lease.PID,
			Action:     "reattached",
			Reason:     "verified PID start ticks and live VMM control socket",
		})
	}
	return rep, nil
}

func (m *Manager) countLeasesOnDisk(leaseDir string) int {
	entries, err := os.ReadDir(leaseDir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".lease.json") {
			n++
		}
	}
	return n
}

// RecordAuditViolations increments the cumulative audit violation counter.
func (m *Manager) RecordAuditViolations(n int) {
	if n > 0 {
		atomic.AddUint64(&m.auditViolations, uint64(n))
	}
}

// Metrics returns an atomic snapshot of jail and survival lease counters.
func (m *Manager) Metrics() Metrics {
	m.mu.Lock()
	mode := m.mode
	activeJails := len(m.byInstance)
	leaseDir := m.leaseDir
	m.mu.Unlock()

	return Metrics{
		Mode:              mode,
		ActiveJails:       activeJails,
		ActiveLeases:      m.countLeasesOnDisk(leaseDir),
		JailsPlanned:      atomic.LoadUint64(&m.jailsPlanned),
		LeasesPersisted:   atomic.LoadUint64(&m.leasesPersisted),
		ReattachedVMs:     atomic.LoadUint64(&m.reattachedVMs),
		StalePIDPrevented: atomic.LoadUint64(&m.stalePIDPrevented),
		OrphanedCleaned:   atomic.LoadUint64(&m.orphanedCleaned),
		AuditViolations:   atomic.LoadUint64(&m.auditViolations),
	}
}

// Snapshot returns full manager configuration, telemetry, and active jail specs.
func (m *Manager) Snapshot() StatusSnapshot {
	m.mu.Lock()
	specs := make([]JailSpec, 0, len(m.byInstance))
	for _, sp := range m.byInstance {
		specs = append(specs, sp)
	}
	rootDir := m.rootDir
	leaseDir := m.leaseDir
	uidBase := m.uidBase
	poolSize := m.poolSize
	mode := m.mode
	m.mu.Unlock()

	sort.Slice(specs, func(i, j int) bool {
		return specs[i].InstanceID < specs[j].InstanceID
	})
	met := m.Metrics()
	met.Mode = mode
	return StatusSnapshot{
		Mode:       mode,
		RootDir:    rootDir,
		LeaseDir:   leaseDir,
		UIDBase:    uidBase,
		PoolSize:   poolSize,
		Metrics:    met,
		ActiveSpec: specs,
	}
}

// DefaultProcInspector checks process liveness via signal 0 and reads /proc/<pid>/stat start_time (field 22).
func DefaultProcInspector(pid int) (bool, uint64) {
	if pid <= 0 {
		return false, 0
	}
	err := syscall.Kill(pid, 0)
	if err != nil && !errors.Is(err, syscall.EPERM) {
		return false, 0
	}
	ticks := ReadProcStartTicks(pid)
	return true, ticks
}

// ReadProcStartTicks parses field 22 (starttime in clock ticks) from /proc/<pid>/stat.
func ReadProcStartTicks(pid int) uint64 {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	return ParseProcStatStartTicks(string(raw))
}

// ParseProcStatStartTicks extracts field 22 from a Linux /proc/<pid>/stat line, safely
// handling process comm names containing spaces or parentheses.
func ParseProcStatStartTicks(statLine string) uint64 {
	closeIdx := strings.LastIndexByte(statLine, ')')
	if closeIdx < 0 || closeIdx+2 >= len(statLine) {
		return 0
	}
	rest := strings.Fields(statLine[closeIdx+2:])
	// Field 3 is rest[0], so field 22 is rest[19].
	if len(rest) < 20 {
		return 0
	}
	v, err := strconv.ParseUint(rest[19], 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// DefaultSocketProber performs a non-destructive Unix domain socket dial check.
func DefaultSocketProber(sockPath string) bool {
	conn, err := net.DialTimeout("unix", sockPath, 250*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
