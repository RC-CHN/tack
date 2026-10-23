# Tack management development baseline

This directory contains Tack's independent control, node and protocol modules. `tack-control` is hosted by the operator, `tack` is the operator CLI, and `tack-node` runs on customer machines. There is no Web UI. The node is a foreground process: deployment paths, service installation and boot persistence are entirely external.

The accepted requirements and remaining stages are in [the design](../docs/tack-design.md). Use the repository [deployment skill](../skills/tack-deploy/SKILL.md) to adapt installation to a customer machine. The current binaries are a development baseline, not the obfuscated, packed or hardware-key-protected commercial release.

## Implemented behavior

- Separate Node and administrator HTTP handlers and listeners; the administrator listener is restricted to literal loopback addresses.
- Per-node credentials, explicit node selection, durable task queue, cancellation, bounded final stdout/stderr and exit status.
- HTTPS certificate validation, optional private CA, no redirects; plaintext development access requires explicit literal-loopback opt-in.
- Per-task execution intent journal, replay suppression and result retry. Interrupted execution becomes `unknown` on restart rather than being rerun.
- Linux process-group cancellation and Windows suspended process creation followed by Job Object assignment before execution.
- `exec`, `posix-sh`, `portable-sh`, `bash`, `zsh`, `powershell`, `cmd` modes. BusyBox is configured externally; its binaries are not bundled yet.
- A single running task per Node. A different live instance using the same node identity is rejected for 30 seconds after the last heartbeat.

Remaining work includes certificate enrollment/rotation, tenants and RBAC, online revocation, streaming output, retention, Chisel process supervision, native old-platform validation and the protected release pipeline. No automatic changes are made to existing Chisel configuration.

## Build

Run from the repository root with a compatible Go toolchain:

```sh
mkdir -p build/tack
(cd management/control && go build -trimpath -o ../../build/tack/tack-control ./cmd/tack-control)
(cd management/control && go build -trimpath -o ../../build/tack/tack ./cmd/tack)
(cd management/node && go build -trimpath -o ../../build/tack/tack-node ./cmd/tack-node)
```

The controller requires Go 1.25 or newer. Node and protocol declare Go 1.23 and use only the standard library. Legacy builds must explicitly select the compatible toolchain, e.g. `GOTOOLCHAIN=go1.23.12 CGO_ENABLED=0`; setting `go 1.23` alone does not select an old runtime. Root Chisel has its own separate toolchain and dependencies.

Tack modules use `tack.local/management/...` with relative `replace` directives. This is a local module namespace, not a download address; build from this checkout. Chisel's upstream module path remains attached to the tunnel component.

## Renaming an existing development deployment

Replace the old management executables with `tack-control`, `tack-node` and `tack`, and update the external startup definitions to their selected paths. Keep the same configuration files, node identities and state directories. The JSON format, protocol version 1 endpoint `/v1/agent/sync`, and state files (including `agent.lock`) remain compatible. Do not delete or move the journal merely to rename the software.

New provisioning uses `-node-config` and `-node-ca`. The CLI still accepts `-agent-config` and `-agent-ca` as deprecated aliases. The deployment skill is now `tack-deploy`. The tunnel executable remains `chisel`.

## Provision and start the Controller

These paths are examples for a regular server. Create directories with restricted ownership using the chosen deployment account. Do not apply them unchanged to a NAS or router. The target node directories supplied to `provision` are only written into configuration, not created on the node.

```sh
umask 077
mkdir -p ./operator-config ./operator-state
./build/tack/tack provision \
  -config ./operator-config/controller.json \
  -node-config ./operator-config/customer-a-node-01.json \
  -node customer-a-node-01 \
  -server https://control.example.com:8443 \
  -state-dir /srv/tack-node/state \
  -work-dir /srv/tack-node/work
```

Provisioning creates a random administrator credential only when the Controller configuration is new, and a unique credential for the new node. It refuses duplicate IDs or an existing Node configuration output file. Transfer only that node's generated Node configuration privately. The Controller configuration contains administrative and other node credentials and must never be copied to a customer machine.

Supply an existing certificate valid for the advertised hostname:

```sh
./build/tack/tack-control \
  -config ./operator-config/controller.json \
  -state ./operator-state/tasks.json \
  -listen 0.0.0.0:8443 \
  -admin-listen 127.0.0.1:8081 \
  -tls-cert /path/to/fullchain.pem \
  -tls-key /path/to/private-key.pem
```

Only the Node listener is customer-facing. Keep the administrator listener private; operate `tack` locally on the control server or through an administrator SSH forwarding arrangement. If using a reverse proxy, publish only the Node listener. Restart the Controller after provisioning another node; tasks remain in its state file. Remove a node's entry and restart to revoke its management credential. Already running local tasks continue until cancellation is received or their local timeout expires.

