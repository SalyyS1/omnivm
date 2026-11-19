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

	"github.com/SalyyS1/omnivm/internal/netpolicy"
	"github.com/SalyyS1/omnivm/internal/version"
	"github.com/SalyyS1/omnivm/internal/virtiofs"
)

func cmdVirtioFS() {
	if len(os.Args) < 3 {
		virtiofsUsage()
		os.Exit(1)
	}

	switch os.Args[2] {
	case "status":
		cmdVirtioFSStatus(os.Args[3:])
	case "mode":
		cmdVirtioFSMode(os.Args[3:])
	case "plan":
		cmdVirtioFSPlan(os.Args[3:])
	case "register-layer":
		cmdVirtioFSRegisterLayer(os.Args[3:])
	case "delete-layer":
		cmdVirtioFSDeleteLayer(os.Args[3:])
	case "help", "--help", "-h":
		virtiofsUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown virtiofs subcommand: %s\n", os.Args[2])
		virtiofsUsage()
		os.Exit(1)
	}
}

func virtiofsUsage() {
	fmt.Println(`Usage: ovm virtiofs <subcommand> [options]

Subcommands:
  status           Inspect shared Virtio-FS / Virtio-PMEM DAX pool layers, mounts, and dedup savings
  mode             Set pool mode on running ovmd (off | auto | dax)
  plan             Preview 2 MiB-aligned DAX window, virtiofsd flags, and Cloud Hypervisor FsConfig/PmemConfig
  register-layer   Register a read-only shared base rootfs layer in the ovmd pool
  delete-layer     Evict an unpinned shared layer by digest

Examples:
  ovm virtiofs status
  ovm virtiofs mode dax
  ovm virtiofs plan --memory-mb 512 --vcpus 4 --mode dax --pmem-file /var/lib/ovm/alpine.raw --json
  ovm virtiofs register-layer python:3.12-alpine --size-mb 64`)
}

