package netpolicy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"
)

// RuleDescriptor defines one static network policy security rule (OVM-NET-001..008).
type RuleDescriptor struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	DefaultSeverity  string `json:"default_severity"`  // "error" or "warning"
	SecuritySeverity string `json:"security_severity"` // CVSS v3.1 score string
	Summary          string `json:"summary"`
	Remediation      string `json:"remediation"`
}

// Catalog lists all 8 OmniVM multi-tenant network policy security rules.
var Catalog = []RuleDescriptor{
	{
		ID:               "OVM-NET-001",
		Name:             "DefaultAllowUnrestrictedEgress",
		DefaultSeverity:  "error",
		SecuritySeverity: "8.6",
		Summary:          "Network policy uses permissive default_egress=allow or allows unrestricted 0.0.0.0/0 / ::/0 traffic across all ports.",
		Remediation:      "Set default_egress to deny and scope outbound allow rules to explicit destination CIDRs, protocols, and TCP/UDP ports.",
	},
	{
		ID:               "OVM-NET-002",
		Name:             "CloudMetadataSSRFExposure",
		DefaultSeverity:  "error",
		SecuritySeverity: "9.3",
		Summary:          "Cloud instance metadata protection (block_metadata) is disabled or an explicit rule permits 169.254.169.254 / fd00:ec2::254.",
		Remediation:      "Enable block_metadata: true and remove any allow rules covering 169.254.169.254/32, 169.254.0.0/16, or fd00:ec2::254/128.",
	},
	{
		ID:               "OVM-NET-003",
		Name:             "CrossVMLateralMovementExposure",
		DefaultSeverity:  "error",
		SecuritySeverity: "8.8",
		Summary:          "Private network isolation (block_private_networks) is disabled or an explicit allow rule exposes sibling microVM / RFC1918 subnets.",
		Remediation:      "Enable block_private_networks: true and avoid broad allow rules over 172.16.0.0/12, 10.0.0.0/8, 192.168.0.0/16, or 127.0.0.0/8.",
	},
	{
		ID:               "OVM-NET-004",
		Name:             "MissingSourceAntiSpoofing",
		DefaultSeverity:  "error",
		SecuritySeverity: "8.1",
		Summary:          "Source IP anti-spoofing (anti_spoofing) is disabled on the guest tap interface.",
		Remediation:      "Enable anti_spoofing: true so the host nftables chain drops packets whose source IPv4 differs from the assigned /30 guest IP.",
	},
	{
		ID:               "OVM-NET-005",
		Name:             "UnrestrictedDNSExfiltrationVector",
		DefaultSeverity:  "warning",
		SecuritySeverity: "7.4",
		Summary:          "Outbound DNS is allowed without pinning allowed_resolvers, permits wildcard '*', or opens port 53 to 0.0.0.0/0.",
		Remediation:      "Pin allowed_resolvers to trusted recursive resolvers and replace wildcard '*' DNS rules with explicit domain allowlists.",
	},
	{
		ID:               "OVM-NET-006",
		Name:             "HighRiskCleartextOrAdminPorts",
		DefaultSeverity:  "warning",
		SecuritySeverity: "7.1",
		Summary:          "CIDR allow rule exposes sensitive admin, lateral-movement, or abuse-prone ports (22, 23, 25, 445, 2375, 3389, 6379, 6443, 10250) on a broad prefix.",
		Remediation:      "Restrict administrative and cleartext ports to narrow /32 host endpoints or limit sandbox egress to port 443.",
	},
	{
		ID:               "OVM-NET-007",
		Name:             "ShadowedOrUnreachableFirewallRule",
		DefaultSeverity:  "warning",
		SecuritySeverity: "5.3",
		Summary:          "A CIDR or DNS firewall rule is completely shadowed by an earlier broader rule and can never be reached.",
		Remediation:      "Order specific deny/allow subnet and subdomain rules before broader superset prefixes or wildcard patterns.",
	},
	{
		ID:               "OVM-NET-008",
		Name:             "BroadWildcardTLDAllowlist",
		DefaultSeverity:  "warning",
		SecuritySeverity: "6.5",
		Summary:          "DNS allowlist permits an entire top-level domain (such as *.com, *.io, or *.org), enabling attacker-controlled domain exfiltration.",
		Remediation:      "Scope wildcard DNS rules to concrete registrable second-level domains (for example *.pythonhosted.org instead of *.org).",
	},
}

var highRiskPorts = []struct {
	Port uint16
	Name string
}{
	{22, "SSH"},
	{23, "Telnet"},
	{25, "SMTP"},
	{445, "SMB"},
	{2375, "Docker-TCP"},
	{2376, "Docker-TLS"},
	{3389, "RDP"},
	{6379, "Redis"},
	{6443, "Kubernetes-API"},
	{10250, "Kubelet"},
}

