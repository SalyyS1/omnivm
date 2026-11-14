// Package lifecycle manages the state machine for serve-mode instances.
//
// State transitions:
//
//	STOPPED → STARTING → RUNNING ⇄ PAUSED → STOPPED
//
// RUNNING → PAUSED after idle timeout (SIGSTOP).
// PAUSED → RUNNING on next request (SIGCONT).
// PAUSED → STOPPED after extended idle (StopVM, resources freed, can reboot on next request).
//
// STOPPED is the only non-running terminal state. Explicit user stop
// (StopInstance) removes the instance from the map entirely.
package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"

	"sync"
	"sync/atomic"
	"time"

	"github.com/SalyyS1/omnivm/internal/config"
	"github.com/SalyyS1/omnivm/internal/image"
	"github.com/SalyyS1/omnivm/internal/logstore"
	"github.com/SalyyS1/omnivm/internal/overlay"
	"github.com/SalyyS1/omnivm/internal/registry"
	"github.com/SalyyS1/omnivm/internal/secrets"
	"github.com/SalyyS1/omnivm/internal/tether"
	"github.com/SalyyS1/omnivm/internal/vmm"
)

// ErrInstanceStopped is returned when exec is attempted on a stopped instance.
var ErrInstanceStopped = errors.New("instance is stopped")

// ErrInstanceDisabled is returned when an operation is attempted on a disabled instance.
var ErrInstanceDisabled = errors.New("instance is disabled")

var rpcIDCounter int64

// Instance states
const (
	StateStopped  = "stopped"
	StateStarting = "starting"
	StateRunning  = "running"
	StatePaused   = "paused"
)

// Instance represents a managed instance.
type Instance struct {
	mu sync.Mutex

	ID          string
	State       string
	Enabled     bool // policy flag: true = wake-on-connect allowed, false = unreachable
	Command     []string
	ExposePorts []vmm.PortExpose
	Handle      vmm.Handle
	Channel     vmm.ControlChannel
	Endpoints   []vmm.HostEndpoint

	// HandleAlias is a user-friendly name for the instance.
	HandleAlias string

	// ImageRef is the OCI image reference (e.g. "python:3.12-alpine").
	ImageRef string

	RootfsPath    string // rootfs path (set by image preparation)
	WorkspacePath string // if set, passed to VMConfig.WorkspacePath
	AutoWorkspace bool   // true = workspace auto-created, delete on instance delete

	// Resource overrides (0 = use global default)
	MemoryMB int
	VCPUs    int
	// DiskMB is the requested root disk size (0 = sized from the image).
	DiskMB int

	// Env holds explicit (non-secret) environment variables to inject.
	Env map[string]string

	// SecretKeys lists secret names to resolve from the store at boot time.
	SecretKeys []string

	// bootEnv is the merged env (secrets + Env) computed at boot time.
	// Not persisted - recomputed on every start.
	bootEnv map[string]string

	// Connection tracking
	activeConns int

	// Idle management
	idleTimer *time.Timer
	stopTimer *time.Timer

	lastActivity time.Time

	// HarnessVersion is reported by the harness in the run RPC response.
	HarnessVersion string

	// Idle policy: "default" (heartbeat + lease + inbound), "leases_only" (lease + inbound only)
	IdlePolicy string

	// Parent-child relationship for guest orchestration
	ParentID     string           // ID of the parent instance that spawned this one (empty = top-level)
	Capabilities *CapabilityToken // spawn capabilities (nil = no guest API spawning)

	// Keepalive lease - prevents pause while held, auto-expires after TTL
	leaseHeld   bool
	leaseExpiry time.Time
	leaseReason string
	leaseTimer  *time.Timer

	// Demuxer for persistent channel Recv loop (nil when stopped)
	demuxer    *channelDemuxer
	logCapture bool // guard against double-attach

	// Exec completion tracking: execID → channel that receives exit code
	execWaiters map[string]chan int

	// Crash backoff - prevents rapid restart loops
	crashCount  int       // consecutive crashes (reset on successful run > 10s)
	lastCrashAt time.Time // time of last crash

	// Transition synchronization and request coalescing (guarded by inst.mu)
	pausing      bool
	resuming     bool
	reaping      bool
	lastBootErr  error
	transitionCh chan struct{}

	// Timestamps
	CreatedAt time.Time
	StoppedAt time.Time // zero if never stopped or currently running
	UpdatedAt time.Time // last state change or significant mutation
}

// InstanceSnapshot is a point-in-time race-free copy of an Instance's metadata and runtime state.
type InstanceSnapshot struct {
	ID                string
	State             string
	Enabled           bool
	Command           []string
	ExposePorts       []vmm.PortExpose
	HandleAlias       string
	ImageRef          string
	RootfsPath        string
	WorkspacePath     string
	AutoWorkspace     bool
	MemoryMB          int
	VCPUs             int
	DiskMB            int
	Env               map[string]string
	SecretKeys        []string
	HarnessVersion    string
	IdlePolicy        string
	ParentID          string
	ActiveConnections int
	LastActivity      time.Time
	LeaseHeld         bool
	LeaseReason       string
	LeaseExpiry       time.Time
	CreatedAt         time.Time
	StoppedAt         time.Time
	UpdatedAt         time.Time
}

func (inst *Instance) waitChanLocked() <-chan struct{} {
	if inst.transitionCh == nil {
		inst.transitionCh = make(chan struct{})
	}
	return inst.transitionCh
}

func (inst *Instance) broadcastLocked() {
	if inst.transitionCh != nil {
		close(inst.transitionCh)
		inst.transitionCh = nil
	}
}

// GetState returns the current lifecycle state under inst.mu.
func (inst *Instance) GetState() string {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	return inst.State
}

// IsEnabled returns whether wake-on-connect is allowed under inst.mu.
func (inst *Instance) IsEnabled() bool {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	return inst.Enabled
}

// SetSecretKeys replaces SecretKeys atomically under inst.mu.
func (inst *Instance) SetSecretKeys(keys []string) {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	copied := make([]string, len(keys))
	copy(copied, keys)
	inst.SecretKeys = copied
	inst.UpdatedAt = time.Now()
}

// RestoreTimestamps restores persisted StoppedAt and UpdatedAt values under inst.mu.
func (inst *Instance) RestoreTimestamps(stoppedAt, updatedAt time.Time) {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	if !stoppedAt.IsZero() {
		inst.StoppedAt = stoppedAt
	}
	if !updatedAt.IsZero() {
		inst.UpdatedAt = updatedAt
	}
}

// Snapshot returns a race-free copy of the instance's state and configuration.
func (inst *Instance) Snapshot() InstanceSnapshot {
	inst.mu.Lock()
	defer inst.mu.Unlock()

	var cmdCopy []string
	if len(inst.Command) > 0 {
		cmdCopy = make([]string, len(inst.Command))
		copy(cmdCopy, inst.Command)
	}
	var portsCopy []vmm.PortExpose
	if len(inst.ExposePorts) > 0 {
		portsCopy = make([]vmm.PortExpose, len(inst.ExposePorts))
		copy(portsCopy, inst.ExposePorts)
	}
	var envCopy map[string]string
	if len(inst.Env) > 0 {
		envCopy = make(map[string]string, len(inst.Env))
		for k, v := range inst.Env {
			envCopy[k] = v
		}
	}
	var secretsCopy []string
	if len(inst.SecretKeys) > 0 {
		secretsCopy = make([]string, len(inst.SecretKeys))
		copy(secretsCopy, inst.SecretKeys)
	}

	return InstanceSnapshot{
		ID:                inst.ID,
		State:             inst.State,
		Enabled:           inst.Enabled,
		Command:           cmdCopy,
		ExposePorts:       portsCopy,
		HandleAlias:       inst.HandleAlias,
		ImageRef:          inst.ImageRef,
		RootfsPath:        inst.RootfsPath,
		WorkspacePath:     inst.WorkspacePath,
		AutoWorkspace:     inst.AutoWorkspace,
		MemoryMB:          inst.MemoryMB,
		VCPUs:             inst.VCPUs,
		DiskMB:            inst.DiskMB,
		Env:               envCopy,
		SecretKeys:        secretsCopy,
		HarnessVersion:    inst.HarnessVersion,
		IdlePolicy:        inst.IdlePolicy,
		ParentID:          inst.ParentID,
		ActiveConnections: inst.activeConns,
		LastActivity:      inst.lastActivity,
		LeaseHeld:         inst.leaseHeld,
		LeaseReason:       inst.leaseReason,
		LeaseExpiry:       inst.leaseExpiry,
		CreatedAt:         inst.CreatedAt,
		StoppedAt:         inst.StoppedAt,
		UpdatedAt:         inst.UpdatedAt,
	}
}

// InstanceStateCounts summarizes managed instances by lifecycle state.
type InstanceStateCounts struct {
	Total    int `json:"total"`
	Running  int `json:"running"`
	Starting int `json:"starting"`
	Paused   int `json:"paused"`
	Stopped  int `json:"stopped"`
	Disabled int `json:"disabled"`
}

// LifecycleMetrics exposes atomic counters and live gauges for scale-to-zero operations.
type LifecycleMetrics struct {
	ColdStarts        uint64 `json:"cold_starts"`
	BootFailures      uint64 `json:"boot_failures"`
	WarmResumes       uint64 `json:"warm_resumes"`
	CoalescedWakes    uint64 `json:"coalesced_wakes"`
	ReapWakeHandoffs  uint64 `json:"reap_wake_handoffs"`
	IdlePauses        uint64 `json:"idle_pauses"`
	AbortedPauses     uint64 `json:"aborted_pauses"`
	IdleStops         uint64 `json:"idle_stops"`
	ActiveConnections int    `json:"active_connections"`
}

