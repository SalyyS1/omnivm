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

// cmdInstanceMigrate live-migrates an instance to a destination ovmd socket.
func cmdInstanceMigrate(client *http.Client) {
	args := os.Args[3:]

	var target, destAddr string
	postCopy := false
	dirtyThreshold := 0
	maxRounds := 0
	jsonOut := false

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--dest", "-d":
			if i+1 < len(args) {
				destAddr = args[i+1]
				i++
			}
		case "--post-copy":
			postCopy = true
		case "--dirty-threshold":
			if i+1 < len(args) {
				n, _ := strconv.Atoi(args[i+1])
				dirtyThreshold = n
				i++
			}
		case "--max-rounds":
			if i+1 < len(args) {
				n, _ := strconv.Atoi(args[i+1])
				maxRounds = n
				i++
			}
		case "--json", "-j":
			jsonOut = true
		default:
			if !strings.HasPrefix(args[i], "-") && target == "" {
				target = args[i]
			}
		}
	}

	if target == "" || destAddr == "" {
		fmt.Fprintln(os.Stderr, "usage: ovm instance migrate HANDLE_OR_ID --dest SOCKET_PATH [--post-copy] [--dirty-threshold N] [--max-rounds N] [--json]")
		os.Exit(1)
	}

	body := map[string]interface{}{
		"dest_addr": destAddr,
		"post_copy": postCopy,
	}
	if dirtyThreshold > 0 {
		body["dirty_threshold_pages"] = dirtyThreshold
	}
	if maxRounds > 0 {
		body["max_rounds"] = maxRounds
	}

	bodyJSON, _ := json.Marshal(body)
	endpoint := fmt.Sprintf("http://ovm/v1/instances/%s/migrate", url.PathEscape(target))
	resp, err := client.Post(endpoint, "application/json", bytes.NewReader(bodyJSON))
	if err != nil {
		fmt.Fprintf(os.Stderr, "migrate: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "migrate failed (%d): %s\n", resp.StatusCode, strings.TrimSpace(string(raw)))
		os.Exit(1)
	}

	if jsonOut {
		fmt.Println(string(raw))
		return
	}

	var result struct {
		Rounds          int    `json:"rounds"`
		PagesSent       uint64 `json:"pages_sent"`
		BytesSent       uint64 `json:"bytes_sent"`
		FinalDirtyPages int    `json:"final_dirty_pages"`
		StopAndCopyMs   int64  `json:"stop_and_copy_ms"`
		PostCopyFaults  uint64 `json:"post_copy_faults,omitempty"`
		TotalMs         int64  `json:"total_ms"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		fmt.Println(string(raw))
		return
	}

	fmt.Printf("migration complete\n")
	fmt.Printf("  pre-copy rounds:    %d\n", result.Rounds)
	fmt.Printf("  pages sent:         %d\n", result.PagesSent)
	fmt.Printf("  bytes sent:         %d\n", result.BytesSent)
	fmt.Printf("  final dirty pages:  %d\n", result.FinalDirtyPages)
	fmt.Printf("  stop-and-copy:      %dms\n", result.StopAndCopyMs)
	if result.PostCopyFaults > 0 {
		fmt.Printf("  post-copy faults:   %d\n", result.PostCopyFaults)
	}
	fmt.Printf("  total:              %dms\n", result.TotalMs)
}
