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
	"time"

	"github.com/SalyyS1/omnivm/internal/hibernate"
	"github.com/SalyyS1/omnivm/internal/version"
	"github.com/SalyyS1/omnivm/internal/vmmjail"
)

func cmdJail() {
	if len(os.Args) < 3 {
		jailUsage()
		os.Exit(1)
	}
	switch os.Args[2] {
	case "status":
		cmdJailStatus(os.Args[3:])
	case "mode":
		cmdJailMode(os.Args[3:])
	case "plan":
		cmdJailPlan(os.Args[3:])
	case "generate":
		cmdJailGenerate(os.Args[3:])
	case "audit":
		cmdJailAudit(os.Args[3:])
	case "rules":
		cmdJailRules(os.Args[3:])
	case "reconcile":
		cmdJailReconcile(os.Args[3:])
	case "help", "--help", "-h":
		jailUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown jail subcommand: %s\n", os.Args[2])
		jailUsage()
		os.Exit(1)
	}
}

func jailUsage() {
	fmt.Println(`Usage: ovm jail <subcommand> [options]

Subcommands:
  status       Inspect unprivileged VMM jail pool, active slots, and survival lease counters
  mode         Set jail mode on running ovmd (off | audit | strict)
  plan         Synthesize a subordinate UID/GID + chroot + capability-zeroed VMM JailSpec
  generate     Emit a hardened baseline JailSpec JSON file (--out <file>)
  audit        Statically audit a JailSpec JSON against OVM-JAIL-001..008 (--sarif <file>, --strict)
  rules        List all static VMM jail security rules (OVM-JAIL-001..008)
  reconcile    Run daemonless survival lease reconciliation against live VMM PIDs and start ticks`)
}

func cmdJailStatus(args []string) {
	jsonOut := false
	for _, a := range args {
		if a == "--json" || a == "-j" {
			jsonOut = true
		}
	}
	client := requireDaemonForQoS()
	resp, err := client.Get("http://ovm/v1/jail")
	if err != nil {
		fmt.Fprintf(os.Stderr, "get jail status: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		fmt.Fprintf(os.Stderr, "jail status failed (%d): %s\n", resp.StatusCode, string(body))
		os.Exit(1)
	}
	if jsonOut {
		fmt.Println(string(body))
		return
	}
	var snap vmmjail.StatusSnapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		fmt.Println(string(body))
		return
	}
	fmt.Printf("OmniVM Unprivileged VMM Jail & Daemonless Survival\n")
	fmt.Printf("  mode:             %s\n", snap.Mode)
	fmt.Printf("  subordinate pool: uid_base=%d  pool_size=%d  active_jails=%d\n",
		snap.UIDBase, snap.PoolSize, snap.Metrics.ActiveJails)
	fmt.Printf("  survival leases:  active=%d  reattached=%d  stale_pid_prevented=%d  orphaned_cleaned=%d\n",
		snap.Metrics.ActiveLeases, snap.Metrics.ReattachedVMs,
		snap.Metrics.StalePIDPrevented, snap.Metrics.OrphanedCleaned)
}

func cmdJailMode(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: ovm jail mode <off|audit|strict> [--json]")
		os.Exit(1)
	}
	payload, _ := json.Marshal(map[string]string{"mode": args[0]})
	client := requireDaemonForQoS()
	req, _ := http.NewRequest("PUT", "http://ovm/v1/jail/mode", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "set jail mode: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		fmt.Fprintf(os.Stderr, "jail mode failed (%d): %s\n", resp.StatusCode, string(body))
		os.Exit(1)
	}
	fmt.Println(string(body))
}

func cmdJailPlan(args []string) {
	instID := "sandbox-vm"
	tenant := "default"
	memMB := 512
	vcpus := 2
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--instance":
			if i+1 < len(args) {
				instID = args[i+1]
				i++
			}
		case "--tenant":
			if i+1 < len(args) {
				tenant = args[i+1]
				i++
			}
		case "--memory-mb":
			if i+1 < len(args) {
				if v, err := strconv.Atoi(args[i+1]); err == nil && v > 0 {
					memMB = v
				}
				i++
			}
		case "--vcpus":
			if i+1 < len(args) {
				if v, err := strconv.Atoi(args[i+1]); err == nil && v > 0 {
					vcpus = v
				}
				i++
			}
		}
	}
	spec := vmmjail.GenerateBaselineJailSpec(instID, tenant, memMB, vcpus)
	out, _ := json.MarshalIndent(spec, "", "  ")
	fmt.Println(string(out))
}

func cmdJailGenerate(args []string) {
	instID := "sandbox-vm"
	tenant := "default"
	outFile := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--instance":
			if i+1 < len(args) {
				instID = args[i+1]
				i++
			}
		case "--tenant":
			if i+1 < len(args) {
				tenant = args[i+1]
				i++
			}
		case "--out", "-o":
			if i+1 < len(args) {
				outFile = args[i+1]
				i++
			}
		}
	}
	spec := vmmjail.GenerateBaselineJailSpec(instID, tenant, 512, 2)
	out, _ := json.MarshalIndent(spec, "", "  ")
	if outFile != "" {
		if err := os.WriteFile(outFile, append(out, '\n'), 0644); err != nil {
			fmt.Fprintf(os.Stderr, "write %s: %v\n", outFile, err)
			os.Exit(1)
		}
		fmt.Printf("wrote baseline VMM jail spec to %s\n", outFile)
		return
	}
	fmt.Println(string(out))
}