type lifecycleCounters struct {
	coldStarts       uint64
	bootFailures     uint64
	warmResumes      uint64
	coalescedWakes   uint64
	reapWakeHandoffs uint64
	idlePauses       uint64
	abortedPauses    uint64
	idleStops        uint64
}

// FirstGuestPort returns the first exposed guest port, or 0 if none.
func (inst *Instance) FirstGuestPort() int {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	if len(inst.ExposePorts) > 0 {
		return inst.ExposePorts[0].GuestPort
	}
	return 0
}

// SetBootEnv sets the env for the next boot without persisting to registry.
// This is the merged result of resolved secrets + explicit Env.
func (inst *Instance) SetBootEnv(env map[string]string) {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	inst.bootEnv = env
}

// MergeSecretKeys adds new keys to SecretKeys without duplicates.
func (inst *Instance) MergeSecretKeys(keys []string) {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	existing := make(map[string]bool, len(inst.SecretKeys))
	for _, k := range inst.SecretKeys {
		existing[k] = true
	}
	for _, k := range keys {
		if !existing[k] {
			inst.SecretKeys = append(inst.SecretKeys, k)
			existing[k] = true
		}
	}
}

// Manager owns instances and drives their lifecycle.
type Manager struct {
	mu        sync.Mutex
	instances map[string]*Instance
	vmm       vmm.VMM
	cfg       *config.Config
	logStore  *logstore.Store

	imageCache  *image.Cache
	overlay     overlay.Overlay
	secretStore *secrets.Store
	registry    *registry.DB
	tetherStore *tether.Store

	metrics lifecycleCounters

	// Callbacks
	onStateChange   func(id, state string)
	onAllocatePorts func(inst *Instance)                                                     // router port allocation
	getPublicPorts  func(id string) map[int]int                                              // guestPort -> publicPort lookup
	onExposePort    func(id string, guestPort, publicPort int, protocol string) (int, error) // runtime expose
	onUnexposePort  func(id string, guestPort int)                                           // runtime unexpose
	onPreBoot       func(inst *Instance)                                                     // resolve secrets before boot
}

// NewManager creates a lifecycle manager.
func NewManager(v vmm.VMM, cfg *config.Config, ls *logstore.Store, imgCache *image.Cache, ov overlay.Overlay) *Manager {
	return &Manager{
		instances:  make(map[string]*Instance),
		vmm:        v,
		cfg:        cfg,
		logStore:   ls,
		imageCache: imgCache,
		overlay:    ov,
	}
}

// InstanceCounts returns a race-free breakdown of instances by state.
func (m *Manager) InstanceCounts() InstanceStateCounts {
	list := m.ListInstances()
	counts := InstanceStateCounts{Total: len(list)}
	for _, inst := range list {
		inst.mu.Lock()
		state := inst.State
		enabled := inst.Enabled
		inst.mu.Unlock()

		if !enabled {
			counts.Disabled++
			continue
		}
		switch state {
		case StateRunning:
			counts.Running++
		case StateStarting:
			counts.Starting++
		case StatePaused:
			counts.Paused++
		default:
			counts.Stopped++
		}
	}
	return counts
}

// Metrics returns atomic scale-to-zero lifecycle counters and live connection counts.
func (m *Manager) Metrics() LifecycleMetrics {
	list := m.ListInstances()
	activeConns := 0
	for _, inst := range list {
		inst.mu.Lock()
		activeConns += inst.activeConns
		inst.mu.Unlock()
	}
	return LifecycleMetrics{
		ColdStarts:        atomic.LoadUint64(&m.metrics.coldStarts),
		BootFailures:      atomic.LoadUint64(&m.metrics.bootFailures),
		WarmResumes:       atomic.LoadUint64(&m.metrics.warmResumes),
		CoalescedWakes:    atomic.LoadUint64(&m.metrics.coalescedWakes),
		ReapWakeHandoffs:  atomic.LoadUint64(&m.metrics.reapWakeHandoffs),
		IdlePauses:        atomic.LoadUint64(&m.metrics.idlePauses),
		AbortedPauses:     atomic.LoadUint64(&m.metrics.abortedPauses),
		IdleStops:         atomic.LoadUint64(&m.metrics.idleStops),
		ActiveConnections: activeConns,
	}
}

// SetSecretStore sets the secret store for capability token operations.
func (m *Manager) SetSecretStore(ss *secrets.Store) {
	m.secretStore = ss
}

// saveToRegistry persists an instance to the registry database.
func (m *Manager) saveToRegistry(inst *Instance) {
	inst.mu.Lock()
	capsJSON := ""
	if inst.Capabilities != nil {
		if b, err := json.Marshal(inst.Capabilities); err == nil {
			capsJSON = string(b)
		}
	}
	portInts := make([]int, len(inst.ExposePorts))
	for i, p := range inst.ExposePorts {
		portInts[i] = p.GuestPort
	}
	var envCopy map[string]string
	if len(inst.Env) > 0 {
		envCopy = make(map[string]string, len(inst.Env))
		for k, v := range inst.Env {
			envCopy[k] = v
		}
	}
	var secretsCopy []string
	if len(inst.SecretKeys) > 0 {
		secretsCopy = make([]string, len(inst.SecretKeys))
		copy(secretsCopy, inst.SecretKeys)
	}
	cmdCopy := make([]string, len(inst.Command))
	copy(cmdCopy, inst.Command)

	regInst := &registry.Instance{
		ID:            inst.ID,
		State:         "stopped",
		Command:       cmdCopy,
		ExposePorts:   portInts,
		Handle:        inst.HandleAlias,
		ImageRef:      inst.ImageRef,
		Workspace:     inst.WorkspacePath,
		AutoWorkspace: inst.AutoWorkspace,
		Env:           envCopy,
		SecretKeys:    secretsCopy,
		Enabled:       inst.Enabled,
		MemoryMB:      inst.MemoryMB,
		DiskMB:        inst.DiskMB,
		VCPUs:         inst.VCPUs,
		ParentID:      inst.ParentID,
		Capabilities:  capsJSON,
		CreatedAt:     inst.CreatedAt,
	}
	inst.mu.Unlock()

	if err := m.registry.SaveInstance(regInst); err != nil {
		log.Printf("save instance to registry: %v", err)
	}
}

// SaveToRegistry persists the current instance state to the registry database.
func (m *Manager) SaveToRegistry(inst *Instance) {
	m.saveToRegistry(inst)
}

// OnStateChange registers a callback for state changes (e.g., to persist to registry).
func (m *Manager) OnStateChange(fn func(id, state string)) {
	m.mu.Lock()
	m.onStateChange = fn
	m.mu.Unlock()
}

// OnAllocatePorts registers a callback for router port allocation.
// Called from CreateInstance for every new instance (host or guest).
func (m *Manager) OnAllocatePorts(fn func(inst *Instance)) {
	m.onAllocatePorts = fn
}

// OnExposePort registers a callback for runtime port expose.
// The callback allocates a router port and returns the public port.
func (m *Manager) OnExposePort(fn func(id string, guestPort, publicPort int, protocol string) (int, error)) {
	m.onExposePort = fn
}

// OnUnexposePort registers a callback for runtime port unexpose.
func (m *Manager) OnUnexposePort(fn func(id string, guestPort int)) {
	m.onUnexposePort = fn
}

// OnPreBoot registers a callback invoked before each instance boot.
// Used by the API server to resolve secrets into bootEnv.
func (m *Manager) OnPreBoot(fn func(inst *Instance)) {
	m.onPreBoot = fn
}

// SetPublicPortsLookup registers a function to query router public ports.
func (m *Manager) SetPublicPortsLookup(fn func(id string) map[int]int) {
	m.getPublicPorts = fn
}

// GetPublicPorts returns guestPort → publicPort mapping for an instance.
func (m *Manager) GetPublicPorts(id string) map[int]int {
	if m.getPublicPorts != nil {
		return m.getPublicPorts(id)
	}
	return nil
}

// SetRegistry sets the registry for instance persistence.
func (m *Manager) SetRegistry(reg *registry.DB) {
	m.registry = reg
}

// SetTetherStore sets the tether store for agent kit messaging.
func (m *Manager) SetTetherStore(ts *tether.Store) {
	m.tetherStore = ts
}

// TetherStore returns the tether store (for API layer access).
func (m *Manager) TetherStore() *tether.Store {
	return m.tetherStore
}

// SendTetherFrame sends an ingress tether frame to a running instance.
func (m *Manager) SendTetherFrame(instanceID string, frame tether.Frame) error {
	m.mu.Lock()
	inst, ok := m.instances[instanceID]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("instance %s not found", instanceID)
	}

	inst.mu.Lock()
	demux := inst.demuxer
	inst.mu.Unlock()
	if demux == nil {
		return fmt.Errorf("instance %s not connected", instanceID)
	}

	return demux.SendNotification("tether.frame", frame)
}

// InstanceOption configures an instance at creation time.
type InstanceOption func(*Instance)

// WithHandle sets a user-friendly handle alias.
func WithHandle(h string) InstanceOption {
	return func(inst *Instance) {
		inst.HandleAlias = h
	}
}

// WithImageRef sets the OCI image reference.
func WithImageRef(ref string) InstanceOption {
	return func(inst *Instance) {
		inst.ImageRef = ref
	}
}

// WithRootfs sets a custom rootfs path (instead of the default base rootfs).
func WithRootfs(path string) InstanceOption {
	return func(inst *Instance) {
		inst.RootfsPath = path
	}
}

