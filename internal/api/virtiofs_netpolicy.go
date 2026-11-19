package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/SalyyS1/omnivm/internal/netpolicy"
	"github.com/SalyyS1/omnivm/internal/version"
	"github.com/SalyyS1/omnivm/internal/virtiofs"
)

func (s *Server) handleGetVirtioFS(w http.ResponseWriter, r *http.Request) {
	if s.lifecycle == nil || s.lifecycle.VirtioFSPool() == nil {
		writeJSON(w, http.StatusOK, virtiofs.StatusSnapshot{
			Mode:    virtiofs.ModeOff,
			Metrics: virtiofs.Metrics{Mode: virtiofs.ModeOff},
		})
		return
	}
	writeJSON(w, http.StatusOK, s.lifecycle.VirtioFSPool().Snapshot())
}

func (s *Server) handleSetVirtioFSMode(w http.ResponseWriter, r *http.Request) {
	if s.lifecycle == nil || s.lifecycle.VirtioFSPool() == nil {
		writeError(w, http.StatusServiceUnavailable, "virtiofs pool is not initialized")
		return
	}
	var req struct {
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16*1024)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid json: %v", err))
		return
	}
	if err := s.lifecycle.VirtioFSPool().SetMode(req.Mode); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.lifecycle.VirtioFSPool().Snapshot())
}

func (s *Server) handleRegisterVirtioFSLayer(w http.ResponseWriter, r *http.Request) {
	if s.lifecycle == nil || s.lifecycle.VirtioFSPool() == nil {
		writeError(w, http.StatusServiceUnavailable, "virtiofs pool is not initialized")
		return
	}
	var req struct {
		Digest    string `json:"digest,omitempty"`
		ImageRef  string `json:"image_ref"`
		FilePath  string `json:"file_path,omitempty"`
		SizeBytes uint64 `json:"size_bytes,omitempty"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 32*1024)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid json: %v", err))
		return
	}
	entry, hit, err := s.lifecycle.VirtioFSPool().RegisterLayer(req.Digest, req.ImageRef, req.FilePath, req.SizeBytes)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"layer":     entry,
		"cache_hit": hit,
	})
}

func (s *Server) handleDeleteVirtioFSLayer(w http.ResponseWriter, r *http.Request) {
	if s.lifecycle == nil || s.lifecycle.VirtioFSPool() == nil {
		writeError(w, http.StatusServiceUnavailable, "virtiofs pool is not initialized")
		return
	}
	digest := strings.TrimSpace(pathParam(r, "digest"))
	if digest == "" {
		writeError(w, http.StatusBadRequest, "digest path parameter is required")
		return
	}
	removed, err := s.lifecycle.VirtioFSPool().DeleteLayer(digest)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"digest":  digest,
		"removed": removed,
	})
}

func (s *Server) handlePlanVirtioFSMount(w http.ResponseWriter, r *http.Request) {
	var req struct {
		InstanceID  string `json:"instance_id"`
		Tag         string `json:"tag,omitempty"`
		SocketPath  string `json:"socket_path,omitempty"`
		SharedDir   string `json:"shared_dir,omitempty"`
		MemoryMB    int    `json:"memory_mb,omitempty"`
		VCPUs       int    `json:"vcpus,omitempty"`
		LayerDigest string `json:"layer_digest,omitempty"`
		Mode        string `json:"mode,omitempty"`
		DryRun      bool   `json:"dry_run,omitempty"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 32*1024)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid json: %v", err))
		return
	}
	if req.InstanceID == "" {
		req.InstanceID = "plan-preview"
	}
	if req.SocketPath == "" {
		req.SocketPath = "/var/run/omnivm/virtiofsd.sock"
	}
	if req.SharedDir == "" {
		req.SharedDir = "/workspace"
	}
	if req.MemoryMB <= 0 {
		req.MemoryMB = 512
	}
	if req.VCPUs <= 0 {
		req.VCPUs = 2
	}

	if req.DryRun || s.lifecycle == nil || s.lifecycle.VirtioFSPool() == nil {
		mode := req.Mode
		if mode == "" {
			mode = virtiofs.ModeDAX
		}
		tmpPool := virtiofs.NewPool(virtiofs.Config{Mode: mode})
		plan, err := tmpPool.PlanMount(req.InstanceID, req.Tag, req.SocketPath, req.SharedDir, req.MemoryMB, req.VCPUs, "")
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, plan)
		return
	}

	plan, err := s.lifecycle.VirtioFSPool().PlanMount(req.InstanceID, req.Tag, req.SocketPath, req.SharedDir, req.MemoryMB, req.VCPUs, req.LayerDigest)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

