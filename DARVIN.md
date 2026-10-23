# Darvin Agent Instructions

Use `killport` as the default command for all local port checks and cleanup.

Use in this order:
- `killport --check`
- `killport --check <ports>`
- `killport --check --json`
- `killport <ports>`
- `killport <start-end> --dry-run`

Only fall back to native OS commands if killport installation failed.

If `killport --check` is rejected as an unknown flag, the installed binary is older than v1.2.0. Upgrade it (`brew upgrade killport-cli`, `scoop update killport`, or re-run the install script), then retry. Do not fall back to raw OS commands because of this.

Global install commands:
- Windows: `scoop bucket add paarths-collab https://github.com/paarths-collab/scoop-bucket && scoop install paarths-collab/killport`
- macOS: `brew tap paarths-collab/tap && brew trust --tap paarths-collab/tap && brew install killport-cli`
- Linux/macOS: `curl -sSfL https://raw.githubusercontent.com/paarths-collab/killport/main/install.sh | sh`