// WithWorkspace sets a workspace volume path.
func WithWorkspace(path string) InstanceOption {
	return func(inst *Instance) {
		inst.WorkspacePath = path
	}
}

// WithAutoWorkspace marks the workspace as auto-created (deleted on instance delete).
func WithAutoWorkspace() InstanceOption {
	return func(inst *Instance) {
		inst.AutoWorkspace = true
	}
}

// WithEnv sets environment variables to inject into the VM.
func WithEnv(env map[string]string) InstanceOption {
	return func(inst *Instance) {
		inst.Env = env
	}
}

// WithSecretKeys sets the secret names to resolve from the store at boot time.
func WithSecretKeys(keys []string) InstanceOption {
	return func(inst *Instance) {
		inst.SecretKeys = keys
	}
}

// WithMemory sets the VM memory in megabytes (0 = use global default).
func WithMemory(mb int) InstanceOption {
	return func(inst *Instance) {
		inst.MemoryMB = mb
	}
}

// WithDiskMB sets the root disk size in megabytes (0 = sized from the image).
func WithDiskMB(mb int) InstanceOption {
	return func(inst *Instance) {
		inst.DiskMB = mb
	}
}

// WithVCPUs sets the number of virtual CPUs (0 = use global default).
func WithVCPUs(n int) InstanceOption {
	return func(inst *Instance) {
		inst.VCPUs = n
	}
}

// WithParentID sets the parent instance ID (for child instances spawned via guest API).
func WithParentID(id string) InstanceOption {
	return func(inst *Instance) {
		inst.ParentID = id
	}
}

// WithCapabilities sets the spawn capabilities for guest orchestration.
func WithCapabilities(caps *CapabilityToken) InstanceOption {
	return func(inst *Instance) {
		inst.Capabilities = caps
	}
}

// WithIdlePolicy sets the idle detection policy.
// "default": heartbeat + lease + inbound connections prevent pause.
// "leases_only": only leases and inbound connections prevent pause (heartbeat ignored).
func WithIdlePolicy(policy string) InstanceOption {
	return func(inst *Instance) {
		if policy == "leases_only" {
			inst.IdlePolicy = policy
		}
		// "default" or empty = default behavior
	}
}

// WithEnabled sets the enabled policy flag.
func WithEnabled(enabled bool) InstanceOption {
	return func(inst *Instance) {
		inst.Enabled = enabled
	}
}

// CreateInstance creates a new instance definition without starting it.
func (m *Manager) CreateInstance(id string, command []string, exposePorts []vmm.PortExpose, opts ...InstanceOption) *Instance {
	now := time.Now()
	inst := &Instance{
		ID:          id,
		State:       StateStopped,
		Enabled:     true,
		Command:     command,
		ExposePorts: exposePorts,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	for _, opt := range opts {
		opt(inst)
	}

	m.mu.Lock()
	m.instances[id] = inst
	m.mu.Unlock()

	// Pre-create logstore entry so logs are available immediately
	m.logStore.GetOrCreate(id)

	// Allocate router ports (external concern, callback)
	if m.onAllocatePorts != nil {
		m.onAllocatePorts(inst)
	}

	// Save to registry
	if m.registry != nil {
		m.saveToRegistry(inst)
	}

	return inst
}

// EnsureInstance ensures an instance is running. This is the single entry point
// for the router - it never needs to know the current state.
//
//   - stopped → boot (cold start, ~1-2s)
//   - paused → SIGCONT resume (<100ms)
//   - starting → block until running or failed (with ctx timeout)
//   - running → noop
//
// The router calls EnsureInstance on every request. If the instance is booting,
// EnsureInstance blocks. The router's request context has a 30s timeout, so if
// boot takes too long the request fails with 503 and the loading page is shown.
func (m *Manager) EnsureInstance(ctx context.Context, id string) error {
	m.mu.Lock()
	inst, ok := m.instances[id]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("instance %s not found", id)
	}
	return m.ensureInstanceLoop(ctx, inst, false)
}

func (m *Manager) ensureInstanceLoop(ctx context.Context, inst *Instance, forceEnable bool) error {
	coalescedRecorded := false
	reapHandoffRecorded := false

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		inst.mu.Lock()
		if forceEnable {
			inst.Enabled = true
			forceEnable = false
		}
		if !inst.Enabled {
			inst.mu.Unlock()
			return ErrInstanceDisabled
		}

		// Serialize with any in-flight VMM transition (reaping, pausing, resuming, or cold-starting).
		if inst.reaping || inst.pausing || inst.resuming || inst.State == StateStarting {
			if inst.reaping && !reapHandoffRecorded {
				reapHandoffRecorded = true
				atomic.AddUint64(&m.metrics.reapWakeHandoffs, 1)
			} else if !inst.reaping && !coalescedRecorded {
				coalescedRecorded = true
				atomic.AddUint64(&m.metrics.coalescedWakes, 1)
			}
			wasStartingOrResuming := (inst.State == StateStarting || inst.resuming) && !inst.reaping
			waitCh := inst.waitChanLocked()
			inst.mu.Unlock()

			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-waitCh:
			}

			if wasStartingOrResuming {
				inst.mu.Lock()
				enabledAfter := inst.Enabled
				stateAfter := inst.State
				reapingAfter := inst.reaping
				pausingAfter := inst.pausing
				bootErr := inst.lastBootErr
				inst.mu.Unlock()

				if !enabledAfter {
					return ErrInstanceDisabled
				}
				if stateAfter == StateRunning && !pausingAfter {
					return nil
				}
				if stateAfter == StateStopped && !reapingAfter && bootErr != nil {
					return fmt.Errorf("instance %s failed to start: %w", inst.ID, bootErr)
				}
			}
			continue
		}

		switch inst.State {
		case StateRunning:
			inst.mu.Unlock()
			return nil
		case StatePaused:
			inst.resuming = true
			inst.lastBootErr = nil
			if inst.stopTimer != nil {
				inst.stopTimer.Stop()
				inst.stopTimer = nil
			}
			handle := inst.Handle
			inst.mu.Unlock()
			return m.finishResume(inst, handle)
		case StateStopped:
			return m.bootInstanceLocked(ctx, inst)
		default:
			st := inst.State
			inst.mu.Unlock()
			return fmt.Errorf("instance %s in unexpected state: %s", inst.ID, st)
		}
	}
}

func (m *Manager) bootInstance(ctx context.Context, inst *Instance) error {
	inst.mu.Lock()
	if inst.State != StateStopped || inst.reaping || inst.pausing || inst.resuming {
		inst.mu.Unlock()
		return m.ensureInstanceLoop(ctx, inst, false)
	}
	return m.bootInstanceLocked(ctx, inst)
}

