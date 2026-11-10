# Benchmarks

Baseline numbers measured before any performance work. They come from
[`scripts/bench/lifecycle-latency.py`](../scripts/bench/lifecycle-latency.py),
which the `benchmark` job of the `Integration (KVM)` workflow runs on every
push to `main` and publishes in the job summary and as the
`benchmark-results` artifact.

**Read these as regression baselines, not product claims.** GitHub-hosted
runners are shared Azure VMs running OmniVM through nested virtualization, and
the CPU model changes from run to run. Bare-metal numbers will differ.

## What is measured

| Measurement | Definition |
|---|---|
| First start | `POST /v1/instances` until the instance reports `running`, image not yet cached (pull + unpack + root disk build + boot) |
| Cold start | Same, with the image already cached; includes building the instance root disk |
| Resume | `POST /v1/instances/{id}/resume` on a paused instance until it reports `running` |
| Host RSS | Resident memory of each `cloud-hypervisor` and `virtiofsd` process, with 5 idle VMs |
| Guest harness RSS | Resident memory of `ovm-harness` (PID 1 inside the guest) |

Each latency is measured 20 times. Image: `python:3.12-alpine`. VMs use the
daemon defaults: 1 vCPU, 512 MB memory.

Not measured: restore from a memory snapshot. The Cloud Hypervisor backend
does not snapshot VMs yet; an idle VM is shut down and the next request cold
boots it.

## Results (2026-11-10)

Host for both runs: GitHub-hosted `ubuntu-latest` (image `ubuntu-26.04`),
4 vCPUs, 15.6 GB RAM, kernel `7.0.0-1019-azure`, Cloud Hypervisor v54.0,
guest kernel `ch-release-v6.16.9-20251112`, data on the runner's ext4 disk.

| Measurement | Intel Xeon 6973P-C | AMD EPYC 9V45 |
|---|---|---|
| First start | 2252 ms | 2322 ms |
| Cold start p50 / p95 | 1154 / 1437 ms | 1207 / 1516 ms |
| Resume p50 / p95 | 2.7 / 5.0 ms | 2.0 / 2.2 ms |
| `cloud-hypervisor` RSS per VM | 76.9 MB | 77.5 MB |
| `virtiofsd` RSS per VM | 4.3 MB | 4.3 MB |
| Guest `ovm-harness` RSS | not measured in this run | 6.1 MB |

Cold start includes copying the cached image into a per-instance directory and
building an ext4 root disk from it with `mkfs.ext4 -d`, then booting the VM.
The script does not yet split this time into stages.

## Reproducing

On a Linux host with KVM, after `make all cloud-hypervisor` and
`sudo make kernel HOME=/root`:

```bash
sudo HOME=/root ./bin/ovmd &
sudo python3 scripts/bench/lifecycle-latency.py --socket /root/.omnivm/ovmd.sock --runs 20 --vms 5 --json results.json
```

Publish results together with the host line the script prints.
