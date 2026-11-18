// Package seccomp provides a static security auditor and hardened baseline
// generator for Linux seccomp-bpf syscall profiles used with Cloud Hypervisor
// microVMs, virtiofsd sidecars, and OCI guest workloads.
package seccomp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// TargetRole identifies the execution boundary governed by a seccomp profile.
type TargetRole string

const (
	TargetWorkload  TargetRole = "workload"
	TargetVMM       TargetRole = "vmm"
	TargetVirtiofsd TargetRole = "virtiofsd"
)

// ParseTargetRole normalizes a target role flag.
func ParseTargetRole(raw string) (TargetRole, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "workload", "guest", "oci":
		return TargetWorkload, nil
	case "vmm", "cloud-hypervisor", "hypervisor":
		return TargetVMM, nil
	case "virtiofsd", "fs":
		return TargetVirtiofsd, nil
	default:
		return "", fmt.Errorf("unsupported seccomp target %q: expected workload, vmm, or virtiofsd", raw)
	}
}

// Severity levels aligned with OASIS SARIF v2.1.0.
const (
	SeverityError   = "error"
	SeverityWarning = "warning"
	SeverityNote    = "note"
)

// RuleMeta documents an OmniVM seccomp diagnostic rule.
type RuleMeta struct {
	ID               string  `json:"id"`
	Name             string  `json:"name"`
	DefaultSeverity  string  `json:"default_severity"`
	SecuritySeverity string  `json:"security_severity"`
	Summary          string  `json:"summary"`
	Remediation      string  `json:"remediation"`
}

