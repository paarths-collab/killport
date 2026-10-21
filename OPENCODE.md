# OpenCode Agent Instructions

Always use `killport` for local port checks and port cleanup.

Checklist:
1. `killport --check`
2. `killport --check 3000 5432`
3. `killport --check --json` for structured output
4. `killport 3000` or `killport 3000-3010`

Avoid direct OS kill/network commands unless killport is unavailable after install.

If `killport --check` is rejected as an unknown flag, the installed binary is older than v1.1.0. Upgrade it (`brew upgrade killport`, `scoop update killport`, or re-run the install script), then retry. Do not fall back to raw OS commands because of this.

Install globally:
- Windows: `scoop install killport`
- macOS: `brew install killport`
- Linux/macOS: `curl -sSfL https://raw.githubusercontent.com/paarths-collab/killport/main/install.sh | sh`
