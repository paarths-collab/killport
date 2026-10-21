# Linux end-to-end evidence for killport

- Date: 2026-10-21
- Commit: c4255b5 (`git rev-parse --short HEAD` at run time; the tree was clean at that commit)
- Image: `golang:1.25`, digest `sha256:699337d620559a59b4a2bb298ad59611e535d2ee755a34cf2d2a98f37578dc80`. The container reports `go1.25.14 linux/arm64`.
- Host: Docker Desktop on Apple silicon, so the container is aarch64. `--memory 1g`, hostname `killport-demo`, run as root.

## Files

- `linux-run.txt`: raw transcript of the single `docker run`.
- `plan.json`, `apply.json`, `apply-drift.json`, `inspect.json`, `check.json`, `legacy-kill.json`, `error.json`: the key JSON documents, copied from the transcript run.

## What is real

- A Linux container with real `lsof`, `ss` (iproute2 6.15.0), `/proc`, and the real `python3 -m http.server` and `nc` fixtures.
- The binary is built inside the container from the read-only mount of the repo at c4255b5.
- Liveness is checked with `kill -0` plus `/proc/<pid>/status`. A killed child that has not been reaped shows as `DEAD (zombie)`. No zombies appeared in this run.
- The sequence covers: check, dry-run, inspect, plan (with `-o`), apply, drift (apply after the owner was replaced, exit 3), legacy kill, and error handling.

## What is NOT covered

- The Docker-container branch: no Docker socket is mounted, so `killport` cannot see or stop containers.
- macOS and Windows.
- Root-owned sockets that are invisible to a non-root user: the container runs as root, so this case is not exercised.
- `tool_version` is `dev`: the binary was built without release ldflags.
- The legacy kill output (`killport 3002 --json`) has no verification fields. "Verified" here means the external `kill -0` check, not a field in the output.

## Reproduce

```sh
sh scripts/demo-run.sh
```

Needs Docker. Writes `docs/evidence/linux-run.txt` and the JSON files above. Set `EVIDENCE_DIR` to write them elsewhere.