// Catalog lists all 10 OmniVM seccomp audit rules in deterministic order.
var Catalog = []RuleMeta{
	{
		ID:               "OVM-SEC-001",
		Name:             "DefaultAllowPolicy",
		DefaultSeverity:  SeverityError,
		SecuritySeverity: "9.1",
		Summary:          "Seccomp profile uses a permissive defaultAction (SCMP_ACT_ALLOW or SCMP_ACT_LOG) instead of fail-closed deny.",
		Remediation:      "Set defaultAction to SCMP_ACT_ERRNO or SCMP_ACT_KILL_PROCESS and allowlist only required syscalls.",
	},
	{
		ID:               "OVM-SEC-002",
		Name:             "KernelModuleAndKexecEscape",
		DefaultSeverity:  SeverityError,
		SecuritySeverity: "9.8",
		Summary:          "Profile permits kernel module loading or kexec image replacement syscalls capable of ring-0 host compromise.",
		Remediation:      "Remove init_module, finit_module, delete_module, kexec_load, kexec_file_load, create_module, and uselib from allow rules.",
	},
	{
		ID:               "OVM-SEC-003",
		Name:             "UnrestrictedBPFPerfUserfaultfd",
		DefaultSeverity:  SeverityError,
		SecuritySeverity: "8.8",
		Summary:          "Profile permits bpf, perf_event_open, or userfaultfd without argument guards, exposing kernel JIT and page-fault race primitives.",
		Remediation:      "Deny bpf, perf_event_open, and userfaultfd in VMM, sidecar, and guest workload seccomp profiles.",
	},
	{
		ID:               "OVM-SEC-004",
		Name:             "CrossProcessMemoryInspection",
		DefaultSeverity:  SeverityError,
		SecuritySeverity: "8.6",
		Summary:          "Profile permits ptrace, process_vm_readv, process_vm_writev, or kcmp, enabling sibling VMM or host memory inspection.",
		Remediation:      "Remove ptrace, process_vm_readv, process_vm_writev, and kcmp from allowed syscalls.",
	},
	{
		ID:               "OVM-SEC-005",
		Name:             "MountAndNamespaceEscape",
		DefaultSeverity:  SeverityError,
		SecuritySeverity: "8.4",
		Summary:          "Profile permits filesystem mount, pivot/chroot, namespace transition, or open_by_handle_at host-fd traversal syscalls.",
		Remediation:      "Block open_by_handle_at everywhere and remove mount, umount2, pivot_root, chroot, unshare, and setns from post-init execution profiles.",
	},
	{
		ID:               "OVM-SEC-006",
		Name:             "UnfilteredHypervisorIoctl",
		DefaultSeverity:  SeverityWarning,
		SecuritySeverity: "7.2",
		Summary:          "VMM or virtio worker profile permits ioctl with no request-code argument filter, exposing broad host kernel driver ioctls.",
		Remediation:      "Constrain ioctl on vmm, vcpu, and virtio threads with SCMP_CMP_EQ or SCMP_CMP_MASKED_EQ rules scoped to KVM and vhost ioctl numbers.",
	},
	{
		ID:               "OVM-SEC-007",
		Name:             "ExecutableMemoryWXAndMemfdExec",
		DefaultSeverity:  SeverityWarning,
		SecuritySeverity: "6.8",
		Summary:          "Profile enables fileless payload execution (memfd_create + execveat) or unrestricted executable memory mapping on VMM threads.",
		Remediation:      "Drop memfd_create/execveat pairs and restrict mmap/mprotect prot arguments on VMM threads to block PROT_WRITE|PROT_EXEC.",
	},
	{
		ID:               "OVM-SEC-008",
		Name:             "HostKeyringAndClockTampering",
		DefaultSeverity:  SeverityWarning,
		SecuritySeverity: "6.1",
		Summary:          "Profile permits host kernel keyring manipulation or host wall-clock mutation syscalls.",
		Remediation:      "Remove add_key, request_key, keyctl, settimeofday, clock_settime, clock_adjtime, and adjtimex from allow rules.",
	},
	{
		ID:               "OVM-SEC-009",
		Name:             "MissingSyscallArchPinning",
		DefaultSeverity:  SeverityWarning,
		SecuritySeverity: "6.5",
		Summary:          "Profile omits explicit architecture pinning or mixes 64-bit and legacy 32-bit syscall ABIs under shared syscall names.",
		Remediation:      "Pin architectures explicitly to SCMP_ARCH_X86_64 or SCMP_ARCH_AARCH64 without enabling 32-bit compatibility ABIs (SCMP_ARCH_X86/X32).",
	},
	{
		ID:               "OVM-SEC-010",
		Name:             "MissingMandatoryRuntimeSyscalls",
		DefaultSeverity:  SeverityWarning,
		SecuritySeverity: "5.0",
		Summary:          "Deny-by-default VMM or virtiofsd profile omits mandatory syscalls required to boot Cloud Hypervisor or serve virtio-fs requests.",
		Remediation:      "Ensure VMM profiles allow ioctl, futex, eventfd2, epoll_pwait/epoll_wait, and pread64/pwrite64; virtiofsd requires futex, epoll_pwait, openat/openat2, and preadv/pwritev.",
	},
}

// SyscallArg represents an OCI seccomp syscall argument comparison filter.
type SyscallArg struct {
	Index    uint   `json:"index"`
	Value    uint64 `json:"value"`
	ValueTwo uint64 `json:"valueTwo,omitempty"`
	Op       string `json:"op"`
}

// SyscallRule represents an OCI seccomp syscall block.
type SyscallRule struct {
	Names  []string     `json:"names"`
	Action string       `json:"action"`
	Args   []SyscallArg `json:"args,omitempty"`
}

// ArchEntry represents an OCI archMap entry.
type ArchEntry struct {
	Architecture      string   `json:"architecture"`
	SubArchitectures  []string `json:"subArchitectures,omitempty"`
}

// OCIProfile models standard OCI / Docker Linux seccomp JSON profiles and
// optional Cloud Hypervisor thread-scoped rule sets.
type OCIProfile struct {
	DefaultAction string                         `json:"defaultAction,omitempty"`
	Architectures []string                       `json:"architectures,omitempty"`
	ArchMap       []ArchEntry                    `json:"archMap,omitempty"`
	Syscalls      []SyscallRule                  `json:"syscalls,omitempty"`
	Threads       map[string]ThreadSeccompPolicy `json:"threads,omitempty"`
}

// ThreadSeccompPolicy models a rust-vmm / Cloud Hypervisor per-thread filter.
type ThreadSeccompPolicy struct {
	DefaultAction string       `json:"default_action"`
	Arch          string       `json:"arch,omitempty"`
	Rules         []CHRuleItem `json:"rules"`
}

