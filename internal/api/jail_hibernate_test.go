package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SalyyS1/omnivm/internal/config"
	"github.com/SalyyS1/omnivm/internal/hibernate"
	"github.com/SalyyS1/omnivm/internal/lifecycle"
	"github.com/SalyyS1/omnivm/internal/logstore"
	"github.com/SalyyS1/omnivm/internal/registry"
	"github.com/SalyyS1/omnivm/internal/secrets"
	"github.com/SalyyS1/omnivm/internal/vmmjail"
)

func TestJailAndHibernateAPIEndpointsAndPrometheusSeries(t *testing.T) {
	tmp := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.DataDir = tmp
	cfg.SocketPath = filepath.Join(tmp, "ovmd.sock")
	cfg.JailMode = "strict"
	cfg.HibernateMode = "idle"

	reg, err := registry.Open(filepath.Join(tmp, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	ss, err := secrets.NewStore(filepath.Join(tmp, "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	ls := logstore.NewStore(filepath.Join(tmp, "logs"))
	fv := &qosCapturingVMM{}
	lm := lifecycle.NewManager(fv, cfg, ls, nil, nil)
	defer lm.Shutdown()
	lm.SetRegistry(reg)

	srv := NewServer(cfg, fv, lm, reg, ss, ls, nil)

	// 1. GET /v1/jail
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, httptest.NewRequest("GET", "/v1/jail", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /v1/jail status=%d body=%s", w.Code, w.Body.String())
	}

	// 2. POST /v1/jail/plan
	w = httptest.NewRecorder()
	srv.mux.ServeHTTP(w, httptest.NewRequest("POST", "/v1/jail/plan",
		bytes.NewBufferString(`{"instance_id":"vm-1","tenant_id":"acme","memory_mb":512,"vcpus":2,"dry_run":false}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("POST /v1/jail/plan status=%d body=%s", w.Code, w.Body.String())
	}
	var planned vmmjail.JailSpec
	if err := json.Unmarshal(w.Body.Bytes(), &planned); err != nil || planned.UID < 100000 {
		t.Fatalf("unexpected planned spec: %+v err=%v", planned, err)
	}

	// 3. POST /v1/jail/audit?format=sarif on unsafe spec
	w = httptest.NewRecorder()
	srv.mux.ServeHTTP(w, httptest.NewRequest("POST", "/v1/jail/audit?format=sarif",
		bytes.NewBufferString(`{"instance_id":"bad","uid":0,"gid":0,"chroot_dir":"/"}`)))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "OVM-JAIL-001") {
		t.Fatalf("POST /v1/jail/audit sarif failed: code=%d body=%s", w.Code, w.Body.String())
	}

	// 4. POST /v1/hibernate/plan & POST /v1/bench/hibernate
	w = httptest.NewRecorder()
	srv.mux.ServeHTTP(w, httptest.NewRequest("POST", "/v1/hibernate/plan",
		bytes.NewBufferString(`{"instance_id":"vm-1","hibernated_ms":120000}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("POST /v1/hibernate/plan status=%d body=%s", w.Code, w.Body.String())
	}
	var plan hibernate.RestorePlan
	if err := json.Unmarshal(w.Body.Bytes(), &plan); err != nil || plan.ClockAdjustmentMode != "step" {
		t.Fatalf("unexpected restore plan: %+v err=%v", plan, err)
	}

	w = httptest.NewRecorder()
	srv.mux.ServeHTTP(w, httptest.NewRequest("POST", "/v1/bench/hibernate",
		bytes.NewBufferString(`{"memory_mb":2,"zero_page_ratio":0.8}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("POST /v1/bench/hibernate status=%d body=%s", w.Code, w.Body.String())
	}

	// 5. Verify Prometheus exposition includes omnivm_jail_* and omnivm_hibernate_*
	w = httptest.NewRecorder()
	srv.mux.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /metrics status=%d", w.Code)
	}
	metricsOut := w.Body.String()
	for _, metric := range []string{
		"omnivm_jail_active_jails",
		"omnivm_jail_jails_planned_total",
		"omnivm_hibernate_clock_syncs_total",
		"omnivm_hibernate_entropy_reseeds_total",
	} {
		if !strings.Contains(metricsOut, metric) {
			t.Errorf("expected Prometheus metric %s in /metrics output", metric)
		}
	}
}