func cmdVirtioFSStatus(args []string) {
	jsonOut := false
	for _, a := range args {
		if a == "--json" || a == "-j" {
			jsonOut = true
		}
	}
	client := requireDaemonForQoS()
	resp, err := client.Get("http://ovm/v1/virtiofs")
	if err != nil {
		fmt.Fprintf(os.Stderr, "get virtiofs status: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		fmt.Fprintf(os.Stderr, "virtiofs status failed (%d): %s\n", resp.StatusCode, string(body))
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

	var snap virtiofs.StatusSnapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		fmt.Println(string(body))
		return
	}
	fmt.Printf("OmniVM Shared Virtio-FS / Virtio-PMEM DAX Pool\n")
	fmt.Printf("  mode:             %s\n", snap.Mode)
	fmt.Printf("  shared layers:    %d cached (%d pinned, %s / %s)\n",
		snap.Metrics.ActiveLayers, snap.Metrics.PinnedLayers,
		formatBytesUint(snap.Metrics.CachedBytes), formatBytesUint(snap.Metrics.MaxCacheBytes))
	fmt.Printf("  cache efficiency: hits=%d misses=%d evictions=%d dedup_saved=%s\n",
		snap.Metrics.CacheHits, snap.Metrics.CacheMisses,
		snap.Metrics.LayerEvictions, formatBytesUint(snap.Metrics.DedupSavedBytes))
	fmt.Printf("  DAX window pool:  %d active mounts (%s / %s reserved, fallbacks=%d)\n",
		snap.Metrics.ActiveDAXMounts,
		formatBytesUint(snap.Metrics.AllocatedDAXBytes),
		formatBytesUint(snap.Metrics.MaxPoolBytes),
		snap.Metrics.DAXBudgetFallbacks)
}

func cmdVirtioFSMode(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: ovm virtiofs mode <off|auto|dax> [--json]")
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
	req, _ := http.NewRequest("PUT", "http://ovm/v1/virtiofs/mode", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "set virtiofs mode: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		fmt.Fprintf(os.Stderr, "set virtiofs mode failed (%d): %s\n", resp.StatusCode, string(body))
		os.Exit(1)
	}
	if jsonOut {
		fmt.Println(string(body))
		return
	}
	fmt.Printf("Virtio-FS / Virtio-PMEM DAX pool mode set to %s\n", mode)
}

func cmdVirtioFSPlan(args []string) {
	memoryMB := 512
	vcpus := 2
	mode := virtiofs.ModeDAX
	pmemFile := ""
	sharedDir := "/workspace"
	socketPath := "/var/run/omnivm/virtiofsd.sock"
	jsonOut := false

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--memory-mb":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--memory-mb requires an integer")
				os.Exit(1)
			}
			v, err := strconv.Atoi(args[i+1])
			if err != nil || v <= 0 {
				fmt.Fprintf(os.Stderr, "invalid --memory-mb %q\n", args[i+1])
				os.Exit(1)
			}
			memoryMB = v
			i++
		case "--vcpus":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--vcpus requires an integer")
				os.Exit(1)
			}
			v, err := strconv.Atoi(args[i+1])
			if err != nil || v <= 0 {
				fmt.Fprintf(os.Stderr, "invalid --vcpus %q\n", args[i+1])
				os.Exit(1)
			}
			vcpus = v
			i++
		case "--mode":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--mode requires off, auto, or dax")
				os.Exit(1)
			}
			mode = args[i+1]
			i++
		case "--pmem-file":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--pmem-file requires a path")
				os.Exit(1)
			}
			pmemFile = args[i+1]
			i++
		case "--shared-dir":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--shared-dir requires a path")
				os.Exit(1)
			}
			sharedDir = args[i+1]
			i++
		case "--json", "-j":
			jsonOut = true
		case "--help", "-h":
			virtiofsUsage()
			return
		default:
			fmt.Fprintf(os.Stderr, "unknown flag: %s\n", args[i])
			os.Exit(1)
		}
	}

	normMode, err := virtiofs.NormalizeMode(mode)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	pool := virtiofs.NewPool(virtiofs.Config{Mode: normMode})
	digest := ""
	if pmemFile != "" {
		entry, _, rErr := pool.RegisterLayer("", "shared-rootfs", pmemFile, 64*1024*1024)
		if rErr != nil {
			fmt.Fprintf(os.Stderr, "register pmem layer: %v\n", rErr)
			os.Exit(1)
		}
		digest = entry.Digest
	}
	plan, err := pool.PlanMount("preview-vm", "workspace", socketPath, sharedDir, memoryMB, vcpus, digest)
	if err != nil {
		fmt.Fprintf(os.Stderr, "plan mount: %v\n", err)
		os.Exit(1)
	}
	if jsonOut {
		out, _ := json.MarshalIndent(plan, "", "  ")
		fmt.Println(string(out))
		return
	}
	fmt.Printf("OmniVM Virtio-FS / Virtio-PMEM Mount Plan\n")
	fmt.Printf("  mode:             %s (cache=%s, dax=%v, window=%s)\n",
		plan.Mode, plan.CachePolicy, plan.DAXEnabled, formatBytesUint(plan.DAXWindowBytes))
	fmt.Printf("  queues & threads: num_queues=%d queue_size=%d thread_pool=%d\n",
		plan.NumQueues, plan.QueueSize, plan.ThreadPoolSize)
	fmt.Printf("  virtiofsd args:   %s\n", strings.Join(plan.VirtiofsdArgs, " "))
}

