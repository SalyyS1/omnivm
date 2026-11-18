package api

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/SalyyS1/omnivm/internal/lifecycle"
	"github.com/SalyyS1/omnivm/internal/router"
	"github.com/SalyyS1/omnivm/internal/telemetry"
	"github.com/SalyyS1/omnivm/internal/version"
)

const (
	promContentType        = "text/plain; version=0.0.4; charset=utf-8"
	openMetricsContentType = "application/openmetrics-text; version=1.0.0; charset=utf-8"
)

func wantsPrometheusFormat(r *http.Request) bool {
	switch strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format"))) {
	case "prometheus", "openmetrics", "prom":
		return true
	}
	accept := strings.ToLower(r.Header.Get("Accept"))
	return strings.Contains(accept, "application/openmetrics-text") ||
		strings.Contains(accept, "version=0.0.4")
}

func wantsOpenMetricsEOF(r *http.Request) bool {
	if strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("format")), "openmetrics") {
		return true
	}
	return strings.Contains(strings.ToLower(r.Header.Get("Accept")), "application/openmetrics-text")
}

func escapePromLabel(val string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(val)
}

func (s *Server) handlePrometheusMetrics(w http.ResponseWriter, r *http.Request) {
	backendName := ""
	if s.vmm != nil {
		backendName = s.vmm.Capabilities().Name
	}

	var counts lifecycle.InstanceStateCounts
	var lmMetrics lifecycle.LifecycleMetrics
	var resMetrics lifecycle.HostResourceMetrics
	if s.lifecycle != nil {
		counts = s.lifecycle.InstanceCounts()
		lmMetrics = s.lifecycle.Metrics()
		resMetrics = s.lifecycle.HostResources()
	} else if s.cfg != nil {
		resMetrics.MaxHostMemoryMB = s.cfg.MaxHostMemoryMB
		resMetrics.MaxActiveVCPUs = s.cfg.MaxActiveVCPUs
		resMetrics.MaxActiveVMs = s.cfg.MaxActiveVMs
	}

	var rtrMetrics router.RouterMetrics
	if s.router != nil {
		rtrMetrics = s.router.Metrics()
	}

	openMetrics := wantsOpenMetricsEOF(r)
	var b strings.Builder
	b.Grow(4096)

	writeGauge := func(name, help string, val int64) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n%s %d\n", name, help, name, name, val)
	}
	writeCounter := func(baseName, help string, val uint64) {
		sampleName := baseName + "_total"
		typeName := sampleName
		if openMetrics {
			typeName = baseName
		}
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s counter\n%s %d\n", typeName, help, typeName, sampleName, val)
	}

	ver := escapePromLabel(version.Version())
	be := escapePromLabel(backendName)
	fmt.Fprintf(&b, "# HELP omnivm_build_info Build and backend metadata for the ovmd control plane.\n")
	fmt.Fprintf(&b, "# TYPE omnivm_build_info gauge\n")
	fmt.Fprintf(&b, "omnivm_build_info{version=%q,backend=%q} 1\n", ver, be)

	writeGauge("omnivm_up", "Whether the ovmd control plane is healthy and serving requests.", 1)
	writeGauge("omnivm_uptime_seconds", "Seconds elapsed since ovmd started.", s.uptimeSeconds())

	fmt.Fprintf(&b, "# HELP omnivm_instances Managed microVM instances partitioned by lifecycle state.\n")
	fmt.Fprintf(&b, "# TYPE omnivm_instances gauge\n")
	fmt.Fprintf(&b, "omnivm_instances{state=\"running\"} %d\n", counts.Running)
	fmt.Fprintf(&b, "omnivm_instances{state=\"starting\"} %d\n", counts.Starting)
	fmt.Fprintf(&b, "omnivm_instances{state=\"paused\"} %d\n", counts.Paused)
	fmt.Fprintf(&b, "omnivm_instances{state=\"stopped\"} %d\n", counts.Stopped)
	fmt.Fprintf(&b, "omnivm_instances{state=\"disabled\"} %d\n", counts.Disabled)
	writeGauge("omnivm_instances_total", "Total number of managed microVM instances registered in ovmd.", int64(counts.Total))

	const bytesPerMB = int64(1024 * 1024)
	writeGauge("omnivm_host_resident_memory_bytes", "Aggregate guest RAM in bytes across starting, running, and warm paused microVMs.", int64(resMetrics.ResidentMemoryMB)*bytesPerMB)
	writeGauge("omnivm_host_effective_resident_memory_bytes", "Effective host guest RAM footprint in bytes after Linux KSM page deduplication.", int64(resMetrics.EffectiveResidentMemoryMB)*bytesPerMB)
	writeGauge("omnivm_host_active_memory_bytes", "Aggregate guest RAM in bytes across starting and running microVMs.", int64(resMetrics.ActiveMemoryMB)*bytesPerMB)
	writeGauge("omnivm_host_paused_memory_bytes", "Aggregate guest RAM in bytes across warm standby paused microVMs.", int64(resMetrics.PausedMemoryMB)*bytesPerMB)
	writeGauge("omnivm_host_quota_memory_bytes", "Configured host guest memory ceiling in bytes (0 when unlimited).", int64(resMetrics.MaxHostMemoryMB)*bytesPerMB)
	writeGauge("omnivm_ksm_saved_bytes", "Physical host RAM bytes currently deduplicated across microVMs via Linux KSM.", int64(resMetrics.KSMSavedMemoryMB)*bytesPerMB)
	writeGauge("omnivm_ksm_pages_sharing", "Number of duplicate guest memory pages currently merged by Linux KSM.", int64(resMetrics.KSMPagesSharing))
	ksmActiveVal := int64(0)
	if resMetrics.KSMActive {
		ksmActiveVal = 1
	}
	writeGauge("omnivm_ksm_active", "Whether the Linux ksmd page scanner is currently active (1=scanning, 0=paused).", ksmActiveVal)
	writeCounter("omnivm_ksm_burst_activations", "Total proactive burst deduplication passes activated by VM spawn events.", resMetrics.KSMBurstActivations)
	writeCounter("omnivm_ksm_volatile_backoffs", "Total scan throttle backoff passes triggered by volatile heap page churn.", resMetrics.KSMVolatileBackoffs)
	writeGauge("omnivm_ksm_scan_rate_pages_per_sec", "Current effective KSM candidate scan rate in pages per second.", int64(resMetrics.KSMScanRatePagesPerSec))
	fmt.Fprintf(&b, "# HELP omnivm_ksm_dedup_ratio Ratio of duplicate shared pages to total unique candidate pages.\n")
	fmt.Fprintf(&b, "# TYPE omnivm_ksm_dedup_ratio gauge\n")
	fmt.Fprintf(&b, "omnivm_ksm_dedup_ratio %.4f\n", resMetrics.KSMDedupRatio)
	fmt.Fprintf(&b, "# HELP omnivm_ksm_volatile_ratio Ratio of volatile mutating pages to candidate scan pages.\n")
	fmt.Fprintf(&b, "# TYPE omnivm_ksm_volatile_ratio gauge\n")
	fmt.Fprintf(&b, "omnivm_ksm_volatile_ratio %.4f\n", resMetrics.KSMVolatileRatio)
	writeGauge("omnivm_host_active_vcpus", "Aggregate virtual CPUs assigned to starting and running microVMs.", int64(resMetrics.ActiveVCPUs))
	writeGauge("omnivm_host_quota_vcpus", "Configured active virtual CPU admission ceiling (0 when unlimited).", int64(resMetrics.MaxActiveVCPUs))
	writeGauge("omnivm_host_active_vms", "Number of concurrently starting or running microVMs.", int64(resMetrics.ActiveVMs))
	writeGauge("omnivm_host_quota_active_vms", "Configured concurrent active microVM admission ceiling (0 when unlimited).", int64(resMetrics.MaxActiveVMs))

	cgAvail := int64(0)
	if resMetrics.CgroupAvailable {
		cgAvail = 1
	}
	writeGauge("omnivm_cgroup_available", "Whether Linux cgroup v2 memory accounting is available for ovmd (1=available, 0=unavailable).", cgAvail)
	writeGauge("omnivm_cgroup_memory_current_bytes", "Current cgroup v2 memory usage in bytes.", int64(resMetrics.CgroupMemoryCurrentBytes))
	writeGauge("omnivm_cgroup_memory_high_bytes", "Active cgroup v2 memory.high soft-throttle watermark in bytes (0 when max/unlimited).", int64(resMetrics.CgroupMemoryHighBytes))
	writeGauge("omnivm_cgroup_memory_max_bytes", "Configured cgroup v2 memory.max hard limit in bytes (0 when max/unlimited).", int64(resMetrics.CgroupMemoryMaxBytes))
	writeCounter("omnivm_cgroup_memory_high_throttles", "Total cgroup v2 memory.high soft-limit throttle and direct-reclaim events.", resMetrics.CgroupHighThrottleEvents)
	writeCounter("omnivm_cgroup_memory_oom_events", "Total cgroup v2 out-of-memory and OOM-kill events.", resMetrics.CgroupOOMEvents)
	fmt.Fprintf(&b, "# HELP omnivm_cgroup_memory_psi_avg10_percent 10-second Linux PSI memory stall percentage partitioned by level.\n")
	fmt.Fprintf(&b, "# TYPE omnivm_cgroup_memory_psi_avg10_percent gauge\n")
	fmt.Fprintf(&b, "omnivm_cgroup_memory_psi_avg10_percent{level=\"some\"} %.4f\n", resMetrics.CgroupPSISomeAvg10)
	fmt.Fprintf(&b, "omnivm_cgroup_memory_psi_avg10_percent{level=\"full\"} %.4f\n", resMetrics.CgroupPSIFullAvg10)
	psiType := "omnivm_cgroup_memory_psi_stall_microseconds_total"
	if openMetrics {
		psiType = "omnivm_cgroup_memory_psi_stall_microseconds"
	}
	fmt.Fprintf(&b, "# HELP %s Cumulative Linux PSI memory stall duration in microseconds.\n", psiType)
	fmt.Fprintf(&b, "# TYPE %s counter\n", psiType)
	fmt.Fprintf(&b, "omnivm_cgroup_memory_psi_stall_microseconds_total{level=\"some\"} %d\n", resMetrics.CgroupPSISomeTotalMicros)
	fmt.Fprintf(&b, "omnivm_cgroup_memory_psi_stall_microseconds_total{level=\"full\"} %d\n", resMetrics.CgroupPSIFullTotalMicros)

	writeCounter("omnivm_lifecycle_cold_starts", "Total cold boots completed from STOPPED to RUNNING.", lmMetrics.ColdStarts)
	writeCounter("omnivm_lifecycle_boot_failures", "Total failed cold boot attempts.", lmMetrics.BootFailures)
	writeCounter("omnivm_lifecycle_warm_resumes", "Total sub-second warm resumes from PAUSED to RUNNING.", lmMetrics.WarmResumes)
	writeCounter("omnivm_lifecycle_coalesced_wakes", "Total concurrent wake requests coalesced onto an in-flight transition.", lmMetrics.CoalescedWakes)
	writeCounter("omnivm_lifecycle_reap_wake_handoffs", "Total wake requests serialized behind an in-flight idle VM reap.", lmMetrics.ReapWakeHandoffs)
	writeCounter("omnivm_lifecycle_idle_pauses", "Total transitions from RUNNING to PAUSED on idle timeout.", lmMetrics.IdlePauses)
	writeCounter("omnivm_lifecycle_aborted_pauses", "Total mid-flight pauses aborted due to arriving traffic or leases.", lmMetrics.AbortedPauses)
	writeCounter("omnivm_lifecycle_idle_stops", "Total transitions from PAUSED to STOPPED by the extended idle reaper.", lmMetrics.IdleStops)
	writeCounter("omnivm_lifecycle_paused_evictions", "Total LRU paused microVMs evicted to reclaim host memory for incoming wakes.", lmMetrics.PausedEvictions)
	writeCounter("omnivm_lifecycle_quota_rejections", "Total instance starts or resumes rejected by host resource admission quotas.", lmMetrics.QuotaRejections)
	writeCounter("omnivm_lifecycle_snapshots_created", "Total microVM disk and workspace snapshots created.", lmMetrics.SnapshotsCreated)
	writeCounter("omnivm_lifecycle_snapshot_branches", "Total Copy-on-Write microVM branches spawned from snapshots or live instances.", lmMetrics.SnapshotBranches)
	writeGauge("omnivm_storage_cow_saved_bytes", "Cumulative host disk bytes saved by Copy-on-Write reflinks and sparse extent cloning.", int64(lmMetrics.CoWSavedBytes))
	writeGauge("omnivm_lifecycle_active_connections", "Current active HTTP and TCP connections tracked across instances.", int64(lmMetrics.ActiveConnections))

	writeCounter("omnivm_disk_prewarm_passes", "Total page-cache disk pre-warming passes executed ahead of cold boot.", lmMetrics.DiskPrewarm.Passes)
	writeCounter("omnivm_disk_prewarm_files_warmed", "Total kernel and rootfs files pre-warmed into the page cache.", lmMetrics.DiskPrewarm.FilesWarmed)
	writeCounter("omnivm_disk_prewarm_cache_hits", "Total pre-warm requests resolved from warm page-cache metadata.", lmMetrics.DiskPrewarm.CacheHits)
	writeCounter("omnivm_disk_prewarm_warmed_bytes", "Total allocated extent bytes read or fadvised into host page cache.", lmMetrics.DiskPrewarm.WarmedBytes)
	writeCounter("omnivm_disk_prewarm_sparse_skipped_bytes", "Total sparse unallocated hole bytes skipped in O(1) time.", lmMetrics.DiskPrewarm.SparseSkippedBytes)
	writeCounter("omnivm_disk_prewarm_duration_microseconds", "Cumulative duration spent in disk pre-warming in microseconds.", lmMetrics.DiskPrewarm.DurationMicrosTotal)

	wakeSeries := []telemetry.LabeledSnapshot{
		{LabelValue: "cold_boot", Snapshot: lmMetrics.WakeLatency.ColdBoot},
		{LabelValue: "warm_resume", Snapshot: lmMetrics.WakeLatency.WarmResume},
		{LabelValue: "coalesced_wait", Snapshot: lmMetrics.WakeLatency.CoalescedWait},
	}
	telemetry.WritePrometheusHistogram(&b, "omnivm_wake_latency_seconds", "MicroVM wake and transition latency in seconds partitioned by transition path.", "path", wakeSeries)

	writeGauge("omnivm_ioqos_active_tenants", "Number of tenants tracked by the I/O QoS token-bucket governor.", int64(lmMetrics.IOQoS.ActiveTenants))
	writeGauge("omnivm_ioqos_active_instances", "Number of active microVMs registered in the I/O QoS governor.", int64(lmMetrics.IOQoS.ActiveInstances))
	fairnessVal := lmMetrics.IOQoS.FairnessIndex
	if fairnessVal == 0 && lmMetrics.IOQoS.ActiveInstances <= 1 {
		fairnessVal = 1.0
	}
	fmt.Fprintf(&b, "# HELP omnivm_ioqos_fairness_index Weighted Jain fairness index across active tenant microVM I/O allocations.\n")
	fmt.Fprintf(&b, "# TYPE omnivm_ioqos_fairness_index gauge\n")
	fmt.Fprintf(&b, "omnivm_ioqos_fairness_index %.4f\n", fairnessVal)

	ioBytesType := "omnivm_ioqos_admitted_bytes_total"
	ioOpsType := "omnivm_ioqos_admitted_ops_total"
	ioThrottlesType := "omnivm_ioqos_throttles_total"
	if openMetrics {
		ioBytesType = "omnivm_ioqos_admitted_bytes"
		ioOpsType = "omnivm_ioqos_admitted_ops"
		ioThrottlesType = "omnivm_ioqos_throttles"
	}
	fmt.Fprintf(&b, "# HELP %s Cumulative guest I/O bytes admitted through token-bucket QoS channels.\n", ioBytesType)
	fmt.Fprintf(&b, "# TYPE %s counter\n", ioBytesType)
	fmt.Fprintf(&b, "omnivm_ioqos_admitted_bytes_total{channel=\"blk\"} %d\n", lmMetrics.IOQoS.BlkAdmittedBytes)
	fmt.Fprintf(&b, "omnivm_ioqos_admitted_bytes_total{channel=\"net\"} %d\n", lmMetrics.IOQoS.NetAdmittedBytes)

	fmt.Fprintf(&b, "# HELP %s Cumulative guest I/O operations or packets admitted through token-bucket QoS channels.\n", ioOpsType)
	fmt.Fprintf(&b, "# TYPE %s counter\n", ioOpsType)
	fmt.Fprintf(&b, "omnivm_ioqos_admitted_ops_total{channel=\"blk\"} %d\n", lmMetrics.IOQoS.BlkAdmittedOps)
	fmt.Fprintf(&b, "omnivm_ioqos_admitted_ops_total{channel=\"net\"} %d\n", lmMetrics.IOQoS.NetAdmittedOps)

	fmt.Fprintf(&b, "# HELP %s Cumulative token-bucket burst throttle events partitioned by scope and channel.\n", ioThrottlesType)
	fmt.Fprintf(&b, "# TYPE %s counter\n", ioThrottlesType)
	fmt.Fprintf(&b, "omnivm_ioqos_throttles_total{scope=\"instance\",channel=\"blk\"} %d\n", lmMetrics.IOQoS.InstanceBlkThrottles)
	fmt.Fprintf(&b, "omnivm_ioqos_throttles_total{scope=\"instance\",channel=\"net\"} %d\n", lmMetrics.IOQoS.InstanceNetThrottles)
	fmt.Fprintf(&b, "omnivm_ioqos_throttles_total{scope=\"tenant\",channel=\"blk\"} %d\n", lmMetrics.IOQoS.TenantBlkThrottles)
	fmt.Fprintf(&b, "omnivm_ioqos_throttles_total{scope=\"tenant\",channel=\"net\"} %d\n", lmMetrics.IOQoS.TenantNetThrottles)

	writeCounter("omnivm_ioqos_throttle_wait_microseconds", "Cumulative retry-after pacing delay computed by the I/O QoS governor in microseconds.", lmMetrics.IOQoS.ThrottleWaitMicros)
	writeCounter("omnivm_ioqos_rebalance_passes", "Total weighted max-min fair-share rebalance passes across tenant microVMs.", lmMetrics.IOQoS.RebalancePasses)

	writeGauge("omnivm_router_active_port_proxies", "Number of bound host L4/HTTP port proxy listeners.", int64(rtrMetrics.ActivePortProxies))
	writeCounter("omnivm_router_http_requests", "Total HTTP ingress requests handled by the main router.", rtrMetrics.HTTPRequests)
	writeCounter("omnivm_router_tcp_connections", "Total TCP connections accepted on exposed instance ports.", rtrMetrics.TCPConnections)
	writeCounter("omnivm_router_probe_retries", "Total readiness probe backoff retries performed after VM wake.", rtrMetrics.ProbeRetries)
	writeCounter("omnivm_router_backend_errors", "Total ingress wake or backend dial errors observed by the router.", rtrMetrics.BackendErrors)

	if openMetrics {
		b.WriteString("# EOF\n")
		w.Header().Set("Content-Type", openMetricsContentType)
	} else {
		w.Header().Set("Content-Type", promContentType)
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(b.String()))
}
