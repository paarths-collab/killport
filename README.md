# ⚡ killport

> [!IMPORTANT]
> **Official Distribution:** This repository is for viewing and auditing only. Please **DO NOT CLONE** this repository for usage. To install **killport**, please use the official binaries or package managers (Brew/Scoop) provided below to ensure you have the latest optimized version.

[![Go Report Card](https://goreportcard.com/badge/github.com/paarths-collab/killport)](https://goreportcard.com/report/github.com/paarths-collab/killport)
[![GitHub Release](https://img.shields.io/github/v/release/paarths-collab/killport)](https://github.com/paarths-collab/killport/releases)
[![License: Proprietary](https://img.shields.io/badge/License-Proprietary-red.svg)](LICENSE)

Inspect and free local ports, with JSON output and an optional identity-bound plan/apply workflow.

---

## 🚀 Why killport?

Use **killport** to inspect local port owners, preview actions, and check the outcome after stopping a process or container:

- 🐳 **Docker Native:** Safely triggers `docker stop` on containers instead of murdering the `docker-proxy` and leaving zombie containers behind.
- 🤖 **AI-Agent Optimized:** Machine-readable `--json` output gives scripts and coding agents explicit owners, actions, failures, and exit classes.
- 🎯 **Port Ranges:** Bounded concurrent execution handles several requested ports in one command (e.g., `3000-3010`).

---

## 📦 Installation

### macOS (Homebrew)
```bash
brew tap paarths-collab/homebrew-tap
brew install paarths-collab/tap/killport
```

The qualified formula name selects this project; Homebrew core also contains a different `killport`.

### Windows (Scoop)
```powershell
scoop bucket add paarths-collab https://github.com/paarths-collab/scoop-bucket
scoop install paarths-collab/killport
```

### Direct downloads

Prebuilt archives are available from [GitHub Releases](https://github.com/paarths-collab/killport/releases). Windows users can use Scoop or the Windows archive.

### Linux / macOS (One-liner)
```bash
curl -sSfL https://raw.githubusercontent.com/paarths-collab/killport/main/install.sh | sh
```
The script runs under any POSIX `sh`, installs to `/usr/local/bin`, and refuses to install unless the archive's SHA-256 matches the release `checksums.txt`. Set `KILLPORT_VERSION` (for example `1.1.0`) to pick a release; the default is the latest.

---

## 🛠 Usage

### Kill one or more ports
```bash
killport 3000 8080 5432
```

### Kill a range of ports (Concurrent)
```bash
killport 3000-3005
```

### Dry Run (Simulate)
```bash
killport 3000-3005 --dry-run
```

### Check which process is using ports (no kill)
```bash
killport --check
```

### Check specific ports only
```bash
killport --check 3000 5432
```

### AI-Agent / Machine Mode

`--json` (`-j`) prints JSON for parsing by LLMs or CI/CD pipelines. Details are in [JSON output](#json-output).

```bash
killport 5432 --json
```

```bash
killport --check --json
```

### Version requirements and upgrades

`--check` and the `inspect`, `plan`, and `apply` workflow are available in **v1.2.0 and later**. Published v1.1.0 packages do not include them. Run `killport --version` to check your installation. Source builds without release version information report `dev`.

Upgrade an existing package installation:

```sh
brew update
brew upgrade paarths-collab/tap/killport
```

```powershell
scoop update
scoop update killport
```

For script installations, rerun the installation command above; it verifies the new archive before replacing the binary.

---

## 📋 Behaviour

- **Port specs:** `N` or `A-B`, decimal digits only, 1–65535, with `A ≤ B`. Any invalid spec rejects the whole command with exit 2; nothing is inspected or killed.
- **What counts as a port user:** only TCP sockets in the `LISTEN` state, matched by their exact local port. Client connections, established connections and UDP sockets are ignored.
- **Tools:** macOS uses `lsof`. Linux uses `lsof`, or `ss` if `lsof` is missing. Windows uses `netstat -ano` and `tasklist`. Docker is optional.
- **Visibility:** without root (Linux/macOS) or Administrator (Windows), sockets owned by other users may be invisible. A listener whose owner is not visible is reported with no PID and is never killed. On Linux, when both tools exist, `ss` is also consulted for ports `lsof` did not show, so such listeners are reported as in use instead of free. If `lsof` exits 1 with no output but prints a message on stderr, killport warns instead of assuming there are no listeners.
- **Windows process names:** if `tasklist` fails, names are unknown, Docker's own processes cannot be recognised, and killport warns and refuses to kill any PID it cannot name.
- **Docker:** only daemons reached through a local Unix socket or Windows named pipe may supply local port owners. TCP and SSH endpoints (including loopback TCP endpoints) are refused with a warning; their published ports may belong to another machine. The active Docker context is resolved before querying containers, and `DOCKER_CONTEXT` takes precedence over `DOCKER_HOST`. For supported local daemons, published host ports (TCP, including ranges and IPv4/IPv6 bindings) are matched to containers. Each container is stopped once with `docker stop -t 10`. Docker's own processes (`docker-proxy`, Docker Desktop backend, `vpnkit`, `containerd`, and similar) are never killed directly. If `docker ps` fails, killport prints a warning to stderr and continues without Docker owners.
- **Protected owners (never signalled):** PID 0 or unknown, PID 1 and below, killport itself, Windows PID 4 (System), and Docker infrastructure that has no container to stop. These rows report `status: "failed"` with the reason.
- **Dry run (`--dry-run` / `-d`):** changes nothing. Rows have `status: "planned"` and `dry_run: true`, and read `Would kill PID … (name) on port …` or `Would stop Docker container … publishing port …`.
- **Verification:** after a real kill or stop, killport re-checks the port for about 3 seconds, per owner. A row is `killed` or `stopped` only when its own PID or container is gone; otherwise it fails with `PID P (name) still holds port N after kill_process` (or `port N still in use after stop_container`). Any other owner still on that port (a refused PID, a replacement process with a new PID, a lingering Docker proxy) keeps or gets its own `failed` row, so a `killed` row never means the port is free while a `failed` row for the same port exists. If the re-check itself cannot run, the row is `failed` with `killed, but could not verify the port is free: …` (or `stopped, …`).
- **Free ports:** a port with no owner gets one row with `status: "free"` (`Port N is already free`).
- **Check mode (`--check` / `-c`):** lists TCP listeners and Docker-published ports. `--check <ports>` limits the output to those ports; free ports show as `no_listener`.
- **Verbose (`--verbose` / `-v`):** logs every external command with its duration, and each kill, to stderr. Works with `--json`.
- **Flags:** `-c/--check`, `-d/--dry-run`, `-j/--json`, `-v/--verbose`, `--version`, `--help`.

## 🧾 JSON output

With `--json`, stdout is always valid JSON, including `--help` (`{"usage": "..."}`) and `--version` (`{"version": "..."}`). Without `--json` those two print plain text. Warnings and `--verbose` logs go to stderr.

### Kill mode

An array with one object per (port, owner) pair, in port order, then containers, then PIDs.

| Field | Type | Notes |
|---|---|---|
| `port` | string | Decimal port number |
| `process_name` | string | Omitted when empty. Docker rows use `[Docker] <container name>` |
| `pid` | string | Omitted when unknown |
| `killed` | bool | `true` only after a verified kill or stop |
| `error` | string | Omitted when empty; the real reason for a `failed` row |
| `is_docker` | bool | `true` for container rows |
| `status` | string | `"free"`, `"planned"`, `"killed"`, `"stopped"` or `"failed"` |
| `action` | string | `"none"`, `"kill_process"` or `"stop_container"` |
| `dry_run` | bool | `true` with `--dry-run` |
| `container_id` | string | Omitted except for Docker rows |

```json
[
  {
    "port": "3000",
    "process_name": "node",
    "pid": "4120",
    "killed": true,
    "is_docker": false,
    "status": "killed",
    "action": "kill_process",
    "dry_run": false
  }
]
```

### Check mode

An array with one object per listener (or one `no_listener` object per free requested port).

| Field | Type | Notes |
|---|---|---|
| `port` | string | Decimal port number |
| `process_name` | string | Omitted when empty |
| `pid` | string | Omitted when not visible |
| `category` | string | `"system"`, `"runtime"`, `"application"` or `"unknown"` (no name) |
| `safe_to_kill` | bool | Always present. Advisory name heuristic, never a guarantee. `true` only for a real PID above 1 (or a Docker container) whose name is classified `runtime` or `application`, and that is not Docker infrastructure. See [Classification heuristic](#classification-heuristic) |
| `is_docker` | bool | Omitted when false |
| `no_listener` | bool | Omitted when false; `true` for a free requested port |
| `error` | string | Omitted when empty (not currently set) |

```json
[
  {
    "port": "3000",
    "process_name": "node",
    "pid": "4120",
    "category": "runtime",
    "safe_to_kill": true
  },
  {
    "port": "8081",
    "safe_to_kill": false,
    "no_listener": true
  }
]
```

### Classification heuristic

`category` and `safe_to_kill` come from the process name only, matched exactly after lowercasing, dropping `.exe`, a trailing `(…)` note, a `/suffix` and trailing version digits. Known system names and `systemd-*` are `system`; known dev runtimes and servers (`node`, `python`, `next-server`, and similar) are `runtime`; an empty name is `unknown`. Every other name is called `application`, so an unfamiliar name is not evidence that stopping it is safe: `safe_to_kill` is advisory, can be wrong in both directions, and never replaces looking at the PID yourself.

### Fatal errors

A single object on stdout, with the exit code from [Exit codes](#exit-codes):

```json
{"error": "requires at least 1 port (or use --check to inspect)", "code": "usage"}
```

`code` is one of `usage` (exit 2), `missing_dependency` (exit 1) or `enumeration_failed` (exit 1). The workflow subcommands also return `write_failed` (exit 1) and add an `exit_class` field. An unexpected internal failure (a recovered panic) is reported as one such document with `code` `internal`, `exit_class` `failed` and exit 1, never as a stack trace.

## 🚦 Exit codes

| Code | Meaning |
|---|---|
| `0` | Success. Kill: every requested port is free at the end (or was already free). Dry run: no failed rows. Check: inspection succeeded, whether ports are free or in use |
| `1` | Runtime failure: any row has `status: "failed"`, a required tool is missing, or listing listeners failed |
| `2` | Usage error: unknown flag, invalid port spec, or no ports without `--check` |

The `inspect`, `plan` and `apply` subcommands also use exit codes 3 and 4. See [Plan / apply workflow (opt-in)](#plan--apply-workflow-opt-in).

## Plan / apply workflow (opt-in)

Use this workflow when you want to see exactly which process or container holds a port, bind that answer to the specific process identity, and act on it later. `inspect` and `plan` never change anything. `apply` acts only on what the plan names, and refuses any process whose identity has changed since planning.

The classic `killport <ports>` command remains the default. Nothing in the workflow runs unless you invoke `inspect`, `plan` or `apply`.

### When to use it

1. Preview the exact owner of a port (`inspect`).
2. Save a plan that binds each owner to its process identity or container ID (`plan -o`).
3. Apply that plan later (`apply`). Apply refreshes ownership for each resource after it leaves the action queue, then re-checks process identity before signalling. A process proven to have closed all its planned ports is skipped even if Docker is unavailable.
4. Check the endpoint afterwards. Apply reports whether each port is free, still held by the original owner, or now held by a new owner.

### Commands and flags

| Command | What it does | Flags |
|---|---|---|
| `killport inspect [port...]` | Read-only observation. With no ports, reports every occupied port. | `--protocol tcp` or `udp`, `--mode auto`, `native` or `container`, `-j/--json`, `-v/--verbose` |
| `killport plan <port...>` | Read-only. Writes the plan of what apply would do. | `--protocol`, `--mode`, `--force` (plan SIGKILL instead of SIGTERM), `--ttl` (default `5m`, maximum `24h`), `--grace` (default `10s`, maximum `1h`; the Docker stop grace period and how long apply waits for the port to free), `-o/--output <file>`, `-j/--json`, `-v/--verbose` |
| `killport apply <plan.json>` or `apply -` | Carries out a plan. `-` reads the plan from standard input. | `-j/--json`, `-v/--verbose` |

- `--protocol tcp` (default) matches LISTEN sockets by local port. `--protocol udp` matches bound sockets by local port. Apply takes the protocol from the plan, so it has no `--protocol`, `--mode`, `--force`, `--ttl` or `--grace` flags.
- `--mode auto` (default) considers containers and native processes. `native` plans only native processes, and `container` plans only containers.
- `plan` with `-o` writes the file (mode 0600) even when the exit class is not `ok`; an existing regular file is replaced, but a symbolic link is refused. With `--json` it also prints the plan to stdout.
- The root-level `-d/--dry-run` flag belongs to the classic command. For the workflow, `plan` is the preview.

```sh
killport inspect 3000
killport plan 3000 3001 -o plan.json
killport apply plan.json
killport plan 3000 --json | killport apply -
```

### Flow

```
request (ports, protocol, mode)
  -> observation          socket table + owner attribution (lsof, ss, or netstat + tasklist) + docker ps
  -> owner resolution     native owner -> PID + start time (identity); container -> full 64-hex ID
  -> plan                 resources to act on, refusals, endpoints; plan_id = sha256 of the plan
  -> identity revalidation  apply: plan expiry first, then a fresh observation, then each resource
                            checked against its bound identity before any signal or docker stop
  -> action               SIGTERM/SIGKILL to native PIDs, or docker stop -t <grace>; up to 4 at once
  -> outcome observation  apply polls the planned endpoints until each is free or --grace has passed
                          (observed once if nothing was acted on)
```

Apply never searches for new targets. Each action comes from the plan document.

### Plan document (`schema_version` 1)

Fields: `schema_version`, `kind` (`killport.plan`), `plan_id`, `tool_version`, `os`, `created_at`, `expires_at`, `observation_id`, `docker_context`, `request` (`protocol`, `scope`, `mode`, `ports`, `force`, `grace_seconds`), `endpoints` (per port: `visibility`, `occupied`, `resources`, `refused`, `provider_errors`), `resources` (`resource_id`, `provider`, `pid`, `container_id`, `name`, `start_time`, `identity_strength`, `ports`, `action`, `signal`, `grace_seconds`), `refused`, `actions_executed`, `exit_class`.

`plan_id` is `sha256:` plus the hash of the compact JSON with `plan_id` empty. Apply recomputes it and rejects a modified plan (exit 2). The hash is a checksum, not a signature. It detects accidental or partial edits but does not prove who wrote the plan.

Example, trimmed from `docs/evidence/plan.json` (the port 3001 endpoint and resource are omitted):

```json
{
  "schema_version": 1,
  "kind": "killport.plan",
  "plan_id": "sha256:a11c0b13098d02546523458067bdbd4c749834a2b0116f2c2f20620ef72e0b87",
  "tool_version": "dev",
  "os": "linux",
  "created_at": "2026-10-21T14:00:38Z",
  "expires_at": "2026-10-21T14:05:38Z",
  "request": {"protocol": "tcp", "scope": "listener", "ports": [3000, 3001], "force": false, "grace_seconds": 10},
  "endpoints": [
    {"protocol": "tcp", "port": 3000, "visibility": "complete_for_requested_scope", "occupied": true,
     "resources": ["proc:362:2164526"], "refused": []}
  ],
  "resources": [
    {"resource_id": "proc:362:2164526", "provider": "native", "pid": 362, "name": "python3",
     "start_time": "2164526", "identity_strength": "pidfd", "ports": [3000], "action": "signal", "signal": "SIGTERM"}
  ],
  "refused": [],
  "actions_executed": 0,
  "exit_class": "ok"
}
```

The captured files predate the `mode`, `observation_id`, `docker_context` and `provider_errors` fields, which the current code writes. They are documented above, but they do not appear in the captured examples.

### Apply result (`killport.apply_result`)

Fields: `schema_version`, `kind`, `plan_id`, `applied_at`, `observation_id`, `docker_context`, `actions_executed`, `resources` (`resource_id`, `provider`, `pid`, `container_id`, `ports`, `result`, `verified_gone`, `forced`, `note`, `error`), `endpoints` (`protocol`, `port`, `status`, `visibility`, `holders`, `provider_errors`), `exit_class`, and `error` (only for an expired plan).

Example, trimmed from `docs/evidence/apply.json` (the port 3001 resource and endpoint are omitted):

```json
{
  "schema_version": 1,
  "kind": "killport.apply_result",
  "plan_id": "sha256:a11c0b13098d02546523458067bdbd4c749834a2b0116f2c2f20620ef72e0b87",
  "applied_at": "2026-10-21T14:00:38Z",
  "actions_executed": 2,
  "resources": [
    {"resource_id": "proc:362:2164526", "provider": "native", "pid": 362, "ports": [3000],
     "result": "signal_sent", "verified_gone": true, "forced": false}
  ],
  "endpoints": [
    {"protocol": "tcp", "port": 3000, "status": "free", "visibility": "complete_for_requested_scope"}
  ],
  "exit_class": "ok"
}
```

Drift example, verbatim from `docs/evidence/apply-drift.json`. The plan was made for PID 484. Before apply, that process exited and a new `python3` (PID 496) took port 3002:

```json
{
  "schema_version": 1,
  "kind": "killport.apply_result",
  "plan_id": "sha256:af85e763f3883e22cb8549ce37e9cd4e253abe810d2895e302ac41def76ebb05",
  "applied_at": "2026-10-21T14:00:42Z",
  "actions_executed": 0,
  "resources": [
    {"resource_id": "proc:484:2164788", "provider": "native", "pid": 484, "ports": [3002],
     "result": "not_attempted", "verified_gone": true, "forced": false, "note": "process already exited"}
  ],
  "endpoints": [
    {"protocol": "tcp", "port": 3002, "status": "blocked_by_new_owner",
     "visibility": "complete_for_requested_scope", "holders": [{"pid": 496, "name": "python3"}]}
  ],
  "exit_class": "needs_fresh_plan"
}
```

### Enumerations

| Field | Values |
|---|---|
| `visibility` | `complete_for_requested_scope`, `permission_limited`, `none_visible`, `provider_unavailable` |
| Resource `result` | `not_attempted`, `identity_changed`, `signal_sent`, `stopped`, `permission_denied`, `timeout_unknown`, `failed` |
| Endpoint `status` | `free`, `blocked_by_original` (the planned owner still holds the port), `blocked_by_new_owner` (someone else holds it), `blocked_by_refused_owner` (the occupant is one the plan itself refused), `not_verifiable` |
| `identity_strength` | `pidfd`, `handle`, `start_time` (the value `none` is never written into a plan) |
| `action` / `signal` | `signal` with `SIGTERM`, `SIGKILL` or `terminate` (Windows); `docker_stop` |
| `provider` | `native`, `docker` |
| `mode` | `auto`, `native`, `container` |
| `scope` | `listener` (tcp), `bound` (udp) |
| `provider_errors[].provider` | `docker`, `lsof`, `ss`, `netstat`, `tasklist`, `proc` |
| `provider_errors[].category` | `unavailable`, `timeout`, `permission`, `failed`, `parse` |
| `exit_class` | `ok`, `failed`, `usage`, `needs_fresh_plan`, `uncertain` |

### Exit classes

The workflow uses exit codes 0 to 4. The classic command uses only 0 to 2.

| Code | Class | What produces it |
|---|---|---|
| `0` | `ok` | Plan or inspect: no refusals, complete visibility, and every occupied endpoint has a resource. Apply: no failed or uncertain resource, and every endpoint is `free` with complete visibility. |
| `1` | `failed` | Plan: any refusal (for example a hidden or denied owner, Docker infrastructure without a container, a protected PID, or a short container ID), or an occupied endpoint with no resource. Inspect: an owner or Docker provider failed. Apply: a resource `failed` or `permission_denied`, or an endpoint `blocked_by_original` or `blocked_by_refused_owner` after acting (there is no SIGKILL escalation; a fresh plan would refuse that occupant again). Also missing tools and a plan file that cannot be written. |
| `2` | `usage` | Invalid arguments or flag values, an unreadable, invalid or modified plan, a plan from another OS, or an unsupported schema version. Returned before anything is observed. |
| `3` | `needs_fresh_plan` | Apply: the plan has expired (checked before observing), a resource's identity changed (`identity_changed`), or an endpoint is `blocked_by_new_owner`. |
| `4` | `uncertain` | Inspect or plan: a `permission_limited` or `none_visible` endpoint, or inspect with no ports while the socket table cannot be read. Apply: a `timeout_unknown` resource, a `not_verifiable` endpoint, any endpoint whose visibility is not complete, or an apply that was interrupted (see Limits). |

When several conditions apply, the worst class wins, in this order: `failed` (1), then `uncertain` (4), then `needs_fresh_plan` (3), then `ok` (0). Usage (2) is returned before anything is observed, so it never competes with the others.

Two details differ by command. In plan and inspect, `provider_unavailable` gives 1. In apply, a non-complete visibility gives 4 unless a failure outranks it, so the same condition can exit 4 in apply. In plan, a permission-limited endpoint always carries a refusal, so it exits 1, and 4 comes from `none_visible` instead.

### Identity binding

Apply signals a process only if the PID still belongs to the process that was planned. How strongly that is enforced depends on the OS:

- **Linux**: identity is the PID plus the start time in `/proc/<pid>/stat` (field 22). When `pidfd_open` works (kernel 5.3 or later, and not blocked by the sandbox), the start-time check and the signal both go through the pidfd, so there is no race (`identity_strength: pidfd`). Otherwise (or when `pidfd_send_signal` is unavailable or filtered) the check is made and then the signal is sent, leaving a small race (`start_time`).
- **macOS**: identity is the kernel process start time (`sec.usec`). The check is made and then the signal is sent, leaving a small race (`start_time`).
- **Windows**: identity is the process creation time, read from an open process handle. The check and `TerminateProcess` both use that handle (`handle`). Windows always terminates the process; `--force` does not change this, and the plan records `terminate`.
- A process whose identity cannot be read is refused. It is never signalled by PID alone.
- Always refused: PID 0 or unknown, PID 1 and below, PIDs above 2147483647, killport itself, and Windows PID 4 (System). A container is bound to its full 64-hex ID. A container listed with a short ID is refused.

### Limits

- The hosted Linux, macOS, and Windows suites passed on 2026-10-23. Linux also passed the race detector and installer regressions. Native Windows checks covered real process-handle identity and termination, TCP/UDP inspect and plan/apply, dry-run, owner drift with zero actions, and classic termination. A separate macOS smoke test covered check, inspect, plan, apply, and owner drift. See [platform validation](docs/platform-validation.md) for the evidence and remaining limits.
- Eight live Docker checks passed using the native macOS ARM64 CLI and Docker Desktop's Linux ARM64 daemon: graceful and forced stops, TCP and UDP, multiple publications, replacement protection, and failed or remote endpoint handling. See [platform validation](docs/platform-validation.md) for reproducible scripts and remaining platform limits. The earlier Linux evidence run mounted no Docker socket.
- Apply never escalates to SIGKILL. If SIGTERM does not free the port, the endpoint is `blocked_by_original` and apply exits 1.
- `--force` plans SIGKILL on Linux and macOS. On Windows the process is always terminated.
- The plan is a checksum, not a signature (see above).
- The Linux evidence run was done as root in a container. Owners that are invisible to a non-root user are therefore not exercised by that run.
- A plan expires after `--ttl` (default 5 minutes). Apply then exits 3 without observing or acting. A plan whose `expires_at` lies more than 24 hours after `created_at` is rejected as invalid.
- Apply stops after the grace period plus 90 seconds per wave of 4 resources (at most 10 minutes), or on SIGINT or SIGTERM. It then starts no further action, observes the outcome once, and prints the partial results with `error` set and exit class `uncertain` (4).

### Verified evidence

- `docs/evidence/README.md` describes the Linux end-to-end run: what it covers, what it does not, and how to reproduce it. `docs/evidence/linux-run.txt` is the raw transcript. The JSON files alongside it are the documents quoted above.
- `scripts/demo-run.sh` reproduces that run in one `docker run` of `golang:1.25`. It needs Docker and writes into `docs/evidence/` (or `EVIDENCE_DIR`).
- `testdata/corpus/` holds 44 table-driven cases (`01-` to `44-`). Each directory has one `case.json`. `testdata/corpus/README.md` records for each case whether its input is real, synthetic or mixed. Run them with `go test -count=1 -run Corpus -v ./...`.
- `acceptance_test.go` has five acceptance tests, `TestAcceptance1_` through `TestAcceptance5_`.
- Run the current suite with `go test -count=1 ./...`, and check concurrency with `go test -race -count=1 ./...`. Test counts change as regression cases are added.

---

## Choosing a workflow

| Need | Command | Behavior |
|---|---|---|
| Inspect TCP listeners | `killport --check --json` | Read-only snapshot with the classic output format |
| Preview immediate cleanup | `killport 3000 --dry-run --json` | Lists planned actions without executing them |
| Free TCP ports immediately | `killport 3000 3001 --json` | Acts on the current owners and verifies the result |
| Inspect TCP or UDP with visibility details | `killport inspect 3000 --json` | Reports owners, provider errors, and visibility |
| Review actions before applying them | `killport plan 3000 -o plan.json` | Saves an expiring plan bound to process identities or full container IDs |
| Apply a reviewed plan | `killport apply plan.json --json` | Refreshes ownership, revalidates identities, and reports endpoint outcomes |

## Machine-readable output

JSON avoids parsing terminal tables and provides explicit fields for scripts and coding agents. Warnings and verbose diagnostics go to stderr. Output size depends on the number of owners and errors; no token reduction or performance benchmark is claimed.

---

## 🛠 Development

In an authorized source checkout, use the whole package (the implementation spans multiple Go files):

```bash
go run . --help
go test -count=1 ./...
```

---

## 📄 License

Licensed under the Killport Proprietary License. Source cloning is restricted; please use official releases for installation.
