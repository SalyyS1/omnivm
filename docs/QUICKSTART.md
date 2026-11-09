# Quickstart

Get from zero to a running agent inside an OmniVM microVM in under 5 minutes.

---

## 1. Prerequisites

| | macOS | Linux |
|---|---|---|
| **Hardware** | Apple Silicon (M1+) | x86_64 or arm64 with KVM |
| **OS** | macOS 13+ | Ubuntu 22.04+ (or any distro with KVM) |
| **Root** | No | Yes (`sudo`) |

Verify your setup:

```bash
# macOS
uname -m        # must be arm64
brew --version

# Linux
uname -m        # x86_64 or aarch64
ls /dev/kvm     # must exist
```

## 2. Install

### macOS

#### Homebrew (recommended)

```bash
brew tap SalyyS1/omnivm
brew install omnivm
```

This installs all binaries and handles hypervisor entitlement signing automatically. On first run, `ovm up` will download the default rootfs (Alpine + Python 3.12).

#### From source

Install system dependencies:

```bash
brew tap slp/krun
brew install libkrun e2fsprogs
```

Clone and build:

```bash
git clone https://github.com/SalyyS1/omnivm.git
cd omnivm
make all
```

`make all` builds into `./bin/`: `ovmd`, `ovm`, `ovm-harness`, `ovm-mcp`, `ovm-vmm-worker`.

### Linux

#### One-liner (recommended)

```bash
curl -sSL https://raw.githubusercontent.com/SalyyS1/omnivm/main/install.sh | sh
```

Installs both `omnivm` and `omnivm-agent-kit` packages, plus dependencies (`virtiofsd`, `e2fsprogs`, `iptables`, `iproute2`). Binaries go to `/usr/bin/` and `/usr/lib/omnivm/`, kernel to `/usr/share/omnivm/kernel/`.

#### From source

Install system dependencies:

```bash
sudo apt install virtiofsd e2fsprogs
```

Clone and build:

```bash
git clone https://github.com/SalyyS1/omnivm.git
cd omnivm
make cloud-hypervisor    # download Cloud Hypervisor static binary
make kernel              # download prebuilt microVM kernel (~30MB)
make all                 # build ovmd, ovm, ovm-harness, ovm-mcp
```

`make all` builds into `./bin/`: `ovmd`, `ovm`, `ovm-harness`, `ovm-mcp` (no vmm-worker on Linux).

> **Unsupported architecture?** If `make kernel` fails (e.g. RISC-V), use `make kernel-build` to compile from source. Requires `build-essential flex bison libelf-dev libssl-dev bc`.

## 3. Base Rootfs

OmniVM needs a base root filesystem to boot VMs. Three pre-built variants are available:

| Variant | Contents | Size |
|---------|----------|------|
| `alpine` | Minimal Alpine Linux (sh, coreutils) | ~8MB |
| `python` | Alpine + Python 3.12 | ~55MB |
| `full` | Alpine + Python 3.12 + Node.js + npm + git + curl + jq | ~90MB |

**Default:** `python` is auto-downloaded on first `ovm up` if no rootfs is installed.

Manage rootfs variants:

```bash
# See what's available and installed
ovm rootfs list

# Download a specific variant (replaces current base-rootfs)
ovm rootfs pull python
ovm rootfs pull alpine
ovm rootfs pull full
```

The active rootfs lives at `~/.omnivm/base-rootfs` (macOS, directory) or `~/.omnivm/base-rootfs.ext4` (Linux, ext4 image). Pulling a variant replaces it. This is the rootfs used when you run instances without `--image`.

**Building rootfs from source (optional):**

```bash
make base-rootfs    # requires Docker
# macOS: creates base/rootfs/ directory
# Linux: creates base/base-rootfs.ext4 image
```

Copy the result to `~/.omnivm/`:

```bash
# macOS
cp -r base/rootfs ~/.omnivm/base-rootfs

# Linux
cp base/base-rootfs.ext4 ~/.omnivm/base-rootfs.ext4
```

## 4. Start OmniVM

```bash
ovm up
```

On Linux, you'll be prompted for your password (sudo is needed for tap networking). Subsequent runs use cached credentials.

This launches `ovmd` in the background. If no rootfs is installed, it downloads the default (`python`) first. Verify it is running:

```bash
ovm status
ovm doctor    # detailed platform + dependency check
```

## 5. Run Your First Command

```bash
ovm run -- echo "hello from omnivm"
```

Expected output:

```
hello from omnivm
```

What happened behind the scenes:

1. The CLI sent a request to `ovmd` over `~/.omnivm/ovmd.sock`.
2. `ovmd` booted a microVM (libkrun on macOS, Cloud Hypervisor on Linux).
3. Inside the VM, `ovm-harness` (PID 1) received the `run` RPC over vsock.
4. The harness executed `echo "hello from omnivm"` and streamed stdout back.
5. When the process exited, the harness sent a `processExited` notification.
6. The CLI received the exit, deleted the instance, and exited with code 0.

## 6. Run with a Custom Image

```bash
ovm run --image alpine:3.21 -- echo hello
```

When you specify `--image`, ovmd pulls the OCI image (if not already cached), extracts it into a rootfs, and boots the VM from that rootfs. Subsequent runs with the same image skip the pull and use the cached copy from `~/.omnivm/data/images/`.

