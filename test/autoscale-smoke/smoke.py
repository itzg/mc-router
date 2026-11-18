"""Opt-in Docker/Swarm fixtures and assertions; ordinary Go tests never start them."""

import argparse
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import re
import signal
import shutil
import subprocess
import tempfile
import time
import uuid


class Harness:
    def __init__(self, args):
        self.provider = args.provider
        self.project = args.project
        self.directory = Path(__file__).resolve().parent
        self.artifacts = Path(args.artifacts or tempfile.mkdtemp(prefix=f"{args.project}-")).resolve()
        self.artifacts.mkdir(parents=True, exist_ok=True)
        self.env = dict(os.environ, SMOKE_PROJECT=args.project)
        self.image = self.env.get("SMOKE_BACKEND_IMAGE") or "python:3.13-alpine@sha256:9ef1a599f863231c3d91e941389bfdc4a2d6070557243d54df1a4e57015b960e"
        self.env["SMOKE_BACKEND_IMAGE"] = self.image
        self.router_image = f"{self.project}-router:local"
        self.command_log = None
        self.deadline = time.monotonic() + args.timeout
        self.compose = ["docker", "compose", "-f", str(self.directory / "compose.yaml"), "-p", self.project]
        self.inner = self.compose + ["exec", "-T", "engine", "docker", "--host=unix:///var/run/docker.sock"]
        self.report = {
            "provider": self.provider, "project": self.project, "checks": [],
            "unavailable": ["Kubernetes: not provisioned by these fixtures"],
            "unverified": ["real Minecraft servers", "Swarm VIP and multi-node networking",
                           "default routes", "invalid-duration fallback on live providers",
                           "conflicting per-route timeouts on one target"],
        }

    def command(self, args, *, timeout=30, check=True, cleanup=False):
        deadline = self.cleanup_deadline if cleanup else self.deadline
        budget = min(timeout, max(1, deadline - time.monotonic()))
        process = subprocess.Popen(args, env=self.env, text=True, stdout=subprocess.PIPE,
                                   stderr=subprocess.PIPE, start_new_session=True)
        try:
            stdout, stderr = process.communicate(timeout=budget)
        except (subprocess.TimeoutExpired, KeyboardInterrupt) as error:
            os.killpg(process.pid, signal.SIGKILL)
            stdout, stderr = process.communicate()
            (self.artifacts / "interrupted-command.log").write_text(stdout + stderr)
            self.record_output(stdout, stderr)
            if isinstance(error, KeyboardInterrupt):
                raise
            raise RuntimeError(f"command timed out: {args[:3]}") from error
        self.record_output(stdout, stderr)
        if check and process.returncode:
            raise RuntimeError(f"command failed ({process.returncode}): {args}\n{stderr}\n{stdout}")
        return stdout, stderr, process.returncode

    def record_output(self, stdout, stderr):
        if self.command_log is not None:
            with self.command_log.open("a") as log:
                log.write(stdout + stderr)

    def docker(self, *args, **kwargs):
        return self.command(self.inner + list(args), **kwargs)[0].strip()

    def poll(self, predicate, description, timeout=90):
        deadline = min(self.deadline, time.monotonic() + timeout)
        while time.monotonic() < deadline:
            if predicate():
                return
            time.sleep(.5)
        raise AssertionError(f"timed out: {description}")

    def running(self, name):
        args = ["inspect", "--format", "{{.State.Running}}", name] if self.provider == "docker" else [
            "service", "inspect", "--format", "{{.Spec.Mode.Replicated.Replicas}}", name]
        out, err, status = self.command(self.inner + args, check=False)
        if status:
            if "No such object" in err or "not found" in err:
                return False  # Creation/replacement is not necessarily visible immediately.
            raise RuntimeError(f"cannot inspect {name}: {err}")
        if not out.strip():
            return False
        return out.strip() == "true" if self.provider == "docker" else int(out.strip()) > 0

    def router_logs(self, cleanup=False):
        args = ["logs", "router"] if self.provider == "docker" else ["service", "logs", "router"]
        out, err, _ = self.command(self.inner + args, check=False, cleanup=cleanup)
        return out + err

    def project_resources(self, kind, cleanup=False):
        args = ["docker", kind, "ls", "-q"]
        if kind == "container":
            args.append("-a")  # Stopped engines also belong to the project.
        args += ["--filter", f"label=com.docker.compose.project={self.project}"]
        return self.command(args, cleanup=cleanup)[0].strip()

    def unused_project(self):
        # A verifier that fails setup must never clean up somebody else's project.
        for kind in ("container", "network", "volume"):
            if self.project_resources(kind):
                raise RuntimeError(f"project already has {kind} resources; choose a unique name")
        _, _, status = self.command(["docker", "image", "inspect", f"{self.project}-router:local"], check=False)
        if status == 0:
            raise RuntimeError("project router image already exists; choose a unique name")

    def up(self):
        print(f"Project: {self.project}; provider: {self.provider}", flush=True)
        repo = str(self.directory.parent.parent)
        self.command(["docker", "build", "-t", self.router_image, "-f", f"{repo}/Dockerfile", repo], timeout=240)
        self.command(self.compose + ["up", "-d", "--wait", "--wait-timeout", "180"], timeout=180)
        self.command(self.compose + ["exec", "-T", "engine", "mkdir", "-p", "/fixtures"])
        engine = self.command(self.compose + ["ps", "-q", "engine"])[0].strip()
        for name in ("backend.py", "probe.py"):
            self.command(["docker", "cp", str(self.directory / name), f"{engine}:/fixtures/{name}"])
        self.load_router_image(engine)
        self.docker("image", "pull", self.image, timeout=120)
        if self.provider == "docker":
            self.docker("network", "create", "backends")
        else:
            # Only the disposable inner daemon joins Swarm; the host is untouched.
            self.docker("swarm", "init", "--advertise-addr", "eth0")
            self.docker("network", "create", "--driver", "overlay", "--attachable", "backends")
        self.start_backends()
        self.start_router()

    def load_router_image(self, engine):
        # Transfer an archive without a shell pipeline or retaining it as an artifact.
        with tempfile.TemporaryDirectory(prefix="mc-router-smoke-image-") as directory:
            archive = str(Path(directory) / "router.tar")
            self.command(["docker", "image", "save", "--output", archive, self.router_image], timeout=90)
            self.command(["docker", "cp", archive, f"{engine}:/fixtures/router.tar"], timeout=90)
        self.docker("image", "load", "--input", "/fixtures/router.tar", timeout=90)
        self.command(self.compose + ["exec", "-T", "engine", "rm", "/fixtures/router.tar"])

    def start_backends(self):
        for name in ("inherited", "override"):
            hosts = "override.smoke.test,alias.smoke.test" if name == "override" else "inherited.smoke.test"
            args = ["run", "-d"] if self.provider == "docker" else [
                "service", "create", "--no-resolve-image", "--replicas", "1", "--endpoint-mode", "dnsrr"]
            args += ["--name", name, "--network", "backends", "--label", f"mc-router.host={hosts}"]
            if name == "override":
                args += ["--label", "mc-router.auto-scale-down-after=15s"]
            self.docker(*args, "--mount", "type=bind,source=/fixtures/backend.py,target=/backend.py,readonly",
                        self.image, "python", "-u", "/backend.py")

    def start_router(self):
        args = ["run", "-d"] if self.provider == "docker" else [
            "service", "create", "--no-resolve-image", "--replicas", "1", "--constraint", "node.role==manager"]
        mode = "IN_DOCKER=true" if self.provider == "docker" else "IN_DOCKER_SWARM=true"
        self.docker(*args, "--name", "router", "--network", "backends", "--user", "0",
                    "--mount", "type=bind,source=/var/run/docker.sock,target=/var/run/docker.sock",
                    "--env", mode, "--env", "AUTO_SCALE_UP=true", "--env", "AUTO_SCALE_DOWN=true",
                    "--env", "AUTO_SCALE_DOWN_AFTER=60s", "--env", "DEBUG=true", self.router_image)

    def setup(self):
        # Preserve a total setup budget, not a separate 240 seconds for every command.
        deadline = self.deadline
        self.deadline = min(deadline, time.monotonic() + 240)
        self.command_log = self.artifacts / "setup.log"
        try:
            self.up()
        finally:
            self.deadline = deadline
            self.command_log = None
        self.poll(lambda: self.running("inherited") and self.running("override"), "backends ready", timeout=10)
        self.report["versions"] = self.command(["docker", "version", "--format", "{{json .}}"])[0]
        self.report["inner_versions"] = self.docker("version", "--format", "{{json .}}")
        self.report["backend_image"] = self.image
        self.report["revision"] = self.command(["git", "-C", str(self.directory.parent.parent), "rev-parse", "HEAD"])[0].strip()

    def down(self, cleanup=False):
        self.command(self.compose + ["down", "--volumes", "--remove-orphans"], timeout=90, cleanup=cleanup)
        _, _, status = self.command(["docker", "image", "inspect", self.router_image], check=False, cleanup=cleanup)
        if status == 0:
            self.command(["docker", "image", "rm", self.router_image], cleanup=cleanup)

    def state(self):
        if self.provider == "docker":
            args = ["inspect", "--format", "{{.Name}} running={{.State.Running}} finished={{.State.FinishedAt}}",
                    "inherited", "override"]
            logs = ["logs", "--tail", "40", "router"]
        else:
            args = ["service", "ls", "--format", "{{.Name}} replicas={{.Replicas}}"]
            logs = ["service", "logs", "--tail", "40", "router"]
        print(self.docker(*args))
        out, err, _ = self.command(self.inner + logs)
        print(out + err)

    def probe(self, args):
        name = "probe-" + uuid.uuid4().hex[:12]
        try:
            self.docker("run", "-d", "--name", name, "--network", "backends", "--mount",
                        "type=bind,source=/fixtures/probe.py,target=/probe.py,readonly",
                        self.image, "python", "-u", "/probe.py", args.hostname,
                        "--hold", str(args.hold), "--wait", str(args.wait),
                        "--router", args.router, "--port", str(args.port))
            while time.monotonic() < self.deadline:
                out, _, status = self.command(self.inner + ["inspect", "--format", "{{json .State}}", name], check=False)
                if status == 0 and out.strip():
                    state = json.loads(out)
                    if not state["Running"]:
                        return state["ExitCode"]
                time.sleep(1)
            raise RuntimeError(f"probe exceeded {args.timeout}s")
        finally:
            # A failed probe retains its output but never leaves a client container behind.
            self.cleanup_deadline = time.monotonic() + 90
            try:
                out, err, _ = self.command(self.inner + ["logs", name], check=False, cleanup=True)
                print(out + err, end="", flush=True)
            finally:
                self.docker("rm", "-f", name, check=False, cleanup=True)

    def change_override(self, value):
        label = "mc-router.auto-scale-down-after"
        inspect = ["inspect", "override"] if self.provider == "docker" else ["service", "inspect", "override"]
        previous = json.loads(self.docker(*inspect))[0]
        id_key = "Id" if self.provider == "docker" else "ID"
        before = self.router_logs().count("route mapping")
        if self.provider == "swarm":
            args = ["service", "update", "--detach=true", "--label-rm", label]
            if value is not None:
                args += ["--label-add", f"{label}={value}"]
            self.docker(*args, "override")
        else:
            # Docker cannot update container labels: exercise replacement discovery.
            self.docker("rm", "-f", "override")
            args = ["run", "-d", "--name", "override", "--network", "backends",
                    "--label", "mc-router.host=override.smoke.test,alias.smoke.test"]
            if value is not None:
                args += ["--label", f"{label}={value}"]
            self.docker(*args, "--mount", "type=bind,source=/fixtures/backend.py,target=/backend.py,readonly",
                        self.image, "python", "-u", "/backend.py")
        self.poll(lambda: self.router_logs().count("route mapping") > before,
                  "discovery observes changed labels", timeout=30)
        current = json.loads(self.docker(*inspect))[0]
        labels = current["Config"]["Labels"] if self.provider == "docker" else current["Spec"]["Labels"]
        assert labels.get(label) == value, "provider metadata did not change as requested"
        if self.provider == "swarm":
            assert current[id_key] == previous[id_key], "service identity changed during label update"
        else:
            assert current[id_key] != previous[id_key], "container labels require replacement"
        self.report.setdefault("metadata_changes", []).append({
            "value": value, "before_id": previous[id_key], "after_id": current[id_key],
            "mode": "container replacement" if self.provider == "docker" else "in-place service labels",
        })

    def update_probe_states(self, name, clients):
        for container, client in clients.items():
            if client.get("finished"):
                continue
            state_text, _, code = self.command(self.inner + ["inspect", "--format", "{{json .State}}", container], check=False)
            if code or not state_text.strip():
                continue  # creation can be asynchronous on nested exec transports
            state = json.loads(state_text)
            out, err, _ = self.command(self.inner + ["logs", container])
            (self.artifacts / f"{name}-{client['host']}.log").write_text(out + err)
            for line in out.splitlines():
                for token, key in ((" CONNECTED ", "connected"), (" DISCONNECTED ", "disconnected")):
                    if token in line:
                        client[key] = datetime.fromisoformat(line.split()[0])
            if not state["Running"]:
                assert state["ExitCode"] == 0, f"{name}: {client['host']} failed: {out}{err}"
                assert "disconnected" in client, f"{name}: missing probe completion record"
                client["finished"] = True

    def assert_shutdown_delays(self, name, clients, stopped, expected):
        delays = {}
        logs = self.router_logs()
        for target, seconds in expected.items():
            peers = [c for c in clients.values() if c["target"] == target]
            left = max(c["disconnected"] for c in peers)
            delay = (stopped[target] - left).total_seconds()
            assert seconds - .75 <= delay <= seconds + 12, f"{name}: expected {seconds}s, observed {delay:.2f}s"
            duration = "1m0s" if seconds == 60 else f"{seconds}s"
            target_name = f"name=/{target}" if self.provider == "docker" else f"name={target}"
            assert any("Starting scale-down timer" in line and f"delay={duration}" in line
                       and target_name in line for line in logs.splitlines()), f"{name}: no matching timer log"
            assert any("Scaling down backend server" in line and target_name in line
                       for line in logs.splitlines()), f"{name}: no scale-down log"
            delays[target] = round(delay, 3)
        return delays

    def cycle(self, name, probes, expected, update=None):
        print(f"START {self.provider}: {name}", flush=True)
        clients = {}
        for host, hold in probes:
            container = "probe-" + uuid.uuid4().hex[:12]
            self.docker("run", "-d", "--name", container, "--network", "backends", "--mount",
                        "type=bind,source=/fixtures/probe.py,target=/probe.py,readonly",
                        self.image, "python", "-u", "/probe.py", host + ".smoke.test",
                        "--hold", str(hold), "--wait", "60")
            clients[container] = {"host": host, "target": "inherited" if host == "inherited" else "override"}
        stopped = {}
        snapshots = []
        record = {"name": name, "result": "RUNNING", "snapshots": snapshots}
        self.report["checks"].append(record)
        updated = False
        deadline = min(self.deadline, time.monotonic() + 150)
        while time.monotonic() < deadline:
            self.update_probe_states(name, clients)
            states = {target: self.running(target) for target in expected}
            now = datetime.now(timezone.utc)
            snapshots.append({"at": now.isoformat(), **states})
            for target, running in states.items():
                peers = [c for c in clients.values() if c["target"] == target]
                active = [c for c in peers if "connected" in c and "disconnected" not in c]
                assert not active or running, f"{name}: {target} stopped with active players"
                if all("disconnected" in c for c in peers) and not running:
                    stopped.setdefault(target, now)
            if update and not updated and all(c.get("finished") for c in clients.values()):
                assert states["override"], "pending timer expired before metadata update"
                self.change_override(update)
                updated = True
            if len(stopped) == len(expected) and all(c.get("finished") for c in clients.values()):
                break
            time.sleep(.5)
        else:
            raise AssertionError(f"timed out: {name}")
        delays = self.assert_shutdown_delays(name, clients, stopped, expected)
        record.update(result="PASS", observed_delays_seconds=delays)
        print(f"PASS {name}: {delays}", flush=True)
        for container in clients:
            self.docker("rm", "-f", container)

    def verify(self):
        self.cycle("inheritance-override-shared", [("override", 20), ("alias", 30), ("inherited", 2)],
                   {"override": 15, "inherited": 60})
        if self.provider == "swarm":
            self.cycle("pending-update-preserves-deadline", [("override", 2)], {"override": 15}, update="8s")
        else:
            self.change_override("8s")
            self.report["unverified"].append("Docker in-place label updates: labels require container replacement")
        self.cycle("metadata-updated-timeout", [("alias", 2)], {"override": 8})
        self.change_override(None)
        self.cycle("metadata-removal-inherits", [("override", 2)], {"override": 60})

    def cleanup(self):
        self.cleanup_deadline = time.monotonic() + 90
        errors = []
        volumes = []
        try:
            out, _, status = self.command(["docker", "inspect", f"{self.project}-engine-1"],
                                          check=False, cleanup=True)
            if status == 0:
                volumes = [m["Name"] for m in json.loads(out)[0]["Mounts"] if m["Type"] == "volume"]
        except Exception as error:
            errors.append(f"inspect storage: {error}")
        for name, args in [("router", None), ("engine", self.compose + ["logs", "--no-color", "engine"])]:
            try:
                text = self.router_logs(cleanup=True) if args is None else "".join(self.command(args, check=False, cleanup=True)[:2])
                (self.artifacts / f"{name}.log").write_text(text)
            except Exception as error:
                errors.append(f"diagnostics: {error}")
        try:
            self.command_log = self.artifacts / "cleanup.log"
            self.down(cleanup=True)
            self.command_log = None
            for kind in ("container", "network", "volume"):
                assert not self.project_resources(kind, cleanup=True), f"{kind} resources remain"
            for kind, name in [("volume", v) for v in volumes] + [("image", f"{self.project}-router:local")]:
                _, _, status = self.command(["docker", kind, "inspect", name], check=False, cleanup=True)
                assert status != 0, f"{kind} remains: {name}"
        except Exception as error:
            errors.append(f"cleanup: {error}")
        self.command_log = None
        self.report["cleanup"] = {"result": "FAIL" if errors else "PASS", "errors": errors}
        return not errors

    def run_verification(self):
        owned = False
        try:
            self.unused_project()
            owned = True
            self.setup()
            self.verify()
            self.report["result"] = "PASS"
        except (Exception, KeyboardInterrupt) as error:
            self.report.update(result="FAIL", error=str(error))
            for check in self.report["checks"]:
                if check["result"] == "RUNNING":
                    check.update(result="FAIL", error=str(error))
            print(f"FAIL: {error}", flush=True)
        finally:
            if owned and not self.cleanup():
                self.report["result"] = "FAIL"
            (self.artifacts / "report.json").write_text(json.dumps(self.report, indent=2))
        return 0 if self.report["result"] == "PASS" else 1


