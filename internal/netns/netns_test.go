package netns

import (
	"net"
	"os/exec"
	"testing"
)

// hasBridge checks if ovm-br0 exists on the host so tests that require real
// kernel operations can skip gracefully in CI environments without it.
func hasBridge(name string) bool {
	out, err := exec.Command("ip", "link", "show", name).CombinedOutput()
	return err == nil && len(out) > 0
}

func TestCreateVethPair_ValidID(t *testing.T) {
	if IsAvailable() && !hasBridge("ovm-br0") {
		t.Skip("ovm-br0 bridge not present; skipping kernel-level veth test")
	}
	mgr := NewManager(Config{
		BridgeName:      "ovm-br0",
		SubnetCIDR:      "10.192.0.0/16",
		MasqueradeChain: "OVM-MASQ",
	})

	// CreateVethPair should succeed even when IsAvailable() is false
	// (it records the pair without applying kernel commands).
	vp, err := mgr.CreateVethPair("test-vm1")
	if err != nil {
		t.Fatalf("CreateVethPair: %v", err)
	}
	if vp.HostIface == "" {
		t.Error("HostIface must not be empty")
	}
	if vp.GuestIface == "" {
		t.Error("GuestIface must not be empty")
	}
	if vp.GuestIP == nil {
		t.Error("GuestIP must be set")
	}
	if vp.HostBridgeIP == nil {
		t.Error("HostBridgeIP must be set")
	}
	if vp.MasqueradeChain != "OVM-MASQ" {
		t.Errorf("MasqueradeChain: got %q, want OVM-MASQ", vp.MasqueradeChain)
	}
	t.Logf("veth pair: host=%s guest=%s guestIP=%s hostIP=%s netns=%s",
		vp.HostIface, vp.GuestIface, vp.GuestIP, vp.HostBridgeIP, vp.NetNSPath)
}

func TestCreateVethPair_InterfaceNameTruncation(t *testing.T) {
	if IsAvailable() && !hasBridge("ovm-br0") {
		t.Skip("ovm-br0 bridge not present; skipping kernel-level veth test")
	}
	mgr := NewManager(Config{})

	// A long ID should produce interface names <= 15 chars.
	vp, err := mgr.CreateVethPair("longid123456")
	if err != nil {
		t.Fatalf("CreateVethPair: %v", err)
	}
	if len(vp.HostIface) > 15 {
		t.Errorf("HostIface %q exceeds 15 chars (got %d)", vp.HostIface, len(vp.HostIface))
	}
	if len(vp.GuestIface) > 15 {
		t.Errorf("GuestIface %q exceeds 15 chars (got %d)", vp.GuestIface, len(vp.GuestIface))
	}
}

func TestCreateVethPair_InvalidID(t *testing.T) {
	mgr := NewManager(Config{})

	cases := []string{
		"",
		"has spaces",
		"../etc/passwd",
		"toolongidentifierthatexceedslimit",
	}
	for _, id := range cases {
		_, err := mgr.CreateVethPair(id)
		if err == nil {
			t.Errorf("expected error for id %q", id)
		}
	}
}

func TestCreateVethPair_IPAllocation_Sequential(t *testing.T) {
	if IsAvailable() && !hasBridge("ovm-br0") {
		t.Skip("ovm-br0 bridge not present; skipping kernel-level veth test")
	}
	mgr := NewManager(Config{SubnetCIDR: "10.200.0.0/16"})

	ips := make(map[string]bool)
	for i := 0; i < 10; i++ {
		id := net.IPv4(byte('a'), byte('a'+i), 0, 0).String()
		id = "vm" + string(rune('a'+i))
		vp, err := mgr.CreateVethPair(id)
		if err != nil {
			t.Fatalf("iteration %d: CreateVethPair: %v", i, err)
		}
		if ips[vp.GuestIP.String()] {
			t.Errorf("IP %s allocated twice", vp.GuestIP)
		}
		ips[vp.GuestIP.String()] = true
	}
}

func TestDeleteVethPair_NoopIfMissing(t *testing.T) {
	mgr := NewManager(Config{})
	// Deleting a non-existent pair must not error.
	if err := mgr.DeleteVethPair("does-not-exist"); err != nil {
		t.Errorf("DeleteVethPair for missing pair: %v", err)
	}
}

func TestDeleteVethPair_RemovesFromList(t *testing.T) {
	if IsAvailable() && !hasBridge("ovm-br0") {
		t.Skip("ovm-br0 bridge not present; skipping kernel-level veth test")
	}
	mgr := NewManager(Config{})

	_, err := mgr.CreateVethPair("vm-del1")
	if err != nil {
		t.Fatalf("CreateVethPair: %v", err)
	}
	pairs := mgr.ListPairs()
	if len(pairs) != 1 {
		t.Fatalf("expected 1 pair, got %d", len(pairs))
	}

	if err := mgr.DeleteVethPair("vm-del1"); err != nil && IsAvailable() {
		t.Errorf("DeleteVethPair: %v", err)
	}
	pairs = mgr.ListPairs()
	if len(pairs) != 0 {
		t.Errorf("expected 0 pairs after delete, got %d", len(pairs))
	}
}

func TestListPairs_ReturnsSnapshot(t *testing.T) {
	if IsAvailable() && !hasBridge("ovm-br0") {
		t.Skip("ovm-br0 bridge not present; skipping kernel-level veth test")
	}
	mgr := NewManager(Config{})

	for _, id := range []string{"vm-a", "vm-b", "vm-c"} {
		_, err := mgr.CreateVethPair(id)
		if err != nil {
			t.Fatalf("CreateVethPair(%s): %v", id, err)
		}
	}

	pairs := mgr.ListPairs()
	if len(pairs) != 3 {
		t.Errorf("expected 3 pairs, got %d", len(pairs))
	}
}

func TestManager_DefaultConfig(t *testing.T) {
	mgr := NewManager(Config{})
	if mgr.cfg.BridgeName != "ovm-br0" {
		t.Errorf("default BridgeName: got %q, want ovm-br0", mgr.cfg.BridgeName)
	}
	if mgr.cfg.SubnetCIDR != "10.192.0.0/16" {
		t.Errorf("default SubnetCIDR: got %q, want 10.192.0.0/16", mgr.cfg.SubnetCIDR)
	}
	if mgr.cfg.MasqueradeChain != "OVM-MASQ" {
		t.Errorf("default MasqueradeChain: got %q, want OVM-MASQ", mgr.cfg.MasqueradeChain)
	}
}

func TestAllocateIPs_PoolExhaustion(t *testing.T) {
	// Use a tiny subnet to trigger exhaustion quickly.
	mgr := NewManager(Config{SubnetCIDR: "10.0.0.0/16"})
	// Exhaust to nextOctet3 > 255.
	mgr.mu.Lock()
	mgr.nextOctet3 = 256
	mgr.mu.Unlock()

	_, _, err := mgr.allocateIPs()
	if err == nil {
		t.Error("expected pool exhaustion error")
	}
}
