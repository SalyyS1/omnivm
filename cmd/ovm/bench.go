package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/SalyyS1/omnivm/internal/ioqos"
	"github.com/SalyyS1/omnivm/internal/prefault"
	"github.com/SalyyS1/omnivm/internal/virtiofs"
)

func cmdBench() {
	if len(os.Args) < 3 {
		benchUsage()
		os.Exit(1)
	}

	switch os.Args[2] {
	case "prefault":
		cmdBenchPrefault(os.Args[3:])
	case "ioqos":
		cmdBenchIOQoS(os.Args[3:])
	case "virtiofs":
		cmdBenchVirtioFS(os.Args[3:])
	case "help", "--help", "-h":
		benchUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown bench subcommand: %s\n", os.Args[2])
		benchUsage()
		os.Exit(1)
	}
}

func benchUsage() {
	fmt.Println(`Usage: ovm bench <subcommand> [options]

Subcommands:
  prefault   Benchmark warm-pool page pre-faulting vs demand-paged first touch
  ioqos      Benchmark multi-tenant token-bucket I/O QoS noisy-neighbor isolation
  virtiofs   Benchmark shared Virtio-FS / Virtio-PMEM DAX mapping vs private guest page-cache copies

Options for 'ovm bench prefault':
  --pool-size N      Number of simulated warm microVM slots (default: 4)
  --memory-mb MB     Memory size per microVM slot in MiB (default: 16)
  --workers W        Parallel page-touch worker goroutines (default: GOMAXPROCS, max 8)
  --iterations K     Benchmark rounds (default: 5)
  --strategy NAME    madvise-populate | touch-write | touch-read (default: madvise-populate)
  --json, -j         Emit structured JSON benchmark report

Options for 'ovm bench ioqos':
  --tenants N        Total tenants (1 noisy + N-1 latency-sensitive, default: 3)
  --noisy-vms N      Concurrent microVMs spawned by noisy tenant (default: 4)
  --blk-mbps MB      Per-tenant virtio-blk ceiling in MiB/s (default: 100)
  --net-mbps MB      Per-tenant virtio-net ceiling in MiB/s (default: 50)
  --duration-ms MS   Simulated contention window in ms (default: 2000)
  --json, -j         Emit structured JSON benchmark report`)
}

