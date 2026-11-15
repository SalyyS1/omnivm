package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
)

func cmdSnapshot() {
	if !isDaemonRunning() {
		fmt.Fprintln(os.Stderr, "ovmd is not running. Run 'ovm up' first.")
		os.Exit(1)
	}
	if len(os.Args) < 3 {
		snapshotUsage()
		os.Exit(1)
	}

	client := httpClient()
	switch os.Args[2] {
	case "create":
		cmdSnapshotCreate(client)
	case "list", "ls":
		cmdSnapshotList(client)
	case "info", "get":
		cmdSnapshotInfo(client)
	case "delete", "rm":
		cmdSnapshotDelete(client)
	case "branch":
		cmdSnapshotBranch(client, os.Args[3:], true)
	case "help", "--help", "-h":
		snapshotUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown snapshot command: %s\n", os.Args[2])
		snapshotUsage()
		os.Exit(1)
	}
}

func snapshotUsage() {
	fmt.Println(`Usage: ovm snapshot <command> [options]

Commands:
  create     Capture a Copy-on-Write disk and workspace snapshot of an instance
  list       List snapshots (--instance HANDLE_OR_ID, --json)
  info       Show snapshot details (--json)
  delete     Delete a snapshot and its stored artifacts
  branch     Spawn an isolated Copy-on-Write microVM branch from a snapshot

Examples:
  ovm snapshot create web --name baseline-v1
  ovm snapshot list
  ovm snapshot info baseline-v1
  ovm snapshot branch baseline-v1 --name exp-1
  ovm snapshot branch baseline-v1 --name exp-2 --no-start -- python3 /workspace/eval.py
  ovm snapshot delete baseline-v1`)
}

func cmdSnapshotCreate(client *http.Client) {
	args := os.Args[3:]
	var target, name string
	includeVMM := false
	jsonOut := false

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--name", "-n":
			if i+1 < len(args) {
				name = args[i+1]
				i++
			}
		case "--include-vmm-state":
			includeVMM = true
		case "--json", "-j":
			jsonOut = true
		default:
			if !strings.HasPrefix(args[i], "-") && target == "" {
				target = args[i]
			}
		}
	}

	if target == "" {
		fmt.Fprintln(os.Stderr, "usage: ovm snapshot create HANDLE_OR_ID [--name SNAPSHOT_NAME] [--include-vmm-state] [--json]")
		os.Exit(1)
	}

	bodyJSON, _ := json.Marshal(map[string]interface{}{
		"name":              name,
		"include_vmm_state": includeVMM,
	})
	endpoint := fmt.Sprintf("http://ovm/v1/instances/%s/snapshots", url.PathEscape(target))
	resp, err := client.Post(endpoint, "application/json", bytes.NewReader(bodyJSON))
	if err != nil {
		fmt.Fprintf(os.Stderr, "create snapshot: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		fmt.Fprintf(os.Stderr, "create snapshot failed (%d): %s\n", resp.StatusCode, strings.TrimSpace(string(body)))
		os.Exit(1)
	}

	var snap map[string]interface{}
	_ = json.Unmarshal(body, &snap)
	if jsonOut {
		pretty, _ := json.MarshalIndent(snap, "", "  ")
		fmt.Println(string(pretty))
		return
	}

	fmt.Printf("Snapshot created: %v\n", snap["id"])
	if sName, ok := snap["name"].(string); ok && sName != "" {
		fmt.Printf("Name:             %s\n", sName)
	}
	fmt.Printf("Source Instance:  %v\n", snap["source_instance_id"])
	fmt.Printf("CoW Saved:        %s (logical %s)\n",
		formatBytesUint(uint64(numField(snap, "cow_saved_bytes"))),
		formatBytesUint(uint64(numField(snap, "logical_bytes"))))
}

