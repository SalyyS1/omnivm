package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync/atomic"
	"time"

	"github.com/SalyyS1/omnivm/internal/overlay"
	"github.com/SalyyS1/omnivm/internal/registry"
	"github.com/SalyyS1/omnivm/internal/vmm"
)

// Snapshot aliases registry.Snapshot so control-plane callers share one canonical representation.
type Snapshot = registry.Snapshot

var (
	// ErrSnapshotNotFound is returned when a snapshot ID or name cannot be resolved.
	ErrSnapshotNotFound = errors.New("snapshot not found")
	// ErrSnapshotConflict is returned when a snapshot name collides with an existing snapshot.
	ErrSnapshotConflict = errors.New("snapshot name already exists")
	// ErrInstanceConflict is returned when a requested branch handle collides with an existing instance.
	ErrInstanceConflict = errors.New("instance handle already exists")
)

var validSnapshotName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

var snapshotIDSeq uint64

// SnapshotRequest configures CreateSnapshot.
type SnapshotRequest struct {
	Name            string `json:"name,omitempty"`
	IncludeVMMState bool   `json:"include_vmm_state,omitempty"`
}

// BranchRequest configures BranchInstance from either SnapshotID or SourceInstanceID.
type BranchRequest struct {
	SnapshotID       string            `json:"snapshot_id,omitempty"`
	SourceInstanceID string            `json:"source_instance_id,omitempty"`
	Handle           string            `json:"handle,omitempty"`
	Command          []string          `json:"command,omitempty"`
	Env              map[string]string `json:"env,omitempty"`
	Workspace        string            `json:"workspace,omitempty"`
	MemoryMB         int               `json:"memory_mb,omitempty"`
	VCPUs            int               `json:"vcpus,omitempty"`
	DiskMB           int               `json:"disk_mb,omitempty"`
	ExcludePorts     bool              `json:"exclude_ports,omitempty"`
	Start            bool              `json:"start"`
}

// BranchResult reports lineage and Copy-on-Write telemetry for a newly created branch instance.
type BranchResult struct {
	InstanceID       string `json:"id"`
	Handle           string `json:"handle,omitempty"`
	State            string `json:"state"`
	ParentID         string `json:"parent_id,omitempty"`
	SourceSnapshotID string `json:"source_snapshot_id,omitempty"`
	ImageRef         string `json:"image_ref,omitempty"`
	Workspace        string `json:"workspace,omitempty"`
	LogicalBytes     uint64 `json:"logical_bytes"`
	CoWSavedBytes    uint64 `json:"cow_saved_bytes"`
}

type overlayStatsProvider interface {
	Stats() overlay.OverlayStats
	RecordStats(overlay.CloneStats)
}

func (m *Manager) recordCloneStats(st overlay.CloneStats) {
	if m.overlay != nil {
		if p, ok := m.overlay.(overlayStatsProvider); ok {
			p.RecordStats(st)
			return
		}
	}
	atomic.AddUint64(&m.metrics.cowSavedBytes, st.SavedBytes)
}

func (m *Manager) cowSavedBytesTotal() uint64 {
	total := atomic.LoadUint64(&m.metrics.cowSavedBytes)
	if m.overlay != nil {
		if p, ok := m.overlay.(overlayStatsProvider); ok {
			total += p.Stats().SavedBytes
		}
	}
	return total
}

func (m *Manager) snapshotsDir() string {
	if m.cfg != nil && m.cfg.SnapshotsDir != "" {
		return m.cfg.SnapshotsDir
	}
	if m.cfg != nil && m.cfg.DataDir != "" {
		return filepath.Join(m.cfg.DataDir, "snapshots")
	}
	return filepath.Join(os.TempDir(), "omnivm-snapshots")
}

func (m *Manager) resolveInstance(idOrHandle string) *Instance {
	if idOrHandle == "" {
		return nil
	}
	if inst := m.GetInstance(idOrHandle); inst != nil {
		return inst
	}
	return m.GetInstanceByHandle(idOrHandle)
}

