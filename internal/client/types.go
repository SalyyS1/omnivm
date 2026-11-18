package client

import "time"

// Instance represents an ovmd instance.
type Instance struct {
	ID                string                 `json:"id"`
	State             string                 `json:"state"`
	Enabled           bool                   `json:"enabled"`
	Command           []string               `json:"command"`
	Handle            string                 `json:"handle,omitempty"`
	ImageRef          string                 `json:"image_ref,omitempty"`
	ParentID          string                 `json:"parent_id,omitempty"`
	SourceSnapshotID  string                 `json:"source_snapshot_id,omitempty"`
	Kit               string                 `json:"kit,omitempty"`
	Workspace         string                 `json:"workspace,omitempty"`
	IdlePolicy        string                 `json:"idle_policy,omitempty"`
	CreatedAt         string                 `json:"created_at"`
	StoppedAt         string                 `json:"stopped_at,omitempty"`
	LastActiveAt      string                 `json:"last_active_at,omitempty"`
	ActiveConnections int                    `json:"active_connections,omitempty"`
	ExposePorts       []int                  `json:"expose_ports,omitempty"`
	Endpoints         []Endpoint             `json:"endpoints,omitempty"`
	GatewayRunning    bool                   `json:"gateway_running,omitempty"`
	LeaseHeld         bool                   `json:"lease_held,omitempty"`
	LeaseReason       string                 `json:"lease_reason,omitempty"`
	LeaseExpiresAt    string                 `json:"lease_expires_at,omitempty"`
	Extra             map[string]interface{} `json:"-"` // catch-all for unknown fields
}

// Endpoint represents a port mapping.
type Endpoint struct {
	GuestPort  int    `json:"guest_port"`
	PublicPort int    `json:"public_port"`
	Protocol   string `json:"protocol"`
}

// CreateInstanceRequest is the request body for creating an instance.
type CreateInstanceRequest struct {
	Command      []string          `json:"command"`
	Handle       string            `json:"handle,omitempty"`
	ImageRef     string            `json:"image_ref,omitempty"`
	Env          map[string]string `json:"env,omitempty"`
	Secrets      []string          `json:"secrets,omitempty"`
	Workspace    string            `json:"workspace,omitempty"`
	MemoryMB     int               `json:"memory_mb,omitempty"`
	VCPUs        int               `json:"vcpus,omitempty"`
	IdlePolicy   string            `json:"idle_policy,omitempty"`
	Capabilities interface{}       `json:"capabilities,omitempty"`
	Kit          string            `json:"kit,omitempty"`
}

// CreateInstanceResponse is the response from creating an instance.
type CreateInstanceResponse struct {
	ID       string   `json:"id"`
	State    string   `json:"state"`
	Handle   string   `json:"handle,omitempty"`
	Command  []string `json:"command"`
	ImageRef string   `json:"image_ref,omitempty"`
	Kit      string   `json:"kit,omitempty"`
}

// ExecResult holds the result of an exec command.
type ExecResult struct {
	ExecID    string `json:"exec_id"`
	StartedAt string `json:"started_at"`
	ExitCode  int    `json:"exit_code"`
	Output    string `json:"output"` // collected stdout+stderr
}

// ExposeRequest is the request body for exposing a port.
type ExposeRequest struct {
	Port       int    `json:"port"`
	PublicPort int    `json:"public_port,omitempty"`
	Protocol   string `json:"protocol,omitempty"`
}

// ExposeResponse is the response from exposing a port.
type ExposeResponse struct {
	GuestPort  int    `json:"guest_port"`
	PublicPort int    `json:"public_port"`
	Protocol   string `json:"protocol"`
	URL        string `json:"url"`
}

// TetherSendResult is the response from sending a tether message.
type TetherSendResult struct {
	MsgID      string `json:"msg_id"`
	SessionID  string `json:"session_id"`
	IngressSeq int64  `json:"ingress_seq"`
}

// TetherPollOpts configures a tether poll request.
type TetherPollOpts struct {
	Channel      string
	SessionID    string
	ReplyToMsgID string
	AfterSeq     int64
	Limit        int
	Types        []string
	WaitMs       int
}

