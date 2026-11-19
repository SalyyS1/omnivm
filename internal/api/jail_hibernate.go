package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/SalyyS1/omnivm/internal/hibernate"
	"github.com/SalyyS1/omnivm/internal/version"
	"github.com/SalyyS1/omnivm/internal/vmmjail"
)

func (s *Server) handleGetJail(w http.ResponseWriter, r *http.Request) {
	if s.lifecycle == nil || s.lifecycle.JailManager() == nil {
		writeJSON(w, http.StatusOK, vmmjail.StatusSnapshot{
			Mode:    vmmjail.ModeOff,
			UIDBase: vmmjail.DefaultUIDBase,
			PoolSize: vmmjail.DefaultPoolSize,
			Metrics: vmmjail.Metrics{Mode: vmmjail.ModeOff},
		})
		return
	}
	writeJSON(w, http.StatusOK, s.lifecycle.JailManager().Snapshot())
}

func (s *Server) handleSetJailMode(w http.ResponseWriter, r *http.Request) {
	if s.lifecycle == nil || s.lifecycle.JailManager() == nil {
		writeError(w, http.StatusServiceUnavailable, "jail manager is not initialized")
		return
	}
	var req struct {
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16*1024)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid json: %v", err))
		return
	}
	if err := s.lifecycle.JailManager().SetMode(req.Mode); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.lifecycle.JailManager().Snapshot())
}

