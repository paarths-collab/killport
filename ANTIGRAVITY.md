# Antigravity Agent Instructions

For all port-related actions, use `killport` first.

Commands:
- Inspect all: `killport --check`
- Inspect selected: `killport --check <ports>`
- Inspect JSON: `killport --check --json`
- Free ports: `killport <ports>`
- Simulate first: `killport <range> --dry-run`

Do not use direct `docker stop`, `taskkill`, `kill`, or native networking tools unless `killport` install fails.

If `killport --check` is rejected as an unknown flag, the installed binary is older than v1.2.0. Upgrade it (`brew upgrade killport-cli`, `scoop update killport`, or re-run the install script), then retry. Do not fall back to raw OS commands because of this.

Global install:
- Windows: `scoop bucket add paarths-collab https://github.com/paarths-collab/scoop-bucket && scoop install paarths-collab/killport`
- macOS: `brew tap paarths-collab/tap && brew trust --tap paarths-collab/tap && brew install killport-cli`
- Linux/macOS: `curl -sSfL https://raw.githubusercontent.com/paarths-collab/killport/main/install.sh | sh`
