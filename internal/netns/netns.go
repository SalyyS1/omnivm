// Package netns provides lightweight per-VM network namespace isolation.
//
// Each microVM can be assigned a dedicated Linux network namespace with an
// isolated veth pair:
//
//	host side:  veth-ovm-<id>  (stays in the root netns)
//	guest side: veth-g-<id>    (moved into the VM-specific netns)
//
// The host side is bridged to a named OVS/Linux bridge (default: ovm-br0) and
// an iptables MASQUERADE rule is added for the guest subnet so the VM can reach
// the internet via the host's default route.
//
// All operations are simulated on non-Linux platforms or when the kernel lacks
// the required capabilities; callers check IsAvailable() first.
package netns

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"sync"
)

var (
	// ErrUnavailable is returned on platforms or environments where network
	// namespace operations are not supported.
	ErrUnavailable = errors.New("netns: network namespace isolation is not available on this platform")

	// validID matches safe veth suffix characters.
	validID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,11}$`)
)

// VethPair describes a veth pair created for a VM.
type VethPair struct {
	// HostIface is the interface name on the host side.
	HostIface string `json:"host_iface"`

	// GuestIface is the interface name inside the VM network namespace.
	GuestIface string `json:"guest_iface"`

	// NetNSPath is the path to the network namespace file (e.g. /run/netns/ovm-<id>).
	NetNSPath string `json:"netns_path"`

	// GuestIP is the IPv4 address assigned to the guest-side interface.
	GuestIP net.IP `json:"guest_ip"`

	// HostBridgeIP is the IPv4 address assigned to the host-side bridge endpoint.
	HostBridgeIP net.IP `json:"host_bridge_ip"`

	// MasqueradeChain is the iptables chain name used for MASQUERADE.
	MasqueradeChain string `json:"masquerade_chain"`
}

// Config holds parameters for bridge/veth setup.
type Config struct {
	// BridgeName is the Linux bridge or OVS bridge to attach host veth ends to.
	// Defaults to "ovm-br0".
	BridgeName string

	// SubnetCIDR is the IPv4 subnet pool from which guest addresses are allocated.
	// Defaults to "10.192.0.0/16".
	SubnetCIDR string

	// MasqueradeChain is the iptables chain name. Defaults to "OVM-MASQ".
	MasqueradeChain string
}

func (c *Config) defaults() {
	if c.BridgeName == "" {
		c.BridgeName = "ovm-br0"
	}
	if c.SubnetCIDR == "" {
		c.SubnetCIDR = "10.192.0.0/16"
	}
	if c.MasqueradeChain == "" {
		c.MasqueradeChain = "OVM-MASQ"
	}
}

// Manager allocates and tears down per-VM network namespaces.
type Manager struct {
	cfg  Config
	mu   sync.Mutex
	vpmu sync.Mutex

	// pairs maps instance ID -> VethPair for GC tracking.
	pairs map[string]*VethPair

	// nextOctet3/4 track sequential IP allocation within SubnetCIDR.
	nextOctet3 int
	nextOctet4 int
}

// NewManager creates a Manager with the given Config.
func NewManager(cfg Config) *Manager {
	cfg.defaults()
	return &Manager{
		cfg:        cfg,
		pairs:      make(map[string]*VethPair),
		nextOctet3: 1,
		nextOctet4: 1,
	}
}

// IsAvailable reports whether the running environment supports netns isolation.
// Returns false on non-Linux, in containers without CAP_NET_ADMIN, or when
// /run/netns is unavailable.
func IsAvailable() bool {
	return isAvailable()
}

// CreateVethPair creates an isolated network namespace and veth pair for the
// instance with the given id. The veth host-side is attached to the configured
// bridge. An iptables MASQUERADE rule is added for the guest subnet.
//
// On platforms where netns isolation is unavailable, the call succeeds and
// returns a VethPair describing the intended configuration without modifying
// the kernel; callers that need real isolation should check IsAvailable() first.
func (m *Manager) CreateVethPair(id string) (*VethPair, error) {
	if !validID.MatchString(id) {
		return nil, fmt.Errorf("netns: invalid instance id %q (must match [a-zA-Z0-9][a-zA-Z0-9_-]{0,11})", id)
	}

	guestIP, hostIP, err := m.allocateIPs()
	if err != nil {
		return nil, fmt.Errorf("netns: IP allocation for %s: %w", id, err)
	}

	vp := &VethPair{
		HostIface:       truncIface("veth-h-" + id),
		GuestIface:      truncIface("veth-g-" + id),
		NetNSPath:       "/run/netns/ovm-" + id,
		GuestIP:         guestIP,
		HostBridgeIP:    hostIP,
		MasqueradeChain: m.cfg.MasqueradeChain,
	}

	if IsAvailable() {
		if err := applyVethPair(m.cfg, vp); err != nil {
			return nil, fmt.Errorf("netns: apply veth pair for %s: %w", id, err)
		}
	}

	m.vpmu.Lock()
	m.pairs[id] = vp
	m.vpmu.Unlock()

	return vp, nil
}

// DeleteVethPair removes the veth pair and network namespace created for id,
// and flushes the MASQUERADE rule. It is a no-op if the pair was never created.
func (m *Manager) DeleteVethPair(id string) error {
	m.vpmu.Lock()
	vp, ok := m.pairs[id]
	if !ok {
		m.vpmu.Unlock()
		return nil
	}
	delete(m.pairs, id)
	m.vpmu.Unlock()

	if IsAvailable() {
		return teardownVethPair(vp)
	}
	return nil
}

// ListPairs returns a snapshot of all active veth pairs.
func (m *Manager) ListPairs() []*VethPair {
	m.vpmu.Lock()
	defer m.vpmu.Unlock()
	out := make([]*VethPair, 0, len(m.pairs))
	for _, vp := range m.pairs {
		cp := *vp
		out = append(out, &cp)
	}
	return out
}

// allocateIPs assigns sequential /30 guest and host IPs from the configured subnet.
func (m *Manager) allocateIPs() (guestIP, hostIP net.IP, err error) {
	_, subnet, parseErr := net.ParseCIDR(m.cfg.SubnetCIDR)
	if parseErr != nil {
		return nil, nil, fmt.Errorf("parse SubnetCIDR %q: %w", m.cfg.SubnetCIDR, parseErr)
	}

	m.mu.Lock()
	o3 := m.nextOctet3
	o4 := m.nextOctet4
	// Advance: use pairs /30 -> every 4 addresses; we just use sequential /24-within-/16.
	m.nextOctet4 += 4
	if m.nextOctet4 > 252 {
		m.nextOctet4 = 1
		m.nextOctet3++
	}
	if m.nextOctet3 > 255 {
		m.mu.Unlock()
		return nil, nil, errors.New("netns: IP address pool exhausted")
	}
	m.mu.Unlock()

	base := subnet.IP.To4()
	if base == nil {
		return nil, nil, errors.New("netns: SubnetCIDR must be IPv4")
	}
	host := net.IP{base[0], base[1], byte(o3), byte(o4 + 1)}
	guest := net.IP{base[0], base[1], byte(o3), byte(o4 + 2)}
	return guest, host, nil
}

// truncIface truncates an interface name to 15 bytes (Linux IFNAMSIZ-1 limit).
func truncIface(name string) string {
	if len(name) > 15 {
		return name[:15]
	}
	return name
}