## 7. Start an HTTP Server

```bash
ovm run --expose 8080:80 -- python3 -m http.server 80
```

In a second terminal:

```bash
curl http://127.0.0.1:8080/
```

You should see the directory listing from Python's HTTP server.

How this works:

- `--expose 8080:80` maps public port 8080 to guest port 80. You can also use `--expose 80` to let the OS assign a random public port.
- All exposed ports are owned by the router in ovmd -- traffic is proxied with wake-on-connect.
- When idle for 60 seconds, the VM is paused. After 5 minutes idle, the VM is stopped entirely.
- The next incoming connection wakes the VM automatically (scale-to-zero).

Press Ctrl+C in the first terminal to stop.

## 8. Run a Script from Your Host

Use `--workspace` to mount a host directory into the VM at `/workspace/`:

```bash
ovm run --workspace ./myapp --expose 8080:80 -- python3 /workspace/server.py
```

This mounts `./myapp` from your host as `/workspace/` inside the VM via virtiofs. Files are available read-write and persist after the VM stops.

## 9. Start a Named Instance

Start a long-lived instance with a handle:

```bash
# Start the instance (public port 8080 -> guest port 80)
ovm instance start --name demo --expose 8080:80 -- python3 -m http.server 80

# Set a workspace secret (available as env var inside VMs)
ovm secret set API_KEY sk-test123

# Curl the server
curl http://127.0.0.1:8080/
```

## 10. Exec Into a Running Instance

While the instance is running, exec commands inside the VM:

```bash
ovm exec demo -- echo "hello from inside"
```

Expected output:

```
hello from inside
```

This uses the existing control channel -- no SSH, no serial console. The instance
is auto-resumed if paused.

## 11. Stream Logs

View logs from a running instance:

```bash
ovm logs demo --follow
```

Logs are captured from the moment the VM starts and persisted to
`~/.omnivm/data/logs/`. Without `--follow`, all buffered logs are printed and the
command exits. With `--follow`, new log entries stream live until Ctrl+C.

## 12. Inspect Instances

List all instances:

```bash
ovm instance list
```

Get detailed info about a specific instance:

```bash
ovm instance info demo
```

Disable the instance -- the instance becomes unmanaged. The VM is stopped, port
listeners are closed, and ovmd will not auto-start it for any reason (no
wake-on-connect, no implicit boot). The instance stays in the list as a registry
record with logs preserved:

```bash
ovm instance disable demo
```

The only way to bring it back is an explicit `ovm instance start --name demo`.

Delete the instance entirely (removes from list, cleans logs):

```bash
ovm instance delete demo
```

## 13. MCP (Claude Code Integration)

Register OmniVM as an MCP server so Claude can drive sandboxed instances:

```bash
ovm mcp install
```

Once registered, Claude can start VMs, exec commands, read logs, and manage secrets directly.

## 14. Where Is My Data?

All OmniVM state lives under `~/.omnivm/`:

| Path | Purpose |
|---|---|
| `~/.omnivm/ovmd.sock` | Unix domain socket for CLI-to-daemon communication |
| `~/.omnivm/data/ovmd.pid` | PID file for the running daemon |
| `~/.omnivm/data/ovm.db` | SQLite database (instances, secrets) |
| `~/.omnivm/data/images/` | Cached OCI image layers |
| `~/.omnivm/data/overlays/` | Instance rootfs overlays |
| `~/.omnivm/data/workspaces/` | Workspace directories |
| `~/.omnivm/data/logs/` | Per-instance NDJSON log files |
| `~/.omnivm/data/snapshots/` | VM memory snapshots (Linux only) |
| `~/.omnivm/kernel/vmlinux` | MicroVM kernel (Linux only) |
| `~/.omnivm/master.key` | Encryption key for secrets at rest |
| `~/.omnivm/base-rootfs/` | Active base rootfs directory (macOS) |
| `~/.omnivm/base-rootfs.ext4` | Active base rootfs ext4 image (Linux) |

## 15. Platform Differences

| Behavior | macOS | Linux |
|---|---|---|
| Hypervisor | libkrun (Apple HVF) | Cloud Hypervisor (KVM) |
| Root required | No | Yes |
| Networking | gvproxy (in-process) | tap + NAT |
| Rootfs format | Directory | ext4 block image |
| Pause mechanism | SIGSTOP/SIGCONT | vm.pause/vm.resume API |
| Idle stop | Persistent pause (OS swap) | Snapshot to disk, then stop |
| Cold restart | SIGCONT (instant) | Restore from snapshot |
| Workspace | virtiofs (built-in) | virtiofsd sidecar |

From the user's perspective, the CLI commands are identical. The backend differences are handled transparently by ovmd.

## 16. Clean Up

```bash
ovm down
```

This shuts down `ovmd` and all running VMs.

To remove everything and start fresh:

```bash
ovm down
rm -rf ~/.omnivm
```

## 17. Next Steps

- [AGENT_CONVENTIONS.md](AGENT_CONVENTIONS.md) -- conventions for building agents that run on OmniVM
- [CLI.md](CLI.md) -- full CLI reference
- [cloud-hypervisor-backend.md](../specs/cloud-hypervisor-backend.md) -- Linux backend architecture (for contributors)
- `examples/` -- sample agents