// CHRuleItem models a single syscall rule in a thread-scoped VMM policy.
type CHRuleItem struct {
	Syscall string       `json:"syscall"`
	Action  string       `json:"action,omitempty"`
	Args    []SyscallArg `json:"args,omitempty"`
}

// Finding represents a single security or operational diagnostic produced by AuditProfile.
type Finding struct {
	RuleID      string   `json:"rule_id"`
	RuleName    string   `json:"rule_name"`
	Severity    string   `json:"severity"`
	Scope       string   `json:"scope"`
	Syscalls    []string `json:"syscalls,omitempty"`
	Message     string   `json:"message"`
	Remediation string   `json:"remediation"`
	Fingerprint string   `json:"fingerprint"`
}

// AuditReport summarizes the static analysis of a seccomp profile.
type AuditReport struct {
	SourcePath        string     `json:"source_path"`
	Target            TargetRole `json:"target"`
	Format            string     `json:"format"` // "oci" or "vmm-threads"
	DefaultAction     string     `json:"default_action"`
	Architectures     []string   `json:"architectures"`
	AllowedCount      int        `json:"allowed_syscall_count"`
	HighRiskCount     int        `json:"high_risk_syscall_count"`
	ErrorCount        int        `json:"error_count"`
	WarningCount      int        `json:"warning_count"`
	PostureScore      int        `json:"posture_score"`
	Passed            bool       `json:"passed"`
	Findings          []Finding  `json:"findings"`
}

type allowedSyscallState struct {
	allowed       bool
	hasArgGuard   bool
	unconditional bool
}

func isAllowAction(action string) bool {
	switch strings.ToUpper(strings.TrimSpace(action)) {
	case "SCMP_ACT_ALLOW", "SCMP_ACT_LOG", "ALLOW", "LOG":
		return true
	default:
		return false
	}
}

// AuditProfile parses raw JSON bytes and evaluates rules OVM-SEC-001..010.
func AuditProfile(raw []byte, sourcePath string, target TargetRole) (*AuditReport, error) {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil, errors.New("seccomp profile JSON is empty")
	}
	if target == "" {
		target = TargetWorkload
	}
	if sourcePath == "" {
		sourcePath = "seccomp.json"
	}

	var prof OCIProfile
	if err := json.Unmarshal(raw, &prof); err != nil {
		return nil, fmt.Errorf("parse seccomp profile JSON: %w", err)
	}
	if prof.DefaultAction == "" && len(prof.Syscalls) == 0 && len(prof.Threads) == 0 {
		return nil, errors.New("seccomp profile contains neither OCI syscalls/defaultAction nor VMM threads map")
	}

	if len(prof.Threads) > 0 && prof.DefaultAction == "" {
		return auditThreadProfile(&prof, sourcePath, target), nil
	}
	return auditOCIProfile(&prof, sourcePath, target), nil
}

func collectArchs(prof *OCIProfile) []string {
	seen := make(map[string]struct{})
	var out []string
	add := func(a string) {
		u := strings.ToUpper(strings.TrimSpace(a))
		if u == "" {
			return
		}
		if _, ok := seen[u]; !ok {
			seen[u] = struct{}{}
			out = append(out, u)
		}
	}
	for _, a := range prof.Architectures {
		add(a)
	}
	for _, m := range prof.ArchMap {
		add(m.Architecture)
		for _, sub := range m.SubArchitectures {
			add(sub)
		}
	}
	return out
}

func auditOCIProfile(prof *OCIProfile, sourcePath string, target TargetRole) *AuditReport {
	archs := collectArchs(prof)
	stateMap := make(map[string]*allowedSyscallState)

	for _, rule := range prof.Syscalls {
		if !isAllowAction(rule.Action) {
			continue
		}
		guarded := len(rule.Args) > 0
		for _, n := range rule.Names {
			sc := strings.ToLower(strings.TrimSpace(n))
			if sc == "" {
				continue
			}
			st := stateMap[sc]
			if st == nil {
				st = &allowedSyscallState{}
				stateMap[sc] = st
			}
			st.allowed = true
			if guarded {
				st.hasArgGuard = true
			} else {
				st.unconditional = true
			}
		}
	}

	var findings []Finding
	findings = append(findings, evaluateScopeRules("root", prof.DefaultAction, archs, stateMap, target, sourcePath)...)

	return finalizeReport(sourcePath, target, "oci", prof.DefaultAction, archs, stateMap, findings)
}

