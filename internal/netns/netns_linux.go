//go:build linux

package netns

import (
	"fmt"
	"os"
	"os/exec"
)

// isAvailable checks whether the kernel supports network namespace operations
// by verifying /run/netns is accessible and we can run ip-netns.
func isAvailable() bool {
	if _, err := os.Stat("/proc/self/ns/net"); err != nil {
		return false
	}
	if _, err := exec.LookPath("ip"); err != nil {
		return false
	}
	return true
}

// applyVethPair issues ip(8) commands to create the netns, veth pair, bridge
// attachment, IP assignment, and MASQUERADE rule.
func applyVethPair(cfg Config, vp *VethPair) error {
	cmds := [][]string{
		// Create named network namespace.
		{"ip", "netns", "add", "ovm-" + vethID(vp.GuestIface)},
		// Create veth pair.
		{"ip", "link", "add", vp.HostIface, "type", "veth", "peer", "name", vp.GuestIface, "netns", "ovm-" + vethID(vp.GuestIface)},
		// Bring host-side up and attach to bridge.
		{"ip", "link", "set", vp.HostIface, "up"},
		{"ip", "link", "set", vp.HostIface, "master", cfg.BridgeName},
		// Assign host bridge IP.
		{"ip", "addr", "add", vp.HostBridgeIP.String() + "/30", "dev", vp.HostIface},
		// Bring guest-side up inside its netns.
		{"ip", "netns", "exec", "ovm-" + vethID(vp.GuestIface), "ip", "link", "set", vp.GuestIface, "up"},
		// Assign guest IP.
		{"ip", "netns", "exec", "ovm-" + vethID(vp.GuestIface), "ip", "addr", "add", vp.GuestIP.String() + "/30", "dev", vp.GuestIface},
		// Default route inside netns.
		{"ip", "netns", "exec", "ovm-" + vethID(vp.GuestIface), "ip", "route", "add", "default", "via", vp.HostBridgeIP.String()},
		// MASQUERADE for outbound traffic from the guest.
		{"iptables", "-t", "nat", "-A", vp.MasqueradeChain, "-s", vp.GuestIP.String() + "/30", "-j", "MASQUERADE"},
	}

	for _, args := range cmds {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			return fmt.Errorf("command %v: %w: %s", args, err, string(out))
		}
	}
	return nil
}

// teardownVethPair removes the veth pair, netns, and MASQUERADE rule.
func teardownVethPair(vp *VethPair) error {
	nsName := "ovm-" + vethID(vp.GuestIface)
	cmds := [][]string{
		{"iptables", "-t", "nat", "-D", vp.MasqueradeChain, "-s", vp.GuestIP.String() + "/30", "-j", "MASQUERADE"},
		{"ip", "link", "del", vp.HostIface},
		{"ip", "netns", "del", nsName},
	}
	var lastErr error
	for _, args := range cmds {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			// Best-effort cleanup: record last error but continue.
			lastErr = fmt.Errorf("command %v: %w: %s", args, err, string(out))
		}
	}
	return lastErr
}

// vethID extracts the unique suffix from a guest interface name.
func vethID(guestIface string) string {
	const prefix = "veth-g-"
	if len(guestIface) > len(prefix) {
		return guestIface[len(prefix):]
	}
	return guestIface
}
