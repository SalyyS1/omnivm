#!/usr/bin/env python3
"""Measure OmniVM lifecycle latency and per-VM host memory.

Talks to a running ovmd over its Unix socket (stdlib only) and reports:

  * cold start   – POST /v1/instances until the instance reports "running"
                   (image already cached; the first, uncached start is reported
                   separately)
  * resume       – POST /v1/instances/{id}/resume on a paused instance until
                   it reports "running" again
  * host RSS     – resident memory of each cloud-hypervisor process, and of
                   virtiofsd sidecars if any, with several idle VMs running

Results are printed as a Markdown table and written as JSON with --json.
Numbers depend on the machine; always publish them together with the host
description printed at the top.

Usage: sudo python3 scripts/bench/lifecycle-latency.py [--runs 20] [--vms 5]
"""

import argparse
import http.client
import json
import os
import platform
import socket
import statistics
import subprocess
import sys
import time

POLL_INTERVAL = 0.005


class UnixHTTPConnection(http.client.HTTPConnection):
    def __init__(self, path):
        super().__init__("ovm")
        self.unix_path = path

    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.connect(self.unix_path)


class Client:
    def __init__(self, sock_path):
        self.sock_path = sock_path

    def call(self, method, path, body=None):
        conn = UnixHTTPConnection(self.sock_path)
        data = json.dumps(body).encode() if body is not None else None
        headers = {"Content-Type": "application/json"} if data else {}
        conn.request(method, path, body=data, headers=headers)
        resp = conn.getresponse()
        raw = resp.read()
        conn.close()
        if resp.status >= 400:
            raise RuntimeError(f"{method} {path} -> {resp.status}: {raw.decode(errors='replace')}")
        return json.loads(raw) if raw else {}

    def state(self, inst_id):
        return self.call("GET", f"/v1/instances/{inst_id}").get("state")

    def wait_state(self, inst_id, want, timeout=120):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if self.state(inst_id) == want:
                return
            time.sleep(POLL_INTERVAL)
        raise TimeoutError(f"instance {inst_id} did not reach {want!r} within {timeout}s")

    def create(self, handle, image):
        body = {"handle": handle, "image_ref": image, "command": ["sleep", "3600"]}
        return self.call("POST", "/v1/instances", body)["id"]

    def exec_output(self, inst_id, command):
        """Run a command in the guest and return its stdout lines."""
        conn = UnixHTTPConnection(self.sock_path)
        conn.request("POST", f"/v1/instances/{inst_id}/exec", body=json.dumps({"command": command}).encode(),
                     headers={"Content-Type": "application/json"})
        resp = conn.getresponse()
        if resp.status >= 400:
            raise RuntimeError(f"exec {command} -> {resp.status}: {resp.read().decode(errors='replace')}")
        lines = []
        for raw in resp:
            entry = json.loads(raw)
            if entry.get("done"):
                break
            if entry.get("stream") == "stdout" and "line" in entry:
                lines.append(entry["line"])
        conn.close()
        return lines

    def delete(self, inst_id):
        try:
            self.call("DELETE", f"/v1/instances/{inst_id}")
        except Exception as exc:  # cleanup is best effort
            print(f"warning: delete {inst_id}: {exc}", file=sys.stderr)


def timed_start(client, handle, image):
    t0 = time.monotonic()
    inst_id = client.create(handle, image)
    client.wait_state(inst_id, "running")
    return inst_id, (time.monotonic() - t0) * 1000


def summarize(samples):
    if not samples:
        return {}
    ordered = sorted(samples)
    p95 = ordered[min(len(ordered) - 1, int(round(0.95 * (len(ordered) - 1))))]
    return {
        "n": len(samples),
        "p50_ms": round(statistics.median(samples), 1),
        "p95_ms": round(p95, 1),
        "min_ms": round(ordered[0], 1),
        "max_ms": round(ordered[-1], 1),
    }


def process_rss_kb(name):
    """RSS (KB) of every process whose executable basename is `name`.

    Reads /proc directly: `ps -C` matches the 15-character kernel comm name,
    which truncates "cloud-hypervisor".
    """
    sizes = []
    for pid in filter(str.isdigit, os.listdir("/proc")):
        try:
            with open(f"/proc/{pid}/cmdline", "rb") as f:
                argv0 = f.read().split(bytes(1), 1)[0].decode(errors="replace")
            if os.path.basename(argv0) != name:
                continue
            with open(f"/proc/{pid}/status") as f:
                for line in f:
                    if line.startswith("VmRSS:"):
                        sizes.append(int(line.split()[1]))
                        break
        except OSError:
            continue  # process exited while scanning
    return sizes