// Finding represents a single security violation discovered in a TenantNetPolicy.
type Finding struct {
	RuleID           string `json:"rule_id"`
	RuleName         string `json:"rule_name"`
	Severity         string `json:"severity"`
	SecuritySeverity string `json:"security_severity"`
	Scope            string `json:"scope"`
	Message          string `json:"message"`
	Remediation      string `json:"remediation"`
	Fingerprint      string `json:"fingerprint"`
}

// AuditReport summarizes static security analysis of a TenantNetPolicy.
type AuditReport struct {
	SourcePath   string    `json:"source_path"`
	TenantID     string    `json:"tenant_id"`
	Passed       bool      `json:"passed"`
	PostureScore int       `json:"posture_score"`
	ErrorCount   int       `json:"error_count"`
	WarningCount int       `json:"warning_count"`
	Findings     []Finding `json:"findings"`
}

func ruleByID(id string) RuleDescriptor {
	for _, r := range Catalog {
		if r.ID == id {
			return r
		}
	}
	return RuleDescriptor{ID: id}
}

func makeFinding(ruleID, scope, msg string) Finding {
	desc := ruleByID(ruleID)
	sum := sha256.Sum256([]byte(ruleID + "|" + scope + "|" + msg))
	return Finding{
		RuleID:           desc.ID,
		RuleName:         desc.Name,
		Severity:         desc.DefaultSeverity,
		SecuritySeverity: desc.SecuritySeverity,
		Scope:            scope,
		Message:          msg,
		Remediation:      desc.Remediation,
		Fingerprint:      hex.EncodeToString(sum[:16]),
	}
}

// AuditPolicyBytes parses raw JSON policy bytes and runs OVM-NET-001..008 checks.
func AuditPolicyBytes(raw []byte, sourcePath string) (*AuditReport, error) {
	var pol TenantNetPolicy
	if err := json.Unmarshal(raw, &pol); err != nil {
		return nil, fmt.Errorf("parse network policy json: %w", err)
	}
	if err := ValidatePolicy(&pol); err != nil {
		return nil, err
	}
	return AuditPolicy(pol, sourcePath), nil
}

