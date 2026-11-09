//go:build integration

package integration

import (
	"strings"
	"testing"
)

// M0: Boot + Run — task mode

func TestTaskEchoHello(t *testing.T) {
	out := ovmRun(t, "run", "--", "echo", "hello from ovm")
	if !strings.Contains(out, "hello from ovm") {
		t.Fatalf("expected 'hello from ovm' in output, got: %s", out)
	}
}

func TestTaskExitCode(t *testing.T) {
	out, err := ovm("run", "--", "sh", "-c", "exit 42")
	if err == nil {
		t.Fatal("expected non-zero exit, got success")
	}
	// The CLI should propagate the exit code. err will be an *exec.ExitError.
	_ = out
}

func TestTaskStderr(t *testing.T) {
	out := ovmRun(t, "run", "--", "sh", "-c", "echo errline >&2")
	if !strings.Contains(out, "errline") {
		t.Fatalf("expected stderr output 'errline', got: %s", out)
	}
}

func TestTaskMultilineOutput(t *testing.T) {
	out := ovmRun(t, "run", "--", "sh", "-c", "echo line1 && echo line2 && echo line3")
	for _, want := range []string{"line1", "line2", "line3"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in output: %s", want, out)
		}
	}
}

func TestTaskPythonAvailable(t *testing.T) {
	out := ovmRun(t, "run", "--", "python3", "-c", "print('python works')")
	if !strings.Contains(out, "python works") {
		t.Fatalf("expected 'python works', got: %s", out)
	}
}

func TestDaemonStatus(t *testing.T) {
	out := ovmRun(t, "status")
	if !strings.Contains(out, "running") {
		t.Fatalf("expected 'running' in status, got: %s", out)
	}
	if !strings.Contains(out, "cloud-hypervisor") {
		t.Fatalf("expected 'cloud-hypervisor' backend in status, got: %s", out)
	}
}
