#!/usr/bin/env bash
set -euo pipefail
export HOMEBREW_NO_AUTO_UPDATE=1
export HOMEBREW_NO_INSTALL_CLEANUP=1
export HOMEBREW_NO_ENV_HINTS=1
export HOMEBREW_NO_ANALYTICS=1

brew tap paarths-collab/tap
brew trust --tap paarths-collab/tap
brew info --json=v2 killport-cli > "$RUNNER_TEMP/killport-alias-info.json"
python3 - "$RUNNER_TEMP/killport-alias-info.json" <<'PY'
import json, sys
formula = json.load(open(sys.argv[1]))['formulae'][0]
assert formula['full_name'] == 'paarths-collab/tap/killport', formula
assert formula['homepage'] == 'https://github.com/paarths-collab/killport', formula
assert 'killport-cli' in formula['aliases'], formula
assert formula['versions']['stable'] == '1.2.0', formula
print('PASS short alias resolves this project and canonical v1.2.0 formula')
PY
brew install killport-cli
python3 scripts/test-installed-package.py
echo 'PASS fresh brew install killport-cli delivers a functional v1.2.0 killport executable'
brew uninstall killport-cli

# Also verify the narrower, formula-only trust option in the documentation.
brew untrust --tap paarths-collab/tap
brew trust --formula paarths-collab/tap/killport
brew info killport-cli
tap_dir="$(brew --repository paarths-collab/tap)"
if [[ "$(git -C "$tap_dir" rev-parse --is-shallow-repository)" == true ]]; then
  git -C "$tap_dir" fetch --unshallow
fi
git -C "$tap_dir" reset --hard 9ee3228b0cce7446554b0926d9fefd41f5b5e5dc
brew install paarths-collab/tap/killport
killport --version | tee "$RUNNER_TEMP/killport-alias-baseline.txt"
grep -Eq '(^|[^0-9])v?1\.1\.0([^0-9]|$)' "$RUNNER_TEMP/killport-alias-baseline.txt"
echo 'PASS real pre-alias v1.1.0 installation is present'
brew update
test -L "$tap_dir/Aliases/killport-cli"
brew upgrade killport-cli
python3 scripts/test-installed-package.py
echo 'PASS brew update and brew upgrade killport-cli upgrade the old canonical package to functional v1.2.0'
