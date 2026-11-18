package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/SalyyS1/omnivm/internal/ioqos"
	"github.com/SalyyS1/omnivm/internal/seccomp"
	"github.com/SalyyS1/omnivm/internal/version"
)

func cmdQoS() {
	if len(os.Args) < 3 {
		qosUsage()
		os.Exit(1)
	}

	switch os.Args[2] {
	case "status":
		cmdQoSStatus(os.Args[3:])
	case "mode":
		cmdQoSMode(os.Args[3:])
	case "set-tenant":
		cmdQoSSetTenant(os.Args[3:])
	case "delete-tenant":
		cmdQoSDeleteTenant(os.Args[3:])
	case "help", "--help", "-h":
		qosUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown qos subcommand: %s\n", os.Args[2])
		qosUsage()
		os.Exit(1)
	}
}

func qosUsage() {
	fmt.Println(`Usage: ovm qos <subcommand> [options]

Subcommands:
  status          Inspect active multi-tenant token-bucket policies, allocations, and fairness
  mode            Set governor mode (off | static | fairshare)
  set-tenant      Configure aggregate virtio-blk and virtio-net limits for a tenant
  delete-tenant   Remove a custom tenant QoS policy override

Examples:
  ovm qos status
  ovm qos mode fairshare
  ovm qos set-tenant acme --blk-mbps 120 --blk-iops 6000 --net-mbps 60 --net-pps 15000
  ovm qos delete-tenant acme`)
}

func requireDaemonForQoS() *http.Client {
	if !isDaemonRunning() {
		fmt.Fprintln(os.Stderr, "ovmd is not running. Run 'ovm up' first.")
		os.Exit(1)
	}
	return httpClient()
}

func cmdQoSStatus(args []string) {
	jsonOut := false
	for _, a := range args {
		if a == "--json" || a == "-j" {
			jsonOut = true
		}
	}
	client := requireDaemonForQoS()
	resp, err := client.Get("http://ovm/v1/qos")
	if err != nil {
		fmt.Fprintf(os.Stderr, "get qos status: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		fmt.Fprintf(os.Stderr, "qos status failed (%d): %s\n", resp.StatusCode, string(body))
		os.Exit(1)
	}
	if jsonOut {
		var pretty interface{}
		if json.Unmarshal(body, &pretty) == nil {
			out, _ := json.MarshalIndent(pretty, "", "  ")
			fmt.Println(string(out))
			return
		}
		fmt.Println(string(body))
		return
	}

	var snap ioqos.StatusSnapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		fmt.Println(string(body))
		return
	}
	fmt.Printf("OmniVM Multi-Tenant I/O QoS Governor\n")
	fmt.Printf("  mode:             %s\n", snap.Mode)
	fmt.Printf("  active tenants:   %d (active microVMs: %d, Jain fairness: %.4f)\n",
		snap.Metrics.ActiveTenants, snap.Metrics.ActiveInstances, snap.Metrics.FairnessIndex)
	fmt.Printf("  admitted I/O:     blk=%s (%d ops)  net=%s (%d pkts)\n",
		formatBytesUint(snap.Metrics.BlkAdmittedBytes), snap.Metrics.BlkAdmittedOps,
		formatBytesUint(snap.Metrics.NetAdmittedBytes), snap.Metrics.NetAdmittedOps)
	fmt.Printf("  throttles:        tenant_blk=%d tenant_net=%d inst_blk=%d inst_net=%d\n",
		snap.Metrics.TenantBlkThrottles, snap.Metrics.TenantNetThrottles,
		snap.Metrics.InstanceBlkThrottles, snap.Metrics.InstanceNetThrottles)
	if len(snap.Tenants) > 0 {
		fmt.Println("  tenant policies:")
		for _, p := range snap.Tenants {
			fmt.Printf("    - %s: blk=%d MiB/s (%d IOPS), net=%d MiB/s (%d PPS), burst=%.2fx\n",
				p.TenantID, p.MaxBlkMBps, p.MaxBlkIOPS, p.MaxNetMBps, p.MaxNetPPS, p.BurstRatio)
		}
	}
	if len(snap.Allocations) > 0 {
		fmt.Println("  effective VM allocations:")
		for _, a := range snap.Allocations {
			fmt.Printf("    - %s [tenant=%s weight=%d]: blk=%s/s (%d IOPS)  net=%s/s (%d PPS)\n",
				a.InstanceID, a.TenantID, a.Weight,
				formatBytesUint(a.BlkBytesPerSec), a.BlkIOPS,
				formatBytesUint(a.NetBytesPerSec), a.NetPPS)
		}
	}
}