func auditThreadProfile(prof *OCIProfile, sourcePath string, target TargetRole) *AuditReport {
	if target == TargetWorkload {
		target = TargetVMM
	}
	threadNames := make([]string, 0, len(prof.Threads))
	for name := range prof.Threads {
		threadNames = append(threadNames, name)
	}
	sort.Strings(threadNames)

	unionState := make(map[string]*allowedSyscallState)
	archSet := make(map[string]struct{})
	var archs []string
	var findings []Finding
	defaultActSummary := "deny"

	for _, tName := range threadNames {
		tp := prof.Threads[tName]
		if isAllowAction(tp.DefaultAction) {
			defaultActSummary = tp.DefaultAction
		}
		var tArchs []string
		if tp.Arch != "" {
			u := strings.ToUpper(strings.TrimSpace(tp.Arch))
			tArchs = []string{u}
			if _, ok := archSet[u]; !ok {
				archSet[u] = struct{}{}
				archs = append(archs, u)
			}
		} else if len(prof.Architectures) > 0 {
			tArchs = collectArchs(prof)
			archs = tArchs
		}

		tState := make(map[string]*allowedSyscallState)
		for _, r := range tp.Rules {
			act := r.Action
			if act == "" {
				act = "SCMP_ACT_ALLOW"
			}
			if !isAllowAction(act) {
				continue
			}
			sc := strings.ToLower(strings.TrimSpace(r.Syscall))
			if sc == "" {
				continue
			}
			guarded := len(r.Args) > 0
			for _, m := range []map[string]*allowedSyscallState{tState, unionState} {
				st := m[sc]
				if st == nil {
					st = &allowedSyscallState{}
					m[sc] = st
				}
				st.allowed = true
				if guarded {
					st.hasArgGuard = true
				} else {
					st.unconditional = true
				}
			}
		}

		role := target
		if strings.Contains(strings.ToLower(tName), "fs") {
			role = TargetVirtiofsd
		}
		defAct := tp.DefaultAction
		if defAct == "" {
			defAct = "SCMP_ACT_KILL_THREAD"
		}
		findings = append(findings, evaluateScopeRules("thread:"+tName, defAct, tArchs, tState, role, sourcePath)...)
	}

	return finalizeReport(sourcePath, target, "vmm-threads", defaultActSummary, archs, unionState, findings)
}

var (
	kernelLoadSyscalls = []string{"init_module", "finit_module", "delete_module", "kexec_load", "kexec_file_load", "create_module", "uselib"}
	bpfPerfSyscalls    = []string{"bpf", "perf_event_open", "userfaultfd"}
	ptraceSyscalls     = []string{"ptrace", "process_vm_readv", "process_vm_writev", "kcmp"}
	mountNsSyscalls    = []string{"open_by_handle_at", "mount", "umount", "umount2", "pivot_root", "chroot", "unshare", "setns"}
	keyringClockCalls  = []string{"add_key", "request_key", "keyctl", "settimeofday", "clock_settime", "clock_adjtime", "adjtimex"}
)

func matchAllowed(stateMap map[string]*allowedSyscallState, candidates []string, requireUnconditional bool) []string {
	var hits []string
	for _, c := range candidates {
		if st, ok := stateMap[c]; ok && st.allowed {
			if requireUnconditional && !st.unconditional {
				continue
			}
			hits = append(hits, c)
		}
	}
	return hits
}

func ruleByID(id string) RuleMeta {
	for _, r := range Catalog {
		if r.ID == id {
			return r
		}
	}
	return RuleMeta{ID: id}
}

