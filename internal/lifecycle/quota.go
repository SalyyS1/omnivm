package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"sync/atomic"
	"time"

	"github.com/SalyyS1/omnivm/internal/vmm"
)

// ErrQuotaExceeded is returned when starting or resuming an instance would
// exceed a configured host memory, vCPU, or active microVM admission limit.
var ErrQuotaExceeded = errors.New("host resource quota exceeded")

// QuotaError describes the specific host admission limit that rejected a wake or start.
type QuotaError struct {
	Resource  string
	Requested int
	Used      int
	Limit     int
}

func (e *QuotaError) Error() string {
	return fmt.Sprintf("host resource quota exceeded: %s requested %d + used %d exceeds limit %d",
		e.Resource, e.Requested, e.Used, e.Limit)
}

func (e *QuotaError) Unwrap() error {
	return ErrQuotaExceeded
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

func (m *Manager) effectiveResourcesLocked(inst *Instance) (memMB int, vcpus int) {
	memMB = inst.MemoryMB
	if memMB <= 0 && m.cfg != nil {
		memMB = m.cfg.DefaultMemoryMB
	}
	if memMB <= 0 {
		memMB = 512
	}
	vcpus = inst.VCPUs
	if vcpus <= 0 && m.cfg != nil {
		vcpus = m.cfg.DefaultVCPUs
	}
	if vcpus <= 0 {
		vcpus = 1
	}
	return memMB, vcpus
}

// HostResources returns a race-free snapshot of host resource usage and admission limits.
func (m *Manager) HostResources() HostResourceMetrics {
	var out HostResourceMetrics
	if m.cfg != nil {
		out.MaxHostMemoryMB = m.cfg.MaxHostMemoryMB
		out.MaxActiveVCPUs = m.cfg.MaxActiveVCPUs
		out.MaxActiveVMs = m.cfg.MaxActiveVMs
	}

	list := m.ListInstances()
	for _, inst := range list {
		inst.mu.Lock()
		state := inst.State
		resuming := inst.resuming
		reaping := inst.reaping
		memMB, vcpus := m.effectiveResourcesLocked(inst)
		inst.mu.Unlock()

		if reaping {
			out.ResidentMemoryMB += memMB
			continue
		}
		switch {
		case state == StateStarting || state == StateRunning || resuming:
			out.ActiveMemoryMB += memMB
			out.ResidentMemoryMB += memMB
			out.ActiveVCPUs += vcpus
			out.ActiveVMs++
		case state == StatePaused:
			out.PausedMemoryMB += memMB
			out.ResidentMemoryMB += memMB
		}
	}

	gov := m.KSMGovernor()
	if gov != nil {
		km := gov.ReadMetrics()
		out.KSMSavedMemoryMB = km.SavedMB
		out.KSMPagesSharing = km.PagesSharing
		out.KSMActive = km.Active
	}
	effective := out.ResidentMemoryMB - out.KSMSavedMemoryMB
	if effective < 0 {
		effective = 0
	}
	out.EffectiveResidentMemoryMB = effective

	if cgov := m.CgroupGovernor(); cgov != nil {
		cm := cgov.ReadMetrics()
		if cm.Available {
			out.CgroupAvailable = true
			out.CgroupMemoryCurrentBytes = cm.CurrentBytes
			out.CgroupMemoryHighBytes = cm.HighBytes
			out.CgroupMemoryMaxBytes = cm.MaxBytes
			out.CgroupHighThrottleEvents = cm.Events.High
			out.CgroupOOMEvents = cm.Events.OOM + cm.Events.OOMKill
			out.CgroupPSISomeAvg10 = cm.PSISome.Avg10
			out.CgroupPSIFullAvg10 = cm.PSIFull.Avg10
			out.CgroupPSISomeTotalMicros = cm.PSISome.TotalMicros
			out.CgroupPSIFullTotalMicros = cm.PSIFull.TotalMicros
		}
	}
	return out
}

func (m *Manager) reconcileCgroup() {
	cgov := m.CgroupGovernor()
	if cgov == nil {
		return
	}
	var fallbackCeiling uint64
	if m.cfg != nil && m.cfg.MaxHostMemoryMB > 0 {
		fallbackCeiling = uint64(m.cfg.MaxHostMemoryMB) * 1024 * 1024
	}
	cgov.Reconcile(fallbackCeiling)
}

func (m *Manager) reconcileKSM() {
	gov := m.KSMGovernor()
	if gov == nil {
		return
	}
	residentVMs := 0
	for _, inst := range m.ListInstances() {
		inst.mu.Lock()
		state := inst.State
		resuming := inst.resuming
		reaping := inst.reaping
		inst.mu.Unlock()
		if reaping {
			continue
		}
		if state == StateStarting || state == StateRunning || state == StatePaused || resuming {
			residentVMs++
		}
	}
	gov.Reconcile(residentVMs)
}

type pausedEvictionCandidate struct {
	inst      *Instance
	id        string
	memMB     int
	idleSince time.Time
	updatedAt time.Time
}

func (m *Manager) quotaLimits() (maxMemMB, maxVCPUs, maxActiveVMs int) {
	if m.cfg == nil {
		return 0, 0, 0
	}
	return m.cfg.MaxHostMemoryMB, m.cfg.MaxActiveVCPUs, m.cfg.MaxActiveVMs
}

// admitAndBoot atomically checks host resource quotas, evicting LRU idle PAUSED
// instances if needed to satisfy MaxHostMemoryMB, reserves resources by moving
// inst into StateStarting under m.admissionMu, and completes the cold boot.
func (m *Manager) admitAndBoot(ctx context.Context, inst *Instance) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		m.admissionMu.Lock()

		inst.mu.Lock()
		if !inst.Enabled {
			inst.mu.Unlock()
			m.admissionMu.Unlock()
			return ErrInstanceDisabled
		}
		if inst.State != StateStopped || inst.reaping || inst.pausing || inst.resuming {
			inst.mu.Unlock()
			m.admissionMu.Unlock()
			return m.ensureInstanceLoop(ctx, inst, false)
		}

		// Crash backoff: max 5 consecutive crashes within a 2-minute window.
		if inst.crashCount > 0 && !inst.lastCrashAt.IsZero() {
			if time.Since(inst.lastCrashAt) > 2*time.Minute {
				inst.crashCount = 0
			} else if inst.crashCount >= 5 {
				err := fmt.Errorf("instance %s crash loop (crashed %d times, last at %s)",
					inst.ID, inst.crashCount, inst.lastCrashAt.Format(time.RFC3339))
				inst.lastBootErr = err
				inst.broadcastLocked()
				inst.mu.Unlock()
				m.admissionMu.Unlock()
				atomic.AddUint64(&m.metrics.bootFailures, 1)
				return err
			}
		}

		reqMem, reqVCPUs := m.effectiveResourcesLocked(inst)
		inst.mu.Unlock()

		maxMem, maxCPU, maxVMs := m.quotaLimits()
		if maxMem > 0 || maxCPU > 0 || maxVMs > 0 {
			activeVMs := 0
			activeVCPUs := 0
			activeMem := 0
			pinnedPausedMem := 0
			evictableMem := 0
			reapingMem := 0
			var reapWaitCh <-chan struct{}
			var candidates []pausedEvictionCandidate

			for _, other := range m.ListInstances() {
				if other == inst {
					continue
				}
				other.mu.Lock()
				oMem, oCPU := m.effectiveResourcesLocked(other)
				switch {
				case other.reaping:
					reapingMem += oMem
					if reapWaitCh == nil {
						reapWaitCh = other.waitChanLocked()
					}
				case other.State == StateStarting || other.State == StateRunning || other.resuming:
					activeVMs++
					activeVCPUs += oCPU
					activeMem += oMem
				case other.State == StatePaused:
					if other.pausing || other.activeConns > 0 || other.leaseHeld {
						pinnedPausedMem += oMem
					} else {
						idleSince := other.lastActivity
						if idleSince.IsZero() {
							idleSince = other.UpdatedAt
						}
						if idleSince.IsZero() {
							idleSince = other.CreatedAt
						}
						candidates = append(candidates, pausedEvictionCandidate{
							inst:      other,
							id:        other.ID,
							memMB:     oMem,
							idleSince: idleSince,
							updatedAt: other.UpdatedAt,
						})
						evictableMem += oMem
					}
				}
				other.mu.Unlock()
			}

			reject := func(qErr *QuotaError) error {
				inst.mu.Lock()
				inst.lastBootErr = qErr
				inst.broadcastLocked()
				inst.mu.Unlock()
				m.admissionMu.Unlock()
				atomic.AddUint64(&m.metrics.quotaRejections, 1)
				return qErr
			}

			if maxVMs > 0 && activeVMs+1 > maxVMs {
				return reject(&QuotaError{
					Resource:  "active_vms",
					Requested: 1,
					Used:      activeVMs,
					Limit:     maxVMs,
				})
			}
			if maxCPU > 0 && activeVCPUs+reqVCPUs > maxCPU {
				return reject(&QuotaError{
					Resource:  "active_vcpus",
					Requested: reqVCPUs,
					Used:      activeVCPUs,
					Limit:     maxCPU,
				})
			}

			nonEvictableMem := activeMem + pinnedPausedMem
			totalResidentMem := nonEvictableMem + evictableMem + reapingMem
			if maxMem > 0 && nonEvictableMem+reqMem > maxMem {
				return reject(&QuotaError{
					Resource:  "memory_mb",
					Requested: reqMem,
					Used:      totalResidentMem,
					Limit:     maxMem,
				})
			}

			if maxMem > 0 && totalResidentMem+reqMem > maxMem {
				if len(candidates) > 0 {
					sort.Slice(candidates, func(i, j int) bool {
						if !candidates[i].idleSince.Equal(candidates[j].idleSince) {
							return candidates[i].idleSince.Before(candidates[j].idleSince)
						}
						if !candidates[i].updatedAt.Equal(candidates[j].updatedAt) {
							return candidates[i].updatedAt.Before(candidates[j].updatedAt)
						}
						return candidates[i].id < candidates[j].id
					})
					victim := candidates[0].inst
					m.admissionMu.Unlock()

					evicted, waitCh := m.evictPausedInstance(victim)
					if !evicted && waitCh != nil {
						select {
						case <-ctx.Done():
							return ctx.Err()
						case <-waitCh:
						}
					}
					continue
				}
				if reapWaitCh != nil {
					m.admissionMu.Unlock()
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-reapWaitCh:
					}
					continue
				}
			}
		}

		// Reserve capacity atomically before releasing m.admissionMu.
		inst.mu.Lock()
		if !inst.Enabled {
			inst.mu.Unlock()
			m.admissionMu.Unlock()
			return ErrInstanceDisabled
		}
		if inst.State != StateStopped || inst.reaping || inst.pausing || inst.resuming {
			inst.mu.Unlock()
			m.admissionMu.Unlock()
			return m.ensureInstanceLoop(ctx, inst, false)
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

		m.admissionMu.Unlock()
		return m.finishBootFromStarting(ctx, inst, imageRef, rootfsPath, diskMB, instMemMB, instVCPUs, wsPath, exposePortsSnapshot)
	}
}

