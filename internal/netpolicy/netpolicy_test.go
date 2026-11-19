package netpolicy

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

func TestBaselinePresetsPassAuditCleanly(t *testing.T) {
	for _, preset := range []string{"strict-ai-sandbox", "package-build", "airgapped"} {
		pol, err := GenerateBaselinePolicy("acme", preset)
		if err != nil {
			t.Fatalf("GenerateBaselinePolicy(%s): %v", preset, err)
		}
		rep := AuditPolicy(pol, preset+".json")
		if rep.PostureScore != 100 || len(rep.Findings) != 0 {
			t.Fatalf("preset %s expected 100/100 score and 0 findings, got score=%d findings=%+v", preset, rep.PostureScore, rep.Findings)
		}
	}
}

func TestAuditDetectsAllEightRulesAndExportsSARIF(t *testing.T) {
	bad := TenantNetPolicy{
		TenantID:             "unsafe-tenant",
		DefaultEgress:        ActionAllow, // OVM-NET-001
		BlockMetadata:        false,       // OVM-NET-002
		BlockPrivateNetworks: false,       // OVM-NET-003
		AntiSpoofing:         false,       // OVM-NET-004
		AllowedResolvers:     nil,         // OVM-NET-005
		CIDRRules: []CIDRRule{
			{
				CIDR:     "0.0.0.0/0",
				Action:   ActionAllow,
				Protocol: "tcp",
				Ports:    []PortRange{{From: 22, To: 25}}, // OVM-NET-006 (22 SSH, 23 Telnet, 25 SMTP)
			},
			{
				CIDR:     "10.1.0.0/16",
				Action:   ActionDeny,
				Protocol: "tcp",
				Ports:    []PortRange{{From: 22, To: 22}}, // OVM-NET-007 shadowed by 0.0.0.0/0 tcp 22-25!
			},
		},
		DNSRules: []DNSRule{
			{Pattern: "*.com", Action: ActionAllow},           // OVM-NET-008 bare TLD wildcard
			{Pattern: "api.example.com", Action: ActionDeny}, // OVM-NET-007 shadowed by *.com
		},
	}

	raw, _ := json.Marshal(bad)
	rep, err := AuditPolicyBytes(raw, "unsafe.json")
	if err != nil {
		t.Fatalf("AuditPolicyBytes: %v", err)
	}

	seen := make(map[string]bool)
	for _, f := range rep.Findings {
		seen[f.RuleID] = true
	}
	for _, rule := range Catalog {
		if !seen[rule.ID] {
			t.Errorf("expected rule %s (%s) to trigger, findings=%+v", rule.ID, rule.Name, rep.Findings)
		}
	}
	if rep.PostureScore != 0 {
		t.Fatalf("expected PostureScore = 0 on maximally unsafe policy, got %d", rep.PostureScore)
	}

	sarif := ToSARIF(rep, "v0.9.0")
	if sarif.Version != "2.1.0" || len(sarif.Runs) != 1 {
		t.Fatalf("invalid SARIF header: %+v", sarif)
	}
	if len(sarif.Runs[0].Tool.Driver.Rules) != 8 {
		t.Fatalf("expected 8 SARIF rules, got %d", len(sarif.Runs[0].Tool.Driver.Rules))
	}
	if len(sarif.Runs[0].Results) != len(rep.Findings) {
		t.Fatalf("SARIF result count mismatch: %d vs %d", len(sarif.Runs[0].Results), len(rep.Findings))
	}
}

