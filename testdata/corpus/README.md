# Fixture corpus

Table-driven cases for the `killport` CLI. Each directory is one case: `case.json`
plus any small files it needs. `corpus_test.go` loads every case, builds the fake
system from it, calls `runIO` with the case arguments, and asserts every key of
`expect`. Run with:

    go test -count=1 -run Corpus -v ./...

Input status, per case:

- **REAL**: the input is a captured output from `testdata/linux/`,
  `testdata/sockets/` or `testdata/docker/` (see `testdata/README.md` and
  `testdata/sockets/README.md`), referenced by a relative path and not copied.
- **SYNTHETIC**: hand-written in the real tool format, or a Windows or macOS
  input (those are synthetic per the testdata READMEs), or a copy of a real or
  synthetic file with a deliberate change (sed-derived files are noted).
- **MIXED**: some inputs real, some synthetic. The synthetic part is named.

## case.json format

| Key | Meaning |
|---|---|
| `name`, `description`, `contract`, `platform`, `args` | Required. `name` must equal the directory name. |
| `commands` | Exact command line -> response. A response is `{stdout \| stdout_file, exit, stderr}`. An **array** of responses is served in order, the last one repeating (`lsof` before and after a kill, `docker ps` before and after a stop). `exit: -1` simulates a wrapper timeout. `exit: 0` ignores `stderr`. |
| `files` | Absolute path read by `ReadFile` (for example `/proc/net/tcp`) -> file. A missing key is an unreadable table. |
| `procs` | Live processes `{pid, start, name, strength}`. `strength: "none"` simulates a platform without process identity. |
| `tools` | Extension. Explicit list of installed tools. Default: the platform's required tools plus the program of every command in `commands`. |
| `apply_after_plan` | Extension. `plan_args` run first in the same world, and the plan's stdout is written to a temp file. Then `replace_procs` (PID reuse) and `exit_pids` are applied, and `args` run with `@plan` replaced by that file. |
| `expect` | `exit`, `json_paths` (dot paths; numeric segments index arrays; a final `#` is the array length), `signals` (PIDs in `fake.kills`, that is, delivered signals), `docker_stops` (container IDs of `docker stop` runs), `no_mutation` (assertNoMutation). |

Unknown keys fail the case (strict decoding). Dry-run cases run under
`readOnlySystem`, which panics on any mutating call. Apply cases do not, because
apply runs its actions on goroutines.

## Case table