// RestoreSnapshots loads persisted snapshot metadata from the SQLite registry into memory.
func (m *Manager) RestoreSnapshots() (int, error) {
	if m.registry == nil {
		return 0, nil
	}
	list, err := m.registry.ListSnapshots("")
	if err != nil {
		return 0, err
	}
	m.mu.Lock()
	for _, s := range list {
		m.snapshots[s.ID] = s
	}
	m.mu.Unlock()
	return len(list), nil
}

// GetSnapshot resolves a snapshot by ID or unique name.
func (m *Manager) GetSnapshot(idOrName string) (*Snapshot, error) {
	if idOrName == "" {
		return nil, ErrSnapshotNotFound
	}
	m.mu.Lock()
	if s, ok := m.snapshots[idOrName]; ok {
		cp := cloneSnapshotStruct(s)
		m.mu.Unlock()
		return cp, nil
	}
	for _, s := range m.snapshots {
		if s.Name != "" && s.Name == idOrName {
			cp := cloneSnapshotStruct(s)
			m.mu.Unlock()
			return cp, nil
		}
	}
	m.mu.Unlock()

	if m.registry != nil {
		if s, err := m.registry.GetSnapshot(idOrName); err == nil && s != nil {
			m.mu.Lock()
			m.snapshots[s.ID] = s
			m.mu.Unlock()
			return cloneSnapshotStruct(s), nil
		}
		if s, err := m.registry.GetSnapshotByName(idOrName); err == nil && s != nil {
			m.mu.Lock()
			m.snapshots[s.ID] = s
			m.mu.Unlock()
			return cloneSnapshotStruct(s), nil
		}
	}
	return nil, ErrSnapshotNotFound
}

// ListSnapshots returns snapshots sorted newest-first, optionally filtered by source instance ID or handle.
func (m *Manager) ListSnapshots(instanceFilter string) []*Snapshot {
	resolvedFilter := instanceFilter
	if instanceFilter != "" {
		if inst := m.resolveInstance(instanceFilter); inst != nil {
			resolvedFilter = inst.ID
		}
	}

	m.mu.Lock()
	out := make([]*Snapshot, 0, len(m.snapshots))
	for _, s := range m.snapshots {
		if resolvedFilter != "" && s.SourceInstanceID != resolvedFilter && s.SourceHandle != instanceFilter {
			continue
		}
		out = append(out, cloneSnapshotStruct(s))
	}
	m.mu.Unlock()

	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID > out[j].ID
	})
	return out
}

// DeleteSnapshot removes snapshot artifacts on disk and deletes its metadata record.
func (m *Manager) DeleteSnapshot(idOrName string) error {
	snap, err := m.GetSnapshot(idOrName)
	if err != nil {
		return err
	}

	m.mu.Lock()
	delete(m.snapshots, snap.ID)
	m.mu.Unlock()

	if m.registry != nil {
		_ = m.registry.DeleteSnapshot(snap.ID)
	}

	snapDir := filepath.Join(m.snapshotsDir(), snap.ID)
	_ = os.RemoveAll(snapDir)
	return nil
}