func TestFirewallPacketAndDNSEvaluationIncludingIPv6MappedBypass(t *testing.T) {
	fw := NewFirewall(ModeEnforce)
	pol, err := GenerateBaselinePolicy("tenant-a", "strict-ai-sandbox")
	if err != nil {
		t.Fatal(err)
	}
	if err := fw.SetTenantPolicy(pol); err != nil {
		t.Fatalf("SetTenantPolicy: %v", err)
	}

	// 1. Spoofed source IP blocked.
	dec, err := fw.EvaluatePacket(EgressPacket{
		TenantID:      "tenant-a",
		ExpectedSrcIP: "172.16.0.2",
		SrcIP:         "172.16.0.6",
		DstIP:         "104.18.6.192",
		Protocol:      "tcp",
		DstPort:       443,
	})
	if err != nil || dec.Allowed || dec.Reason != "source_ip_spoof_blocked" {
		t.Fatalf("expected spoof block, got dec=%+v err=%v", dec, err)
	}

	// 2. Cloud IMDS blocked for both IPv4 and IPv4-mapped IPv6 (::ffff:169.254.169.254).
	for _, imds := range []string{"169.254.169.254", "::ffff:169.254.169.254", "fd00:ec2::254"} {
		dec, err := fw.EvaluatePacket(EgressPacket{
			TenantID:      "tenant-a",
			ExpectedSrcIP: "172.16.0.2",
			SrcIP:         "172.16.0.2",
			DstIP:         imds,
			Protocol:      "tcp",
			DstPort:       443,
		})
		if err != nil || dec.Allowed || dec.Reason != "cloud_metadata_blocked" {
			t.Fatalf("expected IMDS block for %s, got dec=%+v err=%v", imds, dec, err)
		}
	}

	// 3. Cross-VM lateral movement (172.16.0.6:443) blocked despite 0.0.0.0/0:443 allow rule!
	for _, lateral := range []string{"172.16.0.6", "::ffff:172.16.0.6", "10.0.1.15", "192.168.1.10"} {
		dec, err := fw.EvaluatePacket(EgressPacket{
			TenantID: "tenant-a",
			DstIP:    lateral,
			Protocol: "tcp",
			DstPort:  443,
		})
		if err != nil || dec.Allowed || dec.Reason != "private_network_blocked" {
			t.Fatalf("expected private/lateral block for %s, got dec=%+v err=%v", lateral, dec, err)
		}
	}

	// 4. Public TLS 443 allowed; public SSH 22 denied.
	decOK, err := fw.EvaluatePacket(EgressPacket{
		TenantID: "tenant-a",
		DstIP:    "104.18.6.192",
		Protocol: "tcp",
		DstPort:  443,
	})
	if err != nil || !decOK.Allowed {
		t.Fatalf("expected public 443 allow, got %+v err=%v", decOK, err)
	}
	decSSH, err := fw.EvaluatePacket(EgressPacket{
		TenantID: "tenant-a",
		DstIP:    "104.18.6.192",
		Protocol: "tcp",
		DstPort:  22,
	})
	if err != nil || decSSH.Allowed {
		t.Fatalf("expected public 22 deny, got %+v err=%v", decSSH, err)
	}

	// 5. DNS resolver pinning & label-boundary wildcard matching.
	dnsOK, err := fw.EvaluateDNS(DNSQuery{
		TenantID:   "tenant-a",
		QName:      "cdn-lfs.huggingface.co.",
		ResolverIP: "1.1.1.1",
	})
	if err != nil || !dnsOK.Allowed {
		t.Fatalf("expected cdn-lfs.huggingface.co allow, got %+v err=%v", dnsOK, err)
	}
	dnsEvilSuffix, err := fw.EvaluateDNS(DNSQuery{
		TenantID:   "tenant-a",
		QName:      "evilhuggingface.co",
		ResolverIP: "1.1.1.1",
	})
	if err != nil || dnsEvilSuffix.Allowed {
		t.Fatalf("expected evilhuggingface.co deny, got %+v err=%v", dnsEvilSuffix, err)
	}
	dnsRogueResolver, err := fw.EvaluateDNS(DNSQuery{
		TenantID:   "tenant-a",
		QName:      "api.openai.com",
		ResolverIP: "9.9.9.9",
	})
	if err != nil || dnsRogueResolver.Allowed || dnsRogueResolver.Reason != "unpinned_dns_resolver_blocked" {
		t.Fatalf("expected unpinned resolver deny, got %+v err=%v", dnsRogueResolver, err)
	}

	// 6. Audit-only dry-run mode sets Allowed=true and AuditOnly=true on denied traffic.
	if err := fw.SetMode(ModeAudit); err != nil {
		t.Fatal(err)
	}
	auditDec, err := fw.EvaluatePacket(EgressPacket{
		TenantID: "tenant-a",
		DstIP:    "104.18.6.192",
		Protocol: "tcp",
		DstPort:  22,
	})
	if err != nil || !auditDec.Allowed || !auditDec.AuditOnly || auditDec.Action != ActionDeny {
		t.Fatalf("expected audit-only pass-through with Action=deny, got %+v err=%v", auditDec, err)
	}
}

func TestCompileNftablesDeterministicAndSafe(t *testing.T) {
	pol, err := GenerateBaselinePolicy("prod", "strict-ai-sandbox")
	if err != nil {
		t.Fatal(err)
	}
	fw := NewFirewall(ModeEnforce)
	script, err := fw.CompileNftables(pol, "ovm0", "172.16.0.2")
	if err != nil {
		t.Fatalf("CompileNftables: %v", err)
	}
	for _, needle := range []string{
		"table inet ovm_ovm0",
		`iifname != "ovm0" accept`,
		`ip saddr != 172.16.0.2 counter drop comment "anti-spoofing"`,
		`ip daddr { 1.1.1.1, 8.8.8.8 } udp dport 53 counter accept`,
		`ip daddr { 169.254.169.254, 169.254.170.2, 100.100.100.200 } counter drop`,
		`ip daddr { 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16`,
		`meta nfproto ipv4 tcp dport 443 counter accept`,
		`counter drop comment "default-egress"`,
	} {
		if !strings.Contains(script, needle) {
			t.Errorf("compiled nftables script missing %q:\n%s", needle, script)
		}
	}
	// Verify private-network drop precedes 0.0.0.0/0 tcp dport 443 accept!
	privIdx := strings.Index(script, "block-private-lateral")
	allowIdx := strings.Index(script, "tcp dport 443 counter accept")
	if privIdx < 0 || allowIdx < 0 || privIdx > allowIdx {
		t.Fatalf("expected private-network drop before wildcard 443 accept:\n%s", script)
	}

	if _, err := fw.CompileNftables(pol, "ovm0; rm -rf /", "172.16.0.2"); err == nil {
		t.Fatalf("expected invalid tap name to be rejected")
	}
}

func TestFirewallConcurrentRaceSafety(t *testing.T) {
	fw := NewFirewall(ModeEnforce)
	pol, _ := GenerateBaselinePolicy("default", "strict-ai-sandbox")
	_ = fw.SetTenantPolicy(pol)

	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				if i%7 == 0 {
					_ = fw.SetTenantPolicy(pol)
				}
				_, _ = fw.EvaluatePacket(EgressPacket{
					TenantID: "default",
					DstIP:    "104.18.6.192",
					Protocol: "tcp",
					DstPort:  443,
				})
				_, _ = fw.EvaluateDNS(DNSQuery{
					TenantID:   "default",
					QName:      "api.openai.com",
					ResolverIP: "1.1.1.1",
				})
				_ = fw.Snapshot()
			}
		}(g)
	}
	wg.Wait()
}
