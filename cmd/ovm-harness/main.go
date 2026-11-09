// ovm-harness is the guest PID 1 process that runs inside OmniVM microVMs.
//
// It listens for JSON-RPC 2.0 commands over vsock and executes tasks.
//
// Build: GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o ovm-harness ./cmd/ovm-harness
package main

import "github.com/SalyyS1/omnivm/internal/harness"

func main() {
	harness.Run()
}