## Start a customer Node

The deployment operator selects and prepares directories before starting the process. Both `state_dir` and `work_dir` must be absolute, existing writable directories. The Node never chooses an alternative directory, installs itself, registers a service, changes mount options or adds a startup entry.

```sh
/chosen/bin/tack-node -config /chosen/config/node.json
```

Additional configuration fields:

| Field | Meaning |
| --- | --- |
| `state_dir` | Exclusive state directory for one node; contains identity, lock and task journal |
| `work_dir` | Task-specific temporary directories; may be on a different writable volume |
| `state_persistence` | `durable` (default) or `volatile`, declared by the deployment operator |
| `ca_file` | Optional node-side PEM CA bundle for a private issuer |
| `busybox` | Optional absolute path to the separately provisioned BusyBox executable |
| `poll_seconds` | 1–10, default 2; cancellation delivery depends on connectivity and polling |

Declare volatile storage honestly: replay suppression cannot survive loss of that storage. Do not share a state directory between nodes, or run multiple Node instances with the same identity. Existing journal state is bound to its node ID. Files are created with restricted POSIX modes; Windows ACLs and protecting the parent directories are responsibilities of deployment.

## Execute on one node

Create a task file on the control server:

```json
{
  "node_id": "customer-a-node-01",
  "command": {
    "mode": "posix-sh",
    "script": "printf 'management connection works\\n'",
    "timeout_seconds": 15,
    "output_limit": 65536
  }
}
```

Use the private administrator listener:

```sh
./build/tack/tack nodes -server http://127.0.0.1:8081 -allow-insecure-loopback -config ./operator-config/controller.json
./build/tack/tack submit -server http://127.0.0.1:8081 -allow-insecure-loopback -config ./operator-config/controller.json -file task.json
./build/tack/tack get -server http://127.0.0.1:8081 -allow-insecure-loopback -config ./operator-config/controller.json -id TASK_ID
./build/tack/tack cancel -server http://127.0.0.1:8081 -allow-insecure-loopback -config ./operator-config/controller.json -id TASK_ID
```

JSON stdout/stderr are base64 byte strings. Decode according to the command's output encoding. `stdout_truncated` and `stderr_truncated` indicate retained output limits; excess output is drained so the process does not block. Logs are returned after execution in this stage. The exit code is absent if the process never started or its final status is unknown.

`exec` uses `program` and `args`; script modes use `script`. `dir` sets the task working directory, otherwise a temporary task directory is used. `env` overlays inherited Node environment, so do not inject operator credentials or unrelated secrets into the Node environment. Every task runs as the Node's account. Explicit Bash/Zsh/PowerShell/CMD requests never fall back to another language. A failed script is never rerun automatically under BusyBox.

The API is version 1: `POST /v1/agent/sync` on the Node listener; `GET /v1/nodes`, `GET/POST /v1/tasks`, `GET /v1/tasks/{id}`, `POST /v1/tasks/{id}/cancel` on the administrator listener. `GET /health` exposes only the protocol version. A new `submit` always creates a new task; a lost submit response must be reconciled using `tasks` before manually retrying.

## Current operational limits

The Controller is single-process and persists queue changes as a complete JSON snapshot. Keep one process per state file and back it up. The Node retains task tombstones, caps the journal at 10,000 entries, and stops accepting new work at that boundary. Do not delete the journal to bypass replay protection; retention and archival are a later stage. Result bodies are discarded locally after Controller acknowledgement. If an old acknowledged task is replayed after a Controller restore, the Node reports `unknown` and refuses to execute it again.

Short network failures do not cancel tasks. Shutdown requests cancel the local task and save its result when possible; an unexpected restart converts unfinished intent to `unknown`. Process-group cleanup does not cover programs deliberately escaping the group. Job Object behavior requires Windows native validation. Filesystem crash durability, flash limits, device drivers and boot persistence require target-system testing.

Current node tokens are readable by the Node's account. No software-only scheme can promise secrecy from root or an attacker controlling the Node process. They authorize only this node's synchronization, not administrative APIs or server shell access. Production still requires the identity, network isolation and credential lifecycle work described in the design.

## Checks

```sh
for module in protocol control node integration; do
  (cd "management/$module" && go test -race ./... && go vet ./...)
done
```

The separate management CI covers these modules, a Go 1.23.12 Node build/test job and an architecture compilation matrix. Cross-compilation is recorded separately from native OS/kernel/device testing.

See the [validation record](VALIDATION.md) for checks actually run in this workspace and remaining native-platform validation.