// TetherFrame is a single tether frame.
type TetherFrame struct {
	Seq     int64                  `json:"seq"`
	Type    string                 `json:"type"`
	MsgID   string                 `json:"msg_id,omitempty"`
	Session map[string]interface{} `json:"session,omitempty"`
	Content map[string]interface{} `json:"content,omitempty"`
	Extra   map[string]interface{} `json:"-"`
}

// TetherPollResult is the response from polling tether frames.
type TetherPollResult struct {
	Frames   []TetherFrame `json:"frames"`
	NextSeq  int64         `json:"next_seq"`
	TimedOut bool          `json:"timed_out"`
}

// SecretInfo represents a secret (name + metadata, no value).
type SecretInfo struct {
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
}

// InstanceCounts summarizes managed instances by lifecycle state.
type InstanceCounts struct {
	Total    int `json:"total"`
	Running  int `json:"running"`
	Starting int `json:"starting"`
	Paused   int `json:"paused"`
	Stopped  int `json:"stopped"`
	Disabled int `json:"disabled"`
}

// HostResourceMetrics reports live host memory, vCPU, active VM usage, KSM deduplication savings,
// and Linux cgroup v2 memory.high watermark / PSI stall counters alongside configured ceilings.
type HostResourceMetrics struct {
	ResidentMemoryMB          int     `json:"resident_memory_mb"`
	EffectiveResidentMemoryMB int     `json:"effective_resident_memory_mb"`
	ActiveMemoryMB            int     `json:"active_memory_mb"`
	PausedMemoryMB            int     `json:"paused_memory_mb"`
	KSMSavedMemoryMB          int     `json:"ksm_saved_memory_mb"`
	KSMPagesSharing           uint64  `json:"ksm_pages_sharing"`
	KSMActive                 bool    `json:"ksm_active"`
	KSMBurstActivations       uint64  `json:"ksm_burst_activations"`
	KSMVolatileBackoffs       uint64  `json:"ksm_volatile_backoffs"`
	KSMScanRatePagesPerSec    uint64  `json:"ksm_scan_rate_pages_per_sec"`
	KSMDedupRatio             float64 `json:"ksm_dedup_ratio"`
	KSMVolatileRatio          float64 `json:"ksm_volatile_ratio"`
	MaxHostMemoryMB           int     `json:"max_host_memory_mb"`
	ActiveVCPUs               int     `json:"active_vcpus"`
	MaxActiveVCPUs            int     `json:"max_active_vcpus"`
	ActiveVMs                 int     `json:"active_vms"`
	MaxActiveVMs              int     `json:"max_active_vms"`
	CgroupAvailable           bool    `json:"cgroup_available"`
	CgroupMemoryCurrentBytes  uint64  `json:"cgroup_memory_current_bytes"`
	CgroupMemoryHighBytes     uint64  `json:"cgroup_memory_high_bytes"`
	CgroupMemoryMaxBytes      uint64  `json:"cgroup_memory_max_bytes"`
	CgroupHighThrottleEvents  uint64  `json:"cgroup_high_throttle_events"`
	CgroupOOMEvents           uint64  `json:"cgroup_oom_events"`
	CgroupPSISomeAvg10        float64 `json:"cgroup_psi_some_avg10"`
	CgroupPSIFullAvg10        float64 `json:"cgroup_psi_full_avg10"`
	CgroupPSISomeTotalMicros  uint64  `json:"cgroup_psi_some_total_micros"`
	CgroupPSIFullTotalMicros  uint64  `json:"cgroup_psi_full_total_micros"`
}

// DiskPrewarmMetrics reports cumulative page-cache pre-warming counters.
type DiskPrewarmMetrics struct {
	Mode                string `json:"mode"`
	Passes              uint64 `json:"passes"`
	FilesWarmed         uint64 `json:"files_warmed"`
	CacheHits           uint64 `json:"cache_hits"`
	WarmedBytes         uint64 `json:"warmed_bytes"`
	SparseSkippedBytes  uint64 `json:"sparse_skipped_bytes"`
	DurationMicrosTotal uint64 `json:"duration_micros_total"`
}

