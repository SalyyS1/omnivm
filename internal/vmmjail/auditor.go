package vmmjail

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

// RuleDescriptor documents one static security rule for VMM jail envelopes.
type RuleDescriptor struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	DefaultSeverity  string `json:"default_severity"`
	SecuritySeverity string `json:"security_severity"`
	Summary          string `json:"summary"`
	Remediation      string `json:"remediation"`
}

// Finding represents one static VMM jail security diagnostic.
type Finding struct {
	RuleID      string `json:"rule_id"`
	Severity    string `json:"severity"`
	SecurityCVSS string `json:"security_cvss"`
	InstanceID  string `json:"instance_id,omitempty"`
	Message     string `json:"message"`
	Remediation string `json:"remediation"`
	Fingerprint string `json:"fingerprint"`
}

// AuditReport summarizes static security analysis across one or more VMM JailSpec definitions.
type AuditReport struct {
	SourcePath   string    `json:"source_path"`
	Passed       bool      `json:"passed"`
	CheckedSpecs int       `json:"checked_specs"`
	ErrorCount   int       `json:"error_count"`
	WarningCount int       `json:"warning_count"`
	Findings     []Finding `json:"findings"`
}

// Catalog defines rules OVM-JAIL-001 through OVM-JAIL-008.
var Catalog = []RuleDescriptor{
	{
		ID:               "OVM-JAIL-001",
		Name:             "PrivilegedOrSystemJailUID",
		DefaultSeverity:  "error",
		SecuritySeverity: "9.3",
		Summary:          "Hypervisor VMM process runs as root (UID/GID 0) or reuses a privileged host system UID (< 65536).",
		Remediation:      "Allocate a dedicated unprivileged subordinate UID/GID >= 100000 per microVM instance.",
	},
	{
		ID:               "OVM-JAIL-002",
		Name:             "UnsafeChrootBoundary",
		DefaultSeverity:  "error",
		SecuritySeverity: "9.1",
		Summary:          "Jail chroot directory is unset, points to host '/', or overlaps sensitive host system hierarchies.",
		Remediation:      "Confine each VMM inside an isolated per-instance empty chroot directory such as /var/run/omnivm/jails/<id>/root.",
	},
	{
		ID:               "OVM-JAIL-003",
		Name:             "RetainedLinuxCapabilitiesOrPrivEscalation",
		DefaultSeverity:  "error",
		SecuritySeverity: "8.8",
		Summary:          "VMM jail disables PR_SET_NO_NEW_PRIVS, fails to clear the capability bounding set, or retains Linux capabilities.",
		Remediation:      "Set no_new_privs=true, clear_bounding_caps=true, and drop all retained_caps after pre-opening KVM and TAP file descriptors.",
	},
	{
		ID:               "OVM-JAIL-004",
		Name:             "UnboundedOrUndersizedRlimits",
		DefaultSeverity:  "warning",
		SecuritySeverity: "7.4",
		Summary:          "Jail omits RLIMIT_AS / RLIMIT_NPROC resource ceilings or sizes RLIMIT_AS below guest RAM plus VMM overhead.",
		Remediation:      "Configure rlimit_as_bytes >= (memory_mb + 64) MiB and cap rlimit_nproc and rlimit_nofile.",
	},
	{
		ID:               "OVM-JAIL-005",
		Name:             "CrossInstanceUIDCollision",
		DefaultSeverity:  "error",
		SecuritySeverity: "8.6",
		Summary:          "Multiple microVM jails share the same UID or GID, enabling cross-VM kill() and /proc/<pid>/mem lateral access.",
		Remediation:      "Assign a unique UID/GID slot per concurrent microVM instance from the subordinate jail pool.",
	},
	{
		ID:               "OVM-JAIL-006",
		Name:             "MissingSeccompOrNetnsIsolation",
		DefaultSeverity:  "warning",
		SecuritySeverity: "6.8",
		Summary:          "Jailed VMM omits seccomp-bpf syscall filtering or runs inside the host root network namespace.",
		Remediation:      "Attach a thread-scoped Cloud Hypervisor seccomp filter and isolate the VMM inside a dedicated per-VM network namespace.",
	},
	{
		ID:               "OVM-JAIL-007",
		Name:             "DirectDeviceNodeExposureWithoutFDHandoff",
		DefaultSeverity:  "warning",
		SecuritySeverity: "6.5",
		Summary:          "Jail disables pre-opened /dev/kvm or /dev/net/tun file-descriptor handoff, requiring device nodes inside chroot.",
		Remediation:      "Enable kvm_fd_handoff=true and tap_fd_handoff=true so the jailed VMM cannot re-open character devices.",
	},
	{
		ID:               "OVM-JAIL-008",
		Name:             "SensitiveHostBindMountExposure",
		DefaultSeverity:  "error",
		SecuritySeverity: "8.9",
		Summary:          "Jail bind-mounts sensitive host directories (/etc, /root, /proc, /sys, /var/run/docker.sock) or traversal paths.",
		Remediation:      "Restrict bind mounts strictly to the instance rootfs overlay and explicit workspace directory.",
	},
}

