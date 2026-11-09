# OmniVM CLI Reference

The `ovm` command is the primary interface to OmniVM.
All commands communicate with the `ovmd` daemon over a Unix socket at
`~/.omnivm/ovmd.sock`. The daemon PID file is stored at `~/.omnivm/data/ovmd.pid`.

```
ovm <command> [options]
```

Run `ovm help` to print top-level usage.

---

## OmniVM Is Not Docker

OmniVM uses OCI images for the **filesystem only**. If you are coming from Docker,
these differences matter:

| Docker concept | OmniVM behavior |
|----------------|----------------|
| `ENTRYPOINT` | Ignored. PID 1 is always `ovm-harness`. |
| `CMD` | Ignored. The command comes from `ovm run -- CMD` or `ovm instance start -- CMD`. |
| `ENV` | Ignored. Environment is set by secrets + RPC params. |
| `EXPOSE` | Ignored. Ports are declared via `--expose` flag (Docker-style port mapping). |
| `VOLUME` | Ignored. Writable paths are fixed (`/workspace`, `/tmp`, `/run`, `/var`). |

One process per VM. No `docker compose`. No restart supervisor -- if your
process exits, the instance stops. See [GUEST_API.md](GUEST_API.md) for the
API available to programs inside the VM.

---

## Platform Commands

### ovm up

Start the ovmd daemon.

```
ovm up
```

Locates the `ovmd` binary next to the `ovm` binary, starts it as a
subprocess, and waits up to 2 seconds for the PID file to appear. If the
daemon is already running, prints a message and exits without error.

**Example:**

```
$ ovm up
ovm v0.4.0
ovmd: started
```

---

### ovm down

Stop the ovmd daemon.

```
ovm down
```

Stops ovmd, which in turn stops all VMs. Sends
SIGTERM and waits up to 5 seconds for the process to exit. If the daemon is
not running, prints a message and exits without error.

**Example:**

```
$ ovm down
ovmd stopping (pid 48201)
ovmd stopped
```

---

### ovm status

Show daemon status.

```
ovm status
```

Checks whether the daemon is running, then queries `GET /v1/status` to display
the backend name.

**Example:**

```
$ ovm status
ovmd: running
backend: cloud-hypervisor
```

---

### ovm doctor

Diagnose the local environment.

```
ovm doctor
```

Checks for the presence of required tools and libraries:

- `/dev/kvm`
- `cloud-hypervisor` binary
- `virtiofsd` binary
- microVM kernel
- e2fsprogs / `mkfs.ext4`
- Daemon status and capabilities

---

## ovm run

Ephemeral command: creates an instance, follows logs, waits for exit, deletes
the instance. If `--workspace` is omitted, a temporary workspace is allocated
and deleted after. If `--workspace` is provided, that workspace is preserved.

```
ovm run [--expose [PUBLIC:]GUEST[/PROTO]] [--name NAME] [--image IMAGE] [--secret KEY] [--workspace NAME_OR_PATH] -- COMMAND [ARGS...]
```

**Flags:**

| Flag | Description |
|---|---|
| `--image IMAGE` | OCI image reference (e.g., `alpine:3.21`). Default: `python:3.12-alpine`. |
| `--expose [PUBLIC:]GUEST[/PROTO]` | Port to expose. `8080:80` maps public 8080 to guest 80. `80` assigns a random public port. Optional `/tcp` or `/http` protocol hint. May be specified multiple times. |
| `--name NAME` | Handle alias for the instance. |
| `--secret KEY` | Secret to inject as env var. Use `--secret '*'` for all secrets. May be specified multiple times. Default: none. |
| `--workspace NAME_OR_PATH` | Named workspace (e.g., `claw`) or host path (e.g., `./myapp`). Named workspaces resolve to `~/.omnivm/data/workspaces/<name>`. |

Creates an instance via `POST /v1/instances`, streams logs, and watches for
process exit. On Ctrl+C or process exit, sends `DELETE /v1/instances/{id}` to
clean up.

Secrets are **not injected by default**. You must explicitly name which secrets
an instance receives via `--secret KEY`. This prevents accidental leakage.

**Examples:**

```
# Run a one-shot command
$ ovm run -- echo "hello from omnivm"
hello from omnivm

# Run with deterministic port (public 8080 → guest 80)
$ ovm run --expose 8080:80 -- python3 -m http.server 80

# Run with random public port
$ ovm run --expose 80 -- python3 -m http.server 80

# Run with secrets
$ ovm run --secret API_KEY --expose 8080:80 -- python app.py
```

---

## Instance Commands

Manage instances -- the core runtime object in OmniVM.

Run `ovm instance help` to print subcommand usage.

### ovm instance start

