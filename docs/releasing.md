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

Package repository writes are deliberately separate from GoReleaser: a
repository-scoped Actions token cannot update the other repositories, and
automatic commits would not preserve the required local commit identity. The
release workflow therefore needs no cross-repository publishing token.

After updating the package repositories, fetch the public formula and manifest
again, confirm their version/URLs/hashes, and check the native release binary's
`--version` and JSON help. Existing users can then run `brew update` followed by
`brew upgrade paarths-collab/tap/killport`, or `scoop update` followed by
`scoop update killport`.