| id | contract | REAL vs SYNTHETIC input | what it proves |
|---|---|---|---|
| 01-win-tab-indented-listener | tcp-listener | SYNTHETIC: tab-indented copy of `windows/netstat_ano.txt` (sed) | Tab and space indentation in netstat rows does not matter; the listener on 3000 is PID 4120. |
| 02-linux-ipv4-listener | tcp-listener | REAL: `linux/lsof_listen.txt`, `sockets/linux_proc_net_tcp{,6}.txt` | IPv4 wildcard listener 3000 is python3 PID 277 with start-time identity and a SIGTERM plan. |
| 03-linux-ipv6-bracketed-listener | tcp-listener | REAL: lsof_listen, `linux_proc_net_tcp6.txt` | `[::1]:8080` (bracketed IPv6) is attributed to PID 278. |
| 04-linux-v4-mapped-wildcard-same-pid | tcp-listener | MIXED: lsof SYNTHETIC (`0.0.0.0`, `[::]`, `[::ffff:127.0.0.1]` on PID 100), tcp6 SYNTHETIC, tcp REAL | v4, v6 and v4-mapped rows of one PID give one resource and one owner. |
| 05-linux-same-pid-two-ports | tcp-listener | SYNTHETIC: lsof and tcp table | One PID on 3000 and 3001 is one resource with both ports; both endpoints reference it. |
| 06-linux-near-miss-13000 | tcp-listener | REAL: lsof_listen, proc tcp | 13000 (PID 279) is not selected for 3000. |
| 07-darwin-near-miss-30001 | tcp-listener | SYNTHETIC (darwin captures): lsof_listen, netstat `-an -p tcp` | macOS: node PID 501 on 3000 only; 13000, 30001 and an ESTABLISHED row on 3000 are excluded. |
| 08-windows-near-miss-13000-30001 | tcp-listener | SYNTHETIC: `windows/netstat_ano.txt`, tasklist | Windows: only node.exe PID 4120 on 3000; 13000 and 30001 excluded. |
| 09-windows-localized-listening-state | tcp-listener | SYNTHETIC: netstat_ano (`ABHÖREN` on 8080) | A localized state name with wildcard foreign and port 0 is still a listener (python.exe PID 6500). |
| 10-windows-unexpected-bound-state | tcp-listener | SYNTHETIC: `netstat_bound.txt` (BOUND on 3000, LISTENING on 3001) | BOUND is not a listener: 3000 is free even though the same PID listens on 3001. |
| 11-windows-crlf-double-cr | tcp-listener | SYNTHETIC: `\r\r\n` netstat and tasklist (sed appends a CR to each CRLF line) | Doubled CRs in netstat and tasklist still parse; name is node.exe. |
| 12-windows-malformed-negative-pid | tcp-listener | SYNTHETIC: PID `abc` and `-5` on 3000 | Non-numeric and negative PIDs give no owner. The occupant is hidden, so it is refused and the plan fails. Never free. |
| 13-windows-pid0-listener-hides-owner | tcp-listener | SYNTHETIC: LISTENING PID 0 and 4120 on 3000 | The real owner is planned, but the PID 0 row is a hidden occupant, so the plan fails (refusal). |
| 14-linux-established-rows-ignored | tcp-listener | REAL: `linux/lsof_all_tcp.txt` served as the LISTEN answer, proc tcp | ESTABLISHED lsof rows are filtered by state; only PID 277 owns 3000. |
| 15-linux-timewait-established-free | tcp-listener | SYNTHETIC: `tcp_timewait.txt` (ESTABLISHED client and TIME_WAIT on local 3000) | Neither is a listener: 3000 is free with complete visibility. |
| 16-linux-remote-port-only-free | tcp-listener | SYNTHETIC: client block (REMOTE 3000) in lsof and proc | A socket whose only 3000 is its REMOTE port keeps the port free; nothing is planned. |
| 17-linux-legacy-kill-client-survives | tcp-listener | MIXED: REAL `linux/lsof_all_tcp.txt`; the empty answer after the kill is SYNTHETIC | Legacy kill signals only PID 277. Client PID 284 (REMOTE 3000) survives; verification sees the port free. |
| 18-windows-timewait-pid0-free | tcp-listener | SYNTHETIC: `netstat_timewait.txt` | TIME_WAIT with PID 0 and an ESTABLISHED row on local 3000 leave the port free. |
| 19-udp-bound-selected-by-protocol | udp-owner | REAL: `sockets/linux_lsof_udp.txt`, `linux_proc_net_udp{,6}.txt` | `--protocol udp 9999` selects the bound socket of PID 281 by local port. |
| 20-udp-connected-remote-port-ignored | udp-owner | SYNTHETIC: connected UDP local 5000 -> REMOTE 3000 | For UDP, a remote port 3000 does not match 3000: free. |
| 21-udp-tcp-contract-ignores-udp | udp-owner | REAL: proc tcp (the UDP socket 9999 exists only in proc udp); lsof answer empty SYNTHETIC | The default TCP contract ignores UDP; 9999 is free for TCP. |
| 22-linux-lsof-absent-ss-fallback | errors | REAL: `linux/ss_ltnp.txt`, proc tcp | Without lsof, owners come from `ss -ltnp`; PID 277 is still planned. |
| 23-linux-no-lsof-no-ss-missing-dependency | errors | SYNTHETIC: no tools, no captures | Neither lsof nor ss is installed: missing_dependency, exit 1, nothing observed. |
| 24-windows-tasklist-failure | errors | SYNTHETIC: netstat_ano; tasklist exit 1 | Names unavailable: PID 4120 is refused and the endpoint is provider_unavailable (exit 1). |
| 25-linux-docker-absent-proxy-refused | docker | MIXED: lsof SYNTHETIC (docker-proxy PID 800 on 3000), proc tcp REAL | docker is not installed: docker-proxy is refused and never signalled. |
| 26-linux-lsof-exit2-provider-unavailable | errors | MIXED: lsof error SYNTHETIC, proc tcp REAL | lsof exit 2 is a provider failure: provider_unavailable, exit 1, not free. |
| 27-linux-lsof-exit1-stderr-not-trusted | errors | MIXED: lsof exit 1 with stderr SYNTHETIC, proc tcp REAL | Exit 1 with stderr is not a trusted "nothing matched": hidden occupant, refused, exit 1. |
| 28-linux-docker-ps-timeout-infra-refused | docker | MIXED: docker-proxy lsof SYNTHETIC, docker ps timeout SYNTHETIC, proc tcp REAL | docker ps times out: the infrastructure proxy is refused because its container is unknown. |
| 29-linux-docker-ps-failure-visibility | visibility | REAL: lsof_listen, proc tcp; docker failure text SYNTHETIC | docker ps fails: the native owner is still planned, the endpoint is provider_unavailable, exit 1. |
| 30-linux-permission-limited | visibility | MIXED: proc tcp REAL, lsof empty SYNTHETIC | Occupied with no visible owner: permission_limited, hidden occupant refused, exit 1. |
| 31-linux-none-visible-unreadable-proc | visibility | SYNTHETIC: no /proc tables, lsof empty | Nothing readable and nothing visible: none_visible, exit 4 (uncertain), never free. |
| 32-linux-empty-tables-complete-free | visibility | SYNTHETIC: header-only proc tcp | A valid empty table is free with complete visibility, which is different from none_visible. |
| 33-linux-docker-one-container-two-ports | docker | SYNTHETIC: lsof (docker-proxy PID 900, 3000 and 3001), proc tcp, full 64-hex `docker ps --no-trunc` | One container, two ports: one docker_stop resource on the full id with ports [3000, 3001]. No refusals. |
| 34-windows-apply-docker-stop-once | docker | SYNTHETIC: netstat before/after, tasklist (com.docker.backend), docker ps full id | Plan then apply: docker stop runs once on the full id, both endpoints verify free, and the proxy is not signalled. |
| 35-linux-pid-reuse-apply-refused | identity | REAL: lsof_listen, proc tcp; replacement start SYNTHETIC | PID 277 is reused between plan and apply: identity_changed, nothing signalled. |
| 36-linux-identity-unavailable-refused | identity | REAL: lsof_listen, proc tcp; strength `none` SYNTHETIC | Without a process identity the owner is refused rather than signalled by PID alone. |
| 37-linux-pid1-and-self-refused | identity | SYNTHETIC: lsof with PID 1 (port 80) and PID 4242 (killport itself, port 3000); proc tcp REAL | PID 1 and the program's own PID are protected and refused. |
| 38-windows-system-pid4-refused | identity | SYNTHETIC: netstat_ano (PID 4 on 445) | The Windows System process is refused. |
| 39-linux-port-range-dedupe | tcp-listener | SYNTHETIC: lsof and tcp with 3000 and 3001 | `3000-3002 3001` gives three endpoints; one resource with ports [3000, 3001]. |
| 40-linux-dry-run-zero-mutation | identity | REAL: lsof_listen, proc tcp | Legacy `--dry-run` on 3000 and 13000: both planned; the read-only wrapper would panic on any kill. |
| 41-darwin-dry-run-zero-mutation | identity | SYNTHETIC (darwin): lsof_listen | Short bundle `-dj` on macOS: node PID 501 planned, nothing signalled. |
| 42-windows-dry-run-zero-mutation | identity | SYNTHETIC: netstat_ano, tasklist | `--dry-run` on Windows: node.exe PID 4120 planned, nothing terminated. |
| 43-linux-inspect-json-owner | visibility | REAL: lsof_listen, proc tcp | `inspect --json` reports owner 277 with complete visibility; exit ok. |
| 44-linux-legacy-docker-real-capture | docker | REAL: `docker/docker_ps.txt` (short IDs, as legacy prints them); lsof empty SYNTHETIC | Legacy dry run: both published ports of the one container are planned as stops; nothing is stopped. |

## Notes

- Plan cases use `docker ps --no-trunc`, which prints full 64-hex IDs. The REAL
  `docker/docker_ps.txt` has 12-character IDs, so it is used only by the legacy
  case 44. A plan refuses a container with a short ID by design, so the plan
  cases use a synthetic full-ID listing.
- The Windows and macOS inputs are synthetic (see `testdata/README.md`).
- Case 12, 13, 27 and 30 are permission_limited or hidden-occupant cases. Their
  exit is 1 (failed), not 4: an occupied endpoint with no actionable resource
  fails, and a hidden occupant is also a refusal (see `plan.go` `planClass`).
- `TestCorpusLegacyPredicateComparison` (in `corpus_test.go`) is not a case
  directory. It compares three predicates on the same eight invented Windows
  netstat rows: the old buggy selection, the npm-style contract, and the current
  `parseNetstat`. It asserts their declared contracts and is not a product benchmark.