func makeFinding(ruleID, severity, scope, sourcePath, msg string, syscalls []string) Finding {
	meta := ruleByID(ruleID)
	if severity == "" {
		severity = meta.DefaultSeverity
	}
	h := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%s|%s", sourcePath, ruleID, scope, strings.Join(syscalls, ","))))
	return Finding{
		RuleID:      ruleID,
		RuleName:    meta.Name,
		Severity:    severity,
		Scope:       scope,
		Syscalls:    syscalls,
		Message:     msg,
		Remediation: meta.Remediation,
		Fingerprint: hex.EncodeToString(h[:12]),
	}
}

func evaluateScopeRules(
	scope string,
	defaultAction string,
	archs []string,
	stateMap map[string]*allowedSyscallState,
	target TargetRole,
	sourcePath string,
) []Finding {
	var out []Finding

	// OVM-SEC-001: Permissive default action
	if defaultAction == "" || isAllowAction(defaultAction) {
		actLabel := defaultAction
		if actLabel == "" {
			actLabel = "<unset>"
		}
		out = append(out, makeFinding(
			"OVM-SEC-001", SeverityError, scope, sourcePath,
			fmt.Sprintf("Scope %s sets defaultAction=%s instead of a fail-closed deny action (SCMP_ACT_ERRNO or SCMP_ACT_KILL_PROCESS).", scope, actLabel),
			nil,
		))
	}

	// OVM-SEC-002: Kernel module & kexec syscalls
	if hits := matchAllowed(stateMap, kernelLoadSyscalls, false); len(hits) > 0 {
		out = append(out, makeFinding(
			"OVM-SEC-002", SeverityError, scope, sourcePath,
			fmt.Sprintf("Scope %s permits ring-0 kernel load/replacement syscalls: %s.", scope, strings.Join(hits, ", ")),
			hits,
		))
	}

	// OVM-SEC-003: Unrestricted bpf / perf_event_open / userfaultfd
	if hits := matchAllowed(stateMap, bpfPerfSyscalls, true); len(hits) > 0 {
		out = append(out, makeFinding(
			"OVM-SEC-003", SeverityError, scope, sourcePath,
			fmt.Sprintf("Scope %s permits unguarded kernel JIT/fault-handling syscalls: %s.", scope, strings.Join(hits, ", ")),
			hits,
		))
	}

	// OVM-SEC-004: Cross-process ptrace / VM memory read-write
	if hits := matchAllowed(stateMap, ptraceSyscalls, false); len(hits) > 0 {
		out = append(out, makeFinding(
			"OVM-SEC-004", SeverityError, scope, sourcePath,
			fmt.Sprintf("Scope %s permits cross-process memory inspection syscalls: %s.", scope, strings.Join(hits, ", ")),
			hits,
		))
	}

	// OVM-SEC-005: Mount, namespace, and open_by_handle_at escapes
	if hits := matchAllowed(stateMap, mountNsSyscalls, false); len(hits) > 0 {
		if target == TargetVirtiofsd {
			// virtiofsd setup may legitimately use pivot_root/mount/umount2 if open_by_handle_at is strictly careful;
			// open_by_handle_at without setup isolation is warned, and unshare/setns post-sandbox is warned.
			sev := SeverityWarning
			for _, h := range hits {
				if h == "open_by_handle_at" {
					sev = SeverityError
					break
				}
			}
			out = append(out, makeFinding(
				"OVM-SEC-005", sev, scope, sourcePath,
				fmt.Sprintf("virtiofsd scope %s exposes host mount/handle syscalls (%s); restrict to pre-sandbox setup or enable --sandbox=namespace with landlock.", scope, strings.Join(hits, ", ")),
				hits,
			))
		} else {
			out = append(out, makeFinding(
				"OVM-SEC-005", SeverityError, scope, sourcePath,
				fmt.Sprintf("Scope %s permits filesystem/namespace escape syscalls: %s.", scope, strings.Join(hits, ", ")),
				hits,
			))
		}
	}

	// OVM-SEC-006: Unfiltered ioctl on VMM / virtio worker profiles
	if target == TargetVMM || strings.HasPrefix(scope, "thread:") {
		if st, ok := stateMap["ioctl"]; ok && st.unconditional {
			out = append(out, makeFinding(
				"OVM-SEC-006", SeverityWarning, scope, sourcePath,
				fmt.Sprintf("Hypervisor scope %s allows ioctl unconditionally without KVM/vhost request-code argument filtering.", scope),
				[]string{"ioctl"},
			))
		}
	}

	// OVM-SEC-007: Fileless memfd_create + execveat chain or unguarded VMM mmap/mprotect
	var wxHits []string
	if hasSyscall(stateMap, "memfd_create") && (hasSyscall(stateMap, "execveat") || hasSyscall(stateMap, "execve")) {
		wxHits = append(wxHits, "memfd_create", "execveat")
	}
	if target == TargetVMM {
		for _, m := range []string{"mmap", "mprotect"} {
			if st, ok := stateMap[m]; ok && st.unconditional {
				wxHits = append(wxHits, m)
			}
		}
	}
	if len(wxHits) > 0 {
		out = append(out, makeFinding(
			"OVM-SEC-007", SeverityWarning, scope, sourcePath,
			fmt.Sprintf("Scope %s permits executable memory or fileless execution primitives without W^X argument masks: %s.", scope, strings.Join(wxHits, ", ")),
			wxHits,
		))
	}

	// OVM-SEC-008: Host keyring and clock mutation
	if hits := matchAllowed(stateMap, keyringClockCalls, false); len(hits) > 0 {
		out = append(out, makeFinding(
			"OVM-SEC-008", SeverityWarning, scope, sourcePath,
			fmt.Sprintf("Scope %s permits host keyring or clock mutation syscalls: %s.", scope, strings.Join(hits, ", ")),
			hits,
		))
	}

	// OVM-SEC-009: Missing architecture pinning or 64-bit/32-bit ABI mixing
	if len(archs) == 0 {
		out = append(out, makeFinding(
			"OVM-SEC-009", SeverityWarning, scope, sourcePath,
			fmt.Sprintf("Scope %s omits architecture pinning, leaving 32-bit/x32 compatibility syscall ABI numbers unguarded.", scope),
			nil,
		))
	} else if has64And32BitArchMix(archs) {
		out = append(out, makeFinding(
			"OVM-SEC-009", SeverityWarning, scope, sourcePath,
			fmt.Sprintf("Scope %s mixes 64-bit and legacy 32-bit architectures (%s) in one profile, risking syscall number confusion.", scope, strings.Join(archs, ", ")),
			nil,
		))
	}

	// OVM-SEC-010: Mandatory VMM / virtiofsd runtime baseline completeness (only when default is deny)
	if defaultAction != "" && !isAllowAction(defaultAction) && (scope == "root" || scope == "thread:vmm") {
		var missing []string
		switch target {
		case TargetVMM:
			if !hasSyscall(stateMap, "ioctl") {
				missing = append(missing, "ioctl")
			}
			if !hasSyscall(stateMap, "futex") {
				missing = append(missing, "futex")
			}
			if !hasSyscall(stateMap, "eventfd2") && !hasSyscall(stateMap, "eventfd") {
				missing = append(missing, "eventfd2")
			}
			if !hasSyscall(stateMap, "epoll_pwait") && !hasSyscall(stateMap, "epoll_wait") {
				missing = append(missing, "epoll_pwait")
			}
			if !hasSyscall(stateMap, "pread64") && !hasSyscall(stateMap, "readv") && !hasSyscall(stateMap, "read") {
				missing = append(missing, "pread64")
			}
		case TargetVirtiofsd:
			if !hasSyscall(stateMap, "futex") {
				missing = append(missing, "futex")
			}
			if !hasSyscall(stateMap, "epoll_pwait") && !hasSyscall(stateMap, "epoll_wait") {
				missing = append(missing, "epoll_pwait")
			}
			if !hasSyscall(stateMap, "openat2") && !hasSyscall(stateMap, "openat") {
				missing = append(missing, "openat2")
			}
			if !hasSyscall(stateMap, "preadv") && !hasSyscall(stateMap, "pread64") && !hasSyscall(stateMap, "read") {
				missing = append(missing, "preadv")
			}
		}
		if len(missing) > 0 {
			out = append(out, makeFinding(
				"OVM-SEC-010", SeverityWarning, scope, sourcePath,
				fmt.Sprintf("Deny-by-default %s profile omits mandatory runtime syscalls (%s) and will abort at microVM startup.", target, strings.Join(missing, ", ")),
				missing,
			))
		}
	}

	return out
}

