package api

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/SalyyS1/omnivm/internal/lifecycle"
	"github.com/SalyyS1/omnivm/internal/router"
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
	writeGauge("omnivm_host_active_vcpus", "Aggregate virtual CPUs assigned to starting and running microVMs.", int64(resMetrics.ActiveVCPUs))
	writeGauge("omnivm_host_quota_vcpus", "Configured active virtual CPU admission ceiling (0 when unlimited).", int64(resMetrics.MaxActiveVCPUs))
	writeGauge("omnivm_host_active_vms", "Number of concurrently starting or running microVMs.", int64(resMetrics.ActiveVMs))
	writeGauge("omnivm_host_quota_active_vms", "Configured concurrent active microVM admission ceiling (0 when unlimited).", int64(resMetrics.MaxActiveVMs))

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
