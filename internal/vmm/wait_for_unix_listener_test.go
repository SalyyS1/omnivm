//go:build linux

package vmm

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestWaitForUnixListenerWaitsForListen binds a socket (creating the file)
// and only calls listen() later, which is the window Cloud Hypervisor's API
// socket goes through at startup.
func TestWaitForUnixListenerWaitsForListen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api.sock")
	fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	if err := syscall.Bind(fd, &syscall.SockaddrUnix{Name: path}); err != nil {
		t.Fatal(err)
	}

	const listenDelay = 300 * time.Millisecond
	go func() {
		time.Sleep(listenDelay)
		syscall.Listen(fd, 16)
	}()

	start := time.Now()
	if err := waitForUnixListener(path, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if waited := time.Since(start); waited < listenDelay {
		t.Fatalf("returned after %v, before the socket was listening", waited)
	}
}

func TestWaitForUnixListenerTimesOut(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.sock")
	if err := waitForUnixListener(path, 200*time.Millisecond); err == nil {
		t.Fatal("expected a timeout for a socket nobody listens on")
	}
}