func cmdSnapshotList(client *http.Client) {
	args := os.Args[3:]
	var instFilter string
	jsonOut := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--instance", "-i":
			if i+1 < len(args) {
				instFilter = args[i+1]
				i++
			}
		case "--json", "-j":
			jsonOut = true
		}
	}

	endpoint := "http://ovm/v1/snapshots"
	if instFilter != "" {
		endpoint += "?instance=" + url.QueryEscape(instFilter)
	}

	resp, err := client.Get(endpoint)
	if err != nil {
		fmt.Fprintf(os.Stderr, "list snapshots: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	var snaps []map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&snaps)
	if jsonOut {
		if snaps == nil {
			snaps = []map[string]interface{}{}
		}
		out, _ := json.MarshalIndent(snaps, "", "  ")
		fmt.Println(string(out))
		return
	}

	if len(snaps) == 0 {
		fmt.Println("No snapshots")
		return
	}

	fmt.Printf("%-30s %-18s %-26s %-14s %-20s\n", "ID", "NAME", "SOURCE", "COW SAVED", "CREATED AT")
	for _, s := range snaps {
		id, _ := s["id"].(string)
		name, _ := s["name"].(string)
		if name == "" {
			name = "-"
		}
		src, _ := s["source_handle"].(string)
		if src == "" {
			src, _ = s["source_instance_id"].(string)
		}
		saved := formatBytesUint(uint64(numField(s, "cow_saved_bytes")))
		created, _ := s["created_at"].(string)
		fmt.Printf("%-30s %-18s %-26s %-14s %-20s\n", id, name, src, saved, created)
	}
}

func cmdSnapshotInfo(client *http.Client) {
	args := os.Args[3:]
	var target string
	jsonOut := false
	for _, a := range args {
		if a == "--json" || a == "-j" {
			jsonOut = true
		} else if !strings.HasPrefix(a, "-") && target == "" {
			target = a
		}
	}
	if target == "" {
		fmt.Fprintln(os.Stderr, "usage: ovm snapshot info SNAPSHOT_ID_OR_NAME [--json]")
		os.Exit(1)
	}

	resp, err := client.Get("http://ovm/v1/snapshots/" + url.PathEscape(target))
	if err != nil {
		fmt.Fprintf(os.Stderr, "get snapshot: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "snapshot %q not found\n", target)
		os.Exit(1)
	}

	var snap map[string]interface{}
	_ = json.Unmarshal(body, &snap)
	if jsonOut {
		out, _ := json.MarshalIndent(snap, "", "  ")
		fmt.Println(string(out))
		return
	}

	fmt.Printf("ID:              %v\n", snap["id"])
	if n, ok := snap["name"].(string); ok && n != "" {
		fmt.Printf("Name:            %s\n", n)
	}
	fmt.Printf("Source Instance: %v\n", snap["source_instance_id"])
	if h, ok := snap["source_handle"].(string); ok && h != "" {
		fmt.Printf("Source Handle:   %s\n", h)
	}
	if img, ok := snap["image_ref"].(string); ok && img != "" {
		fmt.Printf("Image:           %s\n", img)
	}
	fmt.Printf("Logical Size:    %s\n", formatBytesUint(uint64(numField(snap, "logical_bytes"))))
	fmt.Printf("CoW Saved:       %s\n", formatBytesUint(uint64(numField(snap, "cow_saved_bytes"))))
	fmt.Printf("Created:         %v\n", snap["created_at"])
}

func cmdSnapshotDelete(client *http.Client) {
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: ovm snapshot delete SNAPSHOT_ID_OR_NAME")
		os.Exit(1)
	}
	target := os.Args[3]
	req, _ := http.NewRequest("DELETE", "http://ovm/v1/snapshots/"+url.PathEscape(target), nil)
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "delete snapshot: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "delete snapshot failed (%d): %s\n", resp.StatusCode, strings.TrimSpace(string(body)))
		os.Exit(1)
	}
	fmt.Printf("Snapshot %s deleted\n", target)
}