func ruleByID(id string) RuleDescriptor {
	for _, r := range Catalog {
		if r.ID == id {
			return r
		}
	}
	return RuleDescriptor{ID: id, DefaultSeverity: "warning", SecuritySeverity: "5.0"}
}

func makeFinding(ruleID, instID, msg string) Finding {
	r := ruleByID(ruleID)
	h := sha256.Sum256([]byte(ruleID + "|" + instID + "|" + msg))
	return Finding{
		RuleID:       r.ID,
		Severity:     r.DefaultSeverity,
		SecurityCVSS: r.SecuritySeverity,
		InstanceID:   instID,
		Message:      msg,
		Remediation:  r.Remediation,
		Fingerprint:  hex.EncodeToString(h[:8]),
	}
}

var forbiddenChrootPrefixes = []string{
	"/etc", "/root", "/proc", "/sys", "/boot", "/usr", "/bin", "/sbin", "/lib", "/var/run/docker.sock",
}

func isForbiddenHostPath(p string) bool {
	clean := filepath.Clean(strings.TrimSpace(p))
	if clean == "." || clean == "/" {
		return true
	}
	for _, pref := range forbiddenChrootPrefixes {
		if clean == pref || strings.HasPrefix(clean, pref+"/") {
			return true
		}
	}
	return false
}

