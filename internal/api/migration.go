package api

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/SalyyS1/omnivm/internal/migration"
)

// handleMigrateInstance handles POST /v1/instances/{id}/migrate.
//
// Request body:
//
//	{
//	  "dest_addr":             "/run/ovmd-dest.sock",
//	  "post_copy":             false,
//	  "dirty_threshold_pages": 256,
//	  "max_rounds":            8
//	}
//
// Response (200):
//
//	{
//	  "rounds": 4,
//	  "pages_sent": 12800,
//	  "bytes_sent": 52428800,
//	  "final_dirty_pages": 192,
//	  "stop_and_copy_ms": 11,
//	  "post_copy_faults": 0,
//	  "total_ms": 850
//	}
func (s *Server) handleMigrateInstance(w http.ResponseWriter, r *http.Request) {
	id := pathParam(r, "id")
	if !isValidID(id) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid instance id: %s", id))
		return
	}

	var req migration.MigrateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("decode request: %v", err))
		return
	}
	req.SourceInstanceID = id

	// MigrateDriver is an optional capability; check whether the backend supports it.
	driver, ok := s.vmm.(migration.MigrateDriver)
	if !ok {
		writeError(w, http.StatusNotImplemented, migration.ErrNotSupported.Error())
		return
	}

	m := migration.New(driver)
	res, err := m.Migrate(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("migration: %v", err))
		return
	}

	writeJSON(w, http.StatusOK, res)
}
