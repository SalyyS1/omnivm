package vmmjail

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestPlanJailAndBaselinePassAudit(t *testing.T) {
	tmp := t.TempDir()
	mgr := NewManager(Config{
		Mode:     ModeStrict,
		RootDir:  filepath.Join(tmp, "jails"),
		LeaseDir: filepath.Join(tmp, "leases"),
		UIDBase:  100000,
		PoolSize: 8,
	})

	sp1, err := mgr.PlanJail("vm-alpha", "tenant-a", 512, 2, filepath.Join(tmp, "alpha.ext4"), false)
	if err != nil {
		t.Fatalf("PlanJail vm-alpha: %v", err)
	}
	sp2, err := mgr.PlanJail("vm-beta", "tenant-b", 1024, 4, filepath.Join(tmp, "beta.ext4"), false)
	if err != nil {
		t.Fatalf("PlanJail vm-beta: %v", err)
	}
	if sp1.UID == sp2.UID {
		t.Fatalf("expected distinct UIDs, got %d", sp1.UID)
	}

	rep := AuditJailSpecs([]JailSpec{sp1, sp2}, "fleet-jails.json")
	if !rep.Passed || len(rep.Findings) != 0 {
		t.Fatalf("expected clean baseline audit, got %+v", rep)
	}

	if !mgr.ReleaseJail("vm-alpha") {
		t.Fatalf("expected ReleaseJail to return true")
	}
	if mgr.Metrics().ActiveJails != 1 {
		t.Fatalf("expected 1 active jail after release, got %d", mgr.Metrics().ActiveJails)
	}
}

func TestAuditDetectsAllEightRuleViolationsAndExportsSARIF(t *testing.T) {
	bad1 := JailSpec{
		InstanceID:        "vm-root",
		UID:               0,
		GID:               0,
		ChrootDir:         "/etc/omnivm",
		NoNewPrivs:        false,
		ClearBoundingCaps: false,
		RetainedCaps:      []string{"CAP_SYS_ADMIN"},
		KVMFDHandoff:      false,
		TAPFDHandoff:      false,
		SeccompProfile:    "unconfined",
		NetNamespace:      "host",
		MemoryMB:          512,
		RlimitASBytes:     128 * 1024 * 1024,
		RlimitNoFile:      0,
		RlimitNProc:       0,
		BindMounts: []BindMount{
			{Source: "/var/run/docker.sock", Target: "/docker.sock"},
		},
	}
	bad2 := GenerateBaselineJailSpec("vm-dup-a", "t1", 512, 1)
	bad2.UID = 100500
	bad2.GID = 100500
	bad3 := GenerateBaselineJailSpec("vm-dup-b", "t2", 512, 1)
	bad3.UID = 100500
	bad3.GID = 100500

	raw, _ := json.Marshal([]JailSpec{bad1, bad2, bad3})
	rep, err := AuditJailBytes(raw, "bad-jails.json")
	if err != nil {
		t.Fatalf("AuditJailBytes: %v", err)
	}
	if rep.Passed {
		t.Fatalf("expected audit failure on unsafe specs")
	}

	seen := map[string]bool{}
	for _, f := range rep.Findings {
		seen[f.RuleID] = true
	}
	for _, want := range []string{
		"OVM-JAIL-001", "OVM-JAIL-002", "OVM-JAIL-003", "OVM-JAIL-004",
		"OVM-JAIL-005", "OVM-JAIL-006", "OVM-JAIL-007", "OVM-JAIL-008",
	} {
		if !seen[want] {
			t.Errorf("expected rule %s in findings, got %v", want, seen)
		}
	}

	sarif := ToSARIF(rep, "v1.0.0")
	if sarif.Version != "2.1.0" || len(sarif.Runs) != 1 || len(sarif.Runs[0].Tool.Driver.Rules) != 8 {
		t.Fatalf("unexpected SARIF metadata: %+v", sarif)
	}
	if len(sarif.Runs[0].Results) != len(rep.Findings) {
		t.Fatalf("expected %d SARIF results, got %d", len(rep.Findings), len(sarif.Runs[0].Results))
	}
}

