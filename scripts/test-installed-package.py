#!/usr/bin/env python3
"""Exercise the CLI resolved from a package manager's PATH using owned sockets."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import time


def require(condition, message):
    if not condition:
        raise AssertionError(message)


binary = shutil.which("killport")
require(binary, "killport is missing from the package manager's PATH")
env = os.environ.copy()
# These native socket checks must not depend on a runner's Docker daemon.
env["PATH"] = (os.path.join(os.environ["SystemRoot"], "System32") if os.name == "nt"
               else "/usr/bin:/bin:/usr/sbin:/sbin")


def cli(*args):
    result = subprocess.run([binary, *map(str, args), "--json"], env=env,
                            capture_output=True, text=True, timeout=30)
    require(result.returncode == 0,
            f"killport {args} failed ({result.returncode}): {result.stderr} {result.stdout}")
    return json.loads(result.stdout)


require(cli("--version")["version"] == "1.2.0", "Installed version must be 1.2.0")
print("PASS PATH resolves a working killport v1.2.0", flush=True)
for command in ("inspect", "plan", "apply"):
    require(cli(command, "--help").get("usage"), f"Missing {command} JSON help")
cli("--check")

helper = """
import pathlib, socket, sys, time
sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM if sys.argv[2] == 'udp' else socket.SOCK_STREAM)
sock.bind(('127.0.0.1', 0))
if sys.argv[2] == 'tcp': sock.listen()
pathlib.Path(sys.argv[1]).write_text(str(sock.getsockname()[1]))
while True: time.sleep(1)
"""

with tempfile.TemporaryDirectory(prefix="killport-package-") as directory:
    work = Path(directory)
    children = []

    def listener(protocol):
        ready = work / f"ready-{len(children)}"
        process = subprocess.Popen([sys.executable, "-c", helper, str(ready), protocol])
        children.append(process)
        deadline = time.monotonic() + 15
        while not ready.exists():
            require(process.poll() is None and time.monotonic() < deadline,
                    "Owned listener failed to start")
            time.sleep(0.05)
        return process, int(ready.read_text())

    try:
        for protocol in ("tcp", "udp"):
            process, port = listener(protocol)
            if protocol == "tcp":
                rows = cli("--check", port)
                require(any(str(row.get("pid")) == str(process.pid) for row in rows),
                        "Classic inspection missed the owned listener")
            observed = cli("inspect", port, "--protocol", protocol, "--mode", "native")
            endpoint = observed["endpoints"][0]
            require(endpoint["occupied"] and any(owner.get("pid") == process.pid
                    for owner in endpoint["owners"]), "Inspect missed the owned listener")
            plan_path = work / f"{protocol}.json"
            plan = cli("plan", port, "--protocol", protocol, "--mode", "native",
                       "--grace", "2s", "-o", plan_path)
            require(len(plan["resources"]) == 1 and plan["resources"][0]["pid"] == process.pid,
                    "Refusing to apply a plan targeting anything except the owned listener")
            require(process.poll() is None, "Planning changed the owned process")
            applied = cli("apply", plan_path)
            require(applied["actions_executed"] == 1 and
                    all(endpoint["status"] == "free" for endpoint in applied["endpoints"]) and
                    applied["resources"][0]["result"] == "signal_sent",
                    f"Apply did not verify the port was freed: {applied}")
            # Unix retains this parent's exited child as a zombie until wait().
            # Assert actual child termination independently of PID disappearance.
            process.wait(timeout=5)
            print(f"PASS {protocol.upper()} inspect, plan and apply free the owned listener", flush=True)

        process, port = listener("tcp")
        cli("--check", port)
        preview = cli(port, "--dry-run")
        require(preview and all(row["status"] == "planned" for row in preview) and
                process.poll() is None, "Dry run changed the listener")
        rows = cli(port)
        require(any(row.get("killed") for row in rows), "Classic termination was not reported")
        process.wait(timeout=5)
        observed = cli("inspect", port, "--mode", "native")
        require(not observed["endpoints"][0]["occupied"], "Classic termination did not free the port")
        print("PASS classic check, dry-run and termination", flush=True)
    finally:
        for process in children:
            if process.poll() is None:
                process.terminate()
            process.wait(timeout=5)
print("PASS all installed-package checks", flush=True)
