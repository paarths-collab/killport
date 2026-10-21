# Socket-table fixtures

Inputs for the socket-table and UDP owner parsers in `sockets.go`. Each file is
either a **REAL** capture from a Linux container or **SYNTHETIC** (hand-written in
the real tool format with generic values).

## Real captures (Linux)

Captured 2026-10-21 in one throwaway `golang:1.25` container (`docker run --rm`,
no ports published, host untouched) with `lsof`, `iproute2`, `netcat-openbsd`,
`python3`, `procps` installed. The container ran as root. `/proc/net` rows contain
uids (0) and inode numbers; that is expected. The `167.82.58.132:80` peer in the
TIME_WAIT rows is the package mirror contacted by the install step.

Sockets started in the container:

| PID | Process | Role |
|---|---|---|
| 277 | python3 | `http.server 3000 --bind 0.0.0.0` (TCP listener 3000, also holds the server side of the client connection) |
| 278 | python3 | `http.server 8080 --bind ::1` (TCP listener `[::1]:8080`) |
| 279 | nc | `nc -lk 127.0.0.1 5432` (TCP listener 5432) |
| 280 | nc | `nc -u -l 127.0.0.1 5353`; one datagram was sent to it, so the kernel connected it to the sender: local `127.0.0.1:5353`, remote an ephemeral port. A connected UDP socket is bound but not "unconnected", so `ss -lunp` does NOT list it while `lsof` and `/proc/net/udp` do. |
| 281 | python3 | UDP sockets bound and unconnected: `0.0.0.0:9999` (IPv4) and `[::]:5356` (IPv6) |
| 286 | sleep | `sleep 600`, stdin for the client |
| 287 | nc | client `127.0.0.1:<ephemeral> -> 127.0.0.1:3000` (remote port 3000, must never be selected as a port-3000 owner) |

PIDs of the processes are as printed by `ps` in the same capture run.

| File | Command | Type |
|---|---|---|
| `linux_proc_net_tcp.txt` | `cat /proc/net/tcp` (2 LISTEN rows on 3000 and 5432; 2 ESTABLISHED rows between ephemeral port and 3000; 2 TIME_WAIT rows) | REAL |
| `linux_proc_net_tcp6.txt` | `cat /proc/net/tcp6` (LISTEN on `::1` port 8080, `00000000000000000000000001000000:1F90`) | REAL |
| `linux_proc_net_udp.txt` | `cat /proc/net/udp` (connected 5353, bound 9999) | REAL |
| `linux_proc_net_udp6.txt` | `cat /proc/net/udp6` (bound `::` port 5356) | REAL |
| `linux_lsof_udp.txt` | `lsof -nP +c 0 -iUDP -FpcnT` (pid 280 connected `127.0.0.1:5353->127.0.0.1:<port>`; pid 281 `*:9999`, `*:5356`) | REAL |
| `linux_ss_lunp.txt` | `ss -lunp` (only the unconnected sockets of pid 281) | REAL |
| `linux_ss_ltnp.txt` | `ss -ltnp` (3000, 5432, `[::1]:8080`) | REAL |

## Synthetic macOS captures

macOS output cannot be captured from this environment, so these follow the real
`netstat` column layout. Addresses are documentation ranges (192.0.2.0/24,
198.51.100.0/24), loopback and link-local.

| File | Command it imitates | Type |
|---|---|---|
| `darwin_netstat_tcp.txt` | `netstat -an -p tcp` (LISTEN on `*.3000`, `127.0.0.1.5432`, `::1.8080`, `fe80::1%lo0.5000`, `*.13000` as tcp46, `*.30001`; ESTABLISHED/TIME_WAIT/CLOSE_WAIT rows including remote port 3000) | SYNTHETIC |
| `darwin_netstat_udp.txt` | `netstat -an -p udp` (no state column; `*.5353` on udp4 and udp6, a connected `127.0.0.1.5000` with remote port 3000, `*.9999`, `fe80::1%lo0.123`, and an unbound `*.*`) | SYNTHETIC |