func cmdBenchIOQoS(args []string) {
	var cfg ioqos.BenchmarkConfig
	jsonOut := false

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--tenants":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--tenants requires an integer")
				os.Exit(1)
			}
			v, err := strconv.Atoi(args[i+1])
			if err != nil || v < 2 {
				fmt.Fprintf(os.Stderr, "invalid --tenants %q\n", args[i+1])
				os.Exit(1)
			}
			cfg.Tenants = v
			i++
		case "--noisy-vms":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--noisy-vms requires an integer")
				os.Exit(1)
			}
			v, err := strconv.Atoi(args[i+1])
			if err != nil || v < 1 {
				fmt.Fprintf(os.Stderr, "invalid --noisy-vms %q\n", args[i+1])
				os.Exit(1)
			}
			cfg.NoisyVMs = v
			i++
		case "--blk-mbps":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--blk-mbps requires an integer")
				os.Exit(1)
			}
			v, err := strconv.Atoi(args[i+1])
			if err != nil || v < 1 {
				fmt.Fprintf(os.Stderr, "invalid --blk-mbps %q\n", args[i+1])
				os.Exit(1)
			}
			cfg.TenantBlkMBps = v
			i++
		case "--net-mbps":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--net-mbps requires an integer")
				os.Exit(1)
			}
			v, err := strconv.Atoi(args[i+1])
			if err != nil || v < 1 {
				fmt.Fprintf(os.Stderr, "invalid --net-mbps %q\n", args[i+1])
				os.Exit(1)
			}
			cfg.TenantNetMBps = v
			i++
		case "--duration-ms":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--duration-ms requires an integer")
				os.Exit(1)
			}
			v, err := strconv.Atoi(args[i+1])
			if err != nil || v < 100 {
				fmt.Fprintf(os.Stderr, "invalid --duration-ms %q\n", args[i+1])
				os.Exit(1)
			}
			cfg.DurationMs = v
			i++
		case "--json", "-j":
			jsonOut = true
		case "--help", "-h":
			benchUsage()
			return
		default:
			fmt.Fprintf(os.Stderr, "unknown flag: %s\n", args[i])
			benchUsage()
			os.Exit(1)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rep, err := ioqos.RunBenchmark(ctx, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bench ioqos: %v\n", err)
		os.Exit(1)
	}

	if jsonOut {
		out, _ := json.MarshalIndent(rep, "", "  ")
		fmt.Println(string(out))
		return
	}

	fmt.Printf("OmniVM Multi-Tenant Token-Bucket I/O QoS Benchmark\n")
	fmt.Printf("  mode:                 %s (%d tenants, %d noisy VMs, %d ms window)\n",
		rep.Mode, rep.Tenants, rep.NoisyVMs, rep.DurationMs)
	fmt.Printf("  tenant ceilings:      blk=%d MiB/s  net=%d MiB/s\n",
		rep.TenantBlkCeilingMBps, rep.TenantNetCeilingMBps)
	fmt.Printf("  fairness (Jain):      intra_tenant=%.4f  inter_tenant=%.4f\n",
		rep.IntraTenantFairnessIndex, rep.InterTenantFairnessIndex)
	fmt.Printf("  quiet SLO isolated:   zero_throttles=%v  noisy_excess_shed=%.2f%%\n",
		rep.QuietTenantZeroThrottles, rep.NoisyExcessShedPercent)
	for _, tr := range rep.TenantResults {
		fmt.Printf("  [%s] role=%s vms=%d blk=%.1f/%.1f MiB net=%.1f/%.1f MiB throttles=%d slo=%.2f%%\n",
			tr.TenantID, tr.Role, tr.ActiveVMs,
			tr.AdmittedBlkMB, tr.RequestedBlkMB,
			tr.AdmittedNetMB, tr.RequestedNetMB,
			tr.ThrottledBursts, tr.SLOAttainmentRatio*100)
	}
}

func cmdBenchPrefault(args []string) {
	var cfg prefault.BenchmarkConfig
	jsonOut := false

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--pool-size":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--pool-size requires an integer")
				os.Exit(1)
			}
			v, err := strconv.Atoi(args[i+1])
			if err != nil || v <= 0 {
				fmt.Fprintf(os.Stderr, "invalid --pool-size %q\n", args[i+1])
				os.Exit(1)
			}
			cfg.PoolSize = v
			i++
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
			cfg.SlotMemoryMB = v
			i++
		case "--workers":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--workers requires an integer")
				os.Exit(1)
			}
			v, err := strconv.Atoi(args[i+1])
			if err != nil || v <= 0 {
				fmt.Fprintf(os.Stderr, "invalid --workers %q\n", args[i+1])
				os.Exit(1)
			}
			cfg.Workers = v
			i++
		case "--iterations":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--iterations requires an integer")
				os.Exit(1)
			}
			v, err := strconv.Atoi(args[i+1])
			if err != nil || v <= 0 {
				fmt.Fprintf(os.Stderr, "invalid --iterations %q\n", args[i+1])
				os.Exit(1)
			}
			cfg.Iterations = v
			i++
		case "--strategy":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--strategy requires a value")
				os.Exit(1)
			}
			cfg.Strategy = args[i+1]
			i++
		case "--json", "-j":
			jsonOut = true
		case "--help", "-h":
			benchUsage()
			return
		default:
			fmt.Fprintf(os.Stderr, "unknown flag: %s\n", args[i])
			benchUsage()
			os.Exit(1)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	res, err := prefault.RunBenchmark(ctx, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bench prefault: %v\n", err)
		os.Exit(1)
	}

	if jsonOut {
		out, _ := json.MarshalIndent(res, "", "  ")
		fmt.Println(string(out))
		return
	}

	fmt.Printf("OmniVM Warm-Pool Pre-Faulting Benchmark\n")
	fmt.Printf("  strategy:            %s (madvise_populate=%v)\n", res.Strategy, res.UsedMadvise)
	fmt.Printf("  pool:                %d slots x %d MiB (%d pages/iter, workers=%d, iterations=%d)\n",
		res.PoolSize, res.SlotMemoryMB, res.TotalPagesPerIteration, res.Workers, res.Iterations)
	fmt.Printf("  prefault throughput: %.2f MiB/s (mean %.1f us/slot)\n",
		res.PrefaultThroughputMBs, res.PrefaultPassMeanMicros)
	fmt.Printf("  demand-paged wake:   p50=%d us  p95=%d us  p99=%d us  mean=%.1f us  (minflt=%d)\n",
		res.DemandPaged.P50Micros, res.DemandPaged.P95Micros, res.DemandPaged.P99Micros,
		res.DemandPaged.MeanMicros, res.DemandMinorFaults)
	fmt.Printf("  warm-pool wake:      p50=%d us  p95=%d us  p99=%d us  mean=%.1f us  (minflt=%d)\n",
		res.WarmPrefaulted.P50Micros, res.WarmPrefaulted.P95Micros, res.WarmPrefaulted.P99Micros,
		res.WarmPrefaulted.MeanMicros, res.WarmMinorFaults)
	fmt.Printf("  wake speedup:        %.2fx\n", res.SpeedupFactor)
}

