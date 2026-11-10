# Development environment

OmniVM only runs on Linux with KVM. You can develop on any OS, but microVMs
only boot on a Linux host with `/dev/kvm`.

## What runs where

| Task | Any OS (incl. Windows/macOS) | Linux + KVM | CI |
|---|---|---|---|
| `GOOS=linux go build ./...` | yes | yes | `CI` workflow |
| `GOOS=linux go vet ./...` | yes | yes | `CI` workflow |
| Unit tests (`make test`) | partly (some packages use Unix-only syscalls) | yes | `CI` workflow |
| Integration tests (`make integration`) | no | yes, as root | `Integration (KVM)` workflow |
| Benchmarks (`scripts/bench/`) | no | yes, as root | `Integration (KVM)` workflow |

The project's own development loop builds and unit-tests locally, then relies
on the `Integration (KVM)` GitHub Actions workflow to boot real microVMs. It
runs on every push to `main` and can be started by hand on any branch:

```bash
gh workflow run integration.yml --ref <branch>
gh run watch
```

The workflow fails (it does not skip) when the runner has no `/dev/kvm`.

## Running integration tests on your own Linux host

```bash
sudo apt install virtiofsd e2fsprogs iptables iproute2
make all cloud-hypervisor
sudo make kernel HOME=/root
sudo env "PATH=$PATH" HOME=/root go test -tags integration -v -count=1 -short ./test/integration/
```

The tests start their own `ovmd`. They need root because the daemon creates tap
devices and NAT rules. `-short` skips the slow pause and stop/start tests.

## Pinned versions

The Cloud Hypervisor release (`CH_VERSION`) and the guest kernel release
(`CH_KERNEL_RELEASE`) are pinned in the `Makefile`. Feature checks against
Cloud Hypervisor must be made against that version.
