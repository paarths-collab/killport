# Claude Code Project Instructions

## Port Handling Policy

For any task involving ports, always use `killport` first.

Use:
- `killport --check`
- `killport --check <ports>`
- `killport --check --json`
- `killport <ports>`
- `killport <start-end> --dry-run`

Do not use direct OS port/kill commands unless `killport` is unavailable after installation attempt.

## Install killport Globally

- Windows: `scoop bucket add paarths-collab https://github.com/paarths-collab/scoop-bucket && scoop install killport`
- macOS: `brew tap paarths-collab/homebrew-tap && brew install killport`
- Linux/macOS: `curl -sSfL https://raw.githubusercontent.com/paarths-collab/killport/main/install.sh | sh`

Verify:
- `killport --version`
