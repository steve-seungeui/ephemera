# v0.2.0 — Single-Host Feature Complete

**Ephemera** completes the single-host feature set. Every limitation noted in v0.1.0 is resolved: guests shut down gracefully, agent authentication is enforced, tokens reload without a restart, and VMs can be snapshotted and restored in seconds. A new control plane proxy makes goose-agent accessible from external clients without direct access to the private VM subnet.

---

## What's New

### Graceful Guest Shutdown

- `micro-init` (PID 1) now traps `SIGTERM` and calls `sync` + `poweroff(2)` — the guest powers off cleanly with no kernel panic
- `reboot=k` kernel argument lets Firecracker exit cleanly (exit code 0) on guest power-off

### Per-VM Agent Authentication

- Control plane generates a 32-byte random Bearer token per VM at spawn time
- Token written to `/root/.ephemera-agent-token` (mode `0600`) inside the VM disk
- `POST /tasks` and `POST /stop` require `Authorization: Bearer <agent_token>`
- `GET /health` remains unauthenticated (used by the control plane health poller)
- Token returned once in `POST /vms` and `POST /snapshots/{id}/restore` responses; preserved across snapshot/restore cycles

### Per-VM LLM Profiles

- Each VM spawn can specify a named profile via `{"profile": "anthropic"}` in the request body
- Profiles stored under `configs/profiles/<name>/goose.yaml` + `goose-secrets.yaml`
- Enables running multiple VMs with different providers, models, or API keys simultaneously
- Omitting `profile` uses the default `configs/goose.yaml`

### API Token Hot Reload (SIGHUP)

- `kill -HUP <daemon_pid>` re-reads `EPHEMERA_API_TOKENS` / `EPHEMERA_API_TOKEN` and swaps the in-memory client list
- Running VMs are not affected; no downtime required to add, rotate, or revoke tokens

### MicroVM Snapshot & Restore

- `POST /vms/{vm_id}/snapshot` — freezes VM memory state and copies rootfs to `snapshots/<id>/`
- `POST /snapshots/{id}/restore` — resumes a VM from snapshot in ~5 s (vs ~60 s cold boot); preserves agent token
- `DELETE /vms/{vm_id}` with `stop_after: true` destroys the source VM immediately after snapshot
- `GET /snapshots` — list all stored snapshots
- `DELETE /snapshots/{id}` — delete snapshot files

### Diff Snapshots (Multi-Checkpoint)

- First snapshot of a VM → **Full** (2 GB `memory.bin`)
- Subsequent snapshots → **Diff** (sparse `memory.bin`, dirty pages only — typically 1–400 MB)
- Type auto-detected; `"type": "full"` or `"type": "diff"` can override
- `base_snapshot_id` links each Diff to its Full base
- Deleting a Full that has referencing Diffs returns `409 Conflict`
- Restore from Diff merges base + diff memory in-memory; temp file cleaned up immediately after Firecracker opens it

### Post-Restore IP Reconfiguration (vsock)

- Each restored VM receives a fresh IP from the pool; the original IP is freed
- Guest network stack updated in-place via vsock (`CHANGE_IP <cidr> <gw>`) — no reboot required
- `goose-agent` binds `AF_VSOCK` port 1234 inside the VM for reconfiguration commands

### Concurrent Restore

- Multiple VMs can be restored simultaneously from different snapshots
- Each restore gets its own disk COW device; bind-mount stacking ensures Firecracker opens the correct device

### COW Rootfs Restore (dm-snapshot)

- Restore no longer copies the 700 MB rootfs — instead creates a Linux dm-snapshot COW device
- Base disk (`rootfs.ext4`) is read-only and shared; per-VM writes go to a sparse exception store (~0 MB initial)
- On VM delete: dm device removed, loop devices detached, exception store deleted automatically
- Falls back to full copy if dm-snapshot is unavailable

### Control Plane Agent Proxy