func (s *Server) handlePlanJail(w http.ResponseWriter, r *http.Request) {
	var req struct {
		InstanceID string `json:"instance_id"`
		TenantID   string `json:"tenant_id,omitempty"`
		MemoryMB   int    `json:"memory_mb,omitempty"`
		VCPUs      int    `json:"vcpus,omitempty"`
		RootfsPath string `json:"rootfs_path,omitempty"`
		DryRun     *bool  `json:"dry_run,omitempty"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 32*1024)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid json: %v", err))
		return
	}
	if strings.TrimSpace(req.InstanceID) == "" {
		req.InstanceID = "jail-preview"
	}
	dryRun := true
	if req.DryRun != nil {
		dryRun = *req.DryRun
	}
	var mgr *vmmjail.Manager
	if s.lifecycle != nil && s.lifecycle.JailManager() != nil {
		mgr = s.lifecycle.JailManager()
	} else {
		mgr = vmmjail.NewManager(vmmjail.Config{Mode: vmmjail.ModeStrict})
		dryRun = true
	}
	spec, err := mgr.PlanJail(req.InstanceID, req.TenantID, req.MemoryMB, req.VCPUs, req.RootfsPath, dryRun)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, spec)
}

type jailAuditEnvelope struct {
	Spec       json.RawMessage `json:"spec"`
	Specs      json.RawMessage `json:"specs"`
	SourcePath string          `json:"source_path,omitempty"`
	Format     string          `json:"format,omitempty"`
}

func (s *Server) handleJailAudit(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 2*1024*1024))
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("read body: %v", err))
		return
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		writeError(w, http.StatusBadRequest, "request body is empty")
		return
	}
	formatStr := r.URL.Query().Get("format")
	sourcePath := r.URL.Query().Get("source_path")
	specBytes := body

	var env jailAuditEnvelope
	if json.Unmarshal(body, &env) == nil && (len(env.Spec) > 0 || len(env.Specs) > 0) {
		if len(env.Specs) > 0 {
			specBytes = env.Specs
		} else {
			specBytes = env.Spec
		}
		if env.Format != "" {
			formatStr = env.Format
		}
		if env.SourcePath != "" {
			sourcePath = env.SourcePath
		}
	}

	rep, err := vmmjail.AuditJailBytes(specBytes, sourcePath)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if s.lifecycle != nil && s.lifecycle.JailManager() != nil {
		s.lifecycle.JailManager().RecordAuditViolations(len(rep.Findings))
	}
	if strings.EqualFold(strings.TrimSpace(formatStr), "sarif") {
		writeJSON(w, http.StatusOK, vmmjail.ToSARIF(rep, version.Version()))
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

func (s *Server) handleJailBaseline(w http.ResponseWriter, r *http.Request) {
	instID := strings.TrimSpace(r.URL.Query().Get("instance"))
	if instID == "" {
		instID = "sandbox-vm"
	}
	tenant := strings.TrimSpace(r.URL.Query().Get("tenant"))
	spec := vmmjail.GenerateBaselineJailSpec(instID, tenant, 512, 2)
	writeJSON(w, http.StatusOK, spec)
}

func (s *Server) handleJailReconcile(w http.ResponseWriter, r *http.Request) {
	if s.lifecycle == nil || s.lifecycle.JailManager() == nil {
		writeJSON(w, http.StatusOK, vmmjail.ReconcileReport{Outcomes: []vmmjail.ReconcileOutcome{}})
		return
	}
	rep, err := s.lifecycle.JailManager().ReconcileLeases()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

func (s *Server) handleGetHibernate(w http.ResponseWriter, r *http.Request) {
	if s.lifecycle == nil || s.lifecycle.HibernateEngine() == nil {
		writeJSON(w, http.StatusOK, hibernate.StatusSnapshot{
			Mode:          hibernate.ModeOff,
			PageSizeBytes: hibernate.PageSize,
			EntropyBytes:  hibernate.EntropyBytes,
			Metrics:       hibernate.Metrics{Mode: hibernate.ModeOff},
		})
		return
	}
	writeJSON(w, http.StatusOK, s.lifecycle.HibernateEngine().Snapshot())
}

func (s *Server) handleSetHibernateMode(w http.ResponseWriter, r *http.Request) {
	if s.lifecycle == nil || s.lifecycle.HibernateEngine() == nil {
		writeError(w, http.StatusServiceUnavailable, "hibernate engine is not initialized")
		return
	}
	var req struct {
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16*1024)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid json: %v", err))
		return
	}
	if err := s.lifecycle.HibernateEngine().SetMode(req.Mode); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.lifecycle.HibernateEngine().Snapshot())
}

func (s *Server) handleHibernatePlan(w http.ResponseWriter, r *http.Request) {
	var req struct {
		InstanceID   string `json:"instance_id"`
		HibernatedMs int64  `json:"hibernated_ms"`
	}
	if r.Body != nil {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 32*1024))
		if len(strings.TrimSpace(string(body))) > 0 {
			if err := json.Unmarshal(body, &req); err != nil {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid json: %v", err))
				return
			}
		}
	}
	if strings.TrimSpace(req.InstanceID) == "" {
		req.InstanceID = "hibernated-vm"
	}
	if req.HibernatedMs <= 0 {
		req.HibernatedMs = 600000
	}
	now := time.Now().UTC()
	frozen := now.Add(-time.Duration(req.HibernatedMs) * time.Millisecond)

	var eng *hibernate.Engine
	if s.lifecycle != nil && s.lifecycle.HibernateEngine() != nil {
		eng = s.lifecycle.HibernateEngine()
	} else {
		eng = hibernate.NewEngine(hibernate.Config{Mode: hibernate.ModeIdle})
	}
	plan, err := eng.BuildRestorePlan(req.InstanceID, frozen, now)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

func (s *Server) handleBenchHibernate(w http.ResponseWriter, r *http.Request) {
	var cfg hibernate.BenchmarkConfig
	if r.Body != nil {
		body, err := io.ReadAll(io.LimitReader(r.Body, 32*1024))
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("read body: %v", err))
			return
		}
		if len(strings.TrimSpace(string(body))) > 0 {
			if err := json.Unmarshal(body, &cfg); err != nil {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid json: %v", err))
				return
			}
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	rep, err := hibernate.RunBenchmark(ctx, cfg)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rep)
}
