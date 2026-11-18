package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/SalyyS1/omnivm/internal/prefault"
)

func cmdBench() {
	if len(os.Args) < 3 {
		benchUsage()
		os.Exit(1)
	}

	switch os.Args[2] {
	case "prefault":
		cmdBenchPrefault(os.Args[3:])
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

Options for 'ovm bench prefault':
  --pool-size N      Number of simulated warm microVM slots (default: 4)
  --memory-mb MB     Memory size per microVM slot in MiB (default: 16)
  --workers W        Parallel page-touch worker goroutines (default: GOMAXPROCS, max 8)
  --iterations K     Benchmark rounds (default: 5)
  --strategy NAME    madvise-populate | touch-write | touch-read (default: madvise-populate)
  --json, -j         Emit structured JSON benchmark report`)
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
