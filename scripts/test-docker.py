#!/usr/bin/env python3
"""Exercise a built killport against disposable containers on a local daemon.

Requires Python 3 and Docker. Uses an already cached image (never pulls), binds
only loopback ports, and removes only containers carrying this run's unique
label. No Docker socket or host directory is mounted into a test container.

    python3 scripts/test-docker.py --binary ./killport
"""

import argparse
import json
import os
from pathlib import Path
import subprocess
import tempfile
import time
import uuid


SERVER = """
import signal, socket, sys, time
signal.signal(signal.SIGTERM, signal.SIG_IGN if sys.argv[1] == 'stubborn' else lambda *_: sys.exit(0))
sockets = []
for port, kind in [(8080, socket.SOCK_STREAM), (8081, socket.SOCK_STREAM), (5353, socket.SOCK_DGRAM)]:
    s = socket.socket(socket.AF_INET, kind)
    s.bind(('0.0.0.0', port))
    if kind == socket.SOCK_STREAM:
        s.listen()
    sockets.append(s)
print('ready', flush=True)
while True:
    time.sleep(1)
"""


class Validation:
    def __init__(self, binary, image, work):
        self.binary = str(Path(binary).resolve())
        self.image = image
        self.work = work
        self.label = "killport.validation=" + uuid.uuid4().hex
        self.ids = []
        self.passed = 0

    def docker(self, *args):
        return subprocess.run(
            ["docker", *args], check=True, capture_output=True, text=True, timeout=45
        ).stdout.strip()

    def cli(self, *args, expected=0, env=None):
        result = subprocess.run(
            [self.binary, *map(str, args), "--json"],
            capture_output=True, text=True, timeout=60, env=env,
        )
        if result.returncode != expected:
            raise AssertionError(
                f"killport {args[0]} exited {result.returncode}, expected {expected}: "
                f"{result.stderr}\n{result.stdout}"
            )
        return json.loads(result.stdout)

    def inspect(self, cid):
        return json.loads(self.docker("inspect", cid))[0]

    def running(self, cid):
        return self.inspect(cid)["State"]["Running"]

    def create(self, *, protocol="tcp", host_port=None, stubborn=False):
        bindings = [(host_port, 8080)] if protocol == "tcp" else [(host_port, 5353)]
        if protocol == "tcp" and host_port is None:
            bindings.append((None, 8081))
        args = ["run", "-d", "--pull", "never", "--label", self.label]
        for host, target in bindings:
            args += ["-p", f"127.0.0.1:{host or ''}:{target}/{protocol}"]
        args += [self.image, "python", "-u", "-c", SERVER, "stubborn" if stubborn else "graceful"]
        cid = self.docker(*args)
        self.ids.append(cid)
        deadline = time.monotonic() + 15
        while "ready" not in self.docker("logs", cid):
            if not self.running(cid) or time.monotonic() > deadline:
                raise AssertionError("disposable server did not become ready")
            time.sleep(0.1)
        mappings = self.inspect(cid)["NetworkSettings"]["Ports"]
        ports = [int(mappings[f"{target}/{protocol}"][0]["HostPort"]) for _, target in bindings]
        return cid, ports

    def plan(self, cid, ports, *, protocol="tcp", grace="2s"):
        path = self.work / (uuid.uuid4().hex + ".json")
        doc = self.cli("plan", *ports, "--protocol", protocol, "--mode", "container", "--grace", grace, "-o", path)
        assert len(doc["resources"]) == 1, doc
        resource = doc["resources"][0]
        assert resource["provider"] == "docker" and resource["container_id"] == cid, doc
        assert resource["ports"] == sorted(ports), doc
        assert resource["action"] == "docker_stop", doc
        return path

    def passed_case(self, name):
        self.passed += 1
        print("PASS " + name, flush=True)

    def run(self):
        # The initial operation is always read-only, following the port policy.
        self.cli("--check")
        endpoint = ""
        if not os.environ.get("DOCKER_CONTEXT", "").strip():
            endpoint = os.environ.get("DOCKER_HOST", "").strip()
        if not endpoint:
            endpoint = self.docker("context", "inspect", "--format", "{{.Endpoints.docker.Host}}")
        if not (endpoint.startswith("unix:///") or endpoint.startswith("npipe:////./pipe/")):
            raise RuntimeError("validation requires a local Docker daemon; refusing to create remote containers")
        self.docker("image", "inspect", self.image)
        cid, ports = self.create()

        check = self.cli("--check", *ports)
        assert any(row.get("is_docker") for row in check), check
        observation = self.cli("inspect", *ports)
        assert all(any(c["container_id"] == cid for c in e["containers"]) for e in observation["endpoints"]), observation
        preview = self.cli(*ports, "--dry-run")
        assert len(preview) == len(ports), preview
        assert all(r["status"] == "planned" and r["action"] == "stop_container" and not r["killed"] for r in preview), preview
        assert self.running(cid), "read-only operations stopped the container"
        self.passed_case("Docker check, inspect and dry-run preserve the running container")

        path = self.plan(cid, ports)
        assert self.running(cid), "planning stopped the container"

        # No remote daemon should be contacted or used as a local port owner.
        remote = dict(os.environ, DOCKER_CONTEXT="", DOCKER_HOST="ssh://remote.example.invalid")
        rejected = self.cli("plan", *ports, "--mode", "container", expected=1, env=remote)
        assert not rejected["resources"] and self.running(cid), rejected
        self.passed_case("remote Docker endpoint is refused without stopping the local container")

        # Simulate an unavailable daemon without touching the actual daemon.
        unavailable = dict(os.environ, DOCKER_CONTEXT="", DOCKER_HOST="unix://" + str(self.work / "missing.sock"))
        result = self.cli("apply", path, expected=4, env=unavailable)
        assert result["actions_executed"] == 0 and self.running(cid), result
        assert any(pe["provider"] == "docker" for e in result["endpoints"] for pe in e["provider_errors"]), result
        self.passed_case("unavailable Docker endpoint reports uncertainty and executes zero actions")

        result = self.cli("apply", path)
        assert result["actions_executed"] == 1 and len(result["resources"]) == 1, result
        assert result["resources"][0]["result"] == "stopped" and result["resources"][0]["verified_gone"], result
        assert all(e["status"] == "free" for e in result["endpoints"]), result
        state = self.inspect(cid)["State"]
        assert not state["Running"] and state["ExitCode"] == 0, state
        self.passed_case("two TCP publications produce one graceful stop and two verified free endpoints")

        udp, ports = self.create(protocol="udp")
        result = self.cli("apply", self.plan(udp, ports, protocol="udp"))
        assert result["actions_executed"] == 1 and all(e["status"] == "free" for e in result["endpoints"]), result
        assert not self.running(udp), "UDP container remains running"
        self.passed_case("UDP publication is planned and stopped through the explicit UDP workflow")

        stubborn, ports = self.create(stubborn=True)
        result = self.cli("apply", self.plan(stubborn, ports, grace="1s"))
        assert result["resources"][0]["result"] == "stopped" and result["resources"][0]["verified_gone"], result
        assert self.inspect(stubborn)["State"]["ExitCode"] == 137
        self.passed_case("Docker grace timeout kills a stubborn container and verifies the endpoint")

        original, ports = self.create()
        path = self.plan(original, ports)
        # Use killport to stop the original before deliberately replacing it.
        self.cli(*ports)
        assert not self.running(original)
        replacement, _ = self.create(host_port=ports[0])
        result = self.cli("apply", path, expected=3)
        assert result["actions_executed"] == 0 and self.running(replacement), result
        assert any(e["status"] == "blocked_by_new_owner" for e in result["endpoints"]), result
        self.passed_case("container replacement makes the saved plan stale; replacement survives")
        self.cli(ports[0])
        assert not self.running(replacement)
        self.passed_case("classic kill command stops only the container owning the requested test port")

    def cleanup(self):
        for cid in reversed(self.ids):
            info = self.inspect(cid)
            key, value = self.label.split("=", 1)
            if info["Config"]["Labels"].get(key) != value:
                raise AssertionError("refusing to remove a container not owned by this run")
            # Forced removal is a failure-path cleanup of this run's own
            # disposable containers, never a way of resolving a user's port.
            self.docker("rm", "-f", cid)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True)
    parser.add_argument("--image", default="python:3.12-slim")
    args = parser.parse_args()
    with tempfile.TemporaryDirectory(prefix="killport-docker-") as work:
        validation = Validation(args.binary, args.image, Path(work))
        try:
            validation.run()
        finally:
            validation.cleanup()
        print(f"All {validation.passed} Docker validation cases passed; test containers removed.")


if __name__ == "__main__":
    main()