func cmdBenchVirtioFS(args []string) {
	var cfg virtiofs.BenchmarkConfig
	jsonOut := false

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--vms":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--vms requires an integer")
				os.Exit(1)
			}
			v, err := strconv.Atoi(args[i+1])
			if err != nil || v < 1 {
				fmt.Fprintf(os.Stderr, "invalid --vms %q\n", args[i+1])
				os.Exit(1)
			}
			cfg.VMs = v
			i++
		case "--layer-mb":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--layer-mb requires an integer")
				os.Exit(1)
			}
			v, err := strconv.Atoi(args[i+1])
			if err != nil || v < 1 {
				fmt.Fprintf(os.Stderr, "invalid --layer-mb %q\n", args[i+1])
				os.Exit(1)
			}
			cfg.LayerMB = v
			i++
		case "--iterations":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--iterations requires an integer")
				os.Exit(1)
			}
			v, err := strconv.Atoi(args[i+1])
			if err != nil || v < 1 {
				fmt.Fprintf(os.Stderr, "invalid --iterations %q\n", args[i+1])
				os.Exit(1)
			}
			cfg.Iterations = v
			i++
		case "--json", "-j":
			jsonOut = true
		case "--help", "-h":
			benchUsage()
			return
		default:
			fmt.Fprintf(os.Stderr, "unknown flag: %s\n", args[i])
			benchUsage()
			os.Exit(1)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rep, err := virtiofs.RunBenchmark(ctx, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bench virtiofs: %v\n", err)
		os.Exit(1)
	}

	if jsonOut {
		out, _ := json.MarshalIndent(rep, "", "  ")
		fmt.Println(string(out))
		return
	}

	fmt.Printf("OmniVM Shared Virtio-FS / Virtio-PMEM DAX Pool Benchmark\n")
	fmt.Printf("  fleet:                %d VMs x %d MiB base layer (%d pages/VM, %d iterations)\n",
		rep.VMs, rep.LayerMB, rep.PagesPerVM, rep.Iterations)
	fmt.Printf("  private guest cache:  %.1f MiB/s  p50=%d us  p95=%d us  RAM=%d MiB\n",
		rep.PrivateGuestCache.ThroughputMBs, rep.PrivateGuestCache.Latency.P50Micros,
		rep.PrivateGuestCache.Latency.P95Micros, rep.PrivateGuestCache.ResidentMemoryBytes/(1024*1024))
	fmt.Printf("  shared DAX mapping:   %.1f MiB/s  p50=%d us  p95=%d us  RAM=%d MiB\n",
		rep.SharedDAXPool.ThroughputMBs, rep.SharedDAXPool.Latency.P50Micros,
		rep.SharedDAXPool.Latency.P95Micros, rep.SharedDAXPool.ResidentMemoryBytes/(1024*1024))
	fmt.Printf("  gains:                %.2fx throughput  %.2fx RAM reduction (saved %d MiB, %d pool hits)\n",
		rep.ThroughputSpeedup, rep.MemoryReductionRatio,
		rep.MemorySavedBytes/(1024*1024), rep.PoolCacheHits)
}