// bootInstanceLocked starts a cold boot. Caller must hold inst.mu and verify
// inst.State == StateStopped with no in-flight transition; inst.mu is unlocked before returning.
func (m *Manager) bootInstanceLocked(ctx context.Context, inst *Instance) error {
	// Crash backoff: if the instance crashed recently, delay or refuse boot.
	// Max 5 consecutive crashes within a 2-minute window. Resets after 2 minutes.
	if inst.crashCount > 0 && !inst.lastCrashAt.IsZero() {
		if time.Since(inst.lastCrashAt) > 2*time.Minute {
			inst.crashCount = 0
		} else if inst.crashCount >= 5 {
			err := fmt.Errorf("instance %s crash loop (crashed %d times, last at %s)",
				inst.ID, inst.crashCount, inst.lastCrashAt.Format(time.RFC3339))
			inst.lastBootErr = err
			inst.broadcastLocked()
			inst.mu.Unlock()
			atomic.AddUint64(&m.metrics.bootFailures, 1)
			return err
		}
	}

	now := time.Now()
	inst.State = StateStarting
	inst.StoppedAt = time.Time{}
	inst.UpdatedAt = now
	inst.lastBootErr = nil
	imageRef := inst.ImageRef
	rootfsPath := inst.RootfsPath
	diskMB := inst.DiskMB
	instMemMB := inst.MemoryMB
	instVCPUs := inst.VCPUs
	wsPath := inst.WorkspacePath
	exposePortsSnapshot := make([]vmm.PortExpose, len(inst.ExposePorts))
	copy(exposePortsSnapshot, inst.ExposePorts)
	inst.broadcastLocked()
	inst.mu.Unlock()
	m.notifyStateChange(inst.ID, StateStarting)

	failBoot := func(err error) error {
		atomic.AddUint64(&m.metrics.bootFailures, 1)
		stoppedNow := time.Now()
		inst.mu.Lock()
		inst.State = StateStopped
		inst.StoppedAt = stoppedNow
		inst.UpdatedAt = stoppedNow
		inst.lastBootErr = err
		inst.broadcastLocked()
		inst.mu.Unlock()
		m.notifyStateChange(inst.ID, StateStopped)
		return err
	}

	// If image ref is set but no rootfs yet, prepare it
	if imageRef != "" && rootfsPath == "" {
		log.Printf("instance %s: preparing image rootfs for %s", inst.ID, imageRef)
		rootfs, err := m.prepareImageRootfs(ctx, inst)
		if err != nil {
			return failBoot(fmt.Errorf("prepare image rootfs: %w", err))
		}
		inst.mu.Lock()
		inst.RootfsPath = rootfs
		inst.mu.Unlock()
		rootfsPath = rootfs
	}

	// Resolve secrets after image env merge so bootEnv includes image defaults
	if m.onPreBoot != nil {
		m.onPreBoot(inst)
	}

	log.Printf("instance %s: rootfs ready, starting VM", inst.ID)

	// If the backend needs a block image and we have a directory, convert it.
	// Reuse existing ext4 image if present to preserve in-VM changes across cold boots.
	needsBlock := m.vmm.Capabilities().RootFSType == vmm.RootFSBlockImage
	if needsBlock {
		ext4Path := rootfsPath + ".ext4"
		if _, err := os.Stat(ext4Path); err == nil {
			rootfsPath = ext4Path
		} else if info, err := os.Stat(rootfsPath); err == nil && info.IsDir() {
			log.Printf("instance %s: converting directory rootfs to ext4: %s", inst.ID, rootfsPath)
			var convErr error
			rootfsPath, convErr = m.buildRootDisk(rootfsPath, diskMB)
			if convErr != nil {
				return failBoot(fmt.Errorf("convert rootfs to ext4: %w", convErr))
			}
		}
	}

	memoryMB := m.cfg.DefaultMemoryMB
	if instMemMB > 0 {
		memoryMB = instMemMB
	}
	vcpus := m.cfg.DefaultVCPUs
	if instVCPUs > 0 {
		vcpus = instVCPUs
	}

	// Re-snapshot ExposePorts right before CreateVM so ports exposed during image
	// rootfs preparation or ext4 disk creation are passed directly to CreateVM.
	inst.mu.Lock()
	exposePortsSnapshot = make([]vmm.PortExpose, len(inst.ExposePorts))
	copy(exposePortsSnapshot, inst.ExposePorts)
	inst.mu.Unlock()

	vmCfg := vmm.VMConfig{
		Rootfs: vmm.RootFS{
			Type: m.vmm.Capabilities().RootFSType,
			Path: rootfsPath,
		},
		MemoryMB:      memoryMB,
		VCPUs:         vcpus,
		ExposePorts:   exposePortsSnapshot,
		WorkspacePath: wsPath,
	}

	handle, err := m.vmm.CreateVM(vmCfg)
	if err != nil {
		return failBoot(fmt.Errorf("create VM: %w", err))
	}

	ch, err := m.vmm.StartVM(handle)
	if err != nil {
		m.vmm.StopVM(handle)
		return failBoot(fmt.Errorf("start VM: %w", err))
	}

	endpoints, _ := m.vmm.HostEndpoints(handle)

	// Create log store entry for this instance
	il := m.logStore.GetOrCreate(inst.ID)

	// Create demuxer with notification handler
	demux := newChannelDemuxer(ch, func(method string, params json.RawMessage) {
		switch method {
		case "log":
			var lp struct {
				Stream string `json:"stream"`
				Line   string `json:"line"`
				ExecID string `json:"exec_id,omitempty"`
			}
			if json.Unmarshal(params, &lp) == nil {
				source := logstore.SourceServer
				if lp.ExecID != "" {
					source = logstore.SourceExec
				}
				il.Append(lp.Stream, lp.Line, lp.ExecID, source)
			}
		case "processExited":
			var pe struct {
				ExitCode int `json:"exit_code"`
			}
			if json.Unmarshal(params, &pe) == nil {
				go m.handleProcessExited(inst, pe.ExitCode)
			}
		case "execDone":
			var ep struct {
				ExecID   string `json:"exec_id"`
				ExitCode int    `json:"exit_code"`
			}
			if json.Unmarshal(params, &ep) == nil {
				log.Printf("instance %s: exec %s done (exit_code=%d)", inst.ID, ep.ExecID, ep.ExitCode)
				inst.mu.Lock()
				if waiterCh, ok := inst.execWaiters[ep.ExecID]; ok {
					waiterCh <- ep.ExitCode
					close(waiterCh)
					delete(inst.execWaiters, ep.ExecID)
				}
				inst.mu.Unlock()
			}
		case "activity":
			m.bumpActivity(inst)
		case "keepalive":
			var kp struct {
				TTLMs  int    `json:"ttl_ms"`
				Reason string `json:"reason"`
			}
			if json.Unmarshal(params, &kp) == nil && kp.TTLMs > 0 {
				m.acquireLease(inst, kp.TTLMs, kp.Reason)
			}
		case "keepalive.release":
			m.releaseLease(inst)
		case "tether.frame":
			if m.tetherStore != nil {
				var frame tether.Frame
				if json.Unmarshal(params, &frame) == nil && frame.IsEgress() {
					m.tetherStore.Append(inst.ID, frame)
				}
			}
		}
	})

	// Register guest request handler (for guest API -> ovmd calls)
	demux.onGuestRequest = func(method string, params json.RawMessage) (interface{}, error) {
		return m.handleGuestRequest(inst, method, params)
	}

	// Send run RPC to start the primary process inside the VM.
	rpcCtx, rpcCancel := context.WithTimeout(ctx, 30*time.Second)
	defer rpcCancel()

	inst.mu.Lock()
	cmdCopy := make([]string, len(inst.Command))
	copy(cmdCopy, inst.Command)
	var bootEnvCopy map[string]string
	if len(inst.bootEnv) > 0 {
		bootEnvCopy = make(map[string]string, len(inst.bootEnv))
		for k, v := range inst.bootEnv {
			bootEnvCopy[k] = v
		}
	}
	var ports []int
	for _, ep := range inst.ExposePorts {
		ports = append(ports, ep.GuestPort)
	}
	var capsCopy *CapabilityToken
	if inst.Capabilities != nil {
		cp := *inst.Capabilities
		capsCopy = &cp
	}
	inst.mu.Unlock()

	rpcParams := map[string]interface{}{
		"command": cmdCopy,
	}
	if len(bootEnvCopy) > 0 {
		rpcParams["env"] = bootEnvCopy
	}
	if len(ports) > 0 {
		rpcParams["expose_ports"] = ports
	}
	log.Printf("instance %s: run RPC params: expose_ports=%v", inst.ID, rpcParams["expose_ports"])

	// Inject capability token if this instance has spawn capabilities
	if capsCopy != nil && m.secretStore != nil {
		token, err := GenerateToken(m.secretStore, inst.ID, *capsCopy)
		if err != nil {
			log.Printf("instance %s: generate capability token: %v", inst.ID, err)
		} else {
			rpcParams["capability_token"] = token
		}
	}

	resp, err := demux.Call(rpcCtx, "run", rpcParams, nextRPCID())
	if err != nil {
		demux.Stop()
		ch.Close()
		m.vmm.StopVM(handle)
		return failBoot(fmt.Errorf("run RPC: %w", err))
	}

	var respObj struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
		Result struct {
			Version string `json:"version"`
		} `json:"result"`
	}
	if json.Unmarshal(resp, &respObj) == nil && respObj.Error != nil {
		demux.Stop()
		ch.Close()
		m.vmm.StopVM(handle)
		return failBoot(fmt.Errorf("run failed: %s", respObj.Error.Message))
	}

	// Reconcile any ExposePort or UnexposePort mutations that arrived while State == StateStarting
	// so VMM endpoints match inst.ExposePorts atomically when transitioning to StateRunning.
	currentEndpoints := endpoints
	reconciledPorts := make(map[int]bool, len(exposePortsSnapshot))
	for _, ep := range exposePortsSnapshot {
		reconciledPorts[ep.GuestPort] = true
	}
	notifiedPorts := make(map[int]bool, len(ports))
	for _, p := range ports {
		notifiedPorts[p] = true
	}
	dynamicExposer, canDynamicExpose := m.vmm.(interface {
		DynamicExposePort(vmm.Handle, int) (int, error)
	})
	dynamicUnexposer, canDynamicUnexpose := m.vmm.(interface {
		DynamicUnexposePort(vmm.Handle, int) error
	})

	for {
		inst.mu.Lock()
		if inst.State != StateStarting || !inst.Enabled {
			disabled := !inst.Enabled
			inst.broadcastLocked()
			inst.mu.Unlock()
			demux.Stop()
			ch.Close()
			m.vmm.StopVM(handle)
			if disabled {
				return ErrInstanceDisabled
			}
			return ErrInstanceStopped
		}

		var toExpose []vmm.PortExpose
		var toUnexpose []int
		desiredSet := make(map[int]vmm.PortExpose, len(inst.ExposePorts))
		for _, ep := range inst.ExposePorts {
			desiredSet[ep.GuestPort] = ep
			if !reconciledPorts[ep.GuestPort] {
				toExpose = append(toExpose, ep)
			}
		}
		for p, active := range reconciledPorts {
			if active {
				if _, stillDesired := desiredSet[p]; !stillDesired {
					toUnexpose = append(toUnexpose, p)
				}
			}
		}

		if len(toExpose) == 0 && len(toUnexpose) == 0 {
			if respObj.Result.Version != "" {
				inst.HarnessVersion = respObj.Result.Version
			}
			runNow := time.Now()
			inst.Handle = handle
			inst.Channel = ch
			inst.Endpoints = currentEndpoints
			inst.State = StateRunning
			inst.lastActivity = runNow
			inst.UpdatedAt = runNow
			inst.demuxer = demux
			inst.logCapture = true
			inst.lastBootErr = nil
			inst.broadcastLocked()
			inst.mu.Unlock()
			break
		}
		inst.mu.Unlock()

		for _, guestPort := range toUnexpose {
			if canDynamicUnexpose {
				_ = dynamicUnexposer.DynamicUnexposePort(handle, guestPort)
			}
			filtered := make([]vmm.HostEndpoint, 0, len(currentEndpoints))
			for _, ep := range currentEndpoints {
				if ep.GuestPort != guestPort {
					filtered = append(filtered, ep)
				}
			}
			currentEndpoints = filtered
			reconciledPorts[guestPort] = false
			if notifiedPorts[guestPort] {
				notifiedPorts[guestPort] = false
				demux.SendNotification("ports_changed", map[string]interface{}{
					"action":     "unexpose",
					"guest_port": guestPort,
				})
			}
		}

		for _, ep := range toExpose {
			reconciledPorts[ep.GuestPort] = true
			if canDynamicExpose {
				if hostPort, err := dynamicExposer.DynamicExposePort(handle, ep.GuestPort); err != nil {
					log.Printf("instance %s: boot endpoint reconcile expose %d failed: %v", inst.ID, ep.GuestPort, err)
				} else if eps, err := m.vmm.HostEndpoints(handle); err == nil {
					currentEndpoints = eps
				} else {
					currentEndpoints = append(currentEndpoints, vmm.HostEndpoint{
						GuestPort: ep.GuestPort,
						HostPort:  hostPort,
						Protocol:  ep.Protocol,
					})
				}
			}
			if !notifiedPorts[ep.GuestPort] {
				notifiedPorts[ep.GuestPort] = true
				pubPort := 0
				if m.getPublicPorts != nil {
					if pp, ok := m.getPublicPorts(inst.ID)[ep.GuestPort]; ok {
						pubPort = pp
					}
				}
				demux.SendNotification("ports_changed", map[string]interface{}{
					"action":      "expose",
					"guest_port":  ep.GuestPort,
					"public_port": pubPort,
					"protocol":    ep.Protocol,
				})
			}
		}
	}

	atomic.AddUint64(&m.metrics.coldStarts, 1)
	m.notifyStateChange(inst.ID, StateRunning)
	m.startIdleTimer(inst)
	log.Printf("instance %s: running (endpoints: %v)", inst.ID, currentEndpoints)
	return nil
}