func cmdJailAudit(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: ovm jail audit <spec.json> [--sarif <out.sarif>] [--json] [--strict]")
		os.Exit(1)
	}
	specPath := ""
	sarifPath := ""
	jsonOut := false
	strict := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--sarif":
			if i+1 < len(args) {
				sarifPath = args[i+1]
				i++
			}
		case "--json", "-j":
			jsonOut = true
		case "--strict":
			strict = true
		default:
			if !strings.HasPrefix(args[i], "-") && specPath == "" {
				specPath = args[i]
			}
		}
	}
	raw, err := os.ReadFile(specPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read %s: %v\n", specPath, err)
		os.Exit(1)
	}
	rep, err := vmmjail.AuditJailBytes(raw, specPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "audit %s: %v\n", specPath, err)
		os.Exit(1)
	}
	if sarifPath != "" {
		sBytes, _ := json.MarshalIndent(vmmjail.ToSARIF(rep, version.Version()), "", "  ")
		if err := os.WriteFile(sarifPath, append(sBytes, '\n'), 0644); err != nil {
			fmt.Fprintf(os.Stderr, "write sarif: %v\n", err)
			os.Exit(1)
		}
	}
	if jsonOut {
		out, _ := json.MarshalIndent(rep, "", "  ")
		fmt.Println(string(out))
	} else {
		fmt.Printf("VMM Jail Security Audit (%s): passed=%v errors=%d warnings=%d\n",
			rep.SourcePath, rep.Passed, rep.ErrorCount, rep.WarningCount)
		for _, f := range rep.Findings {
			fmt.Printf("  [%s] %s (%s, CVSS %s): %s\n", f.Severity, f.RuleID, f.InstanceID, f.SecurityCVSS, f.Message)
		}
	}
	if !rep.Passed || (strict && rep.WarningCount > 0) {
		os.Exit(2)
	}
}

func cmdJailRules(args []string) {
	for _, a := range args {
		if a == "--json" || a == "-j" {
			out, _ := json.MarshalIndent(vmmjail.Catalog, "", "  ")
			fmt.Println(string(out))
			return
		}
	}
	for _, r := range vmmjail.Catalog {
		fmt.Printf("%-14s [%-7s CVSS %s] %s\n", r.ID, r.DefaultSeverity, r.SecuritySeverity, r.Summary)
	}
}

func cmdJailReconcile(_ []string) {
	client := requireDaemonForQoS()
	resp, err := client.Post("http://ovm/v1/jail/reconcile", "application/json", nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reconcile leases: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	fmt.Println(string(body))
}

func cmdHibernate() {
	if len(os.Args) < 3 {
		hibernateUsage()
		os.Exit(1)
	}
	switch os.Args[2] {
	case "status":
		client := requireDaemonForQoS()
		resp, err := client.Get("http://ovm/v1/hibernate")
		if err != nil {
			fmt.Fprintf(os.Stderr, "get hibernate status: %v\n", err)
			os.Exit(1)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		fmt.Println(string(body))
	case "mode":
		if len(os.Args) < 4 {
			fmt.Fprintln(os.Stderr, "usage: ovm hibernate mode <off|idle|aggressive>")
			os.Exit(1)
		}
		payload, _ := json.Marshal(map[string]string{"mode": os.Args[3]})
		client := requireDaemonForQoS()
		req, _ := http.NewRequest("PUT", "http://ovm/v1/hibernate/mode", bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			fmt.Fprintf(os.Stderr, "set hibernate mode: %v\n", err)
			os.Exit(1)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		fmt.Println(string(body))
	case "plan":
		instID := "hibernated-vm"
		ms := int64(300000)
		args := os.Args[3:]
		for i := 0; i < len(args); i++ {
			if args[i] == "--instance" && i+1 < len(args) {
				instID = args[i+1]
				i++
			} else if args[i] == "--hibernated-ms" && i+1 < len(args) {
				if v, err := strconv.ParseInt(args[i+1], 10, 64); err == nil && v >= 0 {
					ms = v
				}
				i++
			}
		}
		eng := hibernate.NewEngine(hibernate.Config{Mode: hibernate.ModeIdle})
		now := time.Now().UTC()
		plan, err := eng.BuildRestorePlan(instID, now.Add(-time.Duration(ms)*time.Millisecond), now)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hibernate plan: %v\n", err)
			os.Exit(1)
		}
		out, _ := json.MarshalIndent(plan, "", "  ")
		fmt.Println(string(out))
	default:
		hibernateUsage()
	}
}

func hibernateUsage() {
	fmt.Println(`Usage: ovm hibernate <subcommand> [options]

Subcommands:
  status   Inspect memory hibernation zero-page compaction and entropy-reseed counters
  mode     Set hibernation mode on running ovmd (off | idle | aggressive)
  plan     Synthesize a post-hibernation guest clock-sync and virtio-rng entropy reseed RPC plan`)
}