// WakeLatencySummary captures summary latency percentiles for a wake path.
type WakeLatencySummary struct {
	Count  uint64  `json:"count"`
	SumMs  float64 `json:"sum_ms"`
	MinMs  float64 `json:"min_ms"`
	P50Ms  float64 `json:"p50_ms"`
	P95Ms  float64 `json:"p95_ms"`
	P99Ms  float64 `json:"p99_ms"`
	MaxMs  float64 `json:"max_ms"`
	MeanMs float64 `json:"mean_ms"`
}

// WakeLatencyMetrics holds latency summaries across microVM wake paths.
type WakeLatencyMetrics struct {
	ColdBoot      WakeLatencySummary `json:"cold_boot"`
	WarmResume    WakeLatencySummary `json:"warm_resume"`
	CoalescedWait WakeLatencySummary `json:"coalesced_wait"`
}

// IOQoSMetrics reports multi-tenant token-bucket I/O QoS governor counters.
type IOQoSMetrics struct {
	Mode                 string  `json:"mode"`
	ActiveTenants        int     `json:"active_tenants"`
	ActiveInstances      int     `json:"active_instances"`
	FairnessIndex        float64 `json:"fairness_index"`
	BlkAdmittedBytes     uint64  `json:"blk_admitted_bytes"`
	BlkAdmittedOps       uint64  `json:"blk_admitted_ops"`
	NetAdmittedBytes     uint64  `json:"net_admitted_bytes"`
	NetAdmittedOps       uint64  `json:"net_admitted_ops"`
	InstanceBlkThrottles uint64  `json:"instance_blk_throttles"`
	InstanceNetThrottles uint64  `json:"instance_net_throttles"`
	TenantBlkThrottles   uint64  `json:"tenant_blk_throttles"`
	TenantNetThrottles   uint64  `json:"tenant_net_throttles"`
	ThrottleWaitMicros   uint64  `json:"throttle_wait_micros"`
	RebalancePasses      uint64  `json:"rebalance_passes"`
}

// LifecycleMetrics holds scale-to-zero lifecycle counters.
type LifecycleMetrics struct {
	ColdStarts        uint64             `json:"cold_starts"`
	BootFailures      uint64             `json:"boot_failures"`
	WarmResumes       uint64             `json:"warm_resumes"`
	CoalescedWakes    uint64             `json:"coalesced_wakes"`
	ReapWakeHandoffs  uint64             `json:"reap_wake_handoffs"`
	IdlePauses        uint64             `json:"idle_pauses"`
	AbortedPauses     uint64             `json:"aborted_pauses"`
	IdleStops         uint64             `json:"idle_stops"`
	PausedEvictions   uint64             `json:"paused_evictions"`
	QuotaRejections   uint64             `json:"quota_rejections"`
	SnapshotsCreated  uint64             `json:"snapshots_created"`
	SnapshotBranches  uint64             `json:"snapshot_branches"`
	CoWSavedBytes     uint64             `json:"cow_saved_bytes"`
	ActiveConnections int                `json:"active_connections"`
	DiskPrewarm       DiskPrewarmMetrics `json:"disk_prewarm"`
	IOQoS             IOQoSMetrics       `json:"io_qos"`
	WakeLatency       WakeLatencyMetrics `json:"wake_latency"`
}

// Snapshot represents an immutable microVM disk and workspace checkpoint.
type Snapshot struct {
	ID               string            `json:"id"`
	Name             string            `json:"name,omitempty"`
	SourceInstanceID string            `json:"source_instance_id"`
	SourceHandle     string            `json:"source_handle,omitempty"`
	ParentSnapshotID string            `json:"parent_snapshot_id,omitempty"`
	ImageRef         string            `json:"image_ref,omitempty"`
	Command          []string          `json:"command"`
	Env              map[string]string `json:"env,omitempty"`
	SecretKeys       []string          `json:"secret_keys,omitempty"`
	ExposePorts      []int             `json:"expose_ports,omitempty"`
	MemoryMB         int               `json:"memory_mb,omitempty"`
	VCPUs            int               `json:"vcpus,omitempty"`
	DiskMB           int               `json:"disk_mb,omitempty"`
	RootfsPath       string            `json:"rootfs_path"`
	WorkspacePath    string            `json:"workspace_path,omitempty"`
	VMMStateDir      string            `json:"vmm_state_dir,omitempty"`
	LogicalBytes     uint64            `json:"logical_bytes"`
	CoWSavedBytes    uint64            `json:"cow_saved_bytes"`
	CreatedAt        string            `json:"created_at"`
}