// handleProcessExited handles the processExited notification from the harness.
// This is the primary process exit path, distinct from idle timeout (stopIdleInstance)
// and explicit user stop (StopInstance).
func (m *Manager) handleProcessExited(inst *Instance, exitCode int) {
	inst.mu.Lock()
	if inst.State != StateRunning && inst.State != StateStarting {
		inst.mu.Unlock()
		return
	}
	handle := inst.Handle
	demux := inst.demuxer

	if inst.idleTimer != nil {
		inst.idleTimer.Stop()
		inst.idleTimer = nil
	}
	if inst.stopTimer != nil {
		inst.stopTimer.Stop()
		inst.stopTimer = nil
	}
	if inst.leaseTimer != nil {
		inst.leaseTimer.Stop()
		inst.leaseTimer = nil
	}
	inst.leaseHeld = false

	for eid, waiterCh := range inst.execWaiters {
		waiterCh <- -1
		close(waiterCh)
		delete(inst.execWaiters, eid)
	}

	uptime := time.Since(inst.lastActivity)
	if exitCode != 0 && uptime < 10*time.Second {
		inst.crashCount++
		inst.lastCrashAt = time.Now()
		log.Printf("instance %s: crash #%d (uptime=%v)", inst.ID, inst.crashCount, uptime)
	} else {
		inst.crashCount = 0
	}

	inst.reaping = true
	inst.Channel = nil
	inst.Endpoints = nil
	inst.demuxer = nil
	inst.logCapture = false
	inst.mu.Unlock()

	log.Printf("instance %s: process exited (code=%d)", inst.ID, exitCode)

	il := m.logStore.GetOrCreate(inst.ID)
	il.Append("stdout", fmt.Sprintf("process exited (code=%d)", exitCode), "", logstore.SourceSystem)

	if demux != nil {
		demux.Stop()
	}
	m.vmm.StopVM(handle)

	now := time.Now()
	inst.mu.Lock()
	inst.reaping = false
	inst.State = StateStopped
	inst.StoppedAt = now
	inst.UpdatedAt = now
	inst.broadcastLocked()
	inst.mu.Unlock()

	m.notifyStateChange(inst.ID, StateStopped)
}

func nextRPCID() int64 {
	return atomic.AddInt64(&rpcIDCounter, 1)
}

func (m *Manager) resumeInstance(inst *Instance) error {
	inst.mu.Lock()
	if inst.resuming || inst.reaping || inst.pausing {
		inst.mu.Unlock()
		return m.ensureInstanceLoop(context.Background(), inst, false)
	}
	if inst.State != StatePaused {
		inst.mu.Unlock()
		return nil
	}
	inst.resuming = true
	inst.lastBootErr = nil
	if inst.stopTimer != nil {
		inst.stopTimer.Stop()
		inst.stopTimer = nil
	}
	handle := inst.Handle
	inst.mu.Unlock()

	return m.finishResume(inst, handle)
}

func (m *Manager) finishResume(inst *Instance, handle vmm.Handle) error {
	log.Printf("instance %s: resuming (SIGCONT)", inst.ID)
	if err := m.vmm.ResumeVM(handle); err != nil {
		wErr := fmt.Errorf("resume VM: %w", err)
		inst.mu.Lock()
		inst.resuming = false
		inst.lastBootErr = wErr
		inst.broadcastLocked()
		inst.mu.Unlock()
		return wErr
	}

	atomic.AddUint64(&m.metrics.warmResumes, 1)
	now := time.Now()
	inst.mu.Lock()
	inst.resuming = false
	if !inst.Enabled || inst.State != StatePaused {
		inst.broadcastLocked()
		inst.mu.Unlock()
		return nil
	}
	inst.State = StateRunning
	inst.lastActivity = now
	inst.UpdatedAt = now
	inst.lastBootErr = nil
	if inst.stopTimer != nil {
		inst.stopTimer.Stop()
		inst.stopTimer = nil
	}
	inst.broadcastLocked()
	inst.mu.Unlock()
	m.notifyStateChange(inst.ID, StateRunning)
	m.startIdleTimer(inst)
	return nil
}

func (m *Manager) waitForRunning(ctx context.Context, inst *Instance) error {
	for {
		inst.mu.Lock()
		if !inst.Enabled {
			inst.mu.Unlock()
			return ErrInstanceDisabled
		}
		state := inst.State
		reaping := inst.reaping
		pausing := inst.pausing
		bootErr := inst.lastBootErr
		if state == StateRunning && !pausing {
			inst.mu.Unlock()
			return nil
		}
		if state == StateStopped && !reaping {
			inst.mu.Unlock()
			if bootErr != nil {
				return fmt.Errorf("instance %s failed to start: %w", inst.ID, bootErr)
			}
			return fmt.Errorf("instance %s failed to start", inst.ID)
		}
		waitCh := inst.waitChanLocked()
		inst.mu.Unlock()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-waitCh:
		}
	}
}

// ResetActivity resets the idle timer. Called by the router on each connection.
func (m *Manager) ResetActivity(id string) {
	m.mu.Lock()
	inst, ok := m.instances[id]
	m.mu.Unlock()
	if !ok {
		return
	}

	inst.mu.Lock()
	inst.lastActivity = time.Now()
	inst.activeConns++
	if inst.idleTimer != nil {
		inst.idleTimer.Stop()
		inst.idleTimer = nil
	}
	if inst.stopTimer != nil {
		inst.stopTimer.Stop()
		inst.stopTimer = nil
	}
	inst.mu.Unlock()
}

// OnConnectionClose decrements active connections and may start idle timer.
func (m *Manager) OnConnectionClose(id string) {
	m.mu.Lock()
	inst, ok := m.instances[id]
	m.mu.Unlock()
	if !ok {
		return
	}

	inst.mu.Lock()
	inst.activeConns--
	if inst.activeConns < 0 {
		inst.activeConns = 0
	}
	conns := inst.activeConns
	inst.lastActivity = time.Now()
	inst.mu.Unlock()

	if conns == 0 {
		m.startIdleTimer(inst)
	}
}

func (m *Manager) startIdleTimer(inst *Instance) {
	inst.mu.Lock()
	defer inst.mu.Unlock()

	if inst.idleTimer != nil {
		inst.idleTimer.Stop()
		inst.idleTimer = nil
	}

	if inst.State != StateRunning || inst.activeConns > 0 || inst.leaseHeld {
		return
	}

	inst.idleTimer = time.AfterFunc(m.cfg.PauseAfterIdle, func() {
		m.pauseInstance(inst)
	})
}

// bumpActivity resets the idle timer when the harness reports guest activity.
// Called on "activity" notifications from the harness (outbound connections, CPU, network).
// Only effective when idle_policy is "default" (not "leases_only").
func (m *Manager) bumpActivity(inst *Instance) {
	inst.mu.Lock()
	defer inst.mu.Unlock()

	// leases_only policy ignores heartbeat activity
	if inst.IdlePolicy == "leases_only" {
		return
	}

	inst.lastActivity = time.Now()
	if inst.State == StateRunning && inst.activeConns == 0 && !inst.leaseHeld {
		if inst.idleTimer != nil {
			inst.idleTimer.Stop()
		}
		inst.idleTimer = time.AfterFunc(m.cfg.PauseAfterIdle, func() {
			m.pauseInstance(inst)
		})
	}
}

// acquireLease prevents the instance from pausing until the lease expires or is released.
func (m *Manager) acquireLease(inst *Instance, ttlMs int, reason string) {
	inst.mu.Lock()
	defer inst.mu.Unlock()

	inst.leaseHeld = true
	inst.leaseReason = reason
	inst.leaseExpiry = time.Now().Add(time.Duration(ttlMs) * time.Millisecond)
	inst.lastActivity = time.Now()

	// Cancel idle timer while lease is held
	if inst.idleTimer != nil {
		inst.idleTimer.Stop()
		inst.idleTimer = nil
	}

	// Set lease expiry timer
	if inst.leaseTimer != nil {
		inst.leaseTimer.Stop()
	}
	inst.leaseTimer = time.AfterFunc(time.Duration(ttlMs)*time.Millisecond, func() {
		m.releaseLease(inst)
	})

	log.Printf("instance %s: lease acquired (ttl=%dms, reason=%s)", inst.ID, ttlMs, reason)
}