// AuditPolicy statically evaluates a validated TenantNetPolicy against OVM-NET-001..008.
func AuditPolicy(pol TenantNetPolicy, sourcePath string) *AuditReport {
	if sourcePath == "" {
		sourcePath = "netpolicy.json"
	}
	tid := NormalizeTenantID(pol.TenantID)
	var findings []Finding

	// OVM-NET-001: Permissive default_egress=allow or unrestricted 0.0.0.0/0 / ::/0 allow rule.
	if pol.DefaultEgress == ActionAllow {
		findings = append(findings, makeFinding("OVM-NET-001", "default_egress",
			"Policy sets default_egress=\"allow\", permitting unrestricted outbound connections from the microVM."))
	}
	for i, r := range pol.CIDRRules {
		if r.Action != ActionAllow {
			continue
		}
		pfx, err := netip.ParsePrefix(r.CIDR)
		if err != nil {
			continue
		}
		if pfx.Bits() == 0 && (r.Protocol == "" || r.Protocol == "any" || len(r.Ports) == 0) {
			findings = append(findings, makeFinding("OVM-NET-001", fmt.Sprintf("cidr_rules[%d]", i),
				fmt.Sprintf("Rule %s allows %s protocol=%s without destination port restrictions.", r.CIDR, r.Action, emptyOr(r.Protocol, "any"))))
		}
	}

	// OVM-NET-002: Cloud metadata SSRF exposure.
	if !pol.BlockMetadata {
		findings = append(findings, makeFinding("OVM-NET-002", "block_metadata",
			"block_metadata is false; guest workloads may query cloud IMDS (169.254.169.254 / fd00:ec2::254) unless blocked elsewhere."))
	}
	for i, r := range pol.CIDRRules {
		if r.Action != ActionAllow {
			continue
		}
		pfx, err := netip.ParsePrefix(r.CIDR)
		if err != nil || pfx.Bits() == 0 {
			continue
		}
		for _, meta := range metadataPrefixes {
			if prefixesOverlap(pfx, meta) {
				findings = append(findings, makeFinding("OVM-NET-002", fmt.Sprintf("cidr_rules[%d]", i),
					fmt.Sprintf("Explicit allow rule %s overlaps cloud metadata endpoint %s.", r.CIDR, meta.String())))
				break
			}
		}
	}

	// OVM-NET-003: Cross-VM and RFC1918 lateral movement exposure.
	if !pol.BlockPrivateNetworks {
		findings = append(findings, makeFinding("OVM-NET-003", "block_private_networks",
			"block_private_networks is false; microVMs may probe sibling /30 tap subnets (172.16.0.0/12) or RFC1918 host networks."))
	}
	for i, r := range pol.CIDRRules {
		if r.Action != ActionAllow {
			continue
		}
		pfx, err := netip.ParsePrefix(r.CIDR)
		if err != nil || pfx.Bits() == 0 {
			continue
		}
		// Flag broad private allow rules (prefix < 28 on IPv4 or < 120 on IPv6) overlapping private/lateral space.
		broadThreshold := 28
		if pfx.Addr().Is6() {
			broadThreshold = 120
		}
		if pfx.Bits() < broadThreshold {
			for _, priv := range privateAndLateralPrefixes {
				if prefixesOverlap(pfx, priv) {
					findings = append(findings, makeFinding("OVM-NET-003", fmt.Sprintf("cidr_rules[%d]", i),
						fmt.Sprintf("Allow rule %s exposes private or sibling-microVM range %s.", r.CIDR, priv.String())))
					break
				}
			}
		}
	}

	// OVM-NET-004: Missing source IP anti-spoofing.
	if !pol.AntiSpoofing {
		findings = append(findings, makeFinding("OVM-NET-004", "anti_spoofing",
			"anti_spoofing is false; guest tap interface does not drop spoofed source IPv4 packets."))
	}

	// OVM-NET-005: Unrestricted DNS exfiltration vector.
	hasDNSAllow := pol.DefaultEgress == ActionAllow || len(pol.DNSRules) > 0
	for _, dr := range pol.DNSRules {
		if dr.Action == ActionAllow && strings.TrimSpace(dr.Pattern) == "*" {
			findings = append(findings, makeFinding("OVM-NET-005", "dns_rules",
				"DNS rule allows wildcard '*', enabling arbitrary authoritative DNS-tunneling exfiltration."))
		}
	}
	if hasDNSAllow && len(pol.AllowedResolvers) == 0 {
		findings = append(findings, makeFinding("OVM-NET-005", "allowed_resolvers",
			"Policy permits DNS resolution without pinning allowed_resolvers, allowing direct queries to rogue external nameservers."))
	}
	for i, r := range pol.CIDRRules {
		if r.Action != ActionAllow {
			continue
		}
		pfx, err := netip.ParsePrefix(r.CIDR)
		if err != nil {
			continue
		}
		if pfx.Bits() <= 8 && matchesPorts(r.Ports, 53) && len(r.Ports) > 0 && (r.Protocol == "udp" || r.Protocol == "tcp" || r.Protocol == "any") {
			findings = append(findings, makeFinding("OVM-NET-005", fmt.Sprintf("cidr_rules[%d]", i),
				fmt.Sprintf("Rule %s opens port 53 across a broad prefix (%s) instead of pinning specific resolver IPs.", r.CIDR, r.Protocol)))
		}
	}

	// OVM-NET-006: High-risk cleartext or lateral-admin ports on broad prefixes.
	for i, r := range pol.CIDRRules {
		if r.Action != ActionAllow || len(r.Ports) == 0 {
			continue
		}
		pfx, err := netip.ParsePrefix(r.CIDR)
		if err != nil || pfx.Bits() > 16 {
			continue
		}
		var exposed []string
		for _, hp := range highRiskPorts {
			if matchesPorts(r.Ports, hp.Port) {
				exposed = append(exposed, fmt.Sprintf("%d/%s", hp.Port, hp.Name))
			}
		}
		if len(exposed) > 0 {
			findings = append(findings, makeFinding("OVM-NET-006", fmt.Sprintf("cidr_rules[%d]", i),
				fmt.Sprintf("Broad allow rule %s permits high-risk port(s): %s.", r.CIDR, strings.Join(exposed, ", "))))
		}
	}

	// OVM-NET-007: Shadowed CIDR or DNS rules.
	for j := 1; j < len(pol.CIDRRules); j++ {
		later := pol.CIDRRules[j]
		for i := 0; i < j; i++ {
			earlier := pol.CIDRRules[i]
			if cidrRuleCovers(earlier, later, pol.BlockPrivateNetworks) {
				findings = append(findings, makeFinding("OVM-NET-007", fmt.Sprintf("cidr_rules[%d]", j),
					fmt.Sprintf("Rule %s (%s) at index %d is completely shadowed by earlier rule %s (%s) at index %d.",
						later.CIDR, later.Action, j, earlier.CIDR, earlier.Action, i)))
				break
			}
		}
	}
	for j := 1; j < len(pol.DNSRules); j++ {
		later := pol.DNSRules[j]
		for i := 0; i < j; i++ {
			earlier := pol.DNSRules[i]
			if dnsPatternCovers(earlier.Pattern, later.Pattern) {
				findings = append(findings, makeFinding("OVM-NET-007", fmt.Sprintf("dns_rules[%d]", j),
					fmt.Sprintf("DNS rule %q (%s) at index %d is shadowed by earlier pattern %q (%s) at index %d.",
						later.Pattern, later.Action, j, earlier.Pattern, earlier.Action, i)))
				break
			}
		}
	}

	// OVM-NET-008: Bare TLD wildcard allowlist (e.g. *.com, *.io, *.org).
	for i, dr := range pol.DNSRules {
		if dr.Action != ActionAllow {
			continue
		}
		pat := strings.ToLower(strings.TrimSpace(dr.Pattern))
		if strings.HasPrefix(pat, "*.") {
			rest := strings.TrimPrefix(pat, "*.")
			if !strings.Contains(rest, ".") {
				findings = append(findings, makeFinding("OVM-NET-008", fmt.Sprintf("dns_rules[%d]", i),
					fmt.Sprintf("Wildcard DNS allow rule %q covers an entire top-level domain (.%s).", dr.Pattern, rest)))
			}
		}
	}

	if findings == nil {
		findings = []Finding{}
	}

	errCount := 0
	warnCount := 0
	score := 100
	for _, f := range findings {
		if f.Severity == "error" {
			errCount++
			score -= 25
		} else {
			warnCount++
			score -= 10
		}
	}
	if score < 0 {
		score = 0
	}

	return &AuditReport{
		SourcePath:   sourcePath,
		TenantID:     tid,
		Passed:       errCount == 0,
		PostureScore: score,
		ErrorCount:   errCount,
		WarningCount: warnCount,
		Findings:     findings,
	}
}

