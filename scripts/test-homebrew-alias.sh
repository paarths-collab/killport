#!/usr/bin/env bash
set -euo pipefail
export HOMEBREW_NO_AUTO_UPDATE=1
export HOMEBREW_NO_INSTALL_CLEANUP=1
export HOMEBREW_NO_ENV_HINTS=1
export HOMEBREW_NO_ANALYTICS=1

# Disposable Intel images ship an old Azure CLI formula that brew update
# migrates to a cask, compiling unrelated dependencies before it returns.
if [[ "$RUNNER_OS" == macOS && "$(uname -m)" == x86_64 && -d "$(brew --cellar)/azure-cli" ]]; then
  brew uninstall --formula --force --ignore-dependencies azure-cli
fi

brew tap paarths-collab/tap
brew trust --tap paarths-collab/tap
brew info --json=v2 killport-cli > "$RUNNER_TEMP/killport-alias-info.json"
python3 - "$RUNNER_TEMP/killport-alias-info.json" <<'PY'
import json, sys
formula = json.load(open(sys.argv[1]))['formulae'][0]
assert formula['full_name'] == 'paarths-collab/tap/killport', formula
assert formula['homepage'] == 'https://github.com/paarths-collab/killport', formula
assert 'killport-cli' in formula['aliases'], formula
assert formula['versions']['stable'] == '1.2.1', formula
assert formula['license'] == 'MIT', formula
print('PASS short alias resolves this project and canonical v1.2.1 formula')
PY
brew install killport-cli
python3 scripts/test-installed-package.py
echo 'PASS fresh brew install killport-cli delivers a functional v1.2.1 killport executable'
brew uninstall killport-cli

# Also verify the narrower, formula-only trust option in the documentation.
brew untrust --tap paarths-collab/tap
brew trust --formula paarths-collab/tap/killport
brew info killport-cli
tap_dir="$(brew --repository paarths-collab/tap)"
if [[ "$(git -C "$tap_dir" rev-parse --is-shallow-repository)" == true ]]; then
  git -C "$tap_dir" fetch --unshallow
fi
git -C "$tap_dir" reset --hard f835620bb912a448feb80c5d2331d24d8e8db553
brew install paarths-collab/tap/killport
killport --version | tee "$RUNNER_TEMP/killport-alias-baseline.txt"
grep -Eq '(^|[^0-9])v?1\.2\.0([^0-9]|$)' "$RUNNER_TEMP/killport-alias-baseline.txt"
echo 'PASS real v1.2.0 installation is present'
brew update
test -L "$tap_dir/Aliases/killport-cli"
brew upgrade killport-cli
python3 scripts/test-installed-package.py
echo 'PASS brew update and brew upgrade killport-cli upgrade the v1.2.0 canonical package to functional v1.2.1'