// releaseLease clears the lease and restarts the idle timer if appropriate.
func (m *Manager) releaseLease(inst *Instance) {
	inst.mu.Lock()
	wasHeld := inst.leaseHeld
	inst.leaseHeld = false
	inst.leaseReason = ""
	if inst.leaseTimer != nil {
		inst.leaseTimer.Stop()
		inst.leaseTimer = nil
	}

	// Restart idle timer now that lease is gone
	if inst.State == StateRunning && inst.activeConns == 0 {
		if inst.idleTimer != nil {
			inst.idleTimer.Stop()
		}
		inst.idleTimer = time.AfterFunc(m.cfg.PauseAfterIdle, func() {
			m.pauseInstance(inst)
		})
	}
	inst.mu.Unlock()

	if wasHeld {
		log.Printf("instance %s: lease released", inst.ID)
	}
}

func (m *Manager) pauseInstance(inst *Instance) {
	inst.mu.Lock()
	if inst.State != StateRunning || inst.pausing || inst.resuming || inst.reaping || inst.activeConns > 0 || inst.leaseHeld {
		inst.mu.Unlock()
		return
	}
	inst.pausing = true
	handle := inst.Handle
	inst.mu.Unlock()

	log.Printf("instance %s: pausing (idle timeout)", inst.ID)
	if err := m.vmm.PauseVM(handle); err != nil {
		log.Printf("instance %s: pause failed: %v", inst.ID, err)
		inst.mu.Lock()
		inst.pausing = false
		inst.broadcastLocked()
		inst.mu.Unlock()
		return
	}

	inst.mu.Lock()
	// If Disabled/Stopped while PauseVM was running, mark StatePaused (if still StateRunning)
	// and broadcast so DisableInstance/StopInstance can resume+stop cleanly without arming stopTimer.
	if !inst.Enabled || inst.State != StateRunning {
		inst.pausing = false
		if inst.State == StateRunning {
			inst.State = StatePaused
		}
		inst.broadcastLocked()
		inst.mu.Unlock()
		return
	}

	// If an incoming connection, lease, or wake waiter arrived while PauseVM was executing,
	// immediately resume the VM so active requests never hit a frozen guest.
	if inst.activeConns > 0 || inst.leaseHeld || inst.transitionCh != nil {
		inst.pausing = false
		inst.resuming = true
		inst.mu.Unlock()

		log.Printf("instance %s: activity arrived during pause, resuming immediately", inst.ID)
		if rErr := m.vmm.ResumeVM(handle); rErr != nil {
			log.Printf("instance %s: resume after aborted pause failed: %v", inst.ID, rErr)
			inst.mu.Lock()
			inst.resuming = false
			inst.State = StatePaused
			inst.UpdatedAt = time.Now()
			inst.broadcastLocked()
			inst.mu.Unlock()
			m.notifyStateChange(inst.ID, StatePaused)
			return
		}

		atomic.AddUint64(&m.metrics.abortedPauses, 1)
		atomic.AddUint64(&m.metrics.warmResumes, 1)
		now := time.Now()
		inst.mu.Lock()
		inst.resuming = false
		inst.State = StateRunning
		inst.lastActivity = now
		inst.UpdatedAt = now
		inst.broadcastLocked()
		inst.mu.Unlock()
		m.startIdleTimer(inst)
		return
	}

	inst.pausing = false
	inst.State = StatePaused
	inst.UpdatedAt = time.Now()
	if !m.vmm.Capabilities().PersistentPause {
		if inst.stopTimer != nil {
			inst.stopTimer.Stop()
		}
		inst.stopTimer = time.AfterFunc(m.cfg.StopAfterIdle, func() {
			m.stopIdleInstance(inst)
		})
	}
	inst.broadcastLocked()
	inst.mu.Unlock()

	atomic.AddUint64(&m.metrics.idlePauses, 1)
	m.notifyStateChange(inst.ID, StatePaused)
}

func (m *Manager) stopIdleInstance(inst *Instance) {
	inst.mu.Lock()
	if inst.State != StatePaused || inst.resuming || inst.pausing || inst.reaping || inst.activeConns > 0 || inst.leaseHeld {
		inst.mu.Unlock()
		return
	}
	inst.reaping = true
	if inst.stopTimer != nil {
		inst.stopTimer.Stop()
		inst.stopTimer = nil
	}
	handle := inst.Handle
	demux := inst.demuxer
	instID := inst.ID
	inst.Channel = nil
	inst.Endpoints = nil
	inst.demuxer = nil
	inst.logCapture = false
	inst.mu.Unlock()

	// Close the control channel and tear down the VMM while inst.reaping == true.
	// Any concurrent EnsureInstance calls wait on transitionCh until teardown completes
	// and then perform a clean cold boot with zero VMM/disk races.
	if demux != nil {
		demux.Stop()
	}
	m.vmm.StopVM(handle)

	now := time.Now()
	inst.mu.Lock()
	inst.reaping = false
	inst.State = StateStopped
	inst.StoppedAt = now
	inst.UpdatedAt = now
	inst.lastBootErr = nil
	inst.broadcastLocked()
	inst.mu.Unlock()

	atomic.AddUint64(&m.metrics.idleStops, 1)
	log.Printf("instance %s: stopped (extended idle)", instID)
	m.notifyStateChange(instID, StateStopped)
}

// GetEndpoint returns the host endpoint for a guest port on the given instance.
func (m *Manager) GetEndpoint(id string, guestPort int) (string, error) {
	m.mu.Lock()
	inst, ok := m.instances[id]
	m.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("instance %s not found", id)
	}

	inst.mu.Lock()
	defer inst.mu.Unlock()

	for _, ep := range inst.Endpoints {
		if ep.GuestPort == guestPort {
			host := "127.0.0.1"
			if ep.BackendAddr != "" {
				host = ep.BackendAddr
			}
			return fmt.Sprintf("%s:%d", host, ep.HostPort), nil
		}
	}
	return "", fmt.Errorf("no endpoint for guest port %d on instance %s", guestPort, id)
}

// GetInstance returns the instance state (for API responses).
func (m *Manager) GetInstance(id string) *Instance {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.instances[id]
}

// GetInstanceByHandle returns the instance with the given handle alias, or nil.
func (m *Manager) GetInstanceByHandle(handle string) *Instance {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, inst := range m.instances {
		if inst.HandleAlias == handle {
			return inst
		}
	}
	return nil
}

// GetDefaultInstance returns the first instance (for single-instance routing).
func (m *Manager) GetDefaultInstance() *Instance {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, inst := range m.instances {
		return inst
	}
	return nil
}

// prepareImageRootfs pulls an OCI image, creates an overlay, and injects the harness.
func (m *Manager) prepareImageRootfs(ctx context.Context, inst *Instance) (string, error) {
	if m.imageCache == nil || m.overlay == nil {
		return "", fmt.Errorf("image cache or overlay not configured")
	}

	inst.mu.Lock()
	instID := inst.ID
	imageRef := inst.ImageRef
	inst.mu.Unlock()

	progress := func(stage, detail string) {
		m.logStore.GetOrCreate(instID).Append("stderr",
			fmt.Sprintf("[ovm] image %s: %s", stage, detail), "", logstore.SourceSystem)
	}
	cachedDir, _, err := m.imageCache.GetOrPull(ctx, imageRef, progress)
	if err != nil {
		return "", fmt.Errorf("pull image: %w", err)
	}

	// Merge OCI image ENV into instance env (image defaults, explicit env wins)
	if imageEnv := image.ReadImageEnv(cachedDir); len(imageEnv) > 0 {
		inst.mu.Lock()
		if inst.Env == nil {
			inst.Env = make(map[string]string)
		}
		for k, v := range imageEnv {
			if k == "PATH" {
				if existing, ok := inst.Env["PATH"]; ok {
					inst.Env["PATH"] = v + ":" + existing
				} else {
					inst.Env["PATH"] = v
				}
				continue
			}
			if _, exists := inst.Env[k]; !exists {
				inst.Env[k] = v
			}
		}
		inst.mu.Unlock()
		log.Printf("instance %s: merged %d image env vars", instID, len(imageEnv))
	}

	overlayID := instID
	overlayDir, err := m.overlay.Create(ctx, cachedDir, overlayID)
	if err != nil {
		return "", fmt.Errorf("create overlay: %w", err)
	}

	if err := image.InjectGuestBinaries(overlayDir, m.cfg.BinDir); err != nil {
		m.overlay.Remove(overlayID)
		return "", fmt.Errorf("inject harness: %w", err)
	}

	return overlayDir, nil
}

// buildRootDisk converts an instance rootfs directory into its ext4 root
// disk, sized from the directory contents or the requested size.
func (m *Manager) buildRootDisk(dir string, requestedMB int) (string, error) {
	used, err := dirUsageBytes(dir)
	if err != nil {
		return "", fmt.Errorf("measure rootfs: %w", err)
	}
	usedMB := int((used + (1<<20 - 1)) >> 20)
	sizeMB, err := rootDiskSizeMB(usedMB, requestedMB, m.cfg.MinDiskMB, m.cfg.MaxDiskMB)
	if err != nil {
		return "", err
	}
	log.Printf("rootfs %s: contents ~%dMB, root disk %dMB", dir, usedMB, sizeMB)
	return dirToExt4(dir, sizeMB)
}