func emptyOr(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

func prefixesOverlap(a, b netip.Prefix) bool {
	if a.Addr().Is4() != b.Addr().Is4() {
		return false
	}
	am := a.Masked()
	bm := b.Masked()
	return am.Contains(bm.Addr()) || bm.Contains(am.Addr())
}

func prefixContainsPrefix(superset, subset netip.Prefix) bool {
	if superset.Addr().Is4() != subset.Addr().Is4() {
		return false
	}
	return superset.Bits() <= subset.Bits() && superset.Masked().Contains(subset.Masked().Addr())
}

func cidrRuleCovers(earlier, later CIDRRule, blockPrivate bool) bool {
	pEarly, e1 := netip.ParsePrefix(earlier.CIDR)
	pLate, e2 := netip.ParsePrefix(later.CIDR)
	if e1 != nil || e2 != nil {
		return false
	}
	if !prefixContainsPrefix(pEarly, pLate) {
		return false
	}
	// Remember: a 0.0.0.0/0 allow rule does not match private space when blockPrivate is true,
	// so a later private rule is not shadowed by 0.0.0.0/0 allow in that configuration.
	if pEarly.Bits() == 0 && earlier.Action == ActionAllow && blockPrivate && isPrivateOrLateralAddr(pLate.Addr()) {
		return false
	}
	protoEarly, _ := normalizeProtocol(earlier.Protocol)
	protoLate, _ := normalizeProtocol(later.Protocol)
	if protoEarly != "any" && protoEarly != protoLate {
		return false
	}
	return portRangesCover(earlier.Ports, later.Ports)
}

func portRangesCover(earlier, later []PortRange) bool {
	if len(earlier) == 0 {
		return true
	}
	if len(later) == 0 {
		return false
	}
	for _, lr := range later {
		covered := false
		for _, er := range earlier {
			if er.From <= lr.From && er.To >= lr.To {
				covered = true
				break
			}
		}
		if !covered {
			return false
		}
	}
	return true
}

func dnsPatternCovers(earlier, later string) bool {
	e := strings.ToLower(strings.TrimSpace(earlier))
	l := strings.ToLower(strings.TrimSpace(later))
	if e == "*" || e == l {
		return true
	}
	if strings.HasPrefix(e, "*.") {
		suffix := strings.TrimPrefix(e, "*") // ".example.com"
		lClean := strings.TrimPrefix(l, "*.")
		if strings.HasPrefix(l, "*.") {
			return strings.HasSuffix("."+lClean, suffix) && len("."+lClean) >= len(suffix)
		}
		return strings.HasSuffix(l, suffix) && len(l) > len(suffix)
	}
	return false
}
