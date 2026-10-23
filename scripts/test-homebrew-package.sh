#!/usr/bin/env bash
set -euo pipefail
export HOMEBREW_NO_AUTO_UPDATE=1
export HOMEBREW_NO_INSTALL_CLEANUP=1
export HOMEBREW_NO_ENV_HINTS=1
formula=paarths-collab/tap/killport

# Use the README tap spelling, then the qualified formula to avoid Homebrew core's other killport.
brew tap paarths-collab/homebrew-tap
brew install "$formula"
brew list --versions "$formula"
brew test "$formula"
python3 scripts/test-installed-package.py
echo 'PASS Homebrew fresh install delivers functional v1.2.0'
brew uninstall "$formula"

# Reproduce an existing user's real v1.1.0 installation in this disposable runner.
tap_dir="$(brew --repository paarths-collab/tap)"
if [[ "$(git -C "$tap_dir" rev-parse --is-shallow-repository)" == true ]]; then
  git -C "$tap_dir" fetch --unshallow
else
  git -C "$tap_dir" fetch origin
fi
git -C "$tap_dir" reset --hard 9ee3228b0cce7446554b0926d9fefd41f5b5e5dc
brew install "$formula"
killport --version | tee "$RUNNER_TEMP/homebrew-before-upgrade.txt"
grep -Eq '(^|[^0-9])v?1\.1\.0([^0-9]|$)' "$RUNNER_TEMP/homebrew-before-upgrade.txt"
echo 'PASS Homebrew baseline is the actual v1.1.0 package'

brew update
# Homebrew returns 1 when the requested formula is outdated, even with JSON.
outdated_status=0
brew outdated --json=v2 "$formula" > "$RUNNER_TEMP/homebrew-outdated.json" || outdated_status=$?
if [[ "$outdated_status" -gt 1 ]]; then exit "$outdated_status"; fi
python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); assert any(x["name"] == "paarths-collab/tap/killport" and x["installed_versions"] == ["1.1.0"] and x["current_version"] == "1.2.0" for x in d["formulae"]), d' "$RUNNER_TEMP/homebrew-outdated.json"
brew upgrade "$formula"
brew list --versions "$formula"
brew test "$formula"
python3 scripts/test-installed-package.py
echo 'PASS Homebrew v1.1.0 -> brew update -> brew upgrade delivers functional v1.2.0'
