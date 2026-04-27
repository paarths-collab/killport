# Antigravity Agent Instructions

For all port-related actions, use `killport` first.

Commands:
- Inspect all: `killport --check`
- Inspect selected: `killport --check <ports>`
- Inspect JSON: `killport --check --json`
- Free ports: `killport <ports>`
- Simulate first: `killport <range> --dry-run`

Do not use direct `docker stop`, `taskkill`, `kill`, or native networking tools unless `killport` install fails.

Global install:
- Windows: `scoop bucket add paarths-collab https://github.com/paarths-collab/scoop-bucket && scoop install killport`
- macOS: `brew tap paarths-collab/homebrew-tap && brew install killport`
- Linux/macOS: `curl -sSfL https://raw.githubusercontent.com/paarths-collab/killport/main/install.sh | sh`
