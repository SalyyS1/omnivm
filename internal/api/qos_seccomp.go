package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/SalyyS1/omnivm/internal/ioqos"
	"github.com/SalyyS1/omnivm/internal/seccomp"
	"github.com/SalyyS1/omnivm/internal/version"
)

func (s *Server) handleGetQoS(w http.ResponseWriter, r *http.Request) {
	if s.lifecycle == nil || s.lifecycle.IOQoSGovernor() == nil {
		writeJSON(w, http.StatusOK, ioqos.StatusSnapshot{
			Mode:    ioqos.ModeOff,
			Metrics: ioqos.Metrics{Mode: ioqos.ModeOff, FairnessIndex: 1.0},
		})
		return
	}
	writeJSON(w, http.StatusOK, s.lifecycle.IOQoSGovernor().Snapshot())
}

func (s *Server) handleSetQoSMode(w http.ResponseWriter, r *http.Request) {
	if s.lifecycle == nil || s.lifecycle.IOQoSGovernor() == nil {
		writeError(w, http.StatusServiceUnavailable, "I/O QoS governor is not initialized")
		return
	}
	var req struct {
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16*1024)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid json: %v", err))
		return
	}
	if err := s.lifecycle.IOQoSGovernor().SetMode(req.Mode); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.lifecycle.IOQoSGovernor().Snapshot())
}

func (s *Server) handleSetTenantQoS(w http.ResponseWriter, r *http.Request) {
	if s.lifecycle == nil || s.lifecycle.IOQoSGovernor() == nil {
		writeError(w, http.StatusServiceUnavailable, "I/O QoS governor is not initialized")
		return
	}
	tenant := strings.TrimSpace(pathParam(r, "tenant"))
	if tenant == "" {
		writeError(w, http.StatusBadRequest, "tenant path parameter is required")
		return
	}

	var pol ioqos.TenantPolicy
	if err := json.NewDecoder(io.LimitReader(r.Body, 32*1024)).Decode(&pol); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid json: %v", err))
		return
	}
	pol.TenantID = tenant

	gov := s.lifecycle.IOQoSGovernor()
	// Auto-promote governor from "off" to "fairshare" when an operator configures an explicit tenant ceiling
	// unless a mode query parameter overrides it.
	if modeParam := strings.TrimSpace(r.URL.Query().Get("mode")); modeParam != "" {
		if err := gov.SetMode(modeParam); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	} else if gov.Mode() == ioqos.ModeOff {
		_ = gov.SetMode(ioqos.ModeFairShare)
	}

	if err := gov.SetTenantPolicy(pol); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, gov.Snapshot())
}

func (s *Server) handleDeleteTenantQoS(w http.ResponseWriter, r *http.Request) {
	if s.lifecycle == nil || s.lifecycle.IOQoSGovernor() == nil {
		writeError(w, http.StatusServiceUnavailable, "I/O QoS governor is not initialized")
		return
	}
	tenant := strings.TrimSpace(pathParam(r, "tenant"))
	if tenant == "" {
		writeError(w, http.StatusBadRequest, "tenant path parameter is required")
		return
	}
	removed := s.lifecycle.IOQoSGovernor().DeleteTenantPolicy(tenant)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"tenant_id": tenant,
		"removed":   removed,
	})
}

func (s *Server) handleBenchIOQoS(w http.ResponseWriter, r *http.Request) {
	var cfg ioqos.BenchmarkConfig
	if r.Body != nil {
		body, err := io.ReadAll(io.LimitReader(r.Body, 64*1024))
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("read body: %v", err))
			return
		}
		if len(body) > 0 {
			if err := json.Unmarshal(body, &cfg); err != nil {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid json: %v", err))
				return
			}
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	res, err := ioqos.RunBenchmark(ctx, cfg)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

type seccompAuditRequest struct {
	Profile    json.RawMessage `json:"profile"`
	Target     string          `json:"target,omitempty"`
	SourcePath string          `json:"source_path,omitempty"`
	Format     string          `json:"format,omitempty"` // "json" or "sarif"
}

func (s *Server) handleSeccompAudit(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 4*1024*1024))
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("read body: %v", err))
		return
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		writeError(w, http.StatusBadRequest, "request body is empty")
		return
	}

	targetStr := r.URL.Query().Get("target")
	formatStr := r.URL.Query().Get("format")
	sourcePath := r.URL.Query().Get("source_path")
	profileBytes := body

	// Support both wrapped {"profile": {...}, "target": "vmm"} and direct raw seccomp JSON bodies.
	var env seccompAuditRequest
	if json.Unmarshal(body, &env) == nil && len(env.Profile) > 0 {
		profileBytes = env.Profile
		if env.Target != "" {
			targetStr = env.Target
		}
		if env.Format != "" {
			formatStr = env.Format
		}
		if env.SourcePath != "" {
			sourcePath = env.SourcePath
		}
	}

	role, err := seccomp.ParseTargetRole(targetStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	rep, err := seccomp.AuditProfile(profileBytes, sourcePath, role)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if strings.EqualFold(strings.TrimSpace(formatStr), "sarif") {
		writeJSON(w, http.StatusOK, seccomp.ToSARIF(rep, version.Version()))
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

func (s *Server) handleSeccompBaseline(w http.ResponseWriter, r *http.Request) {
	role, err := seccomp.ParseTargetRole(r.URL.Query().Get("target"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	arch := r.URL.Query().Get("arch")
	prof := seccomp.GenerateBaselineProfile(role, arch)
	writeJSON(w, http.StatusOK, prof)
}