func (s *Server) handleBenchVirtioFS(w http.ResponseWriter, r *http.Request) {
	var cfg virtiofs.BenchmarkConfig
	if r.Body != nil {
		body, err := io.ReadAll(io.LimitReader(r.Body, 64*1024))
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

	rep, err := virtiofs.RunBenchmark(ctx, cfg)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

func (s *Server) handleGetNetPolicy(w http.ResponseWriter, r *http.Request) {
	if s.lifecycle == nil || s.lifecycle.NetPolicyFirewall() == nil {
		writeJSON(w, http.StatusOK, netpolicy.StatusSnapshot{
			Mode:    netpolicy.ModeOff,
			Metrics: netpolicy.Metrics{Mode: netpolicy.ModeOff},
		})
		return
	}
	writeJSON(w, http.StatusOK, s.lifecycle.NetPolicyFirewall().Snapshot())
}

func (s *Server) handleSetNetPolicyMode(w http.ResponseWriter, r *http.Request) {
	if s.lifecycle == nil || s.lifecycle.NetPolicyFirewall() == nil {
		writeError(w, http.StatusServiceUnavailable, "netpolicy firewall is not initialized")
		return
	}
	var req struct {
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16*1024)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid json: %v", err))
		return
	}
	if err := s.lifecycle.NetPolicyFirewall().SetMode(req.Mode); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.lifecycle.NetPolicyFirewall().Snapshot())
}

func (s *Server) handleSetTenantNetPolicy(w http.ResponseWriter, r *http.Request) {
	if s.lifecycle == nil || s.lifecycle.NetPolicyFirewall() == nil {
		writeError(w, http.StatusServiceUnavailable, "netpolicy firewall is not initialized")
		return
	}
	tenant := strings.TrimSpace(pathParam(r, "tenant"))
	if tenant == "" {
		writeError(w, http.StatusBadRequest, "tenant path parameter is required")
		return
	}
	var pol netpolicy.TenantNetPolicy
	if err := json.NewDecoder(io.LimitReader(r.Body, 256*1024)).Decode(&pol); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid json: %v", err))
		return
	}
	pol.TenantID = tenant

	fw := s.lifecycle.NetPolicyFirewall()
	if modeParam := strings.TrimSpace(r.URL.Query().Get("mode")); modeParam != "" {
		if err := fw.SetMode(modeParam); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	} else if fw.Mode() == netpolicy.ModeOff {
		_ = fw.SetMode(netpolicy.ModeEnforce)
	}
	if err := fw.SetTenantPolicy(pol); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, fw.Snapshot())
}

func (s *Server) handleGetTenantNetPolicy(w http.ResponseWriter, r *http.Request) {
	if s.lifecycle == nil || s.lifecycle.NetPolicyFirewall() == nil {
		writeError(w, http.StatusServiceUnavailable, "netpolicy firewall is not initialized")
		return
	}
	tenant := strings.TrimSpace(pathParam(r, "tenant"))
	pol, ok := s.lifecycle.NetPolicyFirewall().GetTenantPolicy(tenant)
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("no netpolicy configured for tenant %q", tenant))
		return
	}
	writeJSON(w, http.StatusOK, pol)
}

