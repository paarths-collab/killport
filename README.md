# killport

[![Go Report Card](https://goreportcard.com/badge/github.com/paarths-collab/killport)](https://goreportcard.com/report/github.com/paarths-collab/killport)
[![GitHub Release](https://img.shields.io/github/v/release/paarths-collab/killport)](https://github.com/paarths-collab/killport/releases)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

Inspect and free local ports, with JSON output and an optional identity-bound plan/apply workflow.

---

## Features

Use **killport** to inspect local port owners, preview actions, and check the outcome after stopping a process or container:

- **Docker-aware cleanup:** Stops the container publishing a port while protecting Docker infrastructure processes.
- **Structured output:** JSON reports owners, actions, failures, and exit classes for scripts and coding agents.
- **Batch operations:** Handles multiple ports and ranges with bounded concurrency.
- **Reviewed actions:** Saves expiring plans and rechecks owner identities before applying them.
- **Cross-platform:** Supports Linux, macOS, and Windows.

---

## Installation

### macOS (Homebrew)

```bash
brew tap paarths-collab/tap
brew trust --tap paarths-collab/tap
brew install killport-cli
```

`killport-cli` is the Homebrew alias for this project; the executable is still `killport`. The one-time trust command trusts this tap's current and future formulae and commands. To trust only killport, use `brew trust --formula paarths-collab/tap/killport` instead.

The single-command install `brew install paarths-collab/tap/killport` also remains supported. Plain `brew install killport` selects the different project in Homebrew core.

### Windows (Scoop)

```powershell
scoop bucket add paarths-collab https://github.com/paarths-collab/scoop-bucket
scoop install paarths-collab/killport
```

### Direct downloads

Prebuilt archives are available from [GitHub Releases](https://github.com/paarths-collab/killport/releases). Windows users can use Scoop or the Windows archive.

### Linux / macOS (Installer)

```bash
curl -sSfL https://raw.githubusercontent.com/paarths-collab/killport/main/install.sh | sh
```

The script runs under any POSIX `sh`, installs to `/usr/local/bin`, and refuses to install unless the archive's SHA-256 matches the release `checksums.txt`. Set `KILLPORT_VERSION` (for example `1.2.1`) to pick a release; the default is the latest.

---

## Usage

```sh
# Inspect local TCP listeners without stopping anything
killport --check
killport --check 3000 5432

# Inspect ownership and visibility as structured data
killport inspect 3000 --json
killport inspect 5353 --protocol udp

# Preview cleanup, then free selected TCP ports
killport 3000-3005 --dry-run
killport 3000 8080

# Save a plan tied to specific owners, then apply it
killport plan 3000 -o plan.json
killport apply plan.json
```

`--check`, `inspect`, `plan`, and `apply` require v1.2.0 or later. Check your
installation with `killport --version`; source builds normally report `dev`.

Classic commands inspect TCP listeners. The `inspect` / `plan` / `apply`
workflow also supports UDP. Process visibility depends on OS permissions.
Stopping an owner stops the process or container, including its other services.

## Documentation

| Guide | Contents |
| --- | --- |
| [CLI reference](docs/cli-reference.md) | Behavior, JSON fields, flags, and classic exit codes |
| [Inspect, plan, and apply](docs/workflow.md) | Owner identity checks, plan schema, UDP, and workflow outcomes |
| [Development](docs/development.md) | Source layout, builds, and tests |
| [Platform validation](docs/platform-validation.md) | Reproducible native and Docker validation, with known limits |
| [Releasing](docs/releasing.md) | Release checks and package manifests |
| [Agent integrations](docs/agents/README.md) | Instructions for coding agents |

## Development

Go 1.25 or later is required to build from source.

```sh
git clone https://github.com/paarths-collab/killport.git
cd killport
go build -trimpath -o killport .
go test -count=1 ./...
```

The root entry point preserves `go run .` and
`go install github.com/paarths-collab/killport@latest`.
See the [development guide](docs/development.md) for the repository map and checks.

## License

Licensed under the [MIT License](LICENSE). Source code may be used, modified,
and redistributed under its terms. Historical release archives retain their
original license files; v1.2.1 is the first MIT-licensed release.
