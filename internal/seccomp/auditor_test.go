package seccomp

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestGenerateBaselineProfiles_ZeroAuditFindings(t *testing.T) {
	for _, role := range []TargetRole{TargetWorkload, TargetVMM, TargetVirtiofsd} {
		prof := GenerateBaselineProfile(role, "SCMP_ARCH_X86_64")
		// Trim any accidental whitespace in syscall names first and verify clean audit.
		for i := range prof.Syscalls {
			for j := range prof.Syscalls[i].Names {
				prof.Syscalls[i].Names[j] = strings.TrimSpace(prof.Syscalls[i].Names[j])
			}
		}
		raw, err := json.Marshal(prof)
		if err != nil {
			t.Fatalf("marshal %s baseline: %v", role, err)
		}
		rep, err := AuditProfile(raw, string(role)+"-baseline.json", role)
		if err != nil {
			t.Fatalf("AuditProfile(%s): %v", role, err)
		}
		if len(rep.Findings) != 0 || rep.PostureScore != 100 || !rep.Passed {
			t.Fatalf("expected clean 100-score baseline for %s, got score=%d findings=%+v",
				role, rep.PostureScore, rep.Findings)
		}
	}
}

func TestAuditProfile_DetectsAllEscapeAndMisconfigurationClasses(t *testing.T) {
	insecureJSON := `{
		"defaultAction": "SCMP_ACT_ALLOW",
		"architectures": ["SCMP_ARCH_X86_64", "SCMP_ARCH_X86"],
		"syscalls": [
			{
				"names": [
					"finit_module",
					"bpf",
					"userfaultfd",
					"ptrace",
					"open_by_handle_at",
					"mount",
					"ioctl",
					"memfd_create",
					"execveat",
					"keyctl",
					"clock_settime"
				],
				"action": "SCMP_ACT_ALLOW"
			}
		]
	}`

	rep, err := AuditProfile([]byte(insecureJSON), "insecure-vmm.json", TargetVMM)
	if err != nil {
		t.Fatalf("AuditProfile: %v", err)
	}
	if rep.Passed {
		t.Fatalf("expected Passed=false on insecure profile")
	}

	gotRules := make(map[string]bool)
	for _, f := range rep.Findings {
		gotRules[f.RuleID] = true
	}
	for _, want := range []string{
		"OVM-SEC-001",
		"OVM-SEC-002",
		"OVM-SEC-003",
		"OVM-SEC-004",
		"OVM-SEC-005",
		"OVM-SEC-006",
		"OVM-SEC-007",
		"OVM-SEC-008",
		"OVM-SEC-009",
	} {
		if !gotRules[want] {
			t.Errorf("missing expected rule %s in findings: %+v", want, rep.Findings)
		}
	}

	sarifDoc := ToSARIF(rep, "v0.8.0")
	if sarifDoc.Version != "2.1.0" || len(sarifDoc.Runs) != 1 {
		t.Fatalf("unexpected SARIF envelope: %+v", sarifDoc)
	}
	if len(sarifDoc.Runs[0].Tool.Driver.Rules) != 10 {
		t.Errorf("driver rules = %d, want 10", len(sarifDoc.Runs[0].Tool.Driver.Rules))
	}
	if len(sarifDoc.Runs[0].Results) != len(rep.Findings) {
		t.Errorf("SARIF results = %d, want %d", len(sarifDoc.Runs[0].Results), len(rep.Findings))
	}
}

func TestAuditProfile_DetectsMissingMandatoryVMMSyscallsAndThreadFormat(t *testing.T) {
	// Deny-by-default VMM thread profile missing futex/eventfd2/epoll_pwait and leaving vcpu ioctl unfiltered.
	chThreadsJSON := `{
		"threads": {
			"vmm": {
				"default_action": "SCMP_ACT_KILL_THREAD",
				"arch": "SCMP_ARCH_X86_64",
				"rules": [
					{"syscall": "read"},
					{"syscall": "write"}
				]
			},
			"vcpu": {
				"default_action": "SCMP_ACT_KILL_THREAD",
				"arch": "SCMP_ARCH_X86_64",
				"rules": [
					{"syscall": "ioctl"}
				]
			}
		}
	}`

	rep, err := AuditProfile([]byte(chThreadsJSON), "ch-threads.json", TargetVMM)
	if err != nil {
		t.Fatalf("AuditProfile threads: %v", err)
	}
	if rep.Format != "vmm-threads" {
		t.Errorf("Format = %q, want vmm-threads", rep.Format)
	}
	gotRules := make(map[string]bool)
	for _, f := range rep.Findings {
		gotRules[f.RuleID] = true
	}
	if !gotRules["OVM-SEC-006"] {
		t.Errorf("expected OVM-SEC-006 for unfiltered vcpu ioctl")
	}
	if !gotRules["OVM-SEC-010"] {
		t.Errorf("expected OVM-SEC-010 for missing mandatory vmm syscalls")
	}
}