func cmdQoSMode(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: ovm qos mode <off|static|fairshare> [--json]")
		os.Exit(1)
	}
	mode := args[0]
	jsonOut := false
	for _, a := range args[1:] {
		if a == "--json" || a == "-j" {
			jsonOut = true
		}
	}
	client := requireDaemonForQoS()
	payload, _ := json.Marshal(map[string]string{"mode": mode})
	req, _ := http.NewRequest("PUT", "http://ovm/v1/qos/mode", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "set qos mode: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		fmt.Fprintf(os.Stderr, "set qos mode failed (%d): %s\n", resp.StatusCode, string(body))
		os.Exit(1)
	}
	if jsonOut {
		fmt.Println(string(body))
		return
	}
	fmt.Printf("I/O QoS governor mode set to %s\n", mode)
}

func cmdQoSSetTenant(args []string) {
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		fmt.Fprintln(os.Stderr, "usage: ovm qos set-tenant <tenant> [--blk-mbps N] [--blk-iops N] [--net-mbps N] [--net-pps N] [--burst-ratio F] [--json]")
		os.Exit(1)
	}
	tenant := args[0]
	pol := ioqos.TenantPolicy{TenantID: tenant}
	jsonOut := false

	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--blk-mbps":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--blk-mbps requires an integer")
				os.Exit(1)
			}
			v, err := strconv.Atoi(args[i+1])
			if err != nil || v < 0 {
				fmt.Fprintf(os.Stderr, "invalid --blk-mbps %q\n", args[i+1])
				os.Exit(1)
			}
			pol.MaxBlkMBps = v
			i++
		case "--blk-iops":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--blk-iops requires an integer")
				os.Exit(1)
			}
			v, err := strconv.Atoi(args[i+1])
			if err != nil || v < 0 {
				fmt.Fprintf(os.Stderr, "invalid --blk-iops %q\n", args[i+1])
				os.Exit(1)
			}
			pol.MaxBlkIOPS = v
			i++
		case "--net-mbps":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--net-mbps requires an integer")
				os.Exit(1)
			}
			v, err := strconv.Atoi(args[i+1])
			if err != nil || v < 0 {
				fmt.Fprintf(os.Stderr, "invalid --net-mbps %q\n", args[i+1])
				os.Exit(1)
			}
			pol.MaxNetMBps = v
			i++
		case "--net-pps":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--net-pps requires an integer")
				os.Exit(1)
			}
			v, err := strconv.Atoi(args[i+1])
			if err != nil || v < 0 {
				fmt.Fprintf(os.Stderr, "invalid --net-pps %q\n", args[i+1])
				os.Exit(1)
			}
			pol.MaxNetPPS = v
			i++
		case "--burst-ratio":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--burst-ratio requires a number >= 1.0")
				os.Exit(1)
			}
			v, err := strconv.ParseFloat(args[i+1], 64)
			if err != nil || v < 1.0 {
				fmt.Fprintf(os.Stderr, "invalid --burst-ratio %q\n", args[i+1])
				os.Exit(1)
			}
			pol.BurstRatio = v
			i++
		case "--json", "-j":
			jsonOut = true
		default:
			fmt.Fprintf(os.Stderr, "unknown flag: %s\n", args[i])
			os.Exit(1)
		}
	}

	client := requireDaemonForQoS()
	payload, _ := json.Marshal(pol)
	req, _ := http.NewRequest("PUT", "http://ovm/v1/qos/tenants/"+tenant, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "set tenant qos: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		fmt.Fprintf(os.Stderr, "set tenant qos failed (%d): %s\n", resp.StatusCode, string(body))
		os.Exit(1)
	}
	if jsonOut {
		fmt.Println(string(body))
		return
	}
	fmt.Printf("Configured QoS policy for tenant %q (blk=%d MiB/s, blk_iops=%d, net=%d MiB/s, net_pps=%d)\n",
		tenant, pol.MaxBlkMBps, pol.MaxBlkIOPS, pol.MaxNetMBps, pol.MaxNetPPS)
}

