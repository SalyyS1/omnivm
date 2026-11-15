package registry

import (
	"path/filepath"
	"testing"
	"time"
)

func TestSnapshotsCRUDAndSourceLineage(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	now := time.Now().UTC().Truncate(time.Second)
	snap := &Snapshot{
		ID:               "snap-1001",
		Name:             "clean-deps",
		SourceInstanceID: "inst-base",
		SourceHandle:     "base-worker",
		ImageRef:         "python:3.12-alpine",
		Command:          []string{"python3", "/workspace/agent.py"},
		Env:              map[string]string{"MODE": "eval"},
		SecretKeys:       []string{"OPENAI_API_KEY"},
		ExposePorts:      []int{8080},
		MemoryMB:         512,
		VCPUs:            2,
		DiskMB:           1024,
		RootfsPath:       "/tmp/snapshots/snap-1001/rootfs.ext4",
		WorkspacePath:    "/tmp/snapshots/snap-1001/workspace",
		VMMStateDir:      "/tmp/snapshots/snap-1001/vmm",
		LogicalBytes:     1073741824,
		CoWSavedBytes:    1040187392,
		CreatedAt:        now,
	}

	if err := db.SaveSnapshot(snap); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}

	got, err := db.GetSnapshot("snap-1001")
	if err != nil || got == nil {
		t.Fatalf("GetSnapshot: err=%v got=%v", err, got)
	}
	if got.Name != "clean-deps" || got.SourceHandle != "base-worker" || got.CoWSavedBytes != 1040187392 {
		t.Errorf("unexpected snapshot round-trip: %+v", got)
	}
	if len(got.Command) != 2 || got.Env["MODE"] != "eval" || len(got.ExposePorts) != 1 {
		t.Errorf("unexpected command/env/ports round-trip: %+v", got)
	}

	byName, err := db.GetSnapshotByName("clean-deps")
	if err != nil || byName == nil || byName.ID != "snap-1001" {
		t.Fatalf("GetSnapshotByName: err=%v got=%+v", err, byName)
	}

	filtered, err := db.ListSnapshots("inst-base")
	if err != nil || len(filtered) != 1 {
		t.Fatalf("ListSnapshots(inst-base): len=%d err=%v", len(filtered), err)
	}
	other, err := db.ListSnapshots("inst-other")
	if err != nil || len(other) != 0 {
		t.Fatalf("ListSnapshots(inst-other): len=%d err=%v", len(other), err)
	}

	// Verify SourceSnapshotID persists on Instance records too.
	inst := &Instance{
		ID:               "inst-branch-1",
		State:            "stopped",
		Command:          got.Command,
		Handle:           "rollout-1",
		ParentID:         "inst-base",
		SourceSnapshotID: "snap-1001",
		Enabled:          true,
		CreatedAt:        now,
	}
	if err := db.SaveInstance(inst); err != nil {
		t.Fatalf("SaveInstance: %v", err)
	}
	gotInst, err := db.GetInstance("inst-branch-1")
	if err != nil || gotInst == nil {
		t.Fatalf("GetInstance: err=%v got=%v", err, gotInst)
	}
	if gotInst.SourceSnapshotID != "snap-1001" || gotInst.ParentID != "inst-base" {
		t.Errorf("SourceSnapshotID=%q ParentID=%q", gotInst.SourceSnapshotID, gotInst.ParentID)
	}

	if err := db.DeleteSnapshot("snap-1001"); err != nil {
		t.Fatalf("DeleteSnapshot: %v", err)
	}
	afterDel, err := db.GetSnapshot("snap-1001")
	if err != nil || afterDel != nil {
		t.Fatalf("expected nil after DeleteSnapshot, got %+v (err=%v)", afterDel, err)
	}
}