// CreateSnapshotRequest configures snapshot creation on an instance.
type CreateSnapshotRequest struct {
	Name            string `json:"name,omitempty"`
	IncludeVMMState bool   `json:"include_vmm_state,omitempty"`
}

// BranchInstanceRequest configures Copy-on-Write branching from a snapshot or instance.
type BranchInstanceRequest struct {
	Handle       string            `json:"handle,omitempty"`
	Command      []string          `json:"command,omitempty"`
	Env          map[string]string `json:"env,omitempty"`
	Workspace    string            `json:"workspace,omitempty"`
	MemoryMB     int               `json:"memory_mb,omitempty"`
	VCPUs        int               `json:"vcpus,omitempty"`
	DiskMB       int               `json:"disk_mb,omitempty"`
	ExcludePorts bool              `json:"exclude_ports,omitempty"`
	Start        *bool             `json:"start,omitempty"`
}

// BranchInstanceResponse describes a newly branched instance.
type BranchInstanceResponse struct {
	ID               string     `json:"id"`
	Handle           string     `json:"handle,omitempty"`
	State            string     `json:"state"`
	ParentID         string     `json:"parent_id,omitempty"`
	SourceSnapshotID string     `json:"source_snapshot_id,omitempty"`
	ImageRef         string     `json:"image_ref,omitempty"`
	Workspace        string     `json:"workspace,omitempty"`
	LogicalBytes     uint64     `json:"logical_bytes"`
	CoWSavedBytes    uint64     `json:"cow_saved_bytes"`
	Endpoints        []Endpoint `json:"endpoints,omitempty"`
}

// RouterMetrics holds ingress proxy and probe telemetry.
type RouterMetrics struct {
	ActivePortProxies int    `json:"active_port_proxies"`
	HTTPRequests      uint64 `json:"http_requests"`
	TCPConnections    uint64 `json:"tcp_connections"`
	ProbeRetries      uint64 `json:"probe_retries"`
	BackendErrors     uint64 `json:"backend_errors"`
}

// DaemonStatus is the response from the status endpoint.
type DaemonStatus struct {
	Status        string                 `json:"status"`
	Version       string                 `json:"version,omitempty"`
	Backend       string                 `json:"backend"`
	UptimeSeconds int64                  `json:"uptime_seconds"`
	Capabilities  map[string]interface{} `json:"capabilities"`
	Instances     InstanceCounts         `json:"instances"`
	Resources     HostResourceMetrics    `json:"resources"`
	Metrics       LifecycleMetrics       `json:"metrics"`
}

// DaemonMetrics is the response from the /v1/metrics endpoint.
type DaemonMetrics struct {
	Timestamp     string              `json:"timestamp"`
	Version       string              `json:"version"`
	Backend       string              `json:"backend"`
	UptimeSeconds int64               `json:"uptime_seconds"`
	Instances     InstanceCounts      `json:"instances"`
	Resources     HostResourceMetrics `json:"resources"`
	Lifecycle     LifecycleMetrics    `json:"lifecycle"`
	Router        RouterMetrics       `json:"router"`
}

// Kit represents a kit manifest.
type Kit struct {
	Name        string `json:"name"`
	Version     string `json:"version,omitempty"`
	Description string `json:"description,omitempty"`
	Image       string `json:"image,omitempty"`
}

// LogEntry represents a single log line from an instance.
type LogEntry struct {
	Timestamp time.Time `json:"ts"`
	Stream    string    `json:"stream"`
	Line      string    `json:"line"`
	ExecID    string    `json:"exec_id,omitempty"`
	ExitCode  *int      `json:"exit_code,omitempty"`
	Done      bool      `json:"done,omitempty"`
}

// APIError is returned when the API returns an error response.
type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	return e.Message
}
