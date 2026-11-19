// Package netpolicy implements a per-tenant microVM egress CIDR/DNS policy
// firewall, atomic nftables ruleset compiler, and static security auditor
// (OVM-NET-001..008 with OASIS SARIF v2.1.0 output).
package netpolicy

import (
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

const (
	ModeOff     = "off"
	ModeAudit   = "audit"
	ModeEnforce = "enforce"

	ActionAllow = "allow"
	ActionDeny  = "deny"

	DefaultTenantID = "default"
)

var validTapIdent = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,15}$`)

var metadataPrefixes = []netip.Prefix{
	netip.MustParsePrefix("169.254.169.254/32"),
	netip.MustParsePrefix("169.254.170.2/32"),
	netip.MustParsePrefix("100.100.100.200/32"),
	netip.MustParsePrefix("fd00:ec2::254/128"),
}

var privateAndLateralPrefixes = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("::1/128"),
}

// PortRange represents an inclusive L4 destination port range [From, To].
type PortRange struct {
	From uint16 `json:"from"`
	To   uint16 `json:"to"`
}

// CIDRRule defines one ordered L3/L4 egress firewall rule.
type CIDRRule struct {
	CIDR        string      `json:"cidr"`
	Action      string      `json:"action"`             // "allow" or "deny"
	Protocol    string      `json:"protocol,omitempty"` // "tcp", "udp", "icmp", or "any"
	Ports       []PortRange `json:"ports,omitempty"`
	Description string      `json:"description,omitempty"`
}

// DNSRule defines one domain exact or label-suffix wildcard ("*.pypi.org") rule.
type DNSRule struct {
	Pattern string `json:"pattern"`
	Action  string `json:"action"` // "allow" or "deny"
}

// TenantNetPolicy specifies outbound L3/L4 CIDR and L7 DNS rules for a tenant.
type TenantNetPolicy struct {
	TenantID             string     `json:"tenant_id"`
	DefaultEgress        string     `json:"default_egress"` // "deny" or "allow"
	BlockMetadata        bool       `json:"block_metadata"`
	BlockPrivateNetworks bool       `json:"block_private_networks"`
	AntiSpoofing         bool       `json:"anti_spoofing"`
	AllowedResolvers     []string   `json:"allowed_resolvers,omitempty"`
	CIDRRules            []CIDRRule `json:"cidr_rules,omitempty"`
	DNSRules             []DNSRule  `json:"dns_rules,omitempty"`
}

// EgressPacket describes an outbound packet evaluated against a tenant policy.
type EgressPacket struct {
	TenantID      string `json:"tenant_id,omitempty"`
	ExpectedSrcIP string `json:"expected_src_ip,omitempty"`
	SrcIP         string `json:"src_ip,omitempty"`
	DstIP         string `json:"dst_ip"`
	Protocol      string `json:"protocol"`
	DstPort       uint16 `json:"dst_port"`
}

// DNSQuery describes an outbound DNS lookup evaluated against a tenant policy.
type DNSQuery struct {
	TenantID   string `json:"tenant_id,omitempty"`
	QName      string `json:"qname"`
	ResolverIP string `json:"resolver_ip,omitempty"`
}

// Decision captures the firewall verdict and matched policy reason.
type Decision struct {
	Allowed     bool   `json:"allowed"`
	Action      string `json:"action"`
	Reason      string `json:"reason"`
	MatchedRule string `json:"matched_rule,omitempty"`
	AuditOnly   bool   `json:"audit_only,omitempty"`
}

// Metrics reports live policy counts and cumulative packet/DNS firewall verdicts.
type Metrics struct {
	Mode              string `json:"mode"`
	ActivePolicies    int    `json:"active_policies"`
	AllowedPackets    uint64 `json:"allowed_packets"`
	DeniedPackets     uint64 `json:"denied_packets"`
	IMDSBlocks        uint64 `json:"imds_blocks"`
	LateralBlocks     uint64 `json:"lateral_blocks"`
	SpoofBlocks       uint64 `json:"spoof_blocks"`
	AllowedDNSQueries uint64 `json:"allowed_dns_queries"`
	DeniedDNSQueries  uint64 `json:"denied_dns_queries"`
	CompiledRulesets  uint64 `json:"compiled_rulesets"`
}

// StatusSnapshot is a race-free snapshot of the tenant egress firewall state.
type StatusSnapshot struct {
	Mode     string            `json:"mode"`
	Metrics  Metrics           `json:"metrics"`
	Policies []TenantNetPolicy `json:"policies"`
}

// NormalizeMode validates a firewall operating mode.
func NormalizeMode(raw string) (string, error) {
	m := strings.ToLower(strings.TrimSpace(raw))
	switch m {
	case "", ModeOff:
		return ModeOff, nil
	case ModeAudit:
		return ModeAudit, nil
	case ModeEnforce:
		return ModeEnforce, nil
	default:
		return "", fmt.Errorf("unsupported netpolicy mode %q: expected off, audit, or enforce", raw)
	}
}

// NormalizeTenantID canonicalizes a tenant identifier.
func NormalizeTenantID(tenantID string) string {
	t := strings.TrimSpace(tenantID)
	if t == "" {
		return DefaultTenantID
	}
	return t
}

func normalizeAction(raw string) (string, error) {
	a := strings.ToLower(strings.TrimSpace(raw))
	switch a {
	case ActionAllow, ActionDeny:
		return a, nil
	default:
		return "", fmt.Errorf("invalid action %q: expected allow or deny", raw)
	}
}

func normalizeProtocol(raw string) (string, error) {
	p := strings.ToLower(strings.TrimSpace(raw))
	switch p {
	case "", "any", "all", "*":
		return "any", nil
	case "tcp", "udp", "icmp":
		return p, nil
	default:
		return "", fmt.Errorf("unsupported protocol %q: expected tcp, udp, icmp, or any", raw)
	}
}

// ClonePolicy returns a deep copy of pol with independent backing slices.
func ClonePolicy(pol TenantNetPolicy) TenantNetPolicy {
	out := pol
	if len(pol.AllowedResolvers) > 0 {
		out.AllowedResolvers = append([]string(nil), pol.AllowedResolvers...)
	}
	if len(pol.CIDRRules) > 0 {
		out.CIDRRules = make([]CIDRRule, len(pol.CIDRRules))
		for i, r := range pol.CIDRRules {
			rc := r
			if len(r.Ports) > 0 {
				rc.Ports = append([]PortRange(nil), r.Ports...)
			}
			out.CIDRRules[i] = rc
		}
	}
	if len(pol.DNSRules) > 0 {
		out.DNSRules = append([]DNSRule(nil), pol.DNSRules...)
	}
	return out
}

// ValidatePolicy verifies structural and syntactic invariants of a TenantNetPolicy.
func ValidatePolicy(pol *TenantNetPolicy) error {
	if pol == nil {
		return errors.New("policy is nil")
	}
	pol.TenantID = NormalizeTenantID(pol.TenantID)
	if pol.DefaultEgress == "" {
		pol.DefaultEgress = ActionDeny
	}
	act, err := normalizeAction(pol.DefaultEgress)
	if err != nil {
		return fmt.Errorf("default_egress: %w", err)
	}
	pol.DefaultEgress = act

	for i, r := range pol.AllowedResolvers {
		addr, err := netip.ParseAddr(strings.TrimSpace(r))
		if err != nil {
			return fmt.Errorf("allowed_resolvers[%d] %q: %w", i, r, err)
		}
		pol.AllowedResolvers[i] = addr.Unmap().String()
	}

	for i := range pol.CIDRRules {
		rule := &pol.CIDRRules[i]
		pfx, err := netip.ParsePrefix(strings.TrimSpace(rule.CIDR))
		if err != nil {
			return fmt.Errorf("cidr_rules[%d] %q: %w", i, rule.CIDR, err)
		}
		rule.CIDR = pfx.Masked().String()

		rAct, err := normalizeAction(rule.Action)
		if err != nil {
			return fmt.Errorf("cidr_rules[%d]: %w", i, err)
		}
		rule.Action = rAct

		proto, err := normalizeProtocol(rule.Protocol)
		if err != nil {
			return fmt.Errorf("cidr_rules[%d]: %w", i, err)
		}
		rule.Protocol = proto

		if len(rule.Ports) > 0 && proto != "tcp" && proto != "udp" {
			return fmt.Errorf("cidr_rules[%d]: port ranges require tcp or udp protocol, got %q", i, rule.Protocol)
		}
		for j, pr := range rule.Ports {
			if pr.From == 0 || pr.To == 0 || pr.From > pr.To {
				return fmt.Errorf("cidr_rules[%d].ports[%d]: invalid port range %d-%d", i, j, pr.From, pr.To)
			}
		}
	}

	for i := range pol.DNSRules {
		dr := &pol.DNSRules[i]
		pat, err := normalizeDNSPattern(dr.Pattern)
		if err != nil {
			return fmt.Errorf("dns_rules[%d]: %w", i, err)
		}
		dr.Pattern = pat
		dAct, err := normalizeAction(dr.Action)
		if err != nil {
			return fmt.Errorf("dns_rules[%d]: %w", i, err)
		}
		dr.Action = dAct
	}
	return nil
}

func normalizeDNSPattern(raw string) (string, error) {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = strings.TrimSuffix(s, ".")
	if s == "" {
		return "", errors.New("dns pattern must not be empty")
	}
	if s == "*" {
		return "*", nil
	}
	check := s
	if strings.HasPrefix(s, "*.") {
		check = strings.TrimPrefix(s, "*.")
		if check == "" {
			return "", fmt.Errorf("invalid wildcard dns pattern %q", raw)
		}
	}
	if strings.Contains(check, "*") {
		return "", fmt.Errorf("dns wildcard only supported as leading '*.' prefix in %q", raw)
	}
	for _, r := range check {
		if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '.' || r == '_') {
			return "", fmt.Errorf("invalid character %q in dns pattern %q", r, raw)
		}
	}
	return s, nil
}

func normalizeQName(raw string) (string, error) {
	q := strings.ToLower(strings.TrimSpace(raw))
	q = strings.TrimSuffix(q, ".")
	if q == "" {
		return "", errors.New("qname is empty")
	}
	for _, r := range q {
		if r < 0x20 || r > 0x7e || r == ' ' {
			return "", fmt.Errorf("invalid qname %q", raw)
		}
	}
	return q, nil
}

func matchDNSPattern(pattern, qname string) bool {
	if pattern == "*" {
		return true
	}
	if strings.HasPrefix(pattern, "*.") {
		suffix := strings.TrimPrefix(pattern, "*") // ".example.com"
		return strings.HasSuffix(qname, suffix) && len(qname) > len(suffix)
	}
	return qname == pattern
}

func isMetadataAddr(addr netip.Addr) bool {
	u := addr.Unmap()
	for _, pfx := range metadataPrefixes {
		if pfx.Contains(u) {
			return true
		}
	}
	return false
}

func isPrivateOrLateralAddr(addr netip.Addr) bool {
	u := addr.Unmap()
	for _, pfx := range privateAndLateralPrefixes {
		if pfx.Contains(u) {
			return true
		}
	}
	return false
}

func matchesPorts(ports []PortRange, dstPort uint16) bool {
	if len(ports) == 0 {
		return true
	}
	for _, pr := range ports {
		if dstPort >= pr.From && dstPort <= pr.To {
			return true
		}
	}
	return false
}

// Firewall manages per-tenant network policies and evaluates outbound packets and DNS queries.
type Firewall struct {
	mu       sync.RWMutex
	mode     string
	policies map[string]TenantNetPolicy

	allowedPackets    uint64
	deniedPackets     uint64
	imdsBlocks        uint64
	lateralBlocks     uint64
	spoofBlocks       uint64
	allowedDNSQueries uint64
	deniedDNSQueries  uint64
	compiledRulesets  uint64
}

// NewFirewall creates a multi-tenant egress policy firewall.
func NewFirewall(mode string) *Firewall {
	norm, err := NormalizeMode(mode)
	if err != nil {
		norm = ModeOff
	}
	return &Firewall{
		mode:     norm,
		policies: make(map[string]TenantNetPolicy),
	}
}

// Mode returns the current firewall mode ("off", "audit", or "enforce").
func (f *Firewall) Mode() string {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.mode
}

// SetMode updates the firewall operating mode.
func (f *Firewall) SetMode(mode string) error {
	norm, err := NormalizeMode(mode)
	if err != nil {
		return err
	}
	f.mu.Lock()
	f.mode = norm
	f.mu.Unlock()
	return nil
}

// SetTenantPolicy validates and stores a deep copy of a tenant egress policy.
func (f *Firewall) SetTenantPolicy(pol TenantNetPolicy) error {
	cloned := ClonePolicy(pol)
	if err := ValidatePolicy(&cloned); err != nil {
		return err
	}
	f.mu.Lock()
	f.policies[cloned.TenantID] = cloned
	f.mu.Unlock()
	return nil
}

// GetTenantPolicy retrieves a configured tenant policy, if present.
func (f *Firewall) GetTenantPolicy(tenantID string) (TenantNetPolicy, bool) {
	norm := NormalizeTenantID(tenantID)
	f.mu.RLock()
	pol, ok := f.policies[norm]
	f.mu.RUnlock()
	if !ok {
		return TenantNetPolicy{}, false
	}
	return ClonePolicy(pol), true
}

// DeleteTenantPolicy removes a custom tenant egress policy.
func (f *Firewall) DeleteTenantPolicy(tenantID string) bool {
	norm := NormalizeTenantID(tenantID)
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.policies[norm]; !ok {
		return false
	}
	delete(f.policies, norm)
	return true
}

// EvaluatePacket evaluates an outbound L3/L4 packet against the active tenant policy
// (falling back to DefaultTenantID when no tenant-specific policy exists).
func (f *Firewall) EvaluatePacket(pkt EgressPacket) (Decision, error) {
	f.mu.RLock()
	mode := f.mode
	tid := NormalizeTenantID(pkt.TenantID)
	pol, ok := f.policies[tid]
	if !ok && tid != DefaultTenantID {
		pol, ok = f.policies[DefaultTenantID]
	}
	f.mu.RUnlock()

	if mode == ModeOff || !ok {
		atomic.AddUint64(&f.allowedPackets, 1)
		return Decision{Allowed: true, Action: ActionAllow, Reason: "firewall_inactive_or_unconfigured"}, nil
	}

	dec, category, err := EvaluatePacketWithPolicy(pol, pkt)
	if err != nil {
		return Decision{}, err
	}
	if dec.Action == ActionAllow {
		atomic.AddUint64(&f.allowedPackets, 1)
		return dec, nil
	}

	atomic.AddUint64(&f.deniedPackets, 1)
	switch category {
	case "imds":
		atomic.AddUint64(&f.imdsBlocks, 1)
	case "lateral":
		atomic.AddUint64(&f.lateralBlocks, 1)
	case "spoof":
		atomic.AddUint64(&f.spoofBlocks, 1)
	}
	if mode == ModeAudit {
		dec.Allowed = true
		dec.AuditOnly = true
	}
	return dec, nil
}

// EvaluatePacketWithPolicy evaluates pkt purely against pol without mutating daemon state.
func EvaluatePacketWithPolicy(pol TenantNetPolicy, pkt EgressPacket) (Decision, string, error) {
	dstRaw, err := netip.ParseAddr(strings.TrimSpace(pkt.DstIP))
	if err != nil {
		return Decision{}, "", fmt.Errorf("invalid dst_ip %q: %w", pkt.DstIP, err)
	}
	dst := dstRaw.Unmap()

	proto, err := normalizeProtocol(pkt.Protocol)
	if err != nil {
		return Decision{}, "", err
	}

	if pol.AntiSpoofing && strings.TrimSpace(pkt.ExpectedSrcIP) != "" && strings.TrimSpace(pkt.SrcIP) != "" {
		expRaw, e1 := netip.ParseAddr(strings.TrimSpace(pkt.ExpectedSrcIP))
		srcRaw, e2 := netip.ParseAddr(strings.TrimSpace(pkt.SrcIP))
		if e1 == nil && e2 == nil && expRaw.Unmap() != srcRaw.Unmap() {
			return Decision{
				Allowed:     false,
				Action:      ActionDeny,
				Reason:      "source_ip_spoof_blocked",
				MatchedRule: "anti_spoofing",
			}, "spoof", nil
		}
	}

	if pol.BlockMetadata && isMetadataAddr(dst) {
		return Decision{
			Allowed:     false,
			Action:      ActionDeny,
			Reason:      "cloud_metadata_blocked",
			MatchedRule: "block_metadata",
		}, "imds", nil
	}

	for idx, r := range pol.CIDRRules {
		pfx, pErr := netip.ParsePrefix(strings.TrimSpace(r.CIDR))
		if pErr != nil {
			continue
		}
		if !pfx.Contains(dst) {
			continue
		}
		rProto, _ := normalizeProtocol(r.Protocol)
		if rProto != "any" && proto != "any" && rProto != proto {
			continue
		}
		if !matchesPorts(r.Ports, pkt.DstPort) {
			continue
		}
		// Default-route wildcard allow rules (0.0.0.0/0 or ::/0) never bypass
		// BlockPrivateNetworks lateral-movement protection.
		if pfx.Bits() == 0 && r.Action == ActionAllow && pol.BlockPrivateNetworks && isPrivateOrLateralAddr(dst) {
			return Decision{
				Allowed:     false,
				Action:      ActionDeny,
				Reason:      "private_network_blocked",
				MatchedRule: "block_private_networks",
			}, "lateral", nil
		}
		allowed := r.Action == ActionAllow
		return Decision{
			Allowed:     allowed,
			Action:      r.Action,
			Reason:      "cidr_rule_match",
			MatchedRule: fmt.Sprintf("cidr_rules[%d]:%s", idx, r.CIDR),
		}, "", nil
	}

	if pol.BlockPrivateNetworks && isPrivateOrLateralAddr(dst) {
		return Decision{
			Allowed:     false,
			Action:      ActionDeny,
			Reason:      "private_network_blocked",
			MatchedRule: "block_private_networks",
		}, "lateral", nil
	}

	defAct := pol.DefaultEgress
	if defAct == "" {
		defAct = ActionDeny
	}
	return Decision{
		Allowed:     defAct == ActionAllow,
		Action:      defAct,
		Reason:      "default_egress_" + defAct,
		MatchedRule: "default_egress",
	}, "", nil
}

// EvaluateDNS evaluates an outbound DNS lookup against the active tenant policy.
func (f *Firewall) EvaluateDNS(q DNSQuery) (Decision, error) {
	f.mu.RLock()
	mode := f.mode
	tid := NormalizeTenantID(q.TenantID)
	pol, ok := f.policies[tid]
	if !ok && tid != DefaultTenantID {
		pol, ok = f.policies[DefaultTenantID]
	}
	f.mu.RUnlock()

	if mode == ModeOff || !ok {
		atomic.AddUint64(&f.allowedDNSQueries, 1)
		return Decision{Allowed: true, Action: ActionAllow, Reason: "firewall_inactive_or_unconfigured"}, nil
	}

	dec, err := EvaluateDNSWithPolicy(pol, q)
	if err != nil {
		return Decision{}, err
	}
	if dec.Action == ActionAllow {
		atomic.AddUint64(&f.allowedDNSQueries, 1)
		return dec, nil
	}
	atomic.AddUint64(&f.deniedDNSQueries, 1)
	if mode == ModeAudit {
		dec.Allowed = true
		dec.AuditOnly = true
	}
	return dec, nil
}

// EvaluateDNSWithPolicy evaluates q purely against pol.
func EvaluateDNSWithPolicy(pol TenantNetPolicy, q DNSQuery) (Decision, error) {
	qname, err := normalizeQName(q.QName)
	if err != nil {
		return Decision{}, err
	}

	if len(pol.AllowedResolvers) > 0 && strings.TrimSpace(q.ResolverIP) != "" {
		resAddr, rErr := netip.ParseAddr(strings.TrimSpace(q.ResolverIP))
		if rErr != nil {
			return Decision{}, fmt.Errorf("invalid resolver_ip %q: %w", q.ResolverIP, rErr)
		}
		resNorm := resAddr.Unmap().String()
		pinned := false
		for _, allowed := range pol.AllowedResolvers {
			if aAddr, aErr := netip.ParseAddr(strings.TrimSpace(allowed)); aErr == nil && aAddr.Unmap().String() == resNorm {
				pinned = true
				break
			}
		}
		if !pinned {
			return Decision{
				Allowed:     false,
				Action:      ActionDeny,
				Reason:      "unpinned_dns_resolver_blocked",
				MatchedRule: "allowed_resolvers",
			}, nil
		}
	}

	for idx, dr := range pol.DNSRules {
		pat, pErr := normalizeDNSPattern(dr.Pattern)
		if pErr != nil {
			continue
		}
		if matchDNSPattern(pat, qname) {
			allowed := dr.Action == ActionAllow
			return Decision{
				Allowed:     allowed,
				Action:      dr.Action,
				Reason:      "dns_rule_match",
				MatchedRule: fmt.Sprintf("dns_rules[%d]:%s", idx, pat),
			}, nil
		}
	}

	defAct := pol.DefaultEgress
	if defAct == "" {
		defAct = ActionDeny
	}
	return Decision{
		Allowed:     defAct == ActionAllow,
		Action:      defAct,
		Reason:      "default_egress_" + defAct,
		MatchedRule: "default_egress",
	}, nil
}

// CompileNftables compiles a validated TenantNetPolicy into an atomic, idempotent
// Linux nftables inet script scoped to a single microVM tap interface and guest IP.
func (f *Firewall) CompileNftables(pol TenantNetPolicy, tapName, guestIP string) (string, error) {
	out, err := CompileNftables(pol, tapName, guestIP)
	if err == nil && f != nil {
		atomic.AddUint64(&f.compiledRulesets, 1)
	}
	return out, err
}

// CompileNftables renders an atomic nft -f script for (pol, tapName, guestIP).
func CompileNftables(pol TenantNetPolicy, tapName, guestIP string) (string, error) {
	pol = ClonePolicy(pol)
	if err := ValidatePolicy(&pol); err != nil {
		return "", err
	}
	tapName = strings.TrimSpace(tapName)
	if !validTapIdent.MatchString(tapName) {
		return "", fmt.Errorf("invalid tap interface name %q", tapName)
	}
	gAddr, err := netip.ParseAddr(strings.TrimSpace(guestIP))
	if err != nil || !gAddr.Unmap().Is4() {
		return "", fmt.Errorf("guest_ip %q must be a valid IPv4 address", guestIP)
	}
	gIP := gAddr.Unmap().String()
	tableIdent := "ovm_" + strings.NewReplacer("-", "_", ".", "_").Replace(tapName)

	var b strings.Builder
	b.Grow(1024)
	fmt.Fprintf(&b, "# OmniVM per-VM egress policy ruleset (tenant=%s, tap=%s, guest=%s)\n", pol.TenantID, tapName, gIP)
	fmt.Fprintf(&b, "table inet %s\n", tableIdent)
	fmt.Fprintf(&b, "delete table inet %s\n\n", tableIdent)
	fmt.Fprintf(&b, "table inet %s {\n", tableIdent)
	fmt.Fprintf(&b, "  chain forward_egress {\n")
	fmt.Fprintf(&b, "    type filter hook forward priority 0; policy accept;\n")
	fmt.Fprintf(&b, "    iifname != %q accept\n", tapName)
	fmt.Fprintf(&b, "    ct state established,related accept\n")

	if pol.AntiSpoofing {
		fmt.Fprintf(&b, "    ip saddr != %s counter drop comment \"anti-spoofing\"\n", gIP)
	}
	if len(pol.AllowedResolvers) > 0 {
		var v4Resolvers []string
		for _, r := range pol.AllowedResolvers {
			if a, err := netip.ParseAddr(r); err == nil && a.Unmap().Is4() {
				v4Resolvers = append(v4Resolvers, a.Unmap().String())
			}
		}
		if len(v4Resolvers) > 0 {
			resSet := strings.Join(v4Resolvers, ", ")
			fmt.Fprintf(&b, "    ip daddr { %s } udp dport 53 counter accept comment \"pinned-dns-udp\"\n", resSet)
			fmt.Fprintf(&b, "    ip daddr { %s } tcp dport 53 counter accept comment \"pinned-dns-tcp\"\n", resSet)
			fmt.Fprintf(&b, "    udp dport 53 counter drop comment \"block-unpinned-dns-udp\"\n")
			fmt.Fprintf(&b, "    tcp dport 53 counter drop comment \"block-unpinned-dns-tcp\"\n")
		}
	}
	if pol.BlockMetadata {
		fmt.Fprintf(&b, "    ip daddr { 169.254.169.254, 169.254.170.2, 100.100.100.200 } counter drop comment \"block-cloud-imds\"\n")
		fmt.Fprintf(&b, "    ip6 daddr fd00:ec2::254 counter drop comment \"block-cloud-imds6\"\n")
	}

	for _, r := range pol.CIDRRules {
		pfx := netip.MustParsePrefix(r.CIDR)
		// Emit BlockPrivateNetworks before any 0.0.0.0/0 or ::/0 wildcard allow rule.
		if pfx.Bits() == 0 && r.Action == ActionAllow && pol.BlockPrivateNetworks {
			writePrivateDropRules(&b)
			pol.BlockPrivateNetworks = false
		}
		verdict := "drop"
		if r.Action == ActionAllow {
			verdict = "accept"
		}
		addrFam := "ip"
		if pfx.Addr().Is6() {
			addrFam = "ip6"
		}
		var matchParts []string
		if pfx.Bits() > 0 {
			matchParts = append(matchParts, fmt.Sprintf("%s daddr %s", addrFam, pfx.String()))
		} else if addrFam == "ip6" {
			matchParts = append(matchParts, "meta nfproto ipv6")
		} else {
			matchParts = append(matchParts, "meta nfproto ipv4")
		}
		if r.Protocol != "" && r.Protocol != "any" {
			if len(r.Ports) > 0 && (r.Protocol == "tcp" || r.Protocol == "udp") {
				matchParts = append(matchParts, fmt.Sprintf("%s dport %s", r.Protocol, formatNftPorts(r.Ports)))
			} else if r.Protocol == "icmp" && addrFam == "ip6" {
				matchParts = append(matchParts, "ip6 nexthdr ipv6-icmp")
			} else {
				matchParts = append(matchParts, fmt.Sprintf("meta l4proto %s", r.Protocol))
			}
		}
		fmt.Fprintf(&b, "    %s counter %s\n", strings.Join(matchParts, " "), verdict)
	}

	if pol.BlockPrivateNetworks {
		writePrivateDropRules(&b)
	}

	termVerdict := "drop"
	if pol.DefaultEgress == ActionAllow {
		termVerdict = "accept"
	}
	fmt.Fprintf(&b, "    counter %s comment \"default-egress\"\n", termVerdict)
	fmt.Fprintf(&b, "  }\n")
	fmt.Fprintf(&b, "}\n")
	return b.String(), nil
}

func writePrivateDropRules(b *strings.Builder) {
	fmt.Fprintf(b, "    ip daddr { 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, 100.64.0.0/10, 169.254.0.0/16, 127.0.0.0/8 } counter drop comment \"block-private-lateral\"\n")
	fmt.Fprintf(b, "    ip6 daddr { fc00::/7, fe80::/10, ::1/128 } counter drop comment \"block-private-lateral6\"\n")
}

func formatNftPorts(ports []PortRange) string {
	parts := make([]string, 0, len(ports))
	for _, pr := range ports {
		if pr.From == pr.To {
			parts = append(parts, fmt.Sprintf("%d", pr.From))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", pr.From, pr.To))
		}
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return "{ " + strings.Join(parts, ", ") + " }"
}

// GenerateBaselinePolicy constructs a hardened fail-closed TenantNetPolicy for
// common sandbox profiles ("strict-ai-sandbox", "package-build", or "airgapped").
func GenerateBaselinePolicy(tenantID, preset string) (TenantNetPolicy, error) {
	tid := NormalizeTenantID(tenantID)
	p := strings.ToLower(strings.TrimSpace(preset))
	if p == "" {
		p = "strict-ai-sandbox"
	}
	base := TenantNetPolicy{
		TenantID:             tid,
		DefaultEgress:        ActionDeny,
		BlockMetadata:        true,
		BlockPrivateNetworks: true,
		AntiSpoofing:         true,
	}

	switch p {
	case "strict-ai-sandbox":
		base.AllowedResolvers = []string{"1.1.1.1", "8.8.8.8"}
		base.CIDRRules = []CIDRRule{
			{
				CIDR:        "0.0.0.0/0",
				Action:      ActionAllow,
				Protocol:    "tcp",
				Ports:       []PortRange{{From: 443, To: 443}},
				Description: "Allow outbound TLS 443 to public internet endpoints only",
			},
		}
		base.DNSRules = []DNSRule{
			{Pattern: "api.openai.com", Action: ActionAllow},
			{Pattern: "api.anthropic.com", Action: ActionAllow},
			{Pattern: "*.huggingface.co", Action: ActionAllow},
			{Pattern: "pypi.org", Action: ActionAllow},
			{Pattern: "*.pythonhosted.org", Action: ActionAllow},
		}
		return base, nil
	case "package-build":
		base.AllowedResolvers = []string{"1.1.1.1", "9.9.9.9"}
		base.CIDRRules = []CIDRRule{
			{
				CIDR:        "0.0.0.0/0",
				Action:      ActionAllow,
				Protocol:    "tcp",
				Ports:       []PortRange{{From: 80, To: 80}, {From: 443, To: 443}},
				Description: "Allow HTTP/HTTPS package mirror downloads to public internet",
			},
		}
		base.DNSRules = []DNSRule{
			{Pattern: "pypi.org", Action: ActionAllow},
			{Pattern: "*.pythonhosted.org", Action: ActionAllow},
			{Pattern: "registry.npmjs.org", Action: ActionAllow},
			{Pattern: "proxy.golang.org", Action: ActionAllow},
			{Pattern: "sum.golang.org", Action: ActionAllow},
			{Pattern: "static.crates.io", Action: ActionAllow},
			{Pattern: "*.alpinelinux.org", Action: ActionAllow},
		}
		return base, nil
	case "airgapped":
		// Zero allowed resolvers, zero CIDR rules, default_egress=deny.
		return base, nil
	default:
		return TenantNetPolicy{}, fmt.Errorf("unknown netpolicy preset %q: expected strict-ai-sandbox, package-build, or airgapped", preset)
	}
}

// Metrics returns a thread-safe snapshot of firewall counters.
func (f *Firewall) Metrics() Metrics {
	f.mu.RLock()
	mode := f.mode
	active := len(f.policies)
	f.mu.RUnlock()

	return Metrics{
		Mode:              mode,
		ActivePolicies:    active,
		AllowedPackets:    atomic.LoadUint64(&f.allowedPackets),
		DeniedPackets:     atomic.LoadUint64(&f.deniedPackets),
		IMDSBlocks:        atomic.LoadUint64(&f.imdsBlocks),
		LateralBlocks:     atomic.LoadUint64(&f.lateralBlocks),
		SpoofBlocks:       atomic.LoadUint64(&f.spoofBlocks),
		AllowedDNSQueries: atomic.LoadUint64(&f.allowedDNSQueries),
		DeniedDNSQueries:  atomic.LoadUint64(&f.deniedDNSQueries),
		CompiledRulesets:  atomic.LoadUint64(&f.compiledRulesets),
	}
}

// Snapshot returns a race-free StatusSnapshot with sorted tenant policies.
func (f *Firewall) Snapshot() StatusSnapshot {
	f.mu.RLock()
	policies := make([]TenantNetPolicy, 0, len(f.policies))
	for _, p := range f.policies {
		policies = append(policies, ClonePolicy(p))
	}
	f.mu.RUnlock()

	sort.Slice(policies, func(i, j int) bool {
		return policies[i].TenantID < policies[j].TenantID
	})
	m := f.Metrics()
	return StatusSnapshot{
		Mode:     m.Mode,
		Metrics:  m,
		Policies: policies,
	}
}