func cmdVirtioFSRegisterLayer(args []string) {
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		fmt.Fprintln(os.Stderr, "usage: ovm virtiofs register-layer <image-ref> [--file PATH] [--size-mb MB] [--digest DIGEST] [--json]")
		os.Exit(1)
	}
	imageRef := args[0]
	filePath := ""
	sizeMB := 64
	digest := ""
	jsonOut := false

	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--file":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--file requires a path")
				os.Exit(1)
			}
			filePath = args[i+1]
			i++
		case "--size-mb":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--size-mb requires an integer")
				os.Exit(1)
			}
			v, err := strconv.Atoi(args[i+1])
			if err != nil || v <= 0 {
				fmt.Fprintf(os.Stderr, "invalid --size-mb %q\n", args[i+1])
				os.Exit(1)
			}
			sizeMB = v
			i++
		case "--digest":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--digest requires a value")
				os.Exit(1)
			}
			digest = args[i+1]
			i++
		case "--json", "-j":
			jsonOut = true
		default:
			fmt.Fprintf(os.Stderr, "unknown flag: %s\n", args[i])
			os.Exit(1)
		}
	}

	client := requireDaemonForQoS()
	payload, _ := json.Marshal(map[string]interface{}{
		"digest":     digest,
		"image_ref":  imageRef,
		"file_path":  filePath,
		"size_bytes": uint64(sizeMB) * 1024 * 1024,
	})
	resp, err := client.Post("http://ovm/v1/virtiofs/layers", "application/json", bytes.NewReader(payload))
	if err != nil {
		fmt.Fprintf(os.Stderr, "register layer: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		fmt.Fprintf(os.Stderr, "register layer failed (%d): %s\n", resp.StatusCode, string(body))
		os.Exit(1)
	}
	if jsonOut {
		fmt.Println(string(body))
		return
	}
	fmt.Printf("Registered shared base layer for %s\n", imageRef)
}

func cmdVirtioFSDeleteLayer(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: ovm virtiofs delete-layer <digest>")
		os.Exit(1)
	}
	digest := args[0]
	client := requireDaemonForQoS()
	req, _ := http.NewRequest("DELETE", "http://ovm/v1/virtiofs/layers/"+digest, nil)
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "delete layer: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		fmt.Fprintf(os.Stderr, "delete layer failed (%d): %s\n", resp.StatusCode, string(body))
		os.Exit(1)
	}
	fmt.Printf("Evicted shared layer %s\n", digest)
}

func cmdNetPolicy() {
	if len(os.Args) < 3 {
		netPolicyUsage()
		os.Exit(1)
	}

	switch os.Args[2] {
	case "audit":
		cmdNetPolicyAudit(os.Args[3:])
	case "rules":
		cmdNetPolicyRules(os.Args[3:])
	case "generate":
		cmdNetPolicyGenerate(os.Args[3:])
	case "compile":
		cmdNetPolicyCompile(os.Args[3:])
	case "eval":
		cmdNetPolicyEval(os.Args[3:])
	case "status":
		cmdNetPolicyStatus(os.Args[3:])
	case "mode":
		cmdNetPolicyMode(os.Args[3:])
	case "set-tenant":
		cmdNetPolicySetTenant(os.Args[3:])
	case "delete-tenant":
		cmdNetPolicyDeleteTenant(os.Args[3:])
	case "help", "--help", "-h":
		netPolicyUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown netpolicy subcommand: %s\n", os.Args[2])
		netPolicyUsage()
		os.Exit(1)
	}
}

func netPolicyUsage() {
	fmt.Println(`Usage: ovm netpolicy <subcommand> [options]

Offline Static Analysis & Compilation Subcommands:
  audit          Static security audit of a tenant egress policy JSON (OVM-NET-001..008, --sarif supported)
  rules          List all 8 OmniVM network policy security diagnostic rules
  generate       Generate a hardened fail-closed egress policy (strict-ai-sandbox | package-build | airgapped)
  compile        Compile a tenant egress policy JSON into an atomic Linux nftables inet script
  eval           Simulate packet or DNS query evaluation against a policy JSON file

Daemon Control-Plane Subcommands:
  status         Inspect active ovmd egress firewall policies and block counters
  mode           Set daemon firewall mode (off | audit | enforce)
  set-tenant     Upload and activate a JSON egress policy for a tenant
  delete-tenant  Remove a tenant egress policy override

Examples:
  ovm netpolicy generate --tenant acme --preset strict-ai-sandbox --out policy.json
  ovm netpolicy audit policy.json --sarif netpolicy.sarif --strict
  ovm netpolicy compile policy.json --tap ovm0 --guest-ip 172.16.0.2
  ovm netpolicy eval policy.json --dst 169.254.169.254 --proto tcp --port 80`)
}