// dirToExt4 creates an ext4 block image from a directory.
// Used when the backend requires RootFSBlockImage but the rootfs is a directory
// (e.g. OCI image overlay directory on Linux).
func dirToExt4(dir string, sizeMB int) (string, error) {
	out := dir + ".ext4"
	cmd := exec.Command("mkfs.ext4", "-d", dir, "-L", "ovm", out, fmt.Sprintf("%dM", sizeMB))
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("mkfs.ext4 -d %s: %w", dir, err)
	}
	return out, nil
}

// InstanceCount returns the number of active instances.
func (m *Manager) InstanceCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.instances)
}

// PauseInstance explicitly pauses a running instance.
func (m *Manager) PauseInstance(id string) error {
	m.mu.Lock()
	inst, ok := m.instances[id]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("instance %s not found", id)
	}
	m.pauseInstance(inst)
	return nil
}

// ResumeInstance explicitly resumes a paused instance.
func (m *Manager) ResumeInstance(id string) error {
	m.mu.Lock()
	inst, ok := m.instances[id]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("instance %s not found", id)
	}
	return m.resumeInstance(inst)
}

// StopInstance stops an instance's VM but keeps it in the map with state STOPPED.
// Logs are preserved. Use DeleteInstance to remove entirely.
func (m *Manager) StopInstance(id string) error {
	m.mu.Lock()
	inst, ok := m.instances[id]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("instance %s not found", id)
	}

	inst.mu.Lock()
	for inst.reaping || inst.pausing || inst.resuming {
		waitCh := inst.waitChanLocked()
		inst.mu.Unlock()
		<-waitCh
		inst.mu.Lock()
	}

	state := inst.State
	if state == StateStopped {
		inst.mu.Unlock()
		return nil // already stopped
	}
	handle := inst.Handle
	demux := inst.demuxer

	if inst.idleTimer != nil {
		inst.idleTimer.Stop()
		inst.idleTimer = nil
	}
	if inst.stopTimer != nil {
		inst.stopTimer.Stop()
		inst.stopTimer = nil
	}
	if inst.leaseTimer != nil {
		inst.leaseTimer.Stop()
		inst.leaseTimer = nil
	}
	inst.leaseHeld = false

	// Close all exec waiters so handlers unblock
	for eid, ch := range inst.execWaiters {
		ch <- -1
		close(ch)
		delete(inst.execWaiters, eid)
	}

	inst.reaping = true
	inst.Channel = nil
	inst.Endpoints = nil
	inst.demuxer = nil
	inst.logCapture = false
	inst.mu.Unlock()

	if state == StatePaused {
		m.vmm.ResumeVM(handle)
	}

	m.shutdownVM(demux, handle)

	now := time.Now()
	inst.mu.Lock()
	inst.reaping = false
	inst.State = StateStopped
	inst.StoppedAt = now
	inst.UpdatedAt = now
	inst.lastBootErr = nil
	inst.broadcastLocked()
	inst.mu.Unlock()
	m.notifyStateChange(id, StateStopped)

	// Cascade stop children (orphan policy: cascade)
	go m.CascadeStopChildren(id)

	return nil
}

// StartInstance sets Enabled=true and boots the instance.
// This is the explicit start path (CLI/API), distinct from EnsureInstance (auto-wake).
func (m *Manager) StartInstance(ctx context.Context, id string) error {
	m.mu.Lock()
	inst, ok := m.instances[id]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("instance %s not found", id)
	}
	return m.ensureInstanceLoop(ctx, inst, true)
}

// DisableInstance makes an instance a pure registry object:
// sets Enabled=false, closes exec waiters, cancels timers, tears down demuxer, stops VM.
func (m *Manager) DisableInstance(id string) error {
	m.mu.Lock()
	inst, ok := m.instances[id]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("instance %s not found", id)
	}

	inst.mu.Lock()
	// Set disabled first so router/EnsureInstance stops waking immediately
	inst.Enabled = false
	inst.broadcastLocked()

	for inst.reaping || inst.pausing || inst.resuming {
		waitCh := inst.waitChanLocked()
		inst.mu.Unlock()
		<-waitCh
		inst.mu.Lock()
		inst.Enabled = false
	}

	state := inst.State
	if state == StateStopped {
		inst.UpdatedAt = time.Now()
		inst.broadcastLocked()
		inst.mu.Unlock()
		return nil
	}

	handle := inst.Handle
	demux := inst.demuxer

	// Cancel timers
	if inst.idleTimer != nil {
		inst.idleTimer.Stop()
		inst.idleTimer = nil
	}
	if inst.stopTimer != nil {
		inst.stopTimer.Stop()
		inst.stopTimer = nil
	}
	if inst.leaseTimer != nil {
		inst.leaseTimer.Stop()
		inst.leaseTimer = nil
	}
	inst.leaseHeld = false

	// Close all exec waiters so handlers unblock
	for eid, ch := range inst.execWaiters {
		ch <- -1
		close(ch)
		delete(inst.execWaiters, eid)
	}

	inst.reaping = true
	inst.Channel = nil
	inst.Endpoints = nil
	inst.demuxer = nil
	inst.logCapture = false
	inst.mu.Unlock()

	if state == StatePaused {
		m.vmm.ResumeVM(handle)
	}

	if demux != nil {
		demux.Stop()
	}
	if m.vmm != nil {
		m.vmm.StopVM(handle)
	}

	now := time.Now()
	inst.mu.Lock()
	inst.reaping = false
	inst.State = StateStopped
	inst.StoppedAt = now
	inst.UpdatedAt = now
	inst.lastBootErr = nil
	inst.broadcastLocked()
	inst.mu.Unlock()

	m.notifyStateChange(id, StateStopped)

	// Cascade stop children
	go m.CascadeStopChildren(id)

	return nil
}

// DeleteInstance stops an instance and removes it entirely (from map, registry, logs).
func (m *Manager) DeleteInstance(id string) error {
	m.mu.Lock()
	inst, ok := m.instances[id]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("instance %s not found", id)
	}

	inst.mu.Lock()
	inst.Enabled = false
	inst.broadcastLocked()
	for inst.reaping || inst.pausing || inst.resuming {
		waitCh := inst.waitChanLocked()
		inst.mu.Unlock()
		<-waitCh
		inst.mu.Lock()
		inst.Enabled = false
	}

	state := inst.State
	handle := inst.Handle
	demux := inst.demuxer

	if inst.idleTimer != nil {
		inst.idleTimer.Stop()
		inst.idleTimer = nil
	}
	if inst.stopTimer != nil {
		inst.stopTimer.Stop()
		inst.stopTimer = nil
	}
	if inst.leaseTimer != nil {
		inst.leaseTimer.Stop()
		inst.leaseTimer = nil
	}
	inst.leaseHeld = false

	for eid, ch := range inst.execWaiters {
		ch <- -1
		close(ch)
		delete(inst.execWaiters, eid)
	}

	inst.reaping = state != StateStopped
	inst.Channel = nil
	inst.Endpoints = nil
	inst.demuxer = nil
	inst.logCapture = false
	inst.mu.Unlock()

	if state == StatePaused {
		m.vmm.ResumeVM(handle)
	}

	if state != StateStopped {
		m.shutdownVM(demux, handle)
	}

	now := time.Now()
	inst.mu.Lock()
	inst.reaping = false
	inst.State = StateStopped
	inst.StoppedAt = now
	inst.UpdatedAt = now
	inst.broadcastLocked()
	inst.mu.Unlock()
	m.notifyStateChange(id, StateStopped)

	m.mu.Lock()
	delete(m.instances, id)
	m.mu.Unlock()

	m.logStore.Remove(id)
	if m.tetherStore != nil {
		m.tetherStore.Remove(id)
	}

	return nil
}

// shutdownVM sends shutdown RPC and stops the VM.
func (m *Manager) shutdownVM(demux *channelDemuxer, handle vmm.Handle) {
	if demux != nil {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		demux.Call(shutCtx, "shutdown", nil, 99)
		cancel()
		demux.Stop()
	}
	m.vmm.StopVM(handle)
}

// Shutdown deletes all instances (used on daemon exit).
func (m *Manager) Shutdown() {
	m.mu.Lock()
	ids := make([]string, 0, len(m.instances))
	for id := range m.instances {
		ids = append(ids, id)
	}
	m.mu.Unlock()

	for _, id := range ids {
		if err := m.DeleteInstance(id); err != nil {
			log.Printf("delete instance %s: %v", id, err)
		}
	}
}

// ListInstances returns all active instances.
func (m *Manager) ListInstances() []*Instance {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]*Instance, 0, len(m.instances))
	for _, inst := range m.instances {
		result = append(result, inst)
	}
	return result
}

