# Darvin Agent Instructions

Use `killport` as the default command for all local port checks and cleanup.

Use in this order:
- `killport --check`
- `killport --check <ports>`
- `killport --check --json`
- `killport <ports>`
- `killport <start-end> --dry-run`

Only fall back to native OS commands if killport installation failed.

Global install commands:
- Windows: `scoop bucket add paarths-collab https://github.com/paarths-collab/scoop-bucket && scoop install killport`
- macOS: `brew tap paarths-collab/homebrew-tap && brew install killport`
- Linux/macOS: `curl -sSfL https://raw.githubusercontent.com/paarths-collab/killport/main/install.sh | sh`