func cmdNetPolicyAudit(args []string) {
	filePath := ""
	jsonOut := false
	sarifPath := ""
	strict := false

	for i := 0; i < len(args); i++ {
		switch args[i] {
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
			netPolicyUsage()
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
		fmt.Fprintln(os.Stderr, "usage: ovm netpolicy audit <policy.json|-> [--json] [--sarif FILE] [--strict]")
		os.Exit(1)
	}

	var raw []byte
	var err error
	sourceURI := filePath
	if filePath == "-" {
		sourceURI = "stdin.netpolicy.json"
		raw, err = io.ReadAll(os.Stdin)
	} else {
		raw, err = os.ReadFile(filePath)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "read policy: %v\n", err)
		os.Exit(1)
	}

	rep, err := netpolicy.AuditPolicyBytes(raw, sourceURI)
	if err != nil {
		fmt.Fprintf(os.Stderr, "netpolicy audit error: %v\n", err)
		os.Exit(1)
	}

	if sarifPath != "" {
		sarifDoc := netpolicy.ToSARIF(rep, version.Version())
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
		fmt.Printf("OmniVM Network Policy Security Audit\n")
		fmt.Printf("  source:        %s (tenant=%s)\n", rep.SourcePath, rep.TenantID)
		fmt.Printf("  posture score: %d/100 (errors=%d, warnings=%d)\n", rep.PostureScore, rep.ErrorCount, rep.WarningCount)
		if len(rep.Findings) == 0 {
			fmt.Println("  result:        PASS (0 findings)")
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

func cmdNetPolicyRules(args []string) {
	jsonOut := false
	for _, a := range args {
		if a == "--json" || a == "-j" {
			jsonOut = true
		}
	}
	if jsonOut {
		out, _ := json.MarshalIndent(netpolicy.Catalog, "", "  ")
		fmt.Println(string(out))
		return
	}
	for _, r := range netpolicy.Catalog {
		fmt.Printf("%-12s %-7s (CVSS %s) %s: %s\n", r.ID, strings.ToUpper(r.DefaultSeverity), r.SecuritySeverity, r.Name, r.Summary)
	}
}

func cmdNetPolicyGenerate(args []string) {
	tenant := "default"
	preset := "strict-ai-sandbox"
	outPath := ""

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--tenant":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--tenant requires a tenant ID")
				os.Exit(1)
			}
			tenant = args[i+1]
			i++
		case "--preset":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--preset requires strict-ai-sandbox, package-build, or airgapped")
				os.Exit(1)
			}
			preset = args[i+1]
			i++
		case "--out", "-o":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--out requires a file path")
				os.Exit(1)
			}
			outPath = args[i+1]
			i++
		case "--help", "-h":
			netPolicyUsage()
			return
		default:
			fmt.Fprintf(os.Stderr, "unknown flag: %s\n", args[i])
			os.Exit(1)
		}
	}

	pol, err := netpolicy.GenerateBaselinePolicy(tenant, preset)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	encoded, _ := json.MarshalIndent(pol, "", "  ")
	if outPath != "" {
		if err := os.WriteFile(outPath, append(encoded, '\n'), 0644); err != nil {
			fmt.Fprintf(os.Stderr, "write %s: %v\n", outPath, err)
			os.Exit(1)
		}
		fmt.Printf("Wrote hardened %s network policy for tenant %q to %s\n", preset, pol.TenantID, outPath)
		return
	}
	fmt.Println(string(encoded))
}