// admitAndResume atomically validates active vCPU and active VM quotas before
// transitioning a PAUSED instance into resuming=true.
func (m *Manager) admitAndResume(ctx context.Context, inst *Instance) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	m.admissionMu.Lock()

	inst.mu.Lock()
	if !inst.Enabled {
		inst.mu.Unlock()
		m.admissionMu.Unlock()
		return ErrInstanceDisabled
	}
	if inst.resuming || inst.reaping || inst.pausing {
		inst.mu.Unlock()
		m.admissionMu.Unlock()
		return m.ensureInstanceLoop(ctx, inst, false)
	}
	if inst.State == StateRunning {
		inst.mu.Unlock()
		m.admissionMu.Unlock()
		return nil
	}
	if inst.State != StatePaused {
		inst.mu.Unlock()
		m.admissionMu.Unlock()
		return m.ensureInstanceLoop(ctx, inst, false)
	}
	_, reqVCPUs := m.effectiveResourcesLocked(inst)
	inst.mu.Unlock()

	_, maxCPU, maxVMs := m.quotaLimits()
	if maxCPU > 0 || maxVMs > 0 {
		activeVMs := 0
		activeVCPUs := 0
		for _, other := range m.ListInstances() {
			if other == inst {
				continue
			}
			other.mu.Lock()
			_, oCPU := m.effectiveResourcesLocked(other)
			if !other.reaping && (other.State == StateStarting || other.State == StateRunning || other.resuming) {
				activeVMs++
				activeVCPUs += oCPU
			}
			other.mu.Unlock()
		}

		reject := func(qErr *QuotaError) error {
			inst.mu.Lock()
			inst.lastBootErr = qErr
			inst.broadcastLocked()
			inst.mu.Unlock()
			m.admissionMu.Unlock()
			atomic.AddUint64(&m.metrics.quotaRejections, 1)
			return qErr
		}

		if maxVMs > 0 && activeVMs+1 > maxVMs {
			return reject(&QuotaError{
				Resource:  "active_vms",
				Requested: 1,
				Used:      activeVMs,
				Limit:     maxVMs,
			})
		}
		if maxCPU > 0 && activeVCPUs+reqVCPUs > maxCPU {
			return reject(&QuotaError{
				Resource:  "active_vcpus",
				Requested: reqVCPUs,
				Used:      activeVCPUs,
				Limit:     maxCPU,
			})
		}
	}

	inst.mu.Lock()
	if !inst.Enabled {
		inst.mu.Unlock()
		m.admissionMu.Unlock()
		return ErrInstanceDisabled
	}
	if inst.State != StatePaused || inst.resuming || inst.reaping || inst.pausing {
		inst.mu.Unlock()
		m.admissionMu.Unlock()
		return m.ensureInstanceLoop(ctx, inst, false)
	}
	inst.resuming = true
	inst.lastBootErr = nil
	if inst.stopTimer != nil {
		inst.stopTimer.Stop()
		inst.stopTimer = nil
	}
	handle := inst.Handle
	inst.mu.Unlock()

	m.admissionMu.Unlock()
	return m.finishResume(inst, handle)
}

func (m *Manager) evictPausedInstance(inst *Instance) (bool, <-chan struct{}) {
	inst.mu.Lock()
	if inst.reaping {
		waitCh := inst.waitChanLocked()
		inst.mu.Unlock()
		return false, waitCh
	}
	if inst.State != StatePaused || inst.resuming || inst.pausing || inst.activeConns > 0 || inst.leaseHeld {
		inst.mu.Unlock()
		return false, nil
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

	atomic.AddUint64(&m.metrics.pausedEvictions, 1)
	log.Printf("instance %s: stopped (evicted from paused state to satisfy host memory quota)", instID)
	m.notifyStateChange(instID, StateStopped)
	return true, nil
}
