package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/SalyyS1/omnivm/internal/prefault"
)

func (s *Server) handleBenchPrefault(w http.ResponseWriter, r *http.Request) {
	var cfg prefault.BenchmarkConfig
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

	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()

	res, err := prefault.RunBenchmark(ctx, cfg)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}
