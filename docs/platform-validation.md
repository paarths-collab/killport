# Native platform validation

Unit tests and cross-compilation do not establish native operating-system or
container-runtime behavior. The scripts below exercise built executables using
disposable resources. They do not publish releases or change Git history.

## Docker

Build the CLI for the machine running the script, then run:

```sh
go build -o killport .
python3 scripts/test-docker.py --binary ./killport
```

The local Docker daemon must be running, and `python:3.12-slim` must already be
cached. The script never pulls an image. An alternative cached image containing
Python can be selected with `--image`.

The script accepts only a local Docker endpoint. It publishes automatically
allocated loopback ports, records the full IDs of its own containers, and checks
each container's unique run label before removing it. It mounts no host files or
Docker socket into those containers. Existing containers are never targets.

Eight checks cover:

1. Read-only check, inspect and dry-run preserve the running container.
2. Remote Docker endpoints cannot supply local port owners.
3. An unavailable endpoint produces uncertainty and zero actions.
4. One container publishing two TCP ports receives one graceful stop; both
   endpoints are verified free and the container exits with code 0.
5. UDP publications require the explicit UDP workflow and can be stopped.
6. A container ignoring SIGTERM is stopped after Docker's grace period; its exit
   code is 137 and the outcome is verified.
7. Replacing a container after planning produces `needs_fresh_plan`, executes
   zero actions, and preserves the replacement.
8. The classic command stops the container owning the requested test port.

These eight cases passed on 2026-10-22 using a native macOS ARM64 CLI against
Docker Desktop's Linux ARM64 daemon. This does not establish native Windows
Docker behavior or every alternative container runtime.

## Windows

Run on actual Windows using Windows PowerShell 5.1 or later. Prebuilt test
executables do not require Go on the test machine. Build the CLI and Go test
binary for the target Windows architecture; place `testdata/` alongside the
test binary, then run:

```powershell
.\scripts\test-windows.ps1 -Binary .\killport.exe -TestBinary .\killport.test.exe
```

The script runs the full Go suite and requires the real process-handle identity
and termination regression to pass. It then starts its own loopback TCP and UDP
listeners, verifies that plans name only those child PIDs, and exercises
plan/apply, read-only previews, owner changes, and classic termination. Process
cleanup is restricted to children created by the script. Native inspect and plan
checks explicitly select `--mode native`, so a Docker CLI installed in a Windows
system directory cannot make these socket checks depend on a stopped daemon.
The command search path is also restricted; Docker is tested separately.

Logs are written under a fresh temporary directory, or a fresh directory passed
with `-LogDirectory`. Keep logs local when they contain machine-specific details.
`summary.log` is written only after all checks pass; `failure.log` records a
failure. The runner refuses to execute on non-Windows systems.

On 2026-10-23, native Windows x64 validation passed on GitHub Actions in
[run 39965983069](https://github.com/paarths-collab/killport/actions/runs/39965983069),
for source commit `513781a079bed2ed97eb9f008753da1875116f63`. The same run passed
Linux and macOS. Its `windows-validation` artifact contains the full Go-suite
log and a summary confirming all six Windows validation groups:

1. The full Go suite, including real process-handle identity and termination.
2. TCP inspect and plan/apply, with the owned process terminated and its endpoint
   verified free.
3. The equivalent UDP workflow.
4. Classic check and dry-run preserve the owned listener.
5. Owner drift executes zero actions and preserves both processes.
6. Classic termination stops only the owned replacement process.

The first hosted Windows run exposed automatic LF-to-CRLF checkout conversion
of a captured Linux fixture. `.gitattributes` now preserves every `testdata/`
fixture byte-for-byte; the complete Windows suite and smoke checks passed after
that correction. Deliberate CRLF and CRCRLF parser inputs remain unchanged.

Windows ARM64 CLI and test executables cross-compile, but native ARM64 Windows
execution and Windows Docker behavior remain unverified. The hosted validation
used PowerShell 7; the script's Windows PowerShell 5.1 compatibility has not been
runtime-tested. Neither Linux PowerShell nor cross-compilation counts as native
Windows validation.