func (s *Server) handleDeleteTenantNetPolicy(w http.ResponseWriter, r *http.Request) {
	if s.lifecycle == nil || s.lifecycle.NetPolicyFirewall() == nil {
		writeError(w, http.StatusServiceUnavailable, "netpolicy firewall is not initialized")
		return
	}
	tenant := strings.TrimSpace(pathParam(r, "tenant"))
	removed := s.lifecycle.NetPolicyFirewall().DeleteTenantPolicy(tenant)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"tenant_id": tenant,
		"removed":   removed,
	})
}

func (s *Server) handleEvaluateNetPolicy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Policy *netpolicy.TenantNetPolicy `json:"policy,omitempty"`
		Packet *netpolicy.EgressPacket    `json:"packet,omitempty"`
		DNS    *netpolicy.DNSQuery        `json:"dns,omitempty"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 256*1024)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid json: %v", err))
		return
	}
	if (req.Packet == nil && req.DNS == nil) || (req.Packet != nil && req.DNS != nil) {
		writeError(w, http.StatusBadRequest, "specify exactly one of packet or dns")
		return
	}

	if req.Policy != nil {
		pol := *req.Policy
		if err := netpolicy.ValidatePolicy(&pol); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if req.Packet != nil {
			dec, _, err := netpolicy.EvaluatePacketWithPolicy(pol, *req.Packet)
			if err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
			writeJSON(w, http.StatusOK, dec)
			return
		}
		dec, err := netpolicy.EvaluateDNSWithPolicy(pol, *req.DNS)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, dec)
		return
	}

	if s.lifecycle == nil || s.lifecycle.NetPolicyFirewall() == nil {
		writeError(w, http.StatusServiceUnavailable, "netpolicy firewall is not initialized")
		return
	}
	fw := s.lifecycle.NetPolicyFirewall()
	if req.Packet != nil {
		dec, err := fw.EvaluatePacket(*req.Packet)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, dec)
		return
	}
	dec, err := fw.EvaluateDNS(*req.DNS)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, dec)
}

type netPolicyAuditEnvelope struct {
	Policy     json.RawMessage `json:"policy"`
	SourcePath string          `json:"source_path,omitempty"`
	Format     string          `json:"format,omitempty"`
}

func (s *Server) handleNetPolicyAudit(w http.ResponseWriter, r *http.Request) {
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
	polBytes := body

	var env netPolicyAuditEnvelope
	if json.Unmarshal(body, &env) == nil && len(env.Policy) > 0 {
		polBytes = env.Policy
		if env.Format != "" {
			formatStr = env.Format
		}
		if env.SourcePath != "" {
			sourcePath = env.SourcePath
		}
	}

	rep, err := netpolicy.AuditPolicyBytes(polBytes, sourcePath)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.EqualFold(strings.TrimSpace(formatStr), "sarif") {
		writeJSON(w, http.StatusOK, netpolicy.ToSARIF(rep, version.Version()))
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

func (s *Server) handleNetPolicyCompile(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Policy  netpolicy.TenantNetPolicy `json:"policy"`
		TapName string                    `json:"tap_name"`
		GuestIP string                    `json:"guest_ip"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 256*1024)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid json: %v", err))
		return
	}
	if req.TapName == "" {
		req.TapName = "ovm0"
	}
	if req.GuestIP == "" {
		req.GuestIP = "172.16.0.2"
	}

	var script string
	var err error
	if s.lifecycle != nil && s.lifecycle.NetPolicyFirewall() != nil {
		script, err = s.lifecycle.NetPolicyFirewall().CompileNftables(req.Policy, req.TapName, req.GuestIP)
	} else {
		script, err = netpolicy.CompileNftables(req.Policy, req.TapName, req.GuestIP)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"tenant_id": netpolicy.NormalizeTenantID(req.Policy.TenantID),
		"tap_name":  req.TapName,
		"guest_ip":  req.GuestIP,
		"nftables":  script,
	})
}

func (s *Server) handleNetPolicyBaseline(w http.ResponseWriter, r *http.Request) {
	tenant := r.URL.Query().Get("tenant")
	preset := r.URL.Query().Get("preset")
	pol, err := netpolicy.GenerateBaselinePolicy(tenant, preset)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, pol)
}
