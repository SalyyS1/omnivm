# Tether

Tether is a bidirectional, ordered message channel between the host and a program running inside a VM. A host process (for example an MCP client) can hand work to an in-VM program and read its replies without SSH, exec, or log parsing.

OmniVM provides the channel only. The in-VM program that answers tether messages is yours to write; see [How it works inside the VM](#how-it-works-inside-the-vm).

## Key properties

- **Wake-on-message**: sending a message to a stopped or paused instance wakes it automatically
- **Sessioned**: each conversation gets an independent session with its own history
- **Ordered**: frames have monotonic sequence numbers for reliable cursor-based reading
- **Async**: send a message and read responses later — no blocking required

## Quick start

### Send a message

```bash
# Via MCP (Claude Code)
tether_send(instance="my-worker", text="Analyze the data in /workspace/data.csv")
# Returns: {msg_id, session_id, ingress_seq}

# Via API
curl -X POST --unix-socket ~/.omnivm/ovmd.sock \
  http://ovm/v1/instances/my-worker/tether \
  -H 'Content-Type: application/json' \
  -d '{"v":1,"type":"user.message","session":{"channel":"host","id":"default"},"payload":{"text":"Analyze the data"}}'
```

### Read the response

```bash
# Via MCP — long-poll until response arrives (up to 10s)
tether_read(instance="my-worker", after_seq=1, wait_ms=10000)
# Returns: {frames: [...], next_seq, timed_out}

# Via API
curl --unix-socket ~/.omnivm/ovmd.sock \
  "http://ovm/v1/instances/my-worker/tether/poll?channel=host&session_id=default&after_seq=1&wait_ms=10000"
```

### Typical pattern (Claude calling in a loop)

```
1. tether_send(instance="worker1", text="Research X and summarize")
   → {ingress_seq: 5}

2. tether_read(instance="worker1", after_seq=5, wait_ms=10000)
   → {frames: [{type:"status.presence", state:"thinking"}, {type:"assistant.delta", text:"I found..."}], next_seq: 8}

3. tether_read(instance="worker1", after_seq=8, wait_ms=10000)
   → {frames: [{type:"assistant.done", text:"Full summary here..."}], next_seq: 12}
   # Done — got assistant.done
```

## MCP tools

### tether_send

Send a message to the in-VM program. Fire-and-forget — does not wait for a response.

| Parameter | Required | Description |
|-----------|----------|-------------|
| `instance` | Yes | Instance handle or ID |
| `text` | Yes | Message text |
| `session_id` | No | Session identifier for threading. Default: `"default"` |

Returns `{msg_id, session_id, ingress_seq}`. Use `ingress_seq` as `after_seq` in the first `tether_read` to skip past your own message.

### tether_read

Read frames sent by the in-VM program. Supports long-polling.

| Parameter | Required | Description |
|-----------|----------|-------------|
| `instance` | Yes | Instance handle or ID |
| `session_id` | No | Must match `tether_send`. Default: `"default"` |
| `after_seq` | No | Cursor — return frames with seq > this. Start with 0 or `ingress_seq` |
| `limit` | No | Max frames. Default: 50, max: 200 |
| `wait_ms` | No | Long-poll timeout (ms). Block until frames arrive or timeout. Default: 0 (immediate), max: 30000 |
| `types` | No | Filter by frame type. Example: `["assistant.done"]` |

Returns `{frames, next_seq, timed_out}`. Use `next_seq` as `after_seq` in the next call.

## API endpoints

### Tether ingress

```
POST /v1/instances/{id}/tether
```

Send a tether frame. Triggers wake-on-message if instance is paused or stopped. Returns `{msg_id, session_id, ingress_seq}`.

### Tether poll

```
GET /v1/instances/{id}/tether/poll?channel=host&session_id=default&after_seq=0&limit=50&wait_ms=10000
```

Read frames with filtering and long-poll support. Returns both ingress (user.message) and egress (assistant.*) frames. Returns `{frames, next_seq, timed_out}`.

| Parameter | Description |
|-----------|-------------|
| `channel` | Filter by session channel (e.g. `"host"`) |
| `session_id` | Filter by session ID |
| `after_seq` | Cursor — frames with seq > this |
| `limit` | Max frames (default 50, max 200) |
| `wait_ms` | Long-poll timeout in ms (default 0, max 30000) |
| `types` | Comma-separated frame types to include |
| `reply_to_msg_id` | Filter frames replying to a specific message |

### Tether stream (SSE)

```
GET /v1/instances/{id}/tether/stream
```

NDJSON stream of all stored frames (ingress + egress). For request/response use, prefer the poll endpoint.

## Frame types

| Direction | Type | Description |
|-----------|------|-------------|
| Host → Guest | `user.message` | User/host input |
| Host → Guest | `control.cancel` | Cancel in-flight request |
| Guest → Host | `status.presence` | Thinking/typing indicator |
| Guest → Host | `assistant.delta` | Streaming text chunk |
| Guest → Host | `assistant.done` | Complete response |
| Guest → Host | `assistant.message` | Guest-initiated message (no prior user.message required) |
| Guest → Host | `event.ack` | Message acknowledgment |

## Sessions

Each tether conversation is identified by `channel` + `session_id`:

- `channel` identifies the source, e.g. `"host"` for host-side callers
- `session_id` is caller-chosen: `"default"`, `"debug-1"`, `"task-analyze"`, etc.

Sessions are independent: the same session ID on two channels names two different sessions.

## Sequence numbers

Every frame gets a monotonic `seq` number, per instance. Sequences are:

- **Global**: all channels share one counter per instance
- **Stable**: safe to use as cursors for pagination and resume
- **Persisted**: stored in a per-instance ring buffer (1000 frames) with SQLite write-through — history survives daemon restarts

Use `after_seq` + `next_seq` for reliable cursor-based reading. No missed frames, no duplicates.

## How it works inside the VM

`ovm-harness` (PID 1 in the guest) handles the guest side of tether:

1. For each `user.message` (or other host-to-guest frame), the harness immediately sends an `event.ack` frame back to the host.
2. It appends the frame to `/workspace/tether/inbox.ndjson`.
3. It forwards the frame with `POST http://127.0.0.1:7778/v1/tether/recv` to a program listening inside the VM. Frames that arrive before that program is up are buffered and delivered once it responds.

Your in-VM program sends frames back to the host with `POST http://127.0.0.1:7777/v1/tether/send` (the [Guest API](GUEST_API.md)). Use the frame types and session fields described above.

If nothing listens on port 7778, frames are still acknowledged and persisted to the inbox, so a program can also read the inbox file instead.

## Use cases

- **Delegation**: a host process sends a task to a sandboxed program, does other work, then reads the results.
- **Debugging**: query a running program's state over a structured channel instead of parsing logs.
- **Orchestration**: a host process manages several instances, sending tasks and collecting results.