- Three new proxy endpoints route agent traffic through the control plane — no direct VM subnet access needed:
  - `POST /vms/{vm_id}/tasks` → goose-agent `/tasks`
  - `GET  /vms/{vm_id}/health` → goose-agent `/health`
  - `POST /vms/{vm_id}/stop`  → goose-agent `/stop`
- Callers authenticate with the control plane Bearer token only; agent token is injected internally
- `EPHEMERA_PUBLIC_URL` env var: when set, `agent_url` in VM responses points to the proxy path (`{url}/vms/{vm_id}`) instead of the private VM IP

### End-to-End Test Suite

- `e2e_test.sh` — 50-step integration test covering the full VM and snapshot lifecycle
- Validates: VM spawn, parallel tasks, snapshot/restore, concurrent restore, diff snapshots, COW rootfs, agent proxy, `EPHEMERA_PUBLIC_URL` behavior

---

# v0.1.0 — Initial Implementation

**Ephemera** is an enterprise control plane for running ephemeral AI agents inside Firecracker MicroVMs. This first release delivers a fully working end-to-end implementation: from spinning up an isolated KVM-backed VM to executing Goose AI tasks via HTTP and cleaning up all host resources on teardown.

---

## What's New

### Control Plane API

- `POST /vms` — spawn a MicroVM; blocks until goose-agent is ready (~60 s), returns `vm_id`, `guest_ip`, `agent_url`
- `GET /vms` — list running VMs
- `DELETE /vms/{vm_id}` — stop VM and release all host resources (TAP device, disk clone, IP)

### goose-agent (in-VM HTTP agent)

- Runs inside each MicroVM as PID 1 via `micro-init`
- `POST /tasks {"prompt":"..."}` — execute a Goose task, return output
- `GET /health` — `idle` | `busy` status
- `POST /stop` — graceful agent shutdown

### Self-bootstrapping

- Firecracker v1.15.1 downloaded and SHA256-verified automatically on first run
- Linux 6.1.155 kernel downloaded automatically
- Golden image (Debian Bookworm minbase + Goose + goose-agent) built via `debootstrap` on first run
- `goose-agent` binary compiled from source before image build

### Guest OS — Minimal Debian Bookworm

- Replaced initial Ubuntu 22.04 skeleton with Debian Bookworm `--variant=minbase`
- No SSH, no init daemon, no DHCP client — `micro-init` mounts virtual filesystems and execs goose-agent directly as PID 1
- Network configured via Linux kernel `ip=` boot parameter (live before any user-space process starts)
- Host timezone mirrored into VM via `/etc/localtime` symlink injection at provisioning time

### Runtime Config Injection

- `configs/goose.yaml` (provider, model, extensions) and `configs/goose-secrets.yaml` (API keys) injected into each VM's disk at provisioning time — no image rebuild needed to change provider or model
- Supports Google, Anthropic, OpenAI, Ollama, and all other Goose-compatible providers
- Keyring-free operation (`GOOSE_DISABLE_KEYRING: true`) for headless VM environments

### Network & Storage

- Linux bridge `goose-br0` with iptables MASQUERADE for VM-to-internet connectivity
- IP pool (10.0.1.2–254) sorted and recycled across VM lifecycle
- TAP device IDs recycled via free-list after VM destruction
- All host resources guaranteed to be released on teardown

### Security

- Per-client Bearer token authentication (`EPHEMERA_API_TOKENS=alice:token1,bob:token2`)
- Timing-safe token comparison via `crypto/subtle.ConstantTimeCompare`
- Each request logged with authenticated client name for audit trail
- Control plane binds to `127.0.0.1:3000` by default (localhost only)
- TLS supported via Caddy or Nginx reverse proxy (see README)
- Single-token fallback (`EPHEMERA_API_TOKEN`) for backward compatibility

---

## Prerequisites

- Ubuntu 22.04 or 24.04 host (bare metal or nested virtualization)
- `/dev/kvm` accessible
- Go 1.18+
- `sudo apt install -y curl debootstrap e2fsprogs util-linux`
