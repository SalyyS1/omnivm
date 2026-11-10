//go:build integration

package integration

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

// largeImage is bigger than the old fixed 512MB root disk once unpacked.
const largeImage = "python:3.12"

// guestRootAvailableMB returns the free space of the guest root filesystem.
func guestRootAvailableMB(t *testing.T, id string) int {
	t.Helper()
	out := strings.TrimSpace(ovmExec(t, id, "sh", "-c", "df -Pm / | awk 'NR==2 {print $4}'"))
	mb, err := strconv.Atoi(lastLine(out))
	if err != nil {
		t.Fatalf("parse df output %q: %v", out, err)
	}
	return mb
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// TestDiskSize_LargeImageBootsWithAutomaticSize boots an image whose contents
// exceed 512MB without asking for a disk size.
func TestDiskSize_LargeImageBootsWithAutomaticSize(t *testing.T) {
	inst := apiPost(t, "/v1/instances", map[string]interface{}{
		"command":   []string{"sleep", "300"},
		"handle":    "disk-auto",
		"image_ref": largeImage,
	})
	id := inst["id"].(string)
	t.Cleanup(func() { apiDeleteAllowFail(t, "/v1/instances/"+id) })

	waitForState(t, id, "running", 5*time.Minute)

	out := ovmExec(t, id, "python3", "-c", "print('large image ok')")
	if !strings.Contains(out, "large image ok") {
		t.Fatalf("python in %s did not run: %s", largeImage, out)
	}
	if free := guestRootAvailableMB(t, id); free < 200 {
		t.Fatalf("automatic root disk left only %dMB free", free)
	}
}

// TestDiskSize_ExplicitSizeAllowsLargeWrites requests a 3GB root disk and
// writes a file larger than the old 512MB disk inside the guest.
func TestDiskSize_ExplicitSizeAllowsLargeWrites(t *testing.T) {
	const diskMB = 3072
	const writeMB = 700

	inst := apiPost(t, "/v1/instances", map[string]interface{}{
		"command":   []string{"sleep", "300"},
		"handle":    "disk-explicit",
		"image_ref": largeImage,
		"disk_mb":   diskMB,
	})
	id := inst["id"].(string)
	t.Cleanup(func() { apiDeleteAllowFail(t, "/v1/instances/"+id) })

	waitForState(t, id, "running", 5*time.Minute)

	if free := guestRootAvailableMB(t, id); free < writeMB {
		t.Fatalf("requested %dMB disk but only %dMB free", diskMB, free)
	}

	cmd := fmt.Sprintf("dd if=/dev/zero of=/opt/bigfile bs=1M count=%d status=none && sync && stat -c %%s /opt/bigfile", writeMB)
	out := ovmExec(t, id, "sh", "-c", cmd)
	want := strconv.Itoa(writeMB * 1024 * 1024)
	if lastLine(out) != want {
		t.Fatalf("expected a %s byte file, got: %s", want, out)
	}
}

// TestDiskSize_RejectsOversizedRequest checks the daemon-side upper bound.
func TestDiskSize_RejectsOversizedRequest(t *testing.T) {
	status, body := apiPostRaw(t, "/v1/instances", map[string]interface{}{
		"command": []string{"true"},
		"handle":  "disk-too-big",
		"disk_mb": 10 * 1024 * 1024,
	})
	if status != 400 {
		t.Fatalf("expected 400 for an oversized disk_mb, got %d: %s", status, body)
	}
}
