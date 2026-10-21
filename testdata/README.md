# killport test fixtures

Fixtures for the output parsers of the `killport` CLI. Each file is either a
**REAL** capture from a Linux container, a real Docker CLI capture, or
**SYNTHETIC** (hand-written in the real tool format). Synthetic files contain only
generic process names and documentation-style ports; no data from the author's machine.

Capture date: 2026-10-21.

## Real captures (Linux, golang:1.25 container)

Container: `killport-fixture-linux`, image `golang:1.25` (image ID `699337d62055`),
run with `docker run --rm` and **no host ports published**. Versions reported inside the
container: `lsof` revision 4.99.4, `ss` iproute2-6.15.0. Extra package `procps` was installed
alongside the requested packages so `ps` is available. The container was wrapped in a
300-second alarm; it exited on its own at the end of the script.

Listeners started in the container: python3 http.server on `0.0.0.0:3000`, on `[::1]:8080`,
and on `0.0.0.0:13000`; `nc -lk 127.0.0.1 5432`; `nc -u -l 5353` (UDP); and one held
client connection `nc 127.0.0.1 3000` (fed by `sleep 600`).

| File | Command | Type |
|---|---|---|
| `linux/lsof_listen.txt` | `lsof -nP +c 0 -iTCP -sTCP:LISTEN -FpcnT` | REAL |
| `linux/lsof_all_tcp.txt` | `lsof -nP +c 0 -iTCP -FpcnT` (includes ESTABLISHED) | REAL |
| `linux/lsof_i_port_3000_t.txt` | `lsof -i :3000 -t` (old, buggy command; includes the nc client) | REAL |
| `linux/ss_ltnp.txt` | `ss -ltnp` | REAL |
| `linux/ss_tanp.txt` | `ss -tanp` | REAL (includes one unrelated TIME-WAIT row from the apt step) |

Note: the `ss` and `lsof` captures are TCP-only. The UDP listener on 5353 does not appear in them.

### PID to role (Linux capture)

| PID | Process (comm) | Role |
|---|---|---|
| 277 | python3 | Server on `0.0.0.0:3000`. Holds the ESTABLISHED accepted socket to 127.0.0.1:38660 (fd 4). |
| 278 | python3 | Server on `[::1]:8080` |
| 279 | python3 | Server on `0.0.0.0:13000` (near-miss port for 3000) |
| 280 | nc | Listener `127.0.0.1:5432` (`nc -lk`) |
| 281 | nc | UDP listener on 5353 (`nc -u -l 5353`). Not visible in the TCP captures. Assigned by launch order only; not directly observed. |
| 283 | sleep | `sleep 600`, the stdin feed for the client pipeline |
| 284 | nc | Client connected to remote port 3000 (`127.0.0.1:38660 -> 127.0.0.1:3000`). Appears in `lsof_i_port_3000_t.txt`. |
| 1 | bash | Container entry script (capture driver) |

## Synthetic fixtures (macOS and Windows)

We cannot capture macOS or Windows output from this machine, so these files are
**SYNTHETIC**. They follow the real tool formats. Process names such as `node`, `postgres`,
`python3`, `devserver`, `gateway`, `curl`, `node.exe`, `chrome.exe` are generic. Values are
not copied from any real host.

| File | Format and command it imitates | Type |
|---|---|---|
| `darwin/lsof_listen.txt` | `lsof -nP +c 0 -iTCP -sTCP:LISTEN -FpcnT` (macOS) | SYNTHETIC |
| `darwin/lsof_all_tcp.txt` | `lsof -nP +c 0 -iTCP -FpcnT` (macOS). Adds ESTABLISHED client (pid 905, curl, remote port 3000) and server-side ESTABLISHED for pid 501 | SYNTHETIC |
| `windows/netstat_ano.txt` | `netstat -ano` (Windows). CRLF line endings; includes localized state `ABHÖREN`, a TIME_WAIT row with PID 0, and UDP rows | SYNTHETIC |
| `windows/tasklist_csv.txt` | `tasklist /FO CSV /NH` (Windows). One row per PID that appears in `netstat_ano.txt`, plus `System` and `System Idle Process` | SYNTHETIC |

Line endings: both `windows/` files use **CRLF line endings** (`\r\n`), as Windows
tools write them. An earlier revision had a doubled `\r\r\n`; that was normalised to plain CRLF.
The parsers must still tolerate `\r\r\n`, so the Go tests cover that case with inline strings
rather than with these files.

## Docker

| File | Command | Type |
|---|---|---|
| `docker/docker_ps.txt` | `docker ps --filter name=killport-fixture-ports --format '{{.ID}}\|{{.Names}}\|{{.Ports}}'` run on a container started with `-p 47801:80 -p 127.0.0.1:47802:81 -p 127.0.0.1:47810-47812:8000-8002 -p 127.0.0.1:47820:53/udp --expose 9000` (golang:1.25, `sleep 300`) | REAL |

The `docker ps` line was captured while the fixture container was running. The container was
then removed with `docker rm -f killport-fixture-ports`, and a follow-up listing confirmed it no
longer exists. The capture is a single line that covers the `0.0.0.0` and `[::]` publish of 47801,
the `127.0.0.1` binds, the port ranges, and the UDP mapping. `--expose 9000` is not shown, because
exposed-only ports do not appear in `{{.Ports}}`.
