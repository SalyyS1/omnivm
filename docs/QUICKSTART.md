# Quickstart

Build OmniVM from source and run a command inside a microVM on a Linux host.

> OmniVM is pre-alpha. There are no release packages yet; build from source.

## 1. Prerequisites

| | Requirement |
|---|---|
| **CPU** | x86_64 or arm64 with hardware virtualization |
| **OS** | Linux with KVM (tested target: Ubuntu 22.04+) |
| **Root** | Yes (`sudo`) — tap networking and iptables need it |
| **Go** | 1.25+ (to build) |

Check KVM:

```bash
ls -l /dev/kvm     # must exist
```

## 2. Build

```bash
sudo apt install virtiofsd e2fsprogs iptables iproute2

git clone https://github.com/SalyyS1/omnivm.git
cd omnivm
make cloud-hypervisor    # download the pinned Cloud Hypervisor static binary into ./bin
make kernel              # download the pinned prebuilt microVM kernel to ~/.omnivm/kernel/vmlinux
make all                 # build ovmd, ovm, ovm-harness, ovm-mcp, ovm-mcp-guest into ./bin
```

If `make kernel` has no prebuilt kernel for your architecture, `make kernel-build`
compiles one from source (needs `build-essential flex bison libelf-dev libssl-dev bc`).

## 3. Start the daemon

```bash
./bin/ovm up
./bin/ovm status
./bin/ovm doctor    # platform and dependency check
```

`ovm up` starts `ovmd` in the background. You will be asked for your sudo password.

## 4. Run a command

```bash
ovm run -- echo "hello from omnivm"
```

What happens:

1. The CLI sends a request to `ovmd` over `~/.omnivm/ovmd.sock`.
2. `ovmd` pulls the default OCI image (`python:3.12-alpine`) if it is not cached, builds a root filesystem from it, and boots a Cloud Hypervisor microVM.
3. Inside the VM, `ovm-harness` (PID 1) receives the `run` request over vsock and starts the command.
4. Output streams back to the CLI. When the command exits, the instance is deleted.

Use another image with `--image`:

```bash
ovm run --image alpine:3.21 -- echo hello
```

## 5. Mount a host directory

```bash
ovm run --workspace ./myapp -- python3 /workspace/app.py
```

`./myapp` appears read-write at `/workspace/` inside the VM (virtiofs).

## 6. Long-lived instance with an exposed port

```bash
ovm instance start --name web -- python3 -m http.server 80
ovm instance expose web 8080:80
curl http://127.0.0.1:8080/
```

The router in `ovmd` owns exposed ports and proxies traffic with wake-on-connect.
An idle instance is paused after 60 seconds and stopped (memory snapshot to disk)
after 5 minutes. The next connection wakes it.

The router currently listens on `127.0.0.1` only.

## 7. Exec, logs, lifecycle

```bash
ovm exec web -- ls /
ovm logs web --follow
ovm instance list
ovm instance info web
ovm instance disable web    # stop and stop managing; no wake-on-connect
ovm instance start --name web   # bring it back
ovm instance delete web
```

## 8. Secrets

```bash
ovm secret set API_KEY sk-test123
ovm instance start --name app --secret API_KEY -- python3 /workspace/app.py
```

Secrets are encrypted at rest with `~/.omnivm/master.key` and injected as
environment variables only into instances that request them.

## 9. MCP

```bash
ovm mcp install
```

Registers `ovm-mcp` with Claude Code so an MCP client can start VMs, run
commands, read logs, and manage secrets.

## 10. Where data lives

| Path | Purpose |
|---|---|
| `~/.omnivm/ovmd.sock` | CLI-to-daemon socket |
| `~/.omnivm/data/ovmd.pid` | Daemon PID file |
| `~/.omnivm/data/ovm.db` | SQLite registry (instances, secrets, tether frames) |
| `~/.omnivm/data/images/` | Cached OCI images |
| `~/.omnivm/data/overlays/` | Per-instance root filesystems |
| `~/.omnivm/data/workspaces/` | Workspace directories |
| `~/.omnivm/data/logs/` | Per-instance NDJSON logs |
| `~/.omnivm/data/snapshots/` | VM memory snapshots |
| `~/.omnivm/kernel/vmlinux` | MicroVM kernel |
| `~/.omnivm/master.key` | Secret encryption key |

## 11. Clean up

```bash
ovm down              # stop ovmd and all VMs
rm -rf ~/.omnivm      # remove all state
```

## Next

- [CLI.md](CLI.md) — CLI reference
- [ROUTER.md](ROUTER.md) — routing and wake-on-connect
- [GUEST_API.md](GUEST_API.md) — API available inside the VM
- [TROUBLESHOOTING.md](TROUBLESHOOTING.md)