def check_tools():
    docker = shutil.which("docker")
    if docker is None:
        raise RuntimeError("docker is not installed")
    print(docker, flush=True)
    subprocess.run([docker, "compose", "version"], check=True, timeout=30)
    subprocess.run([docker, "info", "--format",
                    "Host daemon: {{.ServerVersion}}; Swarm={{.Swarm.LocalNodeState}} (unchanged by these fixtures)"],
                   check=True, timeout=30)
    print("Nested-engine and image availability require an explicit up; not verified by check.")


def parse_args():
    parser = argparse.ArgumentParser(description=__doc__)
    actions = parser.add_subparsers(dest="action", required=True)
    actions.add_parser("check", help="check Docker/Compose availability; creates nothing")
    for action in ("verify", "up", "probe", "state", "down"):
        command = actions.add_parser(action)
        if action != "down":
            command.add_argument("provider", choices=("docker", "swarm"))
        else:
            command.set_defaults(provider="docker")
        command.add_argument("--project", default=os.environ.get("SMOKE_PROJECT"))
        command.add_argument("--artifacts")
        timeout = os.environ.get("SMOKE_PROBE_TIMEOUT", "180") if action == "probe" else 600
        command.add_argument("--timeout", type=int, default=timeout)
        if action == "probe":
            command.add_argument("hostname")
            command.add_argument("--hold", type=float, default=20)
            command.add_argument("--wait", type=float, default=90)
            command.add_argument("--router", default="router")
            command.add_argument("--port", type=int, default=25565)
    args = parser.parse_args()
    if args.action == "check":
        return args
    if args.action == "verify" and not args.project:
        args.project = "mc-router-smoke-" + uuid.uuid4().hex[:12]
    if not args.project or not re.fullmatch(r"mc-router-smoke-[a-z0-9-]+", args.project):
        parser.error("set --project or SMOKE_PROJECT to a unique mc-router-smoke-<lowercase suffix>")
    if args.timeout <= 0:
        parser.error("--timeout must be positive")
    if args.action == "probe" and (args.hold < 0 or args.wait <= 0):
        parser.error("--hold must be nonnegative and --wait must be positive")
    return args


def main():
    args = parse_args()
    try:
        if args.action == "check":
            check_tools()
            return 0
        harness = Harness(args)
        print(f"Artifacts: {harness.artifacts}", flush=True)
        if args.action == "verify":
            return harness.run_verification()
        if args.action == "up":
            harness.unused_project()
            harness.command_log = harness.artifacts / "setup.log"
            harness.up()
            print("Fixtures started. Run probe immediately; run down after testing.")
        elif args.action == "probe":
            return harness.probe(args)
        elif args.action == "state":
            harness.state()
        elif args.action == "down":
            harness.command_log = harness.artifacts / "cleanup.log"
            harness.down()
        return 0
    except (Exception, KeyboardInterrupt) as error:
        print(f"FAIL: {error}", flush=True)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