func hasSyscall(m map[string]*allowedSyscallState, name string) bool {
	st, ok := m[name]
	return ok && st.allowed
}

func has64And32BitArchMix(archs []string) bool {
	has64 := false
	has32 := false
	for _, a := range archs {
		switch a {
		case "SCMP_ARCH_X86_64", "X86_64", "SCMP_ARCH_AARCH64", "AARCH64":
			has64 = true
		case "SCMP_ARCH_X86", "X86", "I386", "SCMP_ARCH_X32", "X32", "SCMP_ARCH_ARM", "ARM":
			has32 = true
		}
	}
	return has64 && has32
}

func finalizeReport(
	sourcePath string,
	target TargetRole,
	format string,
	defaultAction string,
	archs []string,
	stateMap map[string]*allowedSyscallState,
	findings []Finding,
) *AuditReport {
	highRiskSet := make(map[string]struct{})
	for _, list := range [][]string{kernelLoadSyscalls, bpfPerfSyscalls, ptraceSyscalls, mountNsSyscalls, keyringClockCalls} {
		for _, sc := range list {
			if hasSyscall(stateMap, sc) {
				highRiskSet[sc] = struct{}{}
			}
		}
	}

	errCount := 0
	warnCount := 0
	penalty := 0
	for _, f := range findings {
		switch f.Severity {
		case SeverityError:
			errCount++
			penalty += 25
		case SeverityWarning:
			warnCount++
			penalty += 10
		default:
			penalty += 3
		}
	}
	score := 100 - penalty
	if score < 0 {
		score = 0
	}

	return &AuditReport{
		SourcePath:    sourcePath,
		Target:        target,
		Format:        format,
		DefaultAction: defaultAction,
		Architectures: archs,
		AllowedCount:  len(stateMap),
		HighRiskCount: len(highRiskSet),
		ErrorCount:    errCount,
		WarningCount:  warnCount,
		PostureScore:  score,
		Passed:        errCount == 0,
		Findings:      findings,
	}
}