func TestSurvivalLeaseReconcilePreventsPIDReuseAndCleansOrphans(t *testing.T) {
	tmp := t.TempDir()
	mgr := NewManager(Config{
		Mode:     ModeStrict,
		RootDir:  filepath.Join(tmp, "jails"),
		LeaseDir: filepath.Join(tmp, "leases"),
		UIDBase:  100000,
		PoolSize: 32,
	})

	procTable := map[int]uint64{
		4101: 9912345, // live matching VMM
		4102: 8888888, // PID recycled by unrelated process (lease had 1111111)
	}
	liveSocks := map[string]bool{
		filepath.Join(tmp, "live.sock"): true,
	}
	mgr.SetProbes(
		func(pid int) (bool, uint64) {
			ticks, ok := procTable[pid]
			return ok, ticks
		},
		func(sock string) bool {
			return liveSocks[sock]
		},
	)

	// 1. Live matching VMM -> reattached
	if _, err := mgr.PersistLease(SurvivalLease{
		InstanceID:     "vm-live",
		PID:            4101,
		ProcStartTicks: 9912345,
		UID:            100001,
		GID:            100001,
		ChrootDir:      filepath.Join(tmp, "jails/vm-live/root"),
		APISocket:      filepath.Join(tmp, "live.sock"),
		MemoryMB:       512,
		VCPUs:          2,
	}); err != nil {
		t.Fatalf("persist vm-live: %v", err)
	}

	// 2. Recycled PID -> stale_pid_reused
	if _, err := mgr.PersistLease(SurvivalLease{
		InstanceID:     "vm-recycled",
		PID:            4102,
		ProcStartTicks: 1111111,
		UID:            100002,
		GID:            100002,
		ChrootDir:      filepath.Join(tmp, "jails/vm-recycled/root"),
		APISocket:      filepath.Join(tmp, "live.sock"),
		MemoryMB:       512,
		VCPUs:          1,
	}); err != nil {
		t.Fatalf("persist vm-recycled: %v", err)
	}

	// 3. Exited VMM -> orphaned_cleaned
	deadSock := filepath.Join(tmp, "dead.sock")
	_ = os.WriteFile(deadSock, []byte("s"), 0600)
	if _, err := mgr.PersistLease(SurvivalLease{
		InstanceID:     "vm-dead",
		PID:            4103,
		ProcStartTicks: 2222222,
		UID:            100003,
		GID:            100003,
		ChrootDir:      filepath.Join(tmp, "jails/vm-dead/root"),
		APISocket:      deadSock,
		MemoryMB:       256,
		VCPUs:          1,
	}); err != nil {
		t.Fatalf("persist vm-dead: %v", err)
	}

	// 4. Tampered lease -> tampered_rejected
	tamperedPath := filepath.Join(tmp, "leases", "vm-tampered.lease.json")
	_ = os.WriteFile(tamperedPath, []byte(`{"schema_version":1,"instance_id":"vm-tampered","pid":4101,"digest":"0000"}`), 0600)

	rep, err := mgr.ReconcileLeases()
	if err != nil {
		t.Fatalf("ReconcileLeases: %v", err)
	}
	if rep.CheckedLeases != 4 || rep.Reattached != 1 || rep.StalePIDReused != 1 || rep.OrphanedCleaned != 1 || rep.TamperedRejected != 1 {
		t.Fatalf("unexpected reconcile counts: %+v", rep)
	}
	if _, err := os.Stat(deadSock); !os.IsNotExist(err) {
		t.Fatalf("expected dead socket to be cleaned up")
	}
}

func TestParseProcStatStartTicksHandlesParensAndSpacesInComm(t *testing.T) {
	line := "4242 (cloud (hypervisor) vmm) S 1 4242 4242 0 -1 4194304 120 0 0 0 14 5 0 0 20 0 4 0 987654321 536870912 1024"
	ticks := ParseProcStatStartTicks(line)
	if ticks != 987654321 {
		t.Fatalf("expected 987654321, got %d", ticks)
	}
}
