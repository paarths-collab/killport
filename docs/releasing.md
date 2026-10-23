# Releasing killport

Releases use a stable `vMAJOR.MINOR.PATCH` tag on a tested commit. GoReleaser
builds Linux, macOS and Windows archives for AMD64 and ARM64, injects the version
into the executable, and writes `checksums.txt`.

The tag workflow creates a **draft** GitHub release. It also runs the packaged
Windows AMD64 executable after verifying the archive checksum. Check the entire
release workflow before publishing the draft. Source-level platform validation
and its limits are described in [platform-validation.md](platform-validation.md).

The workflow can also be dispatched manually with an existing `tag` to rerun
packaged Windows verification without rebuilding or moving that tag. Draft
release downloads require a token with repository write access, even though
this verification job only reads release assets.

## Package definitions

`scripts/package-manifests.py` generates these files from the release checksum
manifest without making any remote writes:

- `Formula/killport.rb`: Homebrew on macOS and Linux, Intel and ARM64.
- `killport.json`: Scoop on Windows AMD64 and ARM64.

The release workflow uploads them as the `package-manifests` Actions artifact.
For a local copy, run:

```sh
python3 scripts/package-manifests.py --version v1.2.0 \
  --checksums checksums.txt --output package-manifests
```

Before publication, download all six release archives and independently verify
their SHA-256 values against `checksums.txt`. Also compare every generated
package URL and hash with the corresponding archive. A checksum detects damaged
or mismatched downloads; it is not a signature or proof of publisher identity.

Publish the verified draft, then copy the generated definitions to
`paarths-collab/homebrew-tap` (`Formula/killport.rb`) and
`paarths-collab/scoop-bucket` (`killport.json`). Create those commits using local
Git, with **both** author and committer set to
`Anonymous <anonymous@example.invalid>`, and verify the metadata before pushing.
Verify that the authenticated publishing account is `paarths-collab` immediately
before each remote write. Do not use server-side file edits or generated package
commits that replace this identity.

Preserve the tap's `Aliases/killport-cli` relative symlink to
`../Formula/killport.rb`. This static alias is deliberately not a second formula:
it shares the canonical package's release checksums and existing installation
history, and does not need a new version when the formula is updated.

Package repository writes are deliberately separate from GoReleaser: a
repository-scoped Actions token cannot update the other repositories, and
automatic commits would not preserve the required local commit identity. The
release workflow therefore needs no cross-repository publishing token.

After updating the package repositories, fetch the public formula and manifest
again and confirm their version/URLs/hashes. On disposable native machines,
exercise both a fresh installation and an upgrade from the previous published
version through each package manager. Check the executable resolved from `PATH`,
its `--version --json`, TCP/UDP inspect and plan/apply, and classic dry-run and
termination against listeners created by the test. Checking a downloaded binary
alone does not establish that installation, linking or upgrading works.

For Homebrew, test the short name `killport-cli` after adding and explicitly
trusting the tap or canonical formula. Current Homebrew requires canonical
formula trust before an alias can load; trusting just an alias name is not
sufficient. The README documents tap-level trust and a narrower per-formula
alternative. Also retain the direct install
`brew install paarths-collab/tap/killport` for users who prefer a single command.
Homebrew core's unqualified `killport` still names a different project.

For Scoop, add the public bucket and use
`scoop install paarths-collab/killport`. Existing users run `brew update` followed
by `brew upgrade killport-cli`, or `scoop update` followed by
`scoop update killport`. Check an upgrade from a canonical pre-alias Homebrew
installation as well as a fresh install through the short alias.