func cmdNetPolicyCompile(args []string) {
	filePath := ""
	tapName := "ovm0"
	guestIP := "172.16.0.2"
	outPath := ""

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--tap":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--tap requires a tap interface name")
				os.Exit(1)
			}
			tapName = args[i+1]
			i++
		case "--guest-ip":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--guest-ip requires an IPv4 address")
				os.Exit(1)
			}
			guestIP = args[i+1]
			i++
		case "--out", "-o":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--out requires a file path")
				os.Exit(1)
			}
			outPath = args[i+1]
			i++
		case "--help", "-h":
			netPolicyUsage()
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
		fmt.Fprintln(os.Stderr, "usage: ovm netpolicy compile <policy.json|-> [--tap ovm0] [--guest-ip 172.16.0.2] [--out FILE]")
		os.Exit(1)
	}

	var raw []byte
	var err error
	if filePath == "-" {
		raw, err = io.ReadAll(os.Stdin)
	} else {
		raw, err = os.ReadFile(filePath)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "read policy: %v\n", err)
		os.Exit(1)
	}

	var pol netpolicy.TenantNetPolicy
	if err := json.Unmarshal(raw, &pol); err != nil {
		fmt.Fprintf(os.Stderr, "parse policy json: %v\n", err)
		os.Exit(1)
	}
	script, err := netpolicy.CompileNftables(pol, tapName, guestIP)
	if err != nil {
		fmt.Fprintf(os.Stderr, "compile nftables: %v\n", err)
		os.Exit(1)
	}
	if outPath != "" {
		if err := os.WriteFile(outPath, []byte(script), 0644); err != nil {
			fmt.Fprintf(os.Stderr, "write %s: %v\n", outPath, err)
			os.Exit(1)
		}
		fmt.Printf("Wrote nftables ruleset to %s\n", outPath)
		return
	}
	fmt.Print(script)
}

func cmdNetPolicyEval(args []string) {
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		fmt.Fprintln(os.Stderr, "usage: ovm netpolicy eval <policy.json> [--dst IP --proto tcp --port N | --dns QNAME] [--resolver IP] [--json]")
		os.Exit(1)
	}
	filePath := args[0]
	dstIP := ""
	srcIP := ""
	expectedSrc := ""
	proto := "tcp"
	port := 443
	qname := ""
	resolver := ""
	jsonOut := false

	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--dst":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--dst requires an IP address")
				os.Exit(1)
			}
			dstIP = args[i+1]
			i++
		case "--src":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--src requires an IP address")
				os.Exit(1)
			}
			srcIP = args[i+1]
			i++
		case "--expected-src":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--expected-src requires an IP address")
				os.Exit(1)
			}
			expectedSrc = args[i+1]
			i++
		case "--proto":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--proto requires tcp, udp, icmp, or any")
				os.Exit(1)
			}
			proto = args[i+1]
			i++
		case "--port":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--port requires an integer")
				os.Exit(1)
			}
			v, err := strconv.Atoi(args[i+1])
			if err != nil || v < 0 || v > 65535 {
				fmt.Fprintf(os.Stderr, "invalid --port %q\n", args[i+1])
				os.Exit(1)
			}
			port = v
			i++
		case "--dns":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--dns requires a domain name")
				os.Exit(1)
			}
			qname = args[i+1]
			i++
		case "--resolver":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--resolver requires an IP address")
				os.Exit(1)
			}
			resolver = args[i+1]
			i++
		case "--json", "-j":
			jsonOut = true
		default:
			fmt.Fprintf(os.Stderr, "unknown flag: %s\n", args[i])
			os.Exit(1)
		}
	}

	raw, err := os.ReadFile(filePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read policy: %v\n", err)
		os.Exit(1)
	}
	var pol netpolicy.TenantNetPolicy
	if err := json.Unmarshal(raw, &pol); err != nil {
		fmt.Fprintf(os.Stderr, "parse policy: %v\n", err)
		os.Exit(1)
	}
	if err := netpolicy.ValidatePolicy(&pol); err != nil {
		fmt.Fprintf(os.Stderr, "validate policy: %v\n", err)
		os.Exit(1)
	}

	var dec netpolicy.Decision
	if qname != "" {
		dec, err = netpolicy.EvaluateDNSWithPolicy(pol, netpolicy.DNSQuery{
			TenantID:   pol.TenantID,
			QName:      qname,
			ResolverIP: resolver,
		})
	} else if dstIP != "" {
		dec, _, err = netpolicy.EvaluatePacketWithPolicy(pol, netpolicy.EgressPacket{
			TenantID:      pol.TenantID,
			ExpectedSrcIP: expectedSrc,
			SrcIP:         srcIP,
			DstIP:         dstIP,
			Protocol:      proto,
			DstPort:       uint16(port),
		})
	} else {
		fmt.Fprintln(os.Stderr, "specify either --dst <ip> or --dns <qname>")
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "eval error: %v\n", err)
		os.Exit(1)
	}
	if jsonOut {
		out, _ := json.MarshalIndent(dec, "", "  ")
		fmt.Println(string(out))
	} else {
		fmt.Printf("verdict=%s allowed=%v reason=%s rule=%s\n", strings.ToUpper(dec.Action), dec.Allowed, dec.Reason, dec.MatchedRule)
	}
	if !dec.Allowed {
		os.Exit(1)
	}
}

