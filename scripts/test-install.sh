#!/bin/sh
# Regression tests for install.sh. Runs it inside throwaway Docker containers
# with a fake `curl` first on PATH, so nothing touches the network or the host.
#
#   scripts/test-install.sh            run every case
#   IMAGE=debian:bookworm-slim scripts/test-install.sh
#
# The same file is re-executed inside each container with `--in-container`.
set -eu

IMAGE="${IMAGE:-golang:1.25}"
CASES="ok-dash ok-bash ok-piped ok-shasum-fallback tampered-archive missing-entry \
checksums-404 no-hash-tool"

# ---------------------------------------------------------------- container
in_container() {
    case_name="$1"
    FIX=/fixtures
    SHIM=/shim
    mkdir -p "$FIX" "$SHIM" /work

    case "$(uname -m)" in
        x86_64) arch=amd64 ;;
        *) arch=arm64 ;;
    esac
    archive="killport_9.9.9_linux_${arch}.tar.gz"

    # Fake release assets.
    printf '{\n  "tag_name": "v9.9.9",\n  "name": "9.9.9"\n}\n' >"$FIX/api.json"
    cat >/work/killport <<'EOF'
#!/bin/sh
echo "killport version 9.9.9"
EOF
    chmod +x /work/killport
    tar -czf "$FIX/$archive" -C /work killport
    (cd "$FIX" && sha256sum "$archive") >"$FIX/checksums.txt"
    printf '%064d  killport_9.9.9_windows_amd64.zip\n' 0 >>"$FIX/checksums.txt"

    # Fake curl: serves fixtures, refuses anything else (no real network).
    cat >"$SHIM/curl" <<'EOF'
#!/bin/sh
out=""; url=""
while [ $# -gt 0 ]; do
    case "$1" in
        -o) out="$2"; shift ;;
        -*) ;;
        *) url="$1" ;;
    esac
    shift
done
case "$url" in
    https://api.github.com/repos/paarths-collab/killport/releases/latest) src=/fixtures/api.json ;;
    https://github.com/paarths-collab/killport/releases/download/v9.9.9/*) src="/fixtures/${url##*/}" ;;
    *) echo "fake curl: unexpected URL $url" >&2; exit 6 ;;
esac
[ -f "$src" ] || { echo "fake curl: 404 $url" >&2; exit 22; }
if [ -n "$out" ]; then cp "$src" "$out"; else cat "$src"; fi
EOF
    chmod +x "$SHIM/curl"
    PATH="$SHIM:$PATH"
    export PATH

    # Prove mkdir -p: the install dir does not exist at the start.
    rm -rf /usr/local/bin

    want_rc=0
    want_installed=1
    want_text="killport version 9.9.9"
    runner="sh /src/install.sh"

    case "$case_name" in
        ok-dash)
            [ "$(readlink -f /bin/sh)" = "$(readlink -f "$(command -v dash)")" ] ||
                { echo "/bin/sh is not dash in this image" >&2; exit 99; }
            ;;
        ok-bash) runner="bash /src/install.sh" ;;
        ok-piped) runner="cat /src/install.sh | sh" ;; # the documented curl|sh form
        ok-shasum-fallback)
            mv /usr/bin/sha256sum /opt-sha256sum
            cat >"$SHIM/shasum" <<'EOF2'
#!/bin/sh
[ "$1" = -a ] && [ "$2" = 256 ] && shift 2
exec /opt-sha256sum "$@"
EOF2
            chmod +x "$SHIM/shasum"
            ;;
        tampered-archive)
            printf 'tampered' >>"$FIX/$archive"
            want_rc=1; want_installed=0; want_text="checksum mismatch"
            ;;
        missing-entry)
            grep -v "$archive" "$FIX/checksums.txt" >"$FIX/c.tmp"; mv "$FIX/c.tmp" "$FIX/checksums.txt"
            want_rc=1; want_installed=0; want_text="no checksum entry"
            ;;
        checksums-404)
            rm "$FIX/checksums.txt"
            want_rc=1; want_installed=0; want_text="checksums.txt"
            ;;
        no-hash-tool)
            rm -f /usr/bin/sha256sum /bin/sha256sum /usr/bin/shasum
            want_rc=1; want_installed=0; want_text="no SHA-256 tool"
            ;;
        *) echo "unknown case $case_name" >&2; exit 98 ;;
    esac

    rc=0
    out="$(sh -c "$runner" 2>&1)" || rc=$?

    fail=""
    [ "$rc" -eq "$want_rc" ] || fail="$fail exit=$rc(want $want_rc)"
    if [ "$want_installed" -eq 1 ]; then
        [ -x /usr/local/bin/killport ] || fail="$fail not-installed"
    else
        [ ! -e /usr/local/bin/killport ] || fail="$fail installed-despite-failure"
    fi
    printf '%s\n' "$out" | grep -qF "$want_text" || fail="$fail missing-output($want_text)"
    for leaked in /tmp/tmp.*; do
        [ -e "$leaked" ] && fail="$fail tmpdir-leaked"
    done

    if [ -n "$fail" ]; then
        echo "--- output ---"; echo "$out"; echo "--------------"
        echo "FAIL $case_name:$fail"
        exit 1
    fi
    echo "PASS $case_name (exit $rc)"
}

if [ "${1:-}" = "--in-container" ]; then
    in_container "$2"
    exit 0
fi

# --------------------------------------------------------------------- host
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
failed=0
for c in $CASES; do
    if docker run --rm --network none -v "$ROOT":/src:ro "$IMAGE" \
        sh /src/scripts/test-install.sh --in-container "$c"; then
        :
    else
        failed=$((failed + 1))
    fi
done

if [ "$failed" -ne 0 ]; then
    echo "$failed case(s) failed" >&2
    exit 1
fi
echo "all install.sh cases passed"