Start a new instance, or restart a stopped instance by handle.

```
ovm instance start [--name NAME] [--expose [PUBLIC:]GUEST[/PROTO]] [--image IMAGE] [--secret KEY] [--workspace NAME_OR_PATH] -- COMMAND [ARGS...]
ovm instance start --name NAME                          (restart stopped instance)
```

Idempotent on `--name`: if the named instance exists and is STOPPED (whether
enabled or disabled), it is re-enabled and restarted using stored config. If it
is RUNNING or STARTING, returns 409. If not found, creates a new instance.

**Flags:**

| Flag | Description |
|---|---|
| `--name NAME` | Handle alias (used for routing, exec, logs, restart). |
| `--expose [PUBLIC:]GUEST[/PROTO]` | Port to expose. `8080:80` maps public 8080 to guest 80. `80` assigns random. May be specified multiple times. |
| `--image IMAGE` | OCI image reference. |
| `--secret KEY` | Secret to inject as env var. Use `--secret '*'` for all secrets. May be specified multiple times. Default: none. |
| `--workspace NAME_OR_PATH` | Named workspace or host path. Named workspaces resolve to `~/.omnivm/data/workspaces/<name>`. |

**Examples:**

```
# Start a named instance
$ ovm instance start --name web --expose 8080:80 --workspace myapp -- python3 -m http.server 80
Instance started: inst-173f...
Handle: web

# Restart a stopped instance
$ ovm instance disable web
$ ovm instance start --name web
Instance restarted: inst-173f...
```

---

### ovm instance list

List instances, optionally filtered by state.

```
ovm instance list [--stopped | --running]
```

Columns: ID, HANDLE, STATUS (enabled/disabled), STATE, STOPPED AT. Output is
color-coded: enabled/running in green, disabled in red, paused in yellow,
stopped in gray.

**Examples:**

```
$ ovm instance list
ID                             HANDLE          STATUS       STATE      STOPPED AT
inst-1739893456789012345       web             enabled      running    -
inst-1739893456789054321       worker          disabled     stopped    2026-02-19T10:30:00Z

$ ovm instance list --stopped
ID                             HANDLE          STATUS       STATE      STOPPED AT
inst-1739893456789054321       worker          disabled     stopped    2026-02-19T10:30:00Z
```

---

### ovm instance info

Show detailed information about an instance.

```
ovm instance info HANDLE_OR_ID
```

Accepts either a handle alias or instance ID.

**Examples:**

```
$ ovm instance info web
Handle:      web
ID:          inst-1739893456789054321
State:       running
Enabled:     true
Command:     python3 -m http.server 80
Endpoints:
  http://127.0.0.1:49152 → vm:80
Connections: 0
Created:     2026-02-19T10:30:00Z
Last Active: 2026-02-19T10:35:00Z
```

---

### ovm instance disable

Disable an instance. Sets Enabled=false, stops the VM, closes all port
listeners, and prevents wake-on-connect. The instance remains in the list with
state STOPPED and logs are preserved. Use `ovm instance start --name` to
re-enable, or `ovm instance delete` to remove entirely.

A disabled instance is completely unreachable — no ingress, no auto-wake, no
exec, no log streaming.

```
ovm instance disable HANDLE_OR_ID
```

---

### ovm instance delete

Delete an instance entirely. Stops the VM (if running), removes from the
instance list, and cleans up logs.

```
ovm instance delete HANDLE_OR_ID
```

---

### Instance Lifecycle States

| Operation | Result | Enabled? | In list? | Logs kept? | VM running? |
|-----------|--------|----------|----------|------------|-------------|
| Process exits naturally | STOPPED | Yes | Yes | Yes | No |
| Idle timeout (StopAfterIdle) | STOPPED | Yes | Yes | Yes | No |
| `ovm instance disable` | STOPPED | **No** | Yes | Yes | No |
| `ovm instance pause` | PAUSED | Yes | Yes | Yes | Suspended |
| `ovm instance delete` | Removed | - | No | No | No |
| `ovm run` + Ctrl+C | Deleted | - | No | No | No |

**Enabled** instances retain their config and auto-wake on incoming connections
(wake-on-connect). **Disabled** instances are unreachable — no ingress, no
auto-wake, no exec, no logs. Use `ovm instance start --name <handle>` to
re-enable a disabled instance. Use `ovm instance prune` to clean up old
stopped instances. `delete` is permanent — the instance and its logs are gone.

---

### ovm instance pause

Pause a running instance (SIGSTOP).

```
ovm instance pause HANDLE_OR_ID
```

---

### ovm instance resume

Resume a paused instance (SIGCONT).

```
ovm instance resume HANDLE_OR_ID
```

---

### ovm instance prune