func cmdQoSDeleteTenant(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: ovm qos delete-tenant <tenant>")
		os.Exit(1)
	}
	tenant := args[0]
	client := requireDaemonForQoS()
	req, _ := http.NewRequest("DELETE", "http://ovm/v1/qos/tenants/"+tenant, nil)
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "delete tenant qos: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "delete tenant qos failed (%d): %s\n", resp.StatusCode, string(body))
		os.Exit(1)
	}
	fmt.Printf("Deleted QoS policy override for tenant %q\n", tenant)
}

func cmdSeccomp() {
	if len(os.Args) < 3 {
		seccompUsage()
		os.Exit(1)
	}

	switch os.Args[2] {
	case "audit":
		cmdSeccompAudit(os.Args[3:])
	case "generate":
		cmdSeccompGenerate(os.Args[3:])
	case "rules":
		cmdSeccompRules(os.Args[3:])
	case "help", "--help", "-h":
		seccompUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown seccomp subcommand: %s\n", os.Args[2])
		seccompUsage()
		os.Exit(1)
	}
}

func seccompUsage() {
	fmt.Println(`Usage: ovm seccomp <subcommand> [options]

Subcommands:
  audit      Static security audit of a Linux seccomp-bpf JSON profile (OVM-SEC-001..010)
  generate   Generate a hardened fail-closed (SCMP_ACT_ERRNO) seccomp baseline profile
  rules      List all 10 OmniVM seccomp security diagnostic rules

Options for 'ovm seccomp audit <profile.json|->':
  --target ROLE      workload | vmm | virtiofsd (default: workload)
  --json, -j         Emit structured JSON audit report
  --sarif FILE       Write OASIS SARIF v2.1.0 report to FILE (use '-' for stdout)
  --strict           Fail with exit code 1 on warnings as well as errors

Options for 'ovm seccomp generate':
  --target ROLE      workload | vmm | virtiofsd (default: workload)
  --arch ARCH        x86_64 | aarch64 (default: x86_64)
  --out FILE         Write generated profile JSON to FILE (default: stdout)`)
}