def host_description():
    cpu = "unknown"
    try:
        with open("/proc/cpuinfo") as f:
            for line in f:
                if line.startswith("model name"):
                    cpu = line.split(":", 1)[1].strip()
                    break
        with open("/proc/meminfo") as f:
            mem_kb = int(f.readline().split()[1])
    except OSError:
        mem_kb = 0
    ch = subprocess.run(["cloud-hypervisor", "--version"], capture_output=True, text=True).stdout.strip()
    return {
        "kernel": platform.release(),
        "cpu": cpu,
        "cpus": os.cpu_count(),
        "mem_gb": round(mem_kb / 1024 / 1024, 1),
        "cloud_hypervisor": ch or "not on PATH",
        "runner": os.environ.get("RUNNER_NAME", ""),
    }


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--socket", default=os.path.expanduser("~/.omnivm/ovmd.sock"))
    ap.add_argument("--image", default="python:3.12-alpine")
    ap.add_argument("--runs", type=int, default=20)
    ap.add_argument("--vms", type=int, default=5, help="idle VMs to run for the RSS measurement")
    ap.add_argument("--json", help="write results to this JSON file")
    args = ap.parse_args()

    client = Client(args.socket)
    results = {"host": host_description(), "image": args.image}

    # First start pulls and unpacks the image; report it separately.
    inst_id, first_ms = timed_start(client, "bench-first", args.image)
    client.delete(inst_id)
    results["first_start_ms"] = round(first_ms, 1)

    cold, resume = [], []
    for i in range(args.runs):
        inst_id, ms = timed_start(client, f"bench-cold-{i}", args.image)
        cold.append(ms)
        client.call("POST", f"/v1/instances/{inst_id}/pause")
        client.wait_state(inst_id, "paused")
        t0 = time.monotonic()
        client.call("POST", f"/v1/instances/{inst_id}/resume")
        client.wait_state(inst_id, "running")
        resume.append((time.monotonic() - t0) * 1000)
        client.delete(inst_id)
    results["cold_start"] = summarize(cold)
    results["resume"] = summarize(resume)

    ids = []
    try:
        for i in range(args.vms):
            ids.append(timed_start(client, f"bench-rss-{i}", args.image)[0])
        time.sleep(5)  # let guests settle
        harness = []
        for inst_id in ids:
            out = client.exec_output(inst_id, ["sh", "-c", "grep VmRSS /proc/1/status"])
            for line in out:
                if line.startswith("VmRSS:"):
                    harness.append(int(line.split()[1]))
        ch = process_rss_kb("cloud-hypervisor")
        vfs = process_rss_kb("virtiofsd")
        results["rss"] = {
            "vms": args.vms,
            "cloud_hypervisor_kb_avg": round(statistics.mean(ch)) if ch else None,
            "virtiofsd_kb_avg": round(statistics.mean(vfs)) if vfs else None,
            "guest_harness_kb_avg": round(statistics.mean(harness)) if harness else None,
            "guest_memory_mb": "daemon default",
        }
    finally:
        for inst_id in ids:
            client.delete(inst_id)

    host = results["host"]
    print(f"Host: {host['cpu']} x{host['cpus']}, {host['mem_gb']} GB RAM, kernel {host['kernel']}, {host['cloud_hypervisor']}")
    print(f"Image: {args.image}\n")
    print("| Measurement | n | p50 (ms) | p95 (ms) | min (ms) | max (ms) |")
    print("|---|---|---|---|---|---|")
    print(f"| First start (uncached image) | 1 | {results['first_start_ms']} | | | |")
    for label, key in (("Cold start (cached image)", "cold_start"), ("Resume from pause", "resume")):
        r = results[key]
        print(f"| {label} | {r['n']} | {r['p50_ms']} | {r['p95_ms']} | {r['min_ms']} | {r['max_ms']} |")
    rss = results["rss"]
    print(f"\nIdle host RSS with {rss['vms']} VMs: cloud-hypervisor avg {rss['cloud_hypervisor_kb_avg']} KB/VM, "
          f"virtiofsd avg {rss['virtiofsd_kb_avg']} KB, guest ovm-harness (PID 1) avg {rss['guest_harness_kb_avg']} KB")

    if args.json:
        with open(args.json, "w") as f:
            json.dump(results, f, indent=2)


if __name__ == "__main__":
    main()
