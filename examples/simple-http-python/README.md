# simple-http-python

Demonstrates a long-running HTTP server OmniVM agent written in Python using only the standard library.

## What this example shows

- **Long-lived instance**: the server runs indefinitely, handling requests via the OmniVM router.
- **Secrets injection**: reads `API_KEY` from environment variables.
- **Workspace listing**: displays files present in `/workspace` on the response page.
- **Structured logging**: writes JSON log lines to stdout for each request.
- **Router access**: the OmniVM router forwards external HTTP traffic to port 80 inside the VM.

## Running

```bash
ovm up
ovm secret set API_KEY sk-test123
ovm run --name http-demo --expose 80 --image python:3.12-alpine -- python server.py
```

In another terminal:

```bash
curl http://127.0.0.1:8099/
```

## Conventions used

This example follows the patterns described in `docs/AGENT_CONVENTIONS.md`:

- Structured JSON logging to stdout
- Listens on port 80 (the exposed port)
- Secrets read from environment variables
- Workspace at `/workspace`