func cmdSeccompAudit(args []string) {
	var filePath string
	targetStr := "workload"
	jsonOut := false
	sarifPath := ""
	strict := false

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--target":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--target requires workload, vmm, or virtiofsd")
				os.Exit(1)
			}
			targetStr = args[i+1]
			i++
		case "--sarif":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--sarif requires an output file path or '-'")
				os.Exit(1)
			}
			sarifPath = args[i+1]
			i++
		case "--json", "-j":
			jsonOut = true
		case "--strict":
			strict = true
		case "--help", "-h":
			seccompUsage()
			return
		default:
			if strings.HasPrefix(args[i], "-") && args[i] != "-" {
				fmt.Fprintf(os.Stderr, "unknown flag: %s\n", args[i])
				os.Exit(1)
			}
			if filePath == "" {
				filePath = args[i]
			}
		}
	}

	if filePath == "" {
		fmt.Fprintln(os.Stderr, "usage: ovm seccomp audit <profile.json|-> [--target workload|vmm|virtiofsd] [--json] [--sarif FILE] [--strict]")
		os.Exit(1)
	}

	role, err := seccomp.ParseTargetRole(targetStr)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	var raw []byte
	sourceURI := filePath
	if filePath == "-" {
		sourceURI = "stdin.seccomp.json"
		raw, err = io.ReadAll(os.Stdin)
	} else {
		raw, err = os.ReadFile(filePath)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "read profile: %v\n", err)
		os.Exit(1)
	}

	rep, err := seccomp.AuditProfile(raw, sourceURI, role)
	if err != nil {
		fmt.Fprintf(os.Stderr, "seccomp audit error: %v\n", err)
		os.Exit(1)
	}

	if sarifPath != "" {
		sarifDoc := seccomp.ToSARIF(rep, version.Version())
		sarifBytes, _ := json.MarshalIndent(sarifDoc, "", "  ")
		if sarifPath == "-" {
			fmt.Println(string(sarifBytes))
		} else if wErr := os.WriteFile(sarifPath, append(sarifBytes, '\n'), 0644); wErr != nil {
			fmt.Fprintf(os.Stderr, "write sarif %s: %v\n", sarifPath, wErr)
			os.Exit(1)
		}
	}

	if jsonOut {
		out, _ := json.MarshalIndent(rep, "", "  ")
		fmt.Println(string(out))
	} else if sarifPath != "-" {
		fmt.Printf("OmniVM Seccomp Syscall Profile Audit\n")
		fmt.Printf("  source:          %s (format=%s, target=%s)\n", rep.SourcePath, rep.Format, rep.Target)
		fmt.Printf("  default_action:  %s (architectures=%v)\n", rep.DefaultAction, rep.Architectures)
		fmt.Printf("  syscall surface: %d allowed (%d high-risk)\n", rep.AllowedCount, rep.HighRiskCount)
		fmt.Printf("  posture score:   %d/100 (errors=%d, warnings=%d)\n", rep.PostureScore, rep.ErrorCount, rep.WarningCount)
		if len(rep.Findings) == 0 {
			fmt.Println("  result:          PASS (0 findings)")
		} else {
			for _, f := range rep.Findings {
				fmt.Printf("  [%s] %s (%s): %s\n", strings.ToUpper(f.Severity), f.RuleID, f.Scope, f.Message)
			}
		}
	}

	if rep.ErrorCount > 0 || (strict && rep.WarningCount > 0) {
		os.Exit(1)
	}
}

func cmdSeccompGenerate(args []string) {
	targetStr := "workload"
	archStr := "x86_64"
	outPath := ""

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--target":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--target requires workload, vmm, or virtiofsd")
				os.Exit(1)
			}
			targetStr = args[i+1]
			i++
		case "--arch":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--arch requires x86_64 or aarch64")
				os.Exit(1)
			}
			archStr = args[i+1]
			i++
		case "--out", "-o":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--out requires a file path")
				os.Exit(1)
			}
			outPath = args[i+1]
			i++
		case "--help", "-h":
			seccompUsage()
			return
		default:
			fmt.Fprintf(os.Stderr, "unknown flag: %s\n", args[i])
			os.Exit(1)
		}
	}

	role, err := seccomp.ParseTargetRole(targetStr)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	prof := seccomp.GenerateBaselineProfile(role, archStr)
	encoded, _ := json.MarshalIndent(prof, "", "  ")
	if outPath != "" {
		if err := os.WriteFile(outPath, append(encoded, '\n'), 0644); err != nil {
			fmt.Fprintf(os.Stderr, "write %s: %v\n", outPath, err)
			os.Exit(1)
		}
		fmt.Printf("Wrote hardened %s seccomp profile to %s\n", role, outPath)
		return
	}
	fmt.Println(string(encoded))
}

func cmdSeccompRules(args []string) {
	jsonOut := false
	for _, a := range args {
		if a == "--json" || a == "-j" {
			jsonOut = true
		}
	}
	if jsonOut {
		out, _ := json.MarshalIndent(seccomp.Catalog, "", "  ")
		fmt.Println(string(out))
		return
	}
	for _, r := range seccomp.Catalog {
		fmt.Printf("%-12s %-7s (CVSS %s) %s: %s\n", r.ID, strings.ToUpper(r.DefaultSeverity), r.SecuritySeverity, r.Name, r.Summary)
	}
}