// CreateSnapshot captures a point-in-time Copy-on-Write checkpoint of an instance's
// root disk, workspace tree, and (optionally) paused hypervisor state.
// If the source instance is currently RUNNING, CreateSnapshot transiently pauses
// the guest VM during the O(1) FICLONE / sparse clone and resumes it immediately.
func (m *Manager) CreateSnapshot(ctx context.Context, idOrHandle string, req SnapshotRequest) (*Snapshot, error) {
	inst := m.resolveInstance(idOrHandle)
	if inst == nil {
		return nil, fmt.Errorf("instance %q not found", idOrHandle)
	}

	if req.Name != "" {
		if !validSnapshotName.MatchString(req.Name) {
			return nil, fmt.Errorf("invalid snapshot name %q: must start with alphanumeric and contain only letters, digits, dots, dashes, or underscores", req.Name)
		}
		if existing, err := m.GetSnapshot(req.Name); err == nil && existing != nil {
			return nil, fmt.Errorf("%w: %s", ErrSnapshotConflict, req.Name)
		}
	}

	// Acquire exclusive transition freeze on inst so guest disk/workspace are quiescent.
	unfreeze, handle, frozenState, err := m.freezeInstanceForClone(ctx, inst)
	if err != nil {
		return nil, err
	}
	defer unfreeze()

	meta := inst.Snapshot()
	seq := atomic.AddUint64(&snapshotIDSeq, 1)
	snapID := fmt.Sprintf("snap-%d-%d", time.Now().UnixNano(), seq)
	snapDir := filepath.Join(m.snapshotsDir(), snapID)
	if err := os.MkdirAll(snapDir, 0700); err != nil {
		return nil, fmt.Errorf("create snapshot dir: %w", err)
	}

	var totalStats overlay.CloneStats
	var snapRootfs string
	if srcRoot, isBlock := m.locateInstanceRootfs(meta); srcRoot != "" {
		if isBlock {
			dstDisk := filepath.Join(snapDir, "rootfs.ext4")
			st, cErr := overlay.CloneFileCoW(srcRoot, dstDisk, 0644)
			if cErr != nil {
				_ = os.RemoveAll(snapDir)
				return nil, fmt.Errorf("clone root disk: %w", cErr)
			}
			snapRootfs = dstDisk
			addCloneStats(&totalStats, st)
		} else {
			dstTree := filepath.Join(snapDir, "rootfs")
			st, cErr := overlay.CloneTreeCoW(ctx, srcRoot, dstTree)
			if cErr != nil {
				_ = os.RemoveAll(snapDir)
				return nil, fmt.Errorf("clone rootfs tree: %w", cErr)
			}
			snapRootfs = dstTree
			addCloneStats(&totalStats, st)
		}
	}

	var snapWorkspace string
	if meta.WorkspacePath != "" {
		if info, sErr := os.Stat(meta.WorkspacePath); sErr == nil && info.IsDir() {
			dstWs := filepath.Join(snapDir, "workspace")
			st, cErr := overlay.CloneTreeCoW(ctx, meta.WorkspacePath, dstWs)
			if cErr != nil {
				_ = os.RemoveAll(snapDir)
				return nil, fmt.Errorf("clone workspace: %w", cErr)
			}
			snapWorkspace = dstWs
			addCloneStats(&totalStats, st)
		}
	}

	var vmmStateDir string
	if req.IncludeVMMState && (frozenState == StateRunning || frozenState == StatePaused) {
		if snapBackend, ok := m.vmm.(vmm.Snapshotter); ok {
			candidateDir := filepath.Join(snapDir, "vmm")
			if sErr := snapBackend.SnapshotVM(handle, candidateDir); sErr != nil {
				_ = os.RemoveAll(snapDir)
				return nil, fmt.Errorf("snapshot hypervisor state: %w", sErr)
			}
			vmmStateDir = candidateDir
		}
	}

	ports := make([]int, len(meta.ExposePorts))
	for i, ep := range meta.ExposePorts {
		ports[i] = ep.GuestPort
	}

	now := time.Now().UTC()
	snap := &Snapshot{
		ID:               snapID,
		Name:             req.Name,
		SourceInstanceID: meta.ID,
		SourceHandle:     meta.HandleAlias,
		ParentSnapshotID: meta.SourceSnapshotID,
		ImageRef:         meta.ImageRef,
		Command:          meta.Command,
		Env:              meta.Env,
		SecretKeys:       meta.SecretKeys,
		ExposePorts:      ports,
		MemoryMB:         meta.MemoryMB,
		VCPUs:            meta.VCPUs,
		DiskMB:           meta.DiskMB,
		RootfsPath:       snapRootfs,
		WorkspacePath:    snapWorkspace,
		VMMStateDir:      vmmStateDir,
		LogicalBytes:     totalStats.LogicalBytes,
		CoWSavedBytes:    totalStats.SavedBytes,
		CreatedAt:        now,
	}

	m.mu.Lock()
	m.snapshots[snap.ID] = snap
	m.mu.Unlock()

	if m.registry != nil {
		if err := m.registry.SaveSnapshot(snap); err != nil {
			log.Printf("save snapshot %s to registry: %v", snap.ID, err)
		}
	}

	m.recordCloneStats(totalStats)
	atomic.AddUint64(&m.metrics.snapshotsCreated, 1)
	log.Printf("snapshot %s created from instance %s (logical=%dB cow_saved=%dB)",
		snap.ID, meta.ID, totalStats.LogicalBytes, totalStats.SavedBytes)

	return cloneSnapshotStruct(snap), nil
}