// GenerateBaselineProfile synthesizes a hardened fail-closed (SCMP_ACT_ERRNO)
// OCI seccomp profile tailored to target ("workload", "vmm", or "virtiofsd").
func GenerateBaselineProfile(target TargetRole, arch string) *OCIProfile {
	if target == "" {
		target = TargetWorkload
	}
	normArch := strings.ToUpper(strings.TrimSpace(arch))
	switch normArch {
	case "", "AMD64", "X86_64", "SCMP_ARCH_X86_64":
		normArch = "SCMP_ARCH_X86_64"
	case "ARM64", "AARCH64", "SCMP_ARCH_AARCH64":
		normArch = "SCMP_ARCH_AARCH64"
	}

	switch target {
	case TargetVMM:
		return &OCIProfile{
			DefaultAction: "SCMP_ACT_ERRNO",
			Architectures: []string{normArch},
			Syscalls: []SyscallRule{
				{
					Names: []string{
						"accept4", "bind", "brk", "clock_gettime", "clone", "clone3", "close",
						"connect", "dup", "epoll_create1", "epoll_ctl", "epoll_pwait", "eventfd2",
						"exit", "exit_group", "fallocate", "fcntl", "fdatasync", "fstat", "fsync",
						"ftruncate", "futex", "getpid", "getrandom", "getsockname", "gettid",
						"listen", "lseek", "madvise", "newfstatat", "openat", "pipe2", "poll",
						"ppoll", "prctl", "pread64", "preadv", "pwrite64", "pwritev", "read",
						"readv", "recvfrom", "recvmsg", "rt_sigaction", "rt_sigprocmask",
						"rt_sigreturn", "sched_getaffinity", "sched_yield", "sendmsg", "sendto",
						"set_robust_list", "setsockopt", "shutdown", "sigaltstack", "socket",
						"socketpair", "statx", "tgkill", "timerfd_create", "timerfd_settime",
						"unlinkat", "write", "writev",
					},
					Action: "SCMP_ACT_ALLOW",
				},
				{
					// Guard ioctl with KVM type byte 0xAE (KVMIO = 0xAE << 8 mask check on low 16 bits)
					Names:  []string{"ioctl"},
					Action: "SCMP_ACT_ALLOW",
					Args: []SyscallArg{
						{Index: 1, Value: 0x0000FF00, ValueTwo: 0x0000AE00, Op: "SCMP_CMP_MASKED_EQ"},
					},
				},
				{
					// Enforce W^X on mmap/mprotect: forbid PROT_EXEC (0x4) on guest/VMM heap mappings
					Names:  []string{"mmap", "mprotect"},
					Action: "SCMP_ACT_ALLOW",
					Args: []SyscallArg{
						{Index: 2, Value: 0x4, ValueTwo: 0x0, Op: "SCMP_CMP_MASKED_EQ"},
					},
				},
			},
		}

	case TargetVirtiofsd:
		return &OCIProfile{
			DefaultAction: "SCMP_ACT_ERRNO",
			Architectures: []string{normArch},
			Syscalls: []SyscallRule{
				{
					Names: []string{
						"brk", "capget", "capset", "clock_gettime", "clone", "close", "copy_file_range",
						"epoll_create1", "epoll_ctl", "epoll_pwait", "eventfd2", "exit", "exit_group",
						"fallocate", "fchdir", "fchmod", "fchmodat", "fchown", "fchownat", "fcntl",
						"fdatasync", "fgetxattr", "flistxattr", "flock", "fremovexattr", "fsetxattr",
						"fstat", "fstatfs", "fsync", "ftruncate", "futex", "getdents64", "getegid",
						"geteuid", "getpid", "getrandom", "gettid", "linkat", "lseek", "madvise",
						"mkdirat", "mknodat", "mmap", "mprotect", "munmap", "newfstatat", "openat2",
						"prctl", "pread64", "preadv", "pwrite64", "pwritev", "read", "readlinkat",
						"recvmsg", "renameat2", "rt_sigaction", "rt_sigprocmask", "rt_sigreturn",
						"sched_yield", "sendmsg", "set_robust_list", "sigaltstack", "statx",
						"symlinkat", "syncfs", "tgkill", "umask", "unlinkat", "utimensat", "write", "writev",
					},
					Action: "SCMP_ACT_ALLOW",
				},
			},
		}

	default:
		return &OCIProfile{
			DefaultAction: "SCMP_ACT_ERRNO",
			Architectures: []string{normArch},
			Syscalls: []SyscallRule{
				{
					Names: []string{
						"accept4", "access", "arch_prctl", "bind", "brk", "chdir", "chmod",
						"clock_getres", "clock_gettime", "clock_nanosleep", "clone", "clone3",
						"close", "connect", "dup", "dup2", "dup3", "epoll_create1", "epoll_ctl",
						"epoll_pwait", "eventfd2", "execve", "exit", "exit_group", "faccessat2",
						"fadvise64", "fallocate", "fcntl", "fdatasync", "flock", "fstat", "fsync",
						"ftruncate", "futex", "getcwd", "getdents64", "getegid", "geteuid", "getgid",
						"getpeername", "getpgid", "getpid", "getppid", "getrandom", "getrlimit",
						"getsockname", "getsockopt", "gettid", "getuid", "ioctl", "kill", "listen",
						"lseek", "madvise", "mkdirat", "mmap", "mprotect", "munmap", "nanosleep",
						"newfstatat", "openat", "pipe2", "poll", "ppoll", "prctl", "pread64",
						"prlimit64", "pwrite64", "read", "readlinkat", "readv", "recvfrom",
						"recvmsg", "renameat2", "rseq", "rt_sigaction", "rt_sigprocmask",
						"rt_sigreturn", "sched_getaffinity", "sched_yield", "sendmsg", "sendto",
						"set_robust_list", "set_tid_address", "setsockopt", "shutdown",
						"sigaltstack", "socket", "socketpair", "statx", "symlinkat", "tgkill",
						"uname", "unlinkat", "utimensat", "wait4", "write", "writev",
					},
					Action: "SCMP_ACT_ALLOW",
				},
			},
		}
	}
}
