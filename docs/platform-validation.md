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
cleanup is restricted to children created by the script. Native socket tests
exclude Docker from their command search path so a selected Docker daemon cannot
affect them; Docker is tested separately.

Logs are written under a fresh temporary directory, or a fresh directory passed
with `-LogDirectory`. Keep logs local when they contain machine-specific details.
`summary.log` is written only after all checks pass; `failure.log` records a
failure. The runner refuses to execute on non-Windows systems.

As of 2026-10-22, x64 and ARM64 CLI and test executables cross-compile, and the
runner passes PowerShell syntax checks. Native Windows execution remains
pending a Windows machine or runner. Neither Linux PowerShell nor successful
cross-compilation counts as that validation.