func cmdNetPolicyStatus(args []string) {
	jsonOut := false
	for _, a := range args {
		if a == "--json" || a == "-j" {
			jsonOut = true
		}
	}
	client := requireDaemonForQoS()
	resp, err := client.Get("http://ovm/v1/netpolicy")
	if err != nil {
		fmt.Fprintf(os.Stderr, "get netpolicy status: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		fmt.Fprintf(os.Stderr, "netpolicy status failed (%d): %s\n", resp.StatusCode, string(body))
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

	var snap netpolicy.StatusSnapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		fmt.Println(string(body))
		return
	}
	fmt.Printf("OmniVM Per-Tenant Egress Firewall\n")
	fmt.Printf("  mode:             %s (active policies: %d)\n", snap.Mode, snap.Metrics.ActivePolicies)
	fmt.Printf("  packet verdicts:  allowed=%d denied=%d (imds=%d lateral=%d spoof=%d)\n",
		snap.Metrics.AllowedPackets, snap.Metrics.DeniedPackets,
		snap.Metrics.IMDSBlocks, snap.Metrics.LateralBlocks, snap.Metrics.SpoofBlocks)
	fmt.Printf("  DNS verdicts:     allowed=%d denied=%d (compiled_nft=%d)\n",
		snap.Metrics.AllowedDNSQueries, snap.Metrics.DeniedDNSQueries, snap.Metrics.CompiledRulesets)
}

func cmdNetPolicyMode(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: ovm netpolicy mode <off|audit|enforce> [--json]")
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
	req, _ := http.NewRequest("PUT", "http://ovm/v1/netpolicy/mode", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "set netpolicy mode: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		fmt.Fprintf(os.Stderr, "set netpolicy mode failed (%d): %s\n", resp.StatusCode, string(body))
		os.Exit(1)
	}
	if jsonOut {
		fmt.Println(string(body))
		return
	}
	fmt.Printf("NetPolicy firewall mode set to %s\n", mode)
}

func cmdNetPolicySetTenant(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: ovm netpolicy set-tenant <tenant> <policy.json> [--json]")
		os.Exit(1)
	}
	tenant := args[0]
	filePath := args[1]
	jsonOut := false
	for _, a := range args[2:] {
		if a == "--json" || a == "-j" {
			jsonOut = true
		}
	}
	raw, err := os.ReadFile(filePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read %s: %v\n", filePath, err)
		os.Exit(1)
	}
	client := requireDaemonForQoS()
	req, _ := http.NewRequest("PUT", "http://ovm/v1/netpolicy/tenants/"+tenant, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "set tenant netpolicy: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		fmt.Fprintf(os.Stderr, "set tenant netpolicy failed (%d): %s\n", resp.StatusCode, string(body))
		os.Exit(1)
	}
	if jsonOut {
		fmt.Println(string(body))
		return
	}
	fmt.Printf("Installed egress CIDR/DNS firewall policy for tenant %q\n", tenant)
}

func cmdNetPolicyDeleteTenant(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: ovm netpolicy delete-tenant <tenant>")
		os.Exit(1)
	}
	tenant := args[0]
	client := requireDaemonForQoS()
	req, _ := http.NewRequest("DELETE", "http://ovm/v1/netpolicy/tenants/"+tenant, nil)
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "delete tenant netpolicy: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "delete tenant netpolicy failed (%d): %s\n", resp.StatusCode, string(body))
		os.Exit(1)
	}
	fmt.Printf("Deleted egress firewall policy for tenant %q\n", tenant)
}