// ExecInstance runs a command in a running instance via the demuxer.
// Returns the exec ID, start time, a done channel that receives the exit code, and any error.
func (m *Manager) ExecInstance(ctx context.Context, id string, command []string, env map[string]string) (string, time.Time, <-chan int, error) {
	m.mu.Lock()
	inst, ok := m.instances[id]
	m.mu.Unlock()
	if !ok {
		return "", time.Time{}, nil, fmt.Errorf("instance not found")
	}

	inst.mu.Lock()
	enabled := inst.Enabled
	state := inst.State
	inst.mu.Unlock()

	if !enabled {
		return "", time.Time{}, nil, ErrInstanceDisabled
	}

	switch state {
	case StateRunning:
		// proceed
	case StatePaused:
		if err := m.resumeInstance(inst); err != nil {
			return "", time.Time{}, nil, fmt.Errorf("resume for exec: %w", err)
		}
	case StateStopped:
		return "", time.Time{}, nil, ErrInstanceStopped
	case StateStarting:
		if err := m.waitForRunning(ctx, inst); err != nil {
			return "", time.Time{}, nil, err
		}
	}

	inst.mu.Lock()
	demux := inst.demuxer
	inst.mu.Unlock()

	if demux == nil {
		return "", time.Time{}, nil, fmt.Errorf("instance has no active channel")
	}

	execID := fmt.Sprintf("exec-%d", time.Now().UnixNano())
	startedAt := time.Now()

	// Register done channel and snapshot bootEnv before sending RPC to avoid data races
	doneCh := make(chan int, 1)
	mergedEnv := make(map[string]string)
	inst.mu.Lock()
	if inst.execWaiters == nil {
		inst.execWaiters = make(map[string]chan int)
	}
	inst.execWaiters[execID] = doneCh
	for k, v := range inst.bootEnv {
		mergedEnv[k] = v
	}
	inst.mu.Unlock()

	// Merge caller's exec env on top (caller env wins over instance env).
	for k, v := range env {
		mergedEnv[k] = v
	}

	resp, err := demux.Call(ctx, "exec", map[string]interface{}{
		"command": command,
		"env":     mergedEnv,
		"exec_id": execID,
	}, nextRPCID())
	if err != nil {
		inst.mu.Lock()
		delete(inst.execWaiters, execID)
		inst.mu.Unlock()
		return "", time.Time{}, nil, fmt.Errorf("exec RPC: %w", err)
	}

	// Check for error in response
	var respObj struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(resp, &respObj) == nil && respObj.Error != nil {
		inst.mu.Lock()
		delete(inst.execWaiters, execID)
		inst.mu.Unlock()
		return "", time.Time{}, nil, fmt.Errorf("exec failed: %s", respObj.Error.Message)
	}

	return execID, startedAt, doneCh, nil
}

// ActiveConns returns the active connection count for an instance.
func (m *Manager) ActiveConns(id string) int {
	m.mu.Lock()
	inst, ok := m.instances[id]
	m.mu.Unlock()
	if !ok {
		return 0
	}
	inst.mu.Lock()
	defer inst.mu.Unlock()
	return inst.activeConns
}

// LeaseInfo returns lease state for an instance. Returns held=false if not found.
func (m *Manager) LeaseInfo(id string) (held bool, reason string, expiresAt time.Time) {
	m.mu.Lock()
	inst, ok := m.instances[id]
	m.mu.Unlock()
	if !ok {
		return false, "", time.Time{}
	}
	inst.mu.Lock()
	defer inst.mu.Unlock()
	return inst.leaseHeld, inst.leaseReason, inst.leaseExpiry
}

// LastActivity returns the last activity time for an instance.
func (m *Manager) LastActivity(id string) time.Time {
	m.mu.Lock()
	inst, ok := m.instances[id]
	m.mu.Unlock()
	if !ok {
		return time.Time{}
	}
	inst.mu.Lock()
	defer inst.mu.Unlock()
	return inst.lastActivity
}

// ExposePort adds a port mapping to a running or stopped instance.
// Returns the allocated public port.
func (m *Manager) ExposePort(id string, guestPort, publicPort int, protocol string) (int, error) {
	m.mu.Lock()
	inst, ok := m.instances[id]
	m.mu.Unlock()
	if !ok {
		return 0, fmt.Errorf("instance %s not found", id)
	}

	if protocol == "" {
		protocol = "http"
	}

	// Check capability cap (max_expose_ports)
	inst.mu.Lock()
	if inst.Capabilities != nil && inst.Capabilities.MaxExposePorts > 0 {
		if len(inst.ExposePorts) >= inst.Capabilities.MaxExposePorts {
			inst.mu.Unlock()
			return 0, fmt.Errorf("max_expose_ports cap reached (%d)", inst.Capabilities.MaxExposePorts)
		}
	}
	// Check for duplicate
	for _, ep := range inst.ExposePorts {
		if ep.GuestPort == guestPort {
			inst.mu.Unlock()
			// Already exposed - return existing public port (idempotent)
			if m.getPublicPorts != nil {
				ports := m.getPublicPorts(id)
				if pp, ok := ports[guestPort]; ok {
					return pp, nil
				}
			}
			return 0, fmt.Errorf("guest port %d already exposed", guestPort)
		}
	}
	inst.ExposePorts = append(inst.ExposePorts, vmm.PortExpose{GuestPort: guestPort, Protocol: protocol})
	inst.mu.Unlock()

	// If instance is running, create VMM-level port forward (gvproxy)
	inst.mu.Lock()
	state := inst.State
	handle := inst.Handle
	inst.mu.Unlock()

	if state == StateRunning {
		lkvm, ok := m.vmm.(interface {
			DynamicExposePort(vmm.Handle, int) (int, error)
		})
		if !ok {
			// Backend doesn't support runtime expose - rollback
			inst.mu.Lock()
			for i, ep := range inst.ExposePorts {
				if ep.GuestPort == guestPort {
					inst.ExposePorts = append(inst.ExposePorts[:i], inst.ExposePorts[i+1:]...)
					break
				}
			}
			inst.mu.Unlock()
			return 0, fmt.Errorf("backend does not support runtime expose; restart instance to apply")
		}
		hostPort, err := lkvm.DynamicExposePort(handle, guestPort)
		if err != nil {
			// Rollback ExposePorts
			inst.mu.Lock()
			for i, ep := range inst.ExposePorts {
				if ep.GuestPort == guestPort {
					inst.ExposePorts = append(inst.ExposePorts[:i], inst.ExposePorts[i+1:]...)
					break
				}
			}
			inst.mu.Unlock()
			return 0, fmt.Errorf("vmm expose: %w", err)
		}
		// Refresh lifecycle endpoints from VMM so GetEndpoint gets BackendAddr
		if eps, err := m.vmm.HostEndpoints(handle); err == nil {
			inst.mu.Lock()
			inst.Endpoints = eps
			inst.mu.Unlock()
		} else {
			// Fallback: add without BackendAddr
			inst.mu.Lock()
			inst.Endpoints = append(inst.Endpoints, vmm.HostEndpoint{
				GuestPort: guestPort,
				HostPort:  hostPort,
				Protocol:  protocol,
			})
			inst.mu.Unlock()
		}
	}

	// Allocate router port
	allocatedPort := 0
	if m.onExposePort != nil {
		var err error
		allocatedPort, err = m.onExposePort(id, guestPort, publicPort, protocol)
		if err != nil {
			// Rollback ExposePorts
			inst.mu.Lock()
			for i, ep := range inst.ExposePorts {
				if ep.GuestPort == guestPort {
					inst.ExposePorts = append(inst.ExposePorts[:i], inst.ExposePorts[i+1:]...)
					break
				}
			}
			inst.mu.Unlock()
			return 0, err
		}
	}

	// Persist to registry
	if m.registry != nil {
		m.saveToRegistry(inst)
	}

	// Notify harness if running
	inst.mu.Lock()
	demux := inst.demuxer
	inst.mu.Unlock()
	if demux != nil {
		demux.SendNotification("ports_changed", map[string]interface{}{
			"action":      "expose",
			"guest_port":  guestPort,
			"public_port": allocatedPort,
			"protocol":    protocol,
		})
	}

	return allocatedPort, nil
}

// UnexposePort removes a port mapping from an instance.
func (m *Manager) UnexposePort(id string, guestPort int) error {
	m.mu.Lock()
	inst, ok := m.instances[id]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("instance %s not found", id)
	}

	inst.mu.Lock()
	found := false
	for i, ep := range inst.ExposePorts {
		if ep.GuestPort == guestPort {
			inst.ExposePorts = append(inst.ExposePorts[:i], inst.ExposePorts[i+1:]...)
			found = true
			break
		}
	}
	inst.mu.Unlock()

	if !found {
		return fmt.Errorf("guest port %d not exposed on instance %s", guestPort, id)
	}

	// Remove VMM-level port forward and lifecycle endpoint if running
	inst.mu.Lock()
	state := inst.State
	handle := inst.Handle
	// Remove from lifecycle endpoints
	for i, ep := range inst.Endpoints {
		if ep.GuestPort == guestPort {
			inst.Endpoints = append(inst.Endpoints[:i], inst.Endpoints[i+1:]...)
			break
		}
	}
	inst.mu.Unlock()

	if state == StateRunning {
		if lkvm, ok := m.vmm.(interface {
			DynamicUnexposePort(vmm.Handle, int) error
		}); ok {
			lkvm.DynamicUnexposePort(handle, guestPort)
		}
	}

	// Free router port
	if m.onUnexposePort != nil {
		m.onUnexposePort(id, guestPort)
	}

	// Persist to registry
	if m.registry != nil {
		m.saveToRegistry(inst)
	}

	// Notify harness if running
	inst.mu.Lock()
	demux := inst.demuxer
	inst.mu.Unlock()
	if demux != nil {
		demux.SendNotification("ports_changed", map[string]interface{}{
			"action":     "unexpose",
			"guest_port": guestPort,
		})
	}

	return nil
}

func (m *Manager) notifyStateChange(id, state string) {
	// Touch UpdatedAt under inst.mu on every state transition
	m.mu.Lock()
	inst := m.instances[id]
	fn := m.onStateChange
	m.mu.Unlock()

	if inst != nil {
		inst.mu.Lock()
		inst.UpdatedAt = time.Now()
		inst.mu.Unlock()
	}

	if fn != nil {
		fn(id, state)
	}
}