// AuditJailSpecs statically audits one or more JailSpec definitions against OVM-JAIL-001..008.
func AuditJailSpecs(specs []JailSpec, sourcePath string) *AuditReport {
	if strings.TrimSpace(sourcePath) == "" {
		sourcePath = "jail-spec.json"
	}
	rep := &AuditReport{
		SourcePath:   sourcePath,
		CheckedSpecs: len(specs),
		Findings:     []Finding{},
	}

	seenUIDs := make(map[uint32]string, len(specs))
	seenGIDs := make(map[uint32]string, len(specs))

	for idx, sp := range specs {
		instID := strings.TrimSpace(sp.InstanceID)
		if instID == "" {
			instID = fmt.Sprintf("spec[%d]", idx)
		}

		// OVM-JAIL-001: UID/GID privilege check
		if sp.UID == 0 || sp.GID == 0 {
			rep.Findings = append(rep.Findings, makeFinding(
				"OVM-JAIL-001", instID,
				fmt.Sprintf("jail %q runs VMM with privileged root identity (uid=%d, gid=%d)", instID, sp.UID, sp.GID),
			))
		} else if sp.UID < 65536 || sp.GID < 65536 {
			rep.Findings = append(rep.Findings, makeFinding(
				"OVM-JAIL-001", instID,
				fmt.Sprintf("jail %q uses low host UID/GID (%d:%d < 65536) risking host daemon collision", instID, sp.UID, sp.GID),
			))
		}

		// OVM-JAIL-005: Cross-instance UID/GID collision
		if sp.UID > 0 {
			if prev, dup := seenUIDs[sp.UID]; dup && prev != instID {
				rep.Findings = append(rep.Findings, makeFinding(
					"OVM-JAIL-005", instID,
					fmt.Sprintf("jail %q shares UID %d with instance %q", instID, sp.UID, prev),
				))
			} else {
				seenUIDs[sp.UID] = instID
			}
		}
		if sp.GID > 0 {
			if prev, dup := seenGIDs[sp.GID]; dup && prev != instID {
				rep.Findings = append(rep.Findings, makeFinding(
					"OVM-JAIL-005", instID,
					fmt.Sprintf("jail %q shares GID %d with instance %q", instID, sp.GID, prev),
				))
			} else {
				seenGIDs[sp.GID] = instID
			}
		}

		// OVM-JAIL-002: Chroot isolation check
		rawChroot := strings.TrimSpace(sp.ChrootDir)
		if rawChroot == "" || !strings.HasPrefix(rawChroot, "/") || isForbiddenHostPath(rawChroot) {
			rep.Findings = append(rep.Findings, makeFinding(
				"OVM-JAIL-002", instID,
				fmt.Sprintf("jail %q configures unsafe chroot_dir %q", instID, sp.ChrootDir),
			))
		}

		// OVM-JAIL-003: Capability bounding set & PR_SET_NO_NEW_PRIVS
		if !sp.NoNewPrivs || !sp.ClearBoundingCaps || len(sp.RetainedCaps) > 0 {
			rep.Findings = append(rep.Findings, makeFinding(
				"OVM-JAIL-003", instID,
				fmt.Sprintf("jail %q leaves privilege escalation surface open (no_new_privs=%v, clear_bounding_caps=%v, retained_caps=%v)",
					instID, sp.NoNewPrivs, sp.ClearBoundingCaps, sp.RetainedCaps),
			))
		}

		// OVM-JAIL-004: RLIMIT ceilings
		minAS := uint64(0)
		if sp.MemoryMB > 0 {
			minAS = uint64(sp.MemoryMB+64) * 1024 * 1024
		}
		if sp.RlimitASBytes == 0 || sp.RlimitNProc == 0 || sp.RlimitNoFile == 0 || (minAS > 0 && sp.RlimitASBytes < minAS) {
			rep.Findings = append(rep.Findings, makeFinding(
				"OVM-JAIL-004", instID,
				fmt.Sprintf("jail %q has missing or undersized resource limits (rlimit_as=%d, min_recommended=%d, rlimit_nproc=%d, rlimit_nofile=%d)",
					instID, sp.RlimitASBytes, minAS, sp.RlimitNProc, sp.RlimitNoFile),
			))
		}

		// OVM-JAIL-006: Seccomp & Network Namespace
		netns := strings.TrimSpace(sp.NetNamespace)
		if strings.TrimSpace(sp.SeccompProfile) == "" || strings.EqualFold(sp.SeccompProfile, "unconfined") ||
			netns == "" || strings.EqualFold(netns, "host") {
			rep.Findings = append(rep.Findings, makeFinding(
				"OVM-JAIL-006", instID,
				fmt.Sprintf("jail %q lacks seccomp profile (%q) or per-VM network namespace (%q)", instID, sp.SeccompProfile, sp.NetNamespace),
			))
		}

		// OVM-JAIL-007: Pre-opened FD handoff
		if !sp.KVMFDHandoff || !sp.TAPFDHandoff {
			rep.Findings = append(rep.Findings, makeFinding(
				"OVM-JAIL-007", instID,
				fmt.Sprintf("jail %q disables pre-opened device FD handoff (kvm_fd_handoff=%v, tap_fd_handoff=%v)", instID, sp.KVMFDHandoff, sp.TAPFDHandoff),
			))
		}

		// OVM-JAIL-008: Sensitive host bind mounts
		for _, bm := range sp.BindMounts {
			if strings.Contains(bm.Source, "..") || strings.Contains(bm.Target, "..") || isForbiddenHostPath(bm.Source) {
				rep.Findings = append(rep.Findings, makeFinding(
					"OVM-JAIL-008", instID,
					fmt.Sprintf("jail %q exposes sensitive or traversal bind mount (%s -> %s)", instID, bm.Source, bm.Target),
				))
			}
		}
	}

	for _, f := range rep.Findings {
		if f.Severity == "error" {
			rep.ErrorCount++
		} else {
			rep.WarningCount++
		}
	}
	rep.Passed = rep.ErrorCount == 0
	return rep
}

// AuditJailBytes parses either a single JailSpec JSON object or an array of JailSpec objects and runs AuditJailSpecs.
func AuditJailBytes(raw []byte, sourcePath string) (*AuditReport, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, fmt.Errorf("jail spec payload is empty")
	}
	if strings.HasPrefix(trimmed, "[") {
		var list []JailSpec
		if err := json.Unmarshal([]byte(trimmed), &list); err != nil {
			return nil, fmt.Errorf("parse jail spec array: %w", err)
		}
		if len(list) == 0 {
			return nil, fmt.Errorf("jail spec array must contain at least one entry")
		}
		return AuditJailSpecs(list, sourcePath), nil
	}
	var single JailSpec
	if err := json.Unmarshal([]byte(trimmed), &single); err != nil {
		return nil, fmt.Errorf("parse jail spec: %w", err)
	}
	return AuditJailSpecs([]JailSpec{single}, sourcePath), nil
}

// GenerateBaselineJailSpec synthesizes a hardened baseline JailSpec passing all 8 security rules.
func GenerateBaselineJailSpec(instanceID, tenantID string, memoryMB, vcpus int) JailSpec {
	mgr := NewManager(Config{Mode: ModeStrict})
	sp, _ := mgr.PlanJail(instanceID, tenantID, memoryMB, vcpus, "", true)
	return sp
}
