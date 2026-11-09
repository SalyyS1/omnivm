# Troubleshooting

Searchable FAQ for common OmniVM issues. Each entry states the problem, explains why it happens, and provides a fix.

---

## "Why can't I write to /etc (or /usr, /bin, /home)?"

**Why:** The root filesystem is remounted read-only by the harness at boot. This protects the rootfs from modification so that every boot starts from a known-good state.

**Fix:** Write to `/workspace` for persistent data, or `/tmp` for ephemeral scratch. Install everything you need at image build time in your Dockerfile.

---

## "Why can't I curl localhost:8000 from the host?"

**Why:** The VM has its own network namespace. `localhost` inside the VM is not the host.

**Fix:** All ingress goes through the OmniVM router at `127.0.0.1:8099`. Use `curl http://127.0.0.1:8099/` to reach your instance, or `curl http://127.0.0.1:8099/myhandle/` for multi-instance routing.

---

## "Where are my logs?"

**Why:** Log routing depends on how the workload was started.

**Fix:**
- `ovm run`: logs stream to your terminal automatically.
- `ovm logs <handle>`: stream logs for a running instance.
- `ovm logs <handle> --follow`: stream live logs.
- Instance logs are persisted at `~/.omnivm/data/logs/<instance-id>.ndjson`.
- Daemon logs: check the terminal where `ovmd` is running, or the output of `ovm up`.

---

## "Why didn't my secrets appear in the environment?"

**Why:** Secrets are injected at instance boot time. If you set a secret after starting an instance, the running instance doesn't get it.

**Fix:**
- Disable and restart the instance to pick up new secrets: `ovm instance disable <name>` then `ovm instance start --name <name>`.
- Check secrets exist: `ovm secret list`

---

## "How do I reset all OmniVM state?"

**Fix:**
1. Stop the daemon: `ovm down`
2. Remove all data: `rm -rf ~/.omnivm/`
3. This deletes: database, images, overlays, workspaces, snapshots, logs, master key.
4. You will need to re-set all secrets. The kernel at `~/.omnivm/kernel/vmlinux` is also removed; run `make kernel` again.

---

## "My VM boots but the command fails immediately"

**Why:** Image architecture mismatch or missing binary.

**Fix:**
- Check the image is linux/arm64.
- Check the binary or script exists in the image.

---

## "ovm run hangs / never returns"

**Fix:**
- If the daemon is not running, start it first: `ovm up`.
- Check daemon logs for errors.
- The process may be long-running. Use Ctrl+C to stop and clean up.

---

## "I deleted master.key and now secrets don't work"

**Why:** The master key encrypts all secrets. Without it, stored secrets are undecryptable.

**Fix:**
- Re-set all secrets via `ovm secret set`.
- A new master key is auto-generated on next daemon start.

---

## "Multiple instances but requests go to wrong one"

**Why:** Without a path prefix or header, the router uses a default fallback only when exactly one instance exists. With multiple instances, it returns 503.

**Fix:** Use explicit routing:
- Path routing: `curl http://127.0.0.1:8099/myhandle/`
- Header routing: `curl -H "X-OmniVM-Instance: inst-id" http://127.0.0.1:8099/`

---

## "Workspace directory is empty on host"

**Fix:**
- Workspaces are at `~/.omnivm/data/workspaces/`.
- A workspace is only created when `--workspace` is specified at instance creation.
