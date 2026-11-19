package vmm

import (
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"testing"
)

func TestBuildCHMemoryPayload_VirtiofsdSafetyAndMergeableHint(t *testing.T) {
	// With workspace (virtiofsd active), shared MUST stay true and mergeable omitted.
	withWs := buildCHMemoryPayload(512*1024*1024, "/tmp/ws", true)
	if shared, _ := withWs["shared"].(bool); !shared {
		t.Fatalf("expected shared=true when workspace is mounted, got %+v", withWs)
	}
	if _, hasMergeable := withWs["mergeable"]; hasMergeable {
		t.Fatalf("mergeable must not be set on MAP_SHARED virtiofs memory: %+v", withWs)
	}

	// Without workspace and mergeable=true, anonymous KSM-mergeable RAM is requested.
	anonMerge := buildCHMemoryPayload(512*1024*1024, "", true)
	if shared, _ := anonMerge["shared"].(bool); shared {
		t.Fatalf("expected shared=false for KSM mergeable memory, got %+v", anonMerge)
	}
	if m, _ := anonMerge["mergeable"].(bool); !m {
		t.Fatalf("expected mergeable=true when workspace is empty, got %+v", anonMerge)
	}

	// Without workspace and mergeable=false, default shared=true is preserved.
	noMerge := buildCHMemoryPayload(512*1024*1024, "", false)
	if shared, _ := noMerge["shared"].(bool); !shared {
		t.Fatalf("expected shared=true when mergeable=false, got %+v", noMerge)
	}
}

func TestCloudHypervisorVMM_SnapshotVMUnixSocketAPI(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "ch.sock")

	var gotMethod, gotPath, gotDest string
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotMethod = r.Method
			gotPath = r.URL.Path
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			gotDest = body["destination_url"]
			w.WriteHeader(http.StatusNoContent)
		}),
	}
	go srv.Serve(ln)
	defer srv.Close()

	v := &CloudHypervisorVMM{
		instances: map[string]*chInstance{
			"vm-1": {
				id:        "vm-1",
				apiSocket: sockPath,
			},
		},
	}

	snapDir := filepath.Join(dir, "snap-out")
	if err := v.SnapshotVM(Handle{ID: "vm-1"}, snapDir); err != nil {
		t.Fatalf("SnapshotVM: %v", err)
	}
	if gotMethod != "PUT" || gotPath != "/api/v1/vm.snapshot" {
		t.Errorf("got %s %s, want PUT /api/v1/vm.snapshot", gotMethod, gotPath)
	}
	if gotDest != "file://"+snapDir {
		t.Errorf("destination_url = %q, want %q", gotDest, "file://"+snapDir)
	}
}

func TestBuildCHDiskAndNetPayloads_RateLimiterConfig(t *testing.T) {
	// Default nil rateLimiter omits rate_limiter_config cleanly.
	plainDisk := buildCHDiskPayload("/tmp/root.ext4", nil)
	if _, ok := plainDisk["rate_limiter_config"]; ok {
		t.Fatalf("unexpected rate_limiter_config on default disk payload: %+v", plainDisk)
	}
	plainNet := buildCHNetPayload("ovm0", nil)
	if _, ok := plainNet["rate_limiter_config"]; ok {
		t.Fatalf("unexpected rate_limiter_config on default net payload: %+v", plainNet)
	}

	rl := map[string]interface{}{
		"bandwidth": map[string]interface{}{"size": uint64(10485760), "refill_time": uint64(100)},
	}
	qosDisk := buildCHDiskPayload("/tmp/root.ext4", rl)
	if qosDisk["rate_limiter_config"] == nil {
		t.Fatalf("expected rate_limiter_config on QoS disk payload")
	}
	qosNet := buildCHNetPayload("ovm0", rl)
	if qosNet["rate_limiter_config"] == nil {
		t.Fatalf("expected rate_limiter_config on QoS net payload")
	}
}

func TestBuildCHVirtioFSAndArgs_DAXAndDefault(t *testing.T) {
	defFs := buildCHVirtioFSPayload("workspace", "/tmp/vfs.sock", nil)
	if _, hasDAX := defFs["dax"]; hasDAX {
		t.Fatalf("default FsConfig must omit dax: %+v", defFs)
	}
	defArgs := buildVirtiofsdArgs("/tmp/vfs.sock", "/tmp/ws", nil)
	if len(defArgs) != 3 || defArgs[2] != "--cache=never" {
		t.Fatalf("unexpected default virtiofsd args: %v", defArgs)
	}

	daxOpts := &VirtioFSOptions{
		CachePolicy:    "always",
		ThreadPoolSize: 4,
		NumQueues:      2,
		QueueSize:      1024,
		DAXWindowBytes: 128 * 1024 * 1024,
	}
	daxFs := buildCHVirtioFSPayload("workspace", "/tmp/vfs.sock", daxOpts)
	if daxFs["dax"] != true || daxFs["cache_size"] != uint64(128*1024*1024) || daxFs["num_queues"] != 2 {
		t.Fatalf("unexpected DAX FsConfig: %+v", daxFs)
	}
	daxArgs := buildVirtiofsdArgs("/tmp/vfs.sock", "/tmp/ws", daxOpts)
	if len(daxArgs) != 4 || daxArgs[2] != "--cache=always" || daxArgs[3] != "--thread-pool-size=4" {
		t.Fatalf("unexpected DAX virtiofsd args: %v", daxArgs)
	}
}