func cmdSnapshotBranch(client *http.Client, args []string, fromSnapshot bool) {
	var (
		source       string
		handle       string
		workspace    string
		memoryMB     int
		vcpus        int
		diskMB       int
		excludePorts bool
		start        = true
		jsonOut      bool
		envVars      = make(map[string]string)
		command      []string
	)

	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			command = args[i+1:]
			break
		}
		switch args[i] {
		case "--name", "-n":
			if i+1 < len(args) {
				handle = args[i+1]
				i++
			}
		case "--workspace", "-w":
			if i+1 < len(args) {
				workspace = args[i+1]
				i++
			}
		case "--memory":
			if i+1 < len(args) {
				memoryMB, _ = strconv.Atoi(args[i+1])
				i++
			}
		case "--vcpus":
			if i+1 < len(args) {
				vcpus, _ = strconv.Atoi(args[i+1])
				i++
			}
		case "--disk":
			if i+1 < len(args) {
				diskMB, _ = strconv.Atoi(args[i+1])
				i++
			}
		case "--env", "-e":
			if i+1 < len(args) {
				if k, v, ok := strings.Cut(args[i+1], "="); ok {
					envVars[k] = v
				}
				i++
			}
		case "--no-start":
			start = false
		case "--exclude-ports":
			excludePorts = true
		case "--json", "-j":
			jsonOut = true
		default:
			if !strings.HasPrefix(args[i], "-") && source == "" {
				source = args[i]
			}
		}
	}

	if source == "" {
		if fromSnapshot {
			fmt.Fprintln(os.Stderr, "usage: ovm snapshot branch SNAPSHOT_ID_OR_NAME [--name HANDLE] [--no-start] [--exclude-ports] [--json] [-- COMMAND...]")
		} else {
			fmt.Fprintln(os.Stderr, "usage: ovm instance branch SOURCE_HANDLE_OR_ID [--name HANDLE] [--no-start] [--exclude-ports] [--json] [-- COMMAND...]")
		}
		os.Exit(1)
	}

	reqBody := map[string]interface{}{
		"start":         start,
		"exclude_ports": excludePorts,
	}
	if handle != "" {
		reqBody["handle"] = handle
	}
	if workspace != "" {
		reqBody["workspace"] = workspace
	}
	if memoryMB > 0 {
		reqBody["memory_mb"] = memoryMB
	}
	if vcpus > 0 {
		reqBody["vcpus"] = vcpus
	}
	if diskMB > 0 {
		reqBody["disk_mb"] = diskMB
	}
	if len(envVars) > 0 {
		reqBody["env"] = envVars
	}
	if len(command) > 0 {
		reqBody["command"] = command
	}

	var endpoint string
	if fromSnapshot {
		endpoint = "http://ovm/v1/snapshots/" + url.PathEscape(source) + "/branch"
	} else {
		endpoint = "http://ovm/v1/instances/" + url.PathEscape(source) + "/branch"
	}

	bodyJSON, _ := json.Marshal(reqBody)
	resp, err := client.Post(endpoint, "application/json", bytes.NewReader(bodyJSON))
	if err != nil {
		fmt.Fprintf(os.Stderr, "branch instance: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		fmt.Fprintf(os.Stderr, "branch failed (%d): %s\n", resp.StatusCode, strings.TrimSpace(string(body)))
		os.Exit(1)
	}

	var out map[string]interface{}
	_ = json.Unmarshal(body, &out)
	if jsonOut {
		pretty, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(pretty))
		return
	}

	fmt.Printf("Branch created: %v\n", out["id"])
	if h, ok := out["handle"].(string); ok && h != "" {
		fmt.Printf("Handle:         %s\n", h)
	}
	fmt.Printf("State:          %v\n", out["state"])
	if p, ok := out["parent_id"].(string); ok && p != "" {
		fmt.Printf("Parent ID:      %s\n", p)
	}
	if s, ok := out["source_snapshot_id"].(string); ok && s != "" {
		fmt.Printf("Snapshot ID:    %s\n", s)
	}
	fmt.Printf("CoW Saved:      %s\n", formatBytesUint(uint64(numField(out, "cow_saved_bytes"))))
}

func formatBytesUint(b uint64) string {
	const (
		kb = 1024
		mb = 1024 * kb
		gb = 1024 * mb
	)
	switch {
	case b >= gb:
		return fmt.Sprintf("%.2f GiB", float64(b)/float64(gb))
	case b >= mb:
		return fmt.Sprintf("%.2f MiB", float64(b)/float64(mb))
	case b >= kb:
		return fmt.Sprintf("%.1f KiB", float64(b)/float64(kb))
	default:
		return fmt.Sprintf("%d B", b)
	}
}