// BranchInstance spawns an isolated Copy-on-Write microVM branch from either a
// Snapshot (req.SnapshotID) or a live/paused/stopped Instance (req.SourceInstanceID).
// When req.Start is false, the branch is registered in StateStopped (0 MB RAM)
// ready for scale-to-zero wake-on-request; when req.Start is true, the branch
// boots immediately under host resource admission quotas.
func (m *Manager) BranchInstance(ctx context.Context, req BranchRequest) (*Instance, *BranchResult, error) {
	if req.SnapshotID == "" && req.SourceInstanceID == "" {
		return nil, nil, errors.New("either snapshot_id or source_instance_id is required")
	}
	if req.Handle != "" {
		if !validSnapshotName.MatchString(req.Handle) {
			return nil, nil, fmt.Errorf("invalid handle %q: must start with alphanumeric and contain only letters, digits, dots, dashes, or underscores", req.Handle)
		}
		if existing := m.GetInstanceByHandle(req.Handle); existing != nil {
			return nil, nil, fmt.Errorf("%w: %s", ErrInstanceConflict, req.Handle)
		}
	}

	seq := atomic.AddUint64(&snapshotIDSeq, 1)
	branchID := fmt.Sprintf("inst-%d-%d", time.Now().UnixNano(), seq)

	var (
		parentInstID   string
		sourceSnapID   string
		imageRef       string
		baseCmd        []string
		baseEnv        map[string]string
		secretKeys     []string
		exposePorts    []vmm.PortExpose
		memMB          int
		vcpus          int
		diskMB         int
		srcRootfs      string
		srcIsBlock     bool
		srcWorkspace   string
		unfreezeSource func()
	)

	if req.SnapshotID != "" {
		snap, err := m.GetSnapshot(req.SnapshotID)
		if err != nil {
			return nil, nil, err
		}
		parentInstID = snap.SourceInstanceID
		sourceSnapID = snap.ID
		imageRef = snap.ImageRef
		baseCmd = snap.Command
		baseEnv = snap.Env
		secretKeys = snap.SecretKeys
		for _, p := range snap.ExposePorts {
			exposePorts = append(exposePorts, vmm.PortExpose{GuestPort: p, Protocol: "http"})
		}
		memMB = snap.MemoryMB
		vcpus = snap.VCPUs
		diskMB = snap.DiskMB
		if snap.RootfsPath != "" {
			if info, sErr := os.Stat(snap.RootfsPath); sErr == nil {
				srcRootfs = snap.RootfsPath
				srcIsBlock = !info.IsDir()
			}
		}
		srcWorkspace = snap.WorkspacePath
	} else {
		srcInst := m.resolveInstance(req.SourceInstanceID)
		if srcInst == nil {
			return nil, nil, fmt.Errorf("source instance %q not found", req.SourceInstanceID)
		}
		var fErr error
		unfreezeSource, _, _, fErr = m.freezeInstanceForClone(ctx, srcInst)
		if fErr != nil {
			return nil, nil, fErr
		}
		meta := srcInst.Snapshot()
		parentInstID = meta.ID
		sourceSnapID = meta.SourceSnapshotID
		imageRef = meta.ImageRef
		baseCmd = meta.Command
		baseEnv = meta.Env
		secretKeys = meta.SecretKeys
		exposePorts = meta.ExposePorts
		memMB = meta.MemoryMB
		vcpus = meta.VCPUs
		diskMB = meta.DiskMB
		srcRootfs, srcIsBlock = m.locateInstanceRootfs(meta)
		srcWorkspace = meta.WorkspacePath
	}

	var totalStats overlay.CloneStats
	var branchRootfs string
	var createdOverlayPaths []string

	cleanupCloneArtifacts := func(autoWs string) {
		for _, p := range createdOverlayPaths {
			_ = os.RemoveAll(p)
		}
		if autoWs != "" {
			_ = os.RemoveAll(autoWs)
		}
	}

	overlaysBase := ""
	if m.cfg != nil && m.cfg.OverlaysDir != "" {
		overlaysBase = m.cfg.OverlaysDir
	} else if m.cfg != nil && m.cfg.DataDir != "" {
		overlaysBase = filepath.Join(m.cfg.DataDir, "overlays")
	} else {
		overlaysBase = filepath.Join(os.TempDir(), "omnivm-overlays")
	}

	if srcRootfs != "" {
		if srcIsBlock {
			dstDisk := filepath.Join(overlaysBase, branchID) + ".ext4"
			st, cErr := overlay.CloneFileCoW(srcRootfs, dstDisk, 0644)
			if cErr != nil {
				if unfreezeSource != nil {
					unfreezeSource()
				}
				return nil, nil, fmt.Errorf("clone branch root disk: %w", cErr)
			}
			branchRootfs = dstDisk
			createdOverlayPaths = append(createdOverlayPaths, dstDisk)
			addCloneStats(&totalStats, st)
		} else {
			dstDir := filepath.Join(overlaysBase, branchID)
			st, cErr := overlay.CloneTreeCoW(ctx, srcRootfs, dstDir)
			if cErr != nil {
				if unfreezeSource != nil {
					unfreezeSource()
				}
				return nil, nil, fmt.Errorf("clone branch rootfs tree: %w", cErr)
			}
			branchRootfs = dstDir
			createdOverlayPaths = append(createdOverlayPaths, dstDir)
			addCloneStats(&totalStats, st)
		}
	}

	// Determine target workspace path and CoW-clone workspace files if present.
	wsDirBase := ""
	if m.cfg != nil && m.cfg.WorkspacesDir != "" {
		wsDirBase = m.cfg.WorkspacesDir
	} else if m.cfg != nil && m.cfg.DataDir != "" {
		wsDirBase = filepath.Join(m.cfg.DataDir, "workspaces")
	}

	targetWs := req.Workspace
	autoWorkspace := false
	if targetWs == "" && (srcWorkspace != "" || wsDirBase != "") {
		wsName := req.Handle
		if wsName == "" {
			wsName = branchID
		}
		if wsDirBase == "" {
			wsDirBase = filepath.Join(os.TempDir(), "omnivm-workspaces")
		}
		targetWs = filepath.Join(wsDirBase, wsName)
		autoWorkspace = true
	}

	if targetWs != "" {
		if srcWorkspace != "" {
			if info, sErr := os.Stat(srcWorkspace); sErr == nil && info.IsDir() {
				st, cErr := overlay.CloneTreeCoW(ctx, srcWorkspace, targetWs)
				if cErr != nil {
					if unfreezeSource != nil {
						unfreezeSource()
					}
					cleanupCloneArtifacts("")
					return nil, nil, fmt.Errorf("clone branch workspace: %w", cErr)
				}
				addCloneStats(&totalStats, st)
			} else {
				_ = os.MkdirAll(targetWs, 0755)
			}
		} else {
			_ = os.MkdirAll(targetWs, 0755)
		}
	}

	if unfreezeSource != nil {
		unfreezeSource()
		unfreezeSource = nil
	}

	finalCmd := baseCmd
	if len(req.Command) > 0 {
		finalCmd = make([]string, len(req.Command))
		copy(finalCmd, req.Command)
	}
	if len(finalCmd) == 0 {
		cleanupCloneArtifacts( boolAutoWs(autoWorkspace, targetWs) )
		return nil, nil, errors.New("branch command is empty")
	}

	mergedEnv := make(map[string]string)
	for k, v := range baseEnv {
		mergedEnv[k] = v
	}
	for k, v := range req.Env {
		mergedEnv[k] = v
	}

	if req.MemoryMB > 0 {
		memMB = req.MemoryMB
	}
	if req.VCPUs > 0 {
		vcpus = req.VCPUs
	}
	if req.DiskMB > 0 {
		diskMB = req.DiskMB
	}

	var branchPorts []vmm.PortExpose
	if !req.ExcludePorts && len(exposePorts) > 0 {
		branchPorts = make([]vmm.PortExpose, len(exposePorts))
		copy(branchPorts, exposePorts)
	}

	var opts []InstanceOption
	if req.Handle != "" {
		opts = append(opts, WithHandle(req.Handle))
	}
	if imageRef != "" {
		opts = append(opts, WithImageRef(imageRef))
	}
	if branchRootfs != "" {
		opts = append(opts, WithRootfs(branchRootfs))
	}
	if targetWs != "" {
		opts = append(opts, WithWorkspace(targetWs))
		if autoWorkspace {
			opts = append(opts, WithAutoWorkspace())
		}
	}
	if len(mergedEnv) > 0 {
		opts = append(opts, WithEnv(mergedEnv))
	}
	if len(secretKeys) > 0 {
		opts = append(opts, WithSecretKeys(secretKeys))
	}
	if memMB > 0 {
		opts = append(opts, WithMemory(memMB))
	}
	if vcpus > 0 {
		opts = append(opts, WithVCPUs(vcpus))
	}
	if diskMB > 0 {
		opts = append(opts, WithDiskMB(diskMB))
	}
	if parentInstID != "" {
		opts = append(opts, WithParentID(parentInstID))
	}
	if sourceSnapID != "" {
		opts = append(opts, WithSourceSnapshotID(sourceSnapID))
	}

	branchInst := m.CreateInstance(branchID, finalCmd, branchPorts, opts...)
	m.recordCloneStats(totalStats)
	atomic.AddUint64(&m.metrics.snapshotBranches, 1)

	if req.Start {
		if err := m.EnsureInstance(ctx, branchID); err != nil {
			// Clean up newly created branch if synchronous boot/admission was rejected.
			if m.onUnexposePort != nil {
				for _, ep := range branchPorts {
					m.onUnexposePort(branchID, ep.GuestPort)
				}
			}
			_ = m.DeleteInstance(branchID)
			if m.registry != nil {
				_ = m.registry.DeleteInstance(branchID)
			}
			cleanupCloneArtifacts(boolAutoWs(autoWorkspace, targetWs))
			return nil, nil, err
		}
	}

	snapView := branchInst.Snapshot()
	return branchInst, &BranchResult{
		InstanceID:       snapView.ID,
		Handle:           snapView.HandleAlias,
		State:            snapView.State,
		ParentID:         snapView.ParentID,
		SourceSnapshotID: snapView.SourceSnapshotID,
		ImageRef:         snapView.ImageRef,
		Workspace:        snapView.WorkspacePath,
		LogicalBytes:     totalStats.LogicalBytes,
		CoWSavedBytes:    totalStats.SavedBytes,
	}, nil
}

