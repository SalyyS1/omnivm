# OmniVM

Scale-to-zero microVMs for running untrusted workloads on Linux hosts.

OmniVM runs each workload in its own [Cloud Hypervisor](https://github.com/cloud-hypervisor/cloud-hypervisor) microVM with its own kernel. Any OCI image can be the root filesystem. Idle VMs are paused and later shut down; an incoming connection, exec or message wakes them again.

> **Status: pre-alpha.** OmniVM is a fork of [AegisVM](https://github.com/xfeldman/aegisvm) that is being reworked for multi-tenant servers. It is **not** safe for hostile multi-tenant use yet: network isolation between VMs, resource limits, and host hardening are on the [roadmap](#roadmap) and not done. No performance numbers are published until they are measured with the benchmark scripts in this repository.

## What it does today

- Boots an OCI image (`python:3.12-alpine` by default) as a microVM on Cloud Hypervisor + KVM.
- Runs one process per VM under a small guest init (`ovm-harness`) that talks to the host over vsock.
- Pauses idle VMs (memory stays resident) and stops them after a longer idle period; an incoming connection or exec resumes a paused VM or cold-boots a stopped one. Memory snapshots to disk are not implemented yet.
- Exposes guest ports through a host router with wake-on-connect.
- Mounts host directories into the guest with virtiofs.
- Stores secrets encrypted at rest and injects them only into instances that ask for them.
- Offers a host↔guest message channel (tether), a guest API for spawning child VMs, and MCP servers for both sides.

## Quick start

Requires Linux with `/dev/kvm`, root, and Go 1.25+.

```bash
sudo apt install virtiofsd e2fsprogs iptables iproute2
git clone https://github.com/SalyyS1/omnivm.git && cd omnivm
make cloud-hypervisor kernel all
./bin/ovm up
./bin/ovm run -- echo "hello from omnivm"
```

See [docs/QUICKSTART.md](docs/QUICKSTART.md) for the full walkthrough.

## Components

| Binary | Role |
|---|---|
| `ovm` | CLI |
| `ovmd` | Host daemon: lifecycle, router, registry, API on a Unix socket |
| `ovm-harness` | Guest PID 1: runs the workload, reports activity, serves the guest API |
| `ovm-mcp` | MCP server on the host |
| `ovm-mcp-guest` | MCP server inside the guest |

## Roadmap

The v1 goal is a single Linux node that can safely host many tenants, with every performance claim backed by a reproducible benchmark.

1. Linux CI with KVM, baseline benchmarks, rootfs sized to the image, configurable DNS.
2. Multi-tenant network isolation: IP allocation with reuse, nftables instead of shell `iptables`, anti-spoofing, no VM-to-VM or host-internal access, egress limits.
3. Copy-on-write instance disks, cgroup v2 limits, disk I/O limits, Prometheus metrics.
4. Unprivileged VMM processes, daemon restarts that keep VMs running, per-tenant API keys.
5. Memory density: balloon, snapshot to disk on idle with safe restore (clock and entropy), memory-pressure-driven hibernation.
6. Public ingress routed by Host/SNI, with protection against wake storms.

Multi-node clustering, live migration and GPU passthrough are out of scope for v1.

## Documentation

- [Quickstart](docs/QUICKSTART.md)
- [CLI reference](docs/CLI.md)
- [Router and wake-on-connect](docs/ROUTER.md)
- [Guest API](docs/GUEST_API.md)
- [Tether](docs/TETHER.md)
- [Secrets](docs/SECRETS.md)
- [Workspaces](docs/WORKSPACES.md)
- [Troubleshooting](docs/TROUBLESHOOTING.md)

## Upstream and license

OmniVM is a fork of [AegisVM](https://github.com/xfeldman/aegisvm) by Max Feldman and the AegisVM contributors, used under the [Apache License 2.0](LICENSE). See [NOTICE](NOTICE) for attribution and a summary of changes. The rename from AegisVM was done with [`scripts/rebrand.sh`](scripts/rebrand.sh), which can be re-applied to upstream patches.

OmniVM is licensed under the Apache License 2.0.
