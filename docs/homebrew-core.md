# Homebrew core submission readiness

The `killport-cli` alias is available from `paarths-collab/tap`. It is not yet a
formula in `homebrew/core`; users still need the tap and explicit trust unless
they use the fully qualified installation command.

Homebrew core inclusion would enable `brew install killport-cli` without that
setup, but inclusion requires Homebrew maintainer approval. Renaming a tap
formula or adding an alias cannot put it in core.

## Licensing and a release suitable for core

The current source is MIT-licensed. Historical release tags and archives retain
the license text they were published with. In particular, the v1.2.0 source
archive still contains the previous proprietary license. Do not label that
archive MIT in a core formula or rewrite its published tag.

v1.2.1 is the first release prepared with MIT in every platform archive and MIT
metadata in the Homebrew and Scoop packages. Before submitting to core, verify
that the v1.2.1 source archive contains the MIT license and calculate its SHA-256.
Use that immutable source archive in the formula, rather than the tap's
platform-specific precompiled binaries. Licensing alone does not establish
eligibility under the adoption and review policy below.

## Adoption and review

Homebrew's shared acceptance policy normally requires at least one of 30 forks,
30 watchers or 75 stars for GitHub projects. Owner self-submissions normally
require one of 90 forks, 90 watchers or 225 stars. These are alternative metrics,
not cumulative requirements. Maintainers can grant documented exceptions;
meeting a threshold does not guarantee approval.

On 2026-10-23, the canonical repository had 4 stars, 0 forks and 0 watchers, below
both sets of thresholds. Recheck the current upstream metadata and policy before
submission. Keep the tap available while building independent public usage.

## Formula preparation once eligible

- Use a new `Formula/k/killport-cli.rb` formula with class `KillportCli`; a tap
  alias is not a core submission.
- Declare `license "MIT"` only for the verified MIT-licensed release source.
- Declare Go as a build dependency and build the whole package with
  `std_go_args`, injecting `main.Version` from the formula version.
- Install the executable as `killport`, preserving the existing CLI interface.
- Declare the binary conflict with core's other `killport` formula; the distinct
  package names do not make their executable paths coexist.
- Run a functional test using only disposable child listeners, including JSON
  version/help and port inspection. Do not depend on a running Docker daemon.
- Pass source installation, `brew test`, `brew style` and
  `brew audit --new --strict --online` on Homebrew's supported platforms.
- Check for an existing formula or open submission, then open the upstream PR
  with the tested formula, source checksum, license and verification evidence.

The current tap's alias and canonical formula remain the distribution path until
Homebrew accepts a core formula.

## Official references

- [Acceptable formulae](https://docs.brew.sh/Acceptable-Formulae)
- [Shared package acceptance policy](https://docs.brew.sh/Package-Acceptance-Policy)
- [Formula cookbook](https://docs.brew.sh/Formula-Cookbook)