func boolAutoWs(auto bool, path string) string {
	if auto {
		return path
	}
	return ""
}

func addCloneStats(dst *overlay.CloneStats, src overlay.CloneStats) {
	dst.FilesCloned += src.FilesCloned
	dst.ReflinkFiles += src.ReflinkFiles
	dst.SparseFiles += src.SparseFiles
	dst.LogicalBytes += src.LogicalBytes
	dst.SavedBytes += src.SavedBytes
}

func (m *Manager) locateInstanceRootfs(meta InstanceSnapshot) (path string, isBlock bool) {
	candidates := []string{}
	if meta.RootfsPath != "" {
		candidates = append(candidates, meta.RootfsPath+".ext4", meta.RootfsPath)
	}
	if m.cfg != nil && m.cfg.OverlaysDir != "" {
		base := filepath.Join(m.cfg.OverlaysDir, meta.ID)
		candidates = append(candidates, base+".ext4", base)
	}
	for _, c := range candidates {
		info, err := os.Stat(c)
		if err == nil {
			return c, !info.IsDir()
		}
	}
	return "", false
}

// freezeInstanceForClone serializes against in-flight VMM transitions and, if
// inst is currently RUNNING, transiently pauses the guest VM during cloning and
// resumes it on release.
func (m *Manager) freezeInstanceForClone(ctx context.Context, inst *Instance) (release func(), handle vmm.Handle, state string, err error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, vmm.Handle{}, "", err
		}

		inst.mu.Lock()
		if inst.reaping || inst.pausing || inst.resuming || inst.State == StateStarting {
			waitCh := inst.waitChanLocked()
			inst.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, vmm.Handle{}, "", ctx.Err()
			case <-waitCh:
			}
			continue
		}

		curState := inst.State
		curHandle := inst.Handle
		// Hold inst.pausing = true while cloning so EnsureInstance / idle timers / eviction serialize cleanly.
		inst.pausing = true
		if inst.idleTimer != nil {
			inst.idleTimer.Stop()
			inst.idleTimer = nil
		}
		if inst.stopTimer != nil {
			inst.stopTimer.Stop()
			inst.stopTimer = nil
		}
		inst.mu.Unlock()

		if curState == StateRunning {
			if pErr := m.vmm.PauseVM(curHandle); pErr != nil {
				inst.mu.Lock()
				inst.pausing = false
				inst.broadcastLocked()
				inst.mu.Unlock()
				m.startIdleTimer(inst)
				return nil, vmm.Handle{}, "", fmt.Errorf("quiesce running VM %s for snapshot: %w", inst.ID, pErr)
			}
			return func() {
				if rErr := m.vmm.ResumeVM(curHandle); rErr != nil {
					log.Printf("instance %s: resume after snapshot failed: %v", inst.ID, rErr)
					inst.mu.Lock()
					inst.pausing = false
					inst.State = StatePaused
					inst.UpdatedAt = time.Now()
					inst.broadcastLocked()
					inst.mu.Unlock()
					m.notifyStateChange(inst.ID, StatePaused)
					return
				}
				inst.mu.Lock()
				inst.pausing = false
				inst.lastActivity = time.Now()
				inst.broadcastLocked()
				inst.mu.Unlock()
				m.startIdleTimer(inst)
			}, curHandle, StateRunning, nil
		}

		return func() {
			inst.mu.Lock()
			stillPaused := inst.State == StatePaused
			inst.pausing = false
			if stillPaused && m.vmm != nil && !m.vmm.Capabilities().PersistentPause && m.cfg != nil && m.cfg.StopAfterIdle > 0 {
				inst.stopTimer = time.AfterFunc(m.cfg.StopAfterIdle, func() {
					m.stopIdleInstance(inst)
				})
			}
			inst.broadcastLocked()
			inst.mu.Unlock()
		}, curHandle, curState, nil
	}
}

func cloneSnapshotStruct(s *Snapshot) *Snapshot {
	if s == nil {
		return nil
	}
	cp := *s
	if len(s.Command) > 0 {
		cp.Command = make([]string, len(s.Command))
		copy(cp.Command, s.Command)
	}
	if len(s.Env) > 0 {
		cp.Env = make(map[string]string, len(s.Env))
		for k, v := range s.Env {
			cp.Env[k] = v
		}
	}
	if len(s.SecretKeys) > 0 {
		cp.SecretKeys = make([]string, len(s.SecretKeys))
		copy(cp.SecretKeys, s.SecretKeys)
	}
	if len(s.ExposePorts) > 0 {
		cp.ExposePorts = make([]int, len(s.ExposePorts))
		copy(cp.ExposePorts, s.ExposePorts)
	}
	return &cp
}
