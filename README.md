# ⚡ killport

> [!IMPORTANT]
> **Official Distribution:** This repository is for viewing and auditing only. Please **DO NOT CLONE** this repository for usage. To install **killport**, please use the official binaries or package managers (Brew/Scoop) provided below to ensure you have the latest optimized version.

[![Go Report Card](https://goreportcard.com/badge/github.com/paarths-collab/killport)](https://goreportcard.com/report/github.com/paarths-collab/killport)
[![GitHub Release](https://img.shields.io/github/v/release/paarths-collab/killport)](https://github.com/paarths-collab/killport/releases)
[![License: Proprietary](https://img.shields.io/badge/License-Proprietary-red.svg)](LICENSE)
[![Downloads](https://img.shields.io/badge/Downloads-10k-blue)](https://github.com/paarths-collab/killport/releases)

**The last port killer you'll ever install.** Lightning-fast, concurrent, Docker-native, and optimized for both humans and AI Agents.

---

## 🚀 Why killport?

Most port killers simply find a PID and send a `kill -9`. This works for simple scripts but fails in modern dev environments. **killport** is engineered for the "Unholy Trinity" of modern workflows:

- 🐳 **Docker Native:** Safely triggers `docker stop` on containers instead of murdering the `docker-proxy` and leaving zombie containers behind.
- 🤖 **AI-Agent Optimized:** Machine-readable `--json` output reduces LLM token usage by 90% and eliminates parsing hallucinations.
- 🎯 **Port Ranges:** Concurrent execution allows you to wipe out entire microservice stacks in one command (e.g., `3000-3010`).

---

## 📦 Installation

### macOS (Homebrew)
```bash
brew tap paarths-collab/homebrew-tap
brew install killport
```

### Windows (Scoop)
```powershell
scoop bucket add paarths-collab https://github.com/paarths-collab/scoop-bucket
scoop install killport
```

### Windows install options

| Option | Type | Notes |
|---|---|---|
| Winget | CLI | Official and fast |
| Chocolatey | CLI | Massive package repository |
| Microsoft Store | GUI | Secure and simple |
| Direct Download | Web | Official source for Windows download |

Direct download:
- https://github.com/paarths-collab/killport/releases/latest

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

### Version requirement for `--check`

`--check` exists in source builds newer than v1.1.0. The published v1.1.0 packages (Homebrew, Scoop, install script) do not have it; they exit non-zero with an unknown-flag error. Run `killport --version` to see the installed version. Source builds without release version information report `dev`.

---

## 📋 Behaviour

- **Port specs:** `N` or `A-B`, decimal digits only, 1–65535, with `A ≤ B`. Any invalid spec rejects the whole command with exit 2; nothing is inspected or killed.
- **What counts as a port user:** only TCP sockets in the `LISTEN` state, matched by their exact local port. Client connections, established connections and UDP sockets are ignored.
- **Tools:** macOS uses `lsof`. Linux uses `lsof`, or `ss` if `lsof` is missing. Windows uses `netstat -ano` and `tasklist`. Docker is optional.
- **Visibility:** without root (Linux/macOS) or Administrator (Windows), sockets owned by other users may be invisible. A listener whose owner is not visible is reported with no PID and is never killed. On Linux, when both tools exist, `ss` is also consulted for ports `lsof` did not show, so such listeners are reported as in use instead of free. If `lsof` exits 1 with no output but prints a message on stderr, killport warns instead of assuming there are no listeners.
- **Windows process names:** if `tasklist` fails, names are unknown, Docker's own processes cannot be recognised, and killport warns and refuses to kill any PID it cannot name.
- **Docker:** published host ports (TCP, including ranges and IPv4/IPv6 bindings) are matched to containers. Each container is stopped once with `docker stop -t 10`. Docker's own processes (`docker-proxy`, Docker Desktop backend, `vpnkit`, `containerd`, and similar) are never killed directly. If `docker ps` fails, killport prints a warning to stderr and continues without Docker owners.
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

`code` is one of `usage` (exit 2), `missing_dependency` (exit 1) or `enumeration_failed` (exit 1).

## 🚦 Exit codes

| Code | Meaning |
|---|---|
| `0` | Success. Kill: every requested port is free at the end (or was already free). Dry run: no failed rows. Check: inspection succeeded, whether ports are free or in use |
| `1` | Runtime failure: any row has `status: "failed"`, a required tool is missing, or listing listeners failed |
| `2` | Usage error: unknown flag, invalid port spec, or no ports without `--check` |

---

## 📊 Comparison

| Feature         | kill-port (NPM) | killport (Rust) | killport (Go) |
|----------------|----------------|-----------------|---------------|
| Runtime        | Node (Slow)    | Compiled (Fast) | Compiled (Fast) |
| Binary Size    | N/A (Node req) | ~2MB            | ~2MB (Zero Deps) |
| Port Ranges    | ❌ No          | ❌ No           | ✅ Yes (3000-3005) |
| AI-Agent Mode  | ❌ No          | ❌ No           | ✅ Yes (--json) |
| Docker Stop    | ❌ No          | ✅ Yes          | ✅ Yes |


---

## 🤖 LLM Token Optimization

If you are using AI Agents (Devin, OpenDevin, Cursor), standard CLI tools waste 500–1,500 tokens per attempt due to unstructured output and ASCII tables.

By using `killport --json`, agents can parse the system state in < 50 tokens, drastically reducing costs and preventing execution errors.

---

## 🛠 Development

```bash
git clone https://github.com/paarths-collab/killport.git
cd killport
go run main.go --help
```

---

## 📄 License

Licensed under the Killport Proprietary License. Source cloning is restricted; please use official releases for installation.