Remove stopped instances older than a threshold. User-invoked cleanup —
no background GC. Workspaces are never deleted by prune.

```
ovm instance prune --stopped-older-than DURATION
```

Duration supports `h` (hours) and `d` (days). Default: `7d`.

**Example:**

```
$ ovm instance prune --stopped-older-than 7d
Pruned 3 stopped instance(s)
```

---

## Exec Command

Execute a command inside a running instance. No SSH required -- uses the
existing control channel.

### ovm exec

```
ovm exec HANDLE_OR_ID -- COMMAND [ARGS...]
```

**Arguments:**

| Argument | Description |
|---|---|
| `HANDLE_OR_ID` | Instance handle or ID. |

Everything after `--` is the command to execute inside the VM.

Valid in RUNNING, PAUSED (auto-resume), and STARTING (waits for ready).
Returns 409 if the instance is STOPPED or DISABLED.

**Examples:**

```
# Exec by handle
$ ovm exec web -- echo hello
hello

# Exec by instance ID
$ ovm exec inst-1739893456789012345 -- ls /workspace
data/
output/
```

---

## Log Commands

Stream instance logs. Logs are captured from the moment the VM starts
and persisted to `~/.omnivm/data/logs/`.

### ovm logs

```
ovm logs HANDLE_OR_ID [--follow]
```

**Arguments:**

| Argument | Description |
|---|---|
| `HANDLE_OR_ID` | Instance handle or ID. |

**Flags:**

| Flag | Description |
|---|---|
| `--follow`, `-f` | Stream live logs (blocks until Ctrl+C). |

Log entries are color-coded by source: `[exec]` in cyan, `[system]` in yellow,
primary process output has no prefix.

**Example:**

```
$ ovm logs web --follow
Serving HTTP on 0.0.0.0 port 80 ...
127.0.0.1 - - [19/Feb/2026 10:35:12] "GET / HTTP/1.1" 200 -
```

---

## Secret Commands

Manage secrets. Secrets are a flat key-value store with AES-256 encryption at
rest. Secrets are injected as env vars only when explicitly requested via
`--secret KEY` or `--secret '*'` (all secrets). Default: none injected. Values are never
displayed by any list command.

Run `ovm secret help` to print subcommand usage.

### ovm secret set

Set a secret (creates or updates).

```
ovm secret set KEY VALUE
```

Sends `PUT /v1/secrets/{key}`.

**Example:**

```
$ ovm secret set API_KEY sk-test123
Secret API_KEY set
```

---

### ovm secret list

List secret names. Values are never shown.

```
ovm secret list
```

**Example:**

```
$ ovm secret list
Secrets:
  API_KEY
  DB_PASSWORD
```

---

### ovm secret delete

Delete a secret.

```
ovm secret delete KEY
```

Sends `DELETE /v1/secrets/{key}`.

**Example:**

```
$ ovm secret delete API_KEY
Secret API_KEY deleted
```

---

## MCP Commands

Manage the MCP (Model Context Protocol) server integration with Claude Code.

### ovm mcp install

Register `ovm-mcp` as an MCP server in Claude Code.

```
ovm mcp install [--project]
```

By default, registers with `user` scope (available across all projects). Pass
`--project` to register with project scope (shared via `.mcp.json`).

**Example:**

```
$ ovm mcp install
ovm MCP server registered in Claude Code
  binary: /usr/local/bin/ovm-mcp
  scope:  user
```

---

### ovm mcp uninstall

Remove the `ovm-mcp` server from Claude Code.

```
ovm mcp uninstall
```

---

## Quick Reference

| Command | Description |
|---|---|
| `ovm up` | Start daemon |
| `ovm down` | Stop daemon (stops all VMs) |
| `ovm status` | Show daemon status |
| `ovm doctor` | Diagnose environment |
| `ovm run [...] -- CMD` | Ephemeral: start + follow + delete |
| `ovm instance start` | Start new or re-enable stopped/disabled instance |
| `ovm instance list` | List instances (`--stopped`, `--running`) |
| `ovm instance info` | Show instance details |
| `ovm instance disable` | Disable an instance (stop VM, close listeners, prevent auto-wake) |
| `ovm instance delete` | Delete an instance (removed entirely) |
| `ovm instance pause` | Pause an instance |
| `ovm instance resume` | Resume an instance |
| `ovm instance prune` | Remove stale stopped instances |
| `ovm exec` | Execute command in instance |
| `ovm logs` | Stream instance logs |
| `ovm secret set` | Set a secret |
| `ovm secret list` | List secret names |
| `ovm secret delete` | Delete a secret |
| `ovm mcp install` | Register MCP server in Claude Code |
| `ovm mcp uninstall` | Remove MCP server from Claude Code |
