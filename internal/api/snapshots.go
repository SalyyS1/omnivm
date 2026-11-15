package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/SalyyS1/omnivm/internal/lifecycle"
)

type createSnapshotHTTPRequest struct {
	Name            string `json:"name,omitempty"`
	IncludeVMMState bool   `json:"include_vmm_state,omitempty"`
}

type branchHTTPRequest struct {
	Handle       string            `json:"handle,omitempty"`
	Command      []string          `json:"command,omitempty"`
	Env          map[string]string `json:"env,omitempty"`
	Workspace    string            `json:"workspace,omitempty"`
	MemoryMB     int               `json:"memory_mb,omitempty"`
	VCPUs        int               `json:"vcpus,omitempty"`
	DiskMB       int               `json:"disk_mb,omitempty"`
	ExcludePorts bool              `json:"exclude_ports,omitempty"`
	Start        *bool             `json:"start,omitempty"`
}

func (s *Server) handleCreateSnapshot(w http.ResponseWriter, r *http.Request) {
	id := pathParam(r, "id")
	var req createSnapshotHTTPRequest
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, "invalid request: "+err.Error())
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	snap, err := s.lifecycle.CreateSnapshot(ctx, id, lifecycle.SnapshotRequest{
		Name:            req.Name,
		IncludeVMMState: req.IncludeVMMState,
	})
	if err != nil {
		switch {
		case errors.Is(err, lifecycle.ErrSnapshotConflict):
			writeError(w, http.StatusConflict, err.Error())
		case strings.Contains(err.Error(), "not found"):
			writeError(w, http.StatusNotFound, err.Error())
		case strings.Contains(err.Error(), "invalid snapshot name"):
			writeError(w, http.StatusBadRequest, err.Error())
		default:
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}

	writeJSON(w, http.StatusCreated, snap)
}

func (s *Server) handleListSnapshots(w http.ResponseWriter, r *http.Request) {
	instFilter := r.URL.Query().Get("instance")
	snaps := s.lifecycle.ListSnapshots(instFilter)
	if snaps == nil {
		snaps = []*lifecycle.Snapshot{}
	}
	writeJSON(w, http.StatusOK, snaps)
}

func (s *Server) handleGetSnapshot(w http.ResponseWriter, r *http.Request) {
	id := pathParam(r, "id")
	snap, err := s.lifecycle.GetSnapshot(id)
	if err != nil {
		if errors.Is(err, lifecycle.ErrSnapshotNotFound) {
			writeError(w, http.StatusNotFound, "snapshot not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

func (s *Server) handleDeleteSnapshot(w http.ResponseWriter, r *http.Request) {
	id := pathParam(r, "id")
	if err := s.lifecycle.DeleteSnapshot(id); err != nil {
		if errors.Is(err, lifecycle.ErrSnapshotNotFound) {
			writeError(w, http.StatusNotFound, "snapshot not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleBranchInstance(w http.ResponseWriter, r *http.Request) {
	sourceID := pathParam(r, "id")
	s.executeBranch(w, r, "", sourceID)
}

func (s *Server) handleBranchFromSnapshot(w http.ResponseWriter, r *http.Request) {
	snapID := pathParam(r, "id")
	s.executeBranch(w, r, snapID, "")
}

func (s *Server) executeBranch(w http.ResponseWriter, r *http.Request, snapID, sourceInstanceID string) {
	var req branchHTTPRequest
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, "invalid request: "+err.Error())
			return
		}
	}

	start := true
	if req.Start != nil {
		start = *req.Start
	}

	ws := req.Workspace
	if ws != "" {
		ws = s.resolveWorkspace(ws)
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	_, res, err := s.lifecycle.BranchInstance(ctx, lifecycle.BranchRequest{
		SnapshotID:       snapID,
		SourceInstanceID: sourceInstanceID,
		Handle:           req.Handle,
		Command:          req.Command,
		Env:              req.Env,
		Workspace:        ws,
		MemoryMB:         req.MemoryMB,
		VCPUs:            req.VCPUs,
		DiskMB:           req.DiskMB,
		ExcludePorts:     req.ExcludePorts,
		Start:            start,
	})
	if err != nil {
		switch {
		case errors.Is(err, lifecycle.ErrQuotaExceeded):
			writeError(w, http.StatusTooManyRequests, err.Error())
		case errors.Is(err, lifecycle.ErrInstanceConflict):
			writeError(w, http.StatusConflict, err.Error())
		case errors.Is(err, lifecycle.ErrSnapshotNotFound) || strings.Contains(err.Error(), "not found"):
			writeError(w, http.StatusNotFound, err.Error())
		case strings.Contains(err.Error(), "invalid handle") || strings.Contains(err.Error(), "empty"):
			writeError(w, http.StatusBadRequest, err.Error())
		default:
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}

	resp := map[string]interface{}{
		"id":              res.InstanceID,
		"state":           res.State,
		"logical_bytes":   res.LogicalBytes,
		"cow_saved_bytes": res.CoWSavedBytes,
	}
	if res.Handle != "" {
		resp["handle"] = res.Handle
	}
	if res.ParentID != "" {
		resp["parent_id"] = res.ParentID
	}
	if res.SourceSnapshotID != "" {
		resp["source_snapshot_id"] = res.SourceSnapshotID
	}
	if res.ImageRef != "" {
		resp["image_ref"] = res.ImageRef
	}
	if res.Workspace != "" {
		resp["workspace"] = res.Workspace
	}
	if s.router != nil {
		publicEps := s.router.GetAllPublicPorts(res.InstanceID)
		if len(publicEps) > 0 {
			eps := make([]map[string]interface{}, len(publicEps))
			for i, ep := range publicEps {
				eps[i] = map[string]interface{}{
					"guest_port":  ep.GuestPort,
					"public_port": ep.PublicPort,
					"protocol":    ep.Protocol,
				}
			}
			resp["endpoints"] = eps
		}
	}

	writeJSON(w, http.StatusCreated, resp)
}
