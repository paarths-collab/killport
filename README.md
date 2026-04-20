# killport

[![Go Report Card](https://goreportcard.com/badge/github.com/paarths-collab/killport)](https://goreportcard.com/report/github.com/paarths-collab/killport)
[![GitHub Release](https://img.shields.io/github/v/release/paarths-collab/killport)](https://github.com/paarths-collab/killport/releases)
[![License](https://img.shields.io/github/license/paarths-collab/killport)](https://github.com/paarths-collab/killport/blob/main/LICENSE)
[![Downloads](https://img.shields.io/github/downloads/paarths-collab/killport/total)](https://github.com/paarths-collab/killport/releases)

Lightning-fast, AI-agent optimized, and Docker-aware port killer.

killport is built for one job: free your blocked development ports quickly, with clean CLI output for humans and strict JSON for agents.

## Why killport

- Port ranges: kill multiple ports in one command, e.g. `killport 3000-3005`
- AI-agent mode: strict JSON output with `--json`
- Docker-aware: stops matching containers before OS-level force kill
- Cross-platform: Linux, macOS, and Windows

## Install

### Homebrew (macOS)

```bash
brew tap paarths-collab/homebrew-tap
brew install killport
```

### Scoop (Windows)

```powershell
scoop bucket add paarths-collab https://github.com/paarths-collab/scoop-bucket
scoop install killport
```

### Linux/macOS one-liner

```bash
curl -sSfL https://raw.githubusercontent.com/paarths-collab/killport/main/install.sh | sh
```

### Manual download

See release archives at:
https://github.com/paarths-collab/killport/releases

## Usage

```bash
# Kill one port
killport 3000

# Kill multiple ports
killport 3000 8080 5432

# Kill a range
killport 3000-3005

# Dry run (no processes are terminated)
killport 3000-3002 --dry-run

# JSON mode for AI tools
killport 5432 --json
```

## JSON output example

```json
[
  {
    "port": "5432",
    "process_name": "postgres",
    "pid": "12345",
    "killed": true,
    "is_docker": false
  }
]
```

## Comparison

| Feature | kill-port (NPM) | killport (Rust) | killport (Go) |
|---|---|---|---|
| Native speed | No (Node runtime) | Yes | Yes |
| AI-agent JSON mode | No | No | Yes (`--json`) |
| Docker-aware stop path | No | Yes | Yes |
| Port range syntax | No | No | Yes (`3000-3005`) |

## Go registry visibility

Module path: `github.com/paarths-collab/killport`

After tagging a release (for example `v1.0.2`), you can trigger proxy indexing:

```bash
curl https://proxy.golang.org/github.com/paarths-collab/killport/@v/v1.0.2.info
```

Then check package visibility on:
https://pkg.go.dev/github.com/paarths-collab/killport

## Development

```bash
go run main.go 3000
```

## License

MIT (add a LICENSE file if not present).
