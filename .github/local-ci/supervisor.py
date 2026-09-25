#!/usr/bin/env python3
"""Run GitHub jobs on demand in disposable macOS VMs or Linux containers."""

import argparse
from collections import namedtuple
from contextlib import contextmanager
import fcntl
import json
import logging
from logging.handlers import RotatingFileHandler
import os
from pathlib import Path
import re
import signal
import shlex
import subprocess
import tempfile
import time
import uuid

PREFIX = "polly-ci-job-"
WORKER_TIMEOUT = 35 * 60
HEALTH_INTERVAL = 10
RUNNER_CHECK_INTERVAL = 30
RUNNER_GRACE = 120
PAGE_SIZE = 100
DEFAULT_ROOT = Path.home() / "Library/Application Support/PollyCI"
HOST_PATH = "/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin"

# A committed tree to check without GitHub; mode is a ci.sh argument.
Local = namedtuple("Local", "directory revision mode")


class Stopped(Exception):
    pass


def host_path(root):
    """PATH for the supervisor and everything it runs on the host."""
    return f"{Path(root) / 'bin'}:{HOST_PATH}"


def queued_local_job(jobs, label):
    return any(job.get("status") == "queued" and label in job.get("labels", []) for job in jobs)


def not_found(error):
    return "HTTP 404" in (error.stderr or "")


def stop_process(process, timeout):
    if process is None or process.poll() is not None:
        return
    process.terminate()
    try:
        process.wait(timeout=timeout)
    except subprocess.TimeoutExpired:
        process.kill()
        process.wait()


def worker_limits(config):
    """Validated Docker resource limits of a worker configuration."""
    cpus = config.get("cpus", 6)
    memory = config.get("memory", "8g")
    if isinstance(cpus, bool) or not isinstance(cpus, int) or cpus < 1:
        raise ValueError("worker cpus must be a positive integer")
    if not isinstance(memory, str) or not re.fullmatch(r"[1-9][0-9]*[kKmMgG]?", memory):
        raise ValueError("worker memory must be a positive Docker memory limit, such as 3g")
    return cpus, memory


class Supervisor:
    def __init__(self, root):
        self.root = Path(root).resolve()
        self.config = json.loads((self.root / "config.json").read_text())
        self.repo = self.config["repository"]
        if not re.fullmatch(r"[\w.-]+/[\w.-]+", self.repo):
            raise ValueError("invalid repository")
        self.env = dict(os.environ, TART_HOME=str(self.root / "tart"), PATH=host_path(self.root))
        self.active_path = self.root / "active.json"
        self.last_platform = None

    def command(self, args, *, check=True, timeout=60, **kwargs):
        return subprocess.run(args, env=self.env, check=check, text=True,
                              capture_output=True, timeout=timeout, **kwargs).stdout.strip()

    def api(self, path, *, method="GET", body=None):
        args = [self.config["gh"], "api", "--method", method,
                "-H", "X-GitHub-Api-Version: 2022-11-28", path]
        kwargs = {}
        if body is not None:
            args += ["--input", "-"]
            kwargs["input"] = json.dumps(body)
        result = self.command(args, **kwargs)
        return json.loads(result) if result else None

    def pages(self, path, key):
        """Yield the key list of every page of a list endpoint."""
        page = 1
        while True:
            result = self.api(f"{path}?per_page={PAGE_SIZE}&page={page}")
            yield result[key]
            if page * PAGE_SIZE >= result["total_count"]:
                return
            page += 1

    def tart(self, *args, **kwargs):
        return self.command([self.config["tart"], *args], **kwargs)

    def docker(self, *args):
        return [self.config["docker"], "--context", self.config["docker_context"], *args]

    def container_command(self, name, config, mode):
        cpus, memory = worker_limits(config)
        args = self.docker("run", "--rm", "--init", "--name", name,
            "--label", "com.polly.ci.repository=" + self.repo,
            "--cpus", str(cpus), "--memory", memory,
            "--env", f"GOMAXPROCS={cpus}", "--env", f"GOFLAGS=-p={cpus}",
            "--cap-drop", "ALL", "--user", "1000:1000",
            "--security-opt", "no-new-privileges", "--security-opt", "seccomp=unconfined",
            "--security-opt", "systempaths=unconfined")
        if mode != "runner":
            args += ["--network", "none"]
        return args + ["-i", config["image"], mode]

    def has_work(self, wanted=None):
        platforms = list(self.config["platforms"])
        if wanted:
            platforms = [wanted]
        elif self.last_platform in platforms:
            pivot = platforms.index(self.last_platform) + 1
            platforms = platforms[pivot:] + platforms[:pivot]
        available = set()
        for status in ("queued", "in_progress"):
            runs = self.api(f"repos/{self.repo}/actions/runs?status={status}&per_page={PAGE_SIZE}")
            for run in runs["workflow_runs"]:
                for jobs in self.pages(f"repos/{self.repo}/actions/runs/{run['id']}/jobs", "jobs"):
                    for platform in platforms:
                        if queued_local_job(jobs, self.config["platforms"][platform]["label"]):
                            available.add(platform)
                    # Scan past lower-priority work in newer runs. Otherwise a
                    # steady macOS backlog can starve Linux (or vice versa).
                    if platforms and platforms[0] in available:
                        return platforms[0]
        return next((platform for platform in platforms if platform in available), None)

    def save_active(self, active):
        pending = self.active_path.with_suffix(".tmp")
        pending.write_text(json.dumps(active))
        os.chmod(pending, 0o600)
        pending.replace(self.active_path)

    def guest(self, vm, *args, stdin=False):
        # The VM control socket carries commands; no network access, SSH keys,
        # agent forwarding or shared filesystem is needed for host control.
        return [self.config["tart"], "exec", *(["-i"] if stdin else []), vm, *args]

    def register(self, name, config, active):
        """Register a one-job runner, journal its id and return the worker's configuration."""
        registration = self.api(f"repos/{self.repo}/actions/runners/generate-jitconfig",
            method="POST", body={"name": name, "runner_group_id": 1,
            "labels": ["self-hosted", config["os"], "ARM64", config["label"]],
            "work_folder": "_work"})
        active["runner_id"] = registration["runner"]["id"]
        self.save_active(active)
        logging.info("registered one-job runner %s", name)
        return registration["encoded_jit_config"]

    @contextmanager
    def archive_commit(self, directory, revision):
        """Archive a committed tree; never a mount or the host's .git/config."""
        revision = self.command(["git", "-C", str(directory), "rev-parse", "--verify",
                                 "--end-of-options", revision + "^{commit}"])
        if not re.fullmatch(r"[a-f0-9]{40,64}", revision):
            raise ValueError("invalid committed revision")
        with tempfile.TemporaryFile() as archive:
            subprocess.run(["git", "-C", str(directory), "archive", "--format=tar", revision],
                           stdout=archive, env=self.env, check=True, timeout=60)
            archive.seek(0)
            yield revision, archive

    def recover(self):
        if not self.active_path.exists():
            return
        active = json.loads(self.active_path.read_text())
        vm = active.get("container") or active["vm"]
        if not re.fullmatch(PREFIX + r"[a-f0-9]{12}", vm):
            raise ValueError("refusing cleanup of an unknown VM name")
        # Reclaim compute first, even if GitHub is offline or has not yet noticed
        # a canceled runner. Retain the journal until remote cleanup succeeds.
        if active.get("container"):
            names = self.command(self.docker("ps", "--all", "--format", "{{.Names}}",
                "--filter", "label=com.polly.ci.repository=" + self.repo)).splitlines()
            if vm in names:
                self.command(self.docker("rm", "--force", vm))
        else:
            names = self.tart("list", "--source", "local", "--quiet").splitlines()
            if vm in names:
                self.tart("stop", vm, check=False, timeout=30)
                self.tart("delete", vm)
        if not active.get("local") and not active.get("runner_id"):
            # Registration can succeed just before the journal write is lost.
            for runners in self.pages(f"repos/{self.repo}/actions/runners", "runners"):
                matches = [r for r in runners if r["name"] == vm]
                if matches:
                    active["runner_id"] = matches[0]["id"]
                    self.save_active(active)
                    break
        if active.get("runner_id"):
            runner_id = int(active["runner_id"])
            try:
                runner = self.api(f"repos/{self.repo}/actions/runners/{runner_id}")
            except subprocess.CalledProcessError as err:
                if not not_found(err):
                    raise
            else:
                if runner["name"] != vm:
                    raise ValueError("refusing cleanup of a different runner")
                self.api(f"repos/{self.repo}/actions/runners/{runner_id}", method="DELETE")
        self.active_path.unlink()
        logging.info("reclaimed %s", vm)

    def run_one(self, platform, force=False, local=None):
        self.last_platform = platform
        logging.info("selected %s worker", platform)
        config = self.config["platforms"][platform]
        if config.get("engine") == "docker":
            return self.run_container(config, local)
        vm = PREFIX + uuid.uuid4().hex[:12]
        active = {"vm": vm, "local": bool(local)}
        self.save_active(active)
        vm_process = runner_process = None
        try:
            self.tart("clone", config["base_vm"], vm)
            logging.info("booting %s", vm)
            with (self.root / "logs" / "vm.log").open("w") as vm_log:
                vm_process = subprocess.Popen(
                    [self.config["tart"], "run", "--no-graphics", "--no-audio",
                     "--no-clipboard", "--net-softnet-block=@host", vm],
                    env=self.env, stdout=vm_log, stderr=subprocess.STDOUT)
                deadline = time.monotonic() + 180
                while time.monotonic() < deadline:
                    if vm_process.poll() is not None:
                        raise RuntimeError("VM exited during boot; inspect logs/vm.log")
                    try:
                        self.command(self.guest(vm, "/usr/bin/true"))
                        break
                    except subprocess.SubprocessError:
                        pass
                    time.sleep(3)
                else:
                    raise TimeoutError("VM guest agent did not become ready")
                if local:
                    self.run_local(vm, platform, local)
                    return
                if not force and not self.has_work(platform):
                    return
                jit_config = self.register(vm, config, active)
                # Only the one-job configuration crosses into the VM. The host's
                # GitHub OAuth credential stays in its Keychain and is never sent.
                with (self.root / "logs" / "runner.log").open("w") as runner_log:
                    runner_process = subprocess.Popen(
                        self.guest(vm, "/bin/bash", config["home"] + "/actions-runner/polly-run.sh", stdin=True),
                        env=self.env, stdin=subprocess.PIPE, stdout=runner_log,
                        stderr=subprocess.STDOUT, text=True)
                    runner_process.communicate(jit_config + "\n", timeout=WORKER_TIMEOUT)
                    if runner_process.returncode:
                        raise RuntimeError("runner exited unsuccessfully; inspect logs/runner.log")
                logging.info("job finished on %s", vm)
        finally:
            stop_process(runner_process, 10)
            # Keep recovery state until both GitHub registration and VM are gone.
            try:
                self.recover()
            finally:
                stop_process(vm_process, 15)

    def run_local(self, vm, platform, local):
        target = shlex.quote(self.config["platforms"][platform]["home"] + "/polly")
        with self.archive_commit(local.directory, local.revision) as (revision, archive):
            subprocess.run(self.guest(vm, "/bin/bash", "-c", f"mkdir -p {target} && tar -xf - -C {target}", stdin=True),
                           stdin=archive, env=self.env, check=True, timeout=120)
        print(f"Running {local.mode} on {platform} at {revision}", flush=True)
        command = (f"cd {target} && export PATH=/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin && "
                   f"/bin/bash .github/ci.sh {shlex.quote(local.mode)}")
        subprocess.run(self.guest(vm, "/bin/bash", "-c", command), env=self.env,
                       check=True, timeout=WORKER_TIMEOUT)

    def wait_container(self, process, name, runner_id, jit_config):
        """Send registration once, then monitor independently of job log output."""
        deadline = time.monotonic() + WORKER_TIMEOUT
        started_at = None
        unhealthy_since = None
        idle_since = time.monotonic()
        next_runner_check = idle_since
        payload = jit_config + "\n"
        while True:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise RuntimeError("container exceeded the 35-minute worker limit")
            try:
                process.communicate(payload, timeout=min(HEALTH_INTERVAL, remaining))
                return
            except subprocess.TimeoutExpired:
                # communicate() retains its input across timeouts. Resending the
                # configuration would be incorrect, especially after a restart.
                payload = None
            try:
                state = json.loads(self.command(self.docker("inspect", "--format", "{{json .State}}", name)))
            except (OSError, subprocess.SubprocessError):
                if process.poll() is not None:
                    return
                logging.warning("container health unavailable for %s; retaining worker", name)
                continue
            if not state["Running"]:
                # Docker can publish the stopped state before its attached CLI
                # has drained output and returned the container's exit status.
                # Give that normal shutdown a bounded grace period instead of
                # turning successful jobs into supervisor failures and backoff.
                try:
                    process.communicate(timeout=min(HEALTH_INTERVAL, max(0, deadline - time.monotonic())))
                    return
                except subprocess.TimeoutExpired:
                    raise RuntimeError("Docker client did not exit after its container stopped") from None
            if started_at is not None and state["StartedAt"] != started_at:
                raise RuntimeError("container restarted; replace it with a fresh one-job runner")
            started_at = state["StartedAt"]
            now = time.monotonic()
            if now < next_runner_check:
                continue
            next_runner_check = now + RUNNER_CHECK_INTERVAL
            try:
                runner = self.api(f"repos/{self.repo}/actions/runners/{runner_id}")
            except subprocess.CalledProcessError as err:
                if not not_found(err):
                    # An API outage is not evidence that a worker has failed.
                    unhealthy_since = idle_since = None
                    logging.warning("runner health unavailable for %s; retaining worker", name)
                    continue
                runner = {"status": "offline"}
            except (OSError, subprocess.TimeoutExpired):
                unhealthy_since = idle_since = None
                logging.warning("runner health unavailable for %s; retaining worker", name)
                continue
            if runner.get("status") != "online":
                if unhealthy_since is None:
                    unhealthy_since = now
                if now - unhealthy_since >= RUNNER_GRACE:
                    raise RuntimeError("runner remained offline for two minutes")
            else:
                unhealthy_since = None
                if runner.get("busy"):
                    idle_since = None
                elif idle_since is None:
                    idle_since = now
                elif now - idle_since >= RUNNER_GRACE:
                    raise RuntimeError("runner claimed no job for two minutes; releasing worker slot")

    def run_container(self, config, local):
        name = PREFIX + uuid.uuid4().hex[:12]
        active = {"container": name, "local": bool(local)}
        self.save_active(active)
        process = None
        try:
            if local:
                with self.archive_commit(local.directory, local.revision) as (revision, archive):
                    print(f"Running {local.mode} in Docker at {revision}", flush=True)
                    process = subprocess.Popen(self.container_command(name, config, local.mode),
                                               stdin=archive, env=self.env)
                    process.wait(timeout=WORKER_TIMEOUT)
            else:
                jit_config = self.register(name, config, active)
                with (self.root / "logs" / "runner.log").open("w") as runner_log:
                    process = subprocess.Popen(self.container_command(name, config, "runner"),
                        env=self.env, stdin=subprocess.PIPE, stdout=runner_log,
                        stderr=subprocess.STDOUT, text=True)
                    self.wait_container(process, name, active["runner_id"], jit_config)
            if process.returncode:
                raise RuntimeError("container exited unsuccessfully; inspect its output")
            logging.info("container finished %s", name)
        finally:
            stop_process(process, 10)
            self.recover()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=DEFAULT_ROOT)
    parser.add_argument("--once", metavar="PLATFORM", help="boot one runner immediately and exit after its job")
    parser.add_argument("--local", choices=("test", "race", "cross", "all"), help="test a committed tree without GitHub")
    parser.add_argument("--platform", default="linux", help="configured platform for --local")
    parser.add_argument("--repository-dir", default=".")
    parser.add_argument("--revision", default="HEAD")
    args = parser.parse_args()
    if args.local and args.once:
        parser.error("--local and --once are mutually exclusive")
    root = args.root
    (root / "logs").mkdir(exist_ok=True)
    handler = RotatingFileHandler(root / "logs/supervisor.log", maxBytes=2_000_000, backupCount=3)
    logging.basicConfig(level=logging.INFO, handlers=[handler],
                        format="%(asctime)s %(levelname)s %(message)s")
    with (root / ("manual.lock" if args.local else "supervisor.lock")).open("w") as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            raise SystemExit("a supervisor is already running")
        def stop(_signum, _frame):
            raise Stopped()
        signal.signal(signal.SIGTERM, stop)
        signal.signal(signal.SIGINT, stop)
        supervisor = Supervisor(root)
        platform = args.once or args.platform
        if (args.once or args.local) and platform not in supervisor.config["platforms"]:
            parser.error(f"platform {platform} is not configured in {root / 'config.json'}")

        def one_job(**kwargs):
            with (root / "job.lock").open("w") as job_lock:
                fcntl.flock(job_lock, fcntl.LOCK_EX)
                supervisor.recover()
                supervisor.run_one(platform, **kwargs)

        try:
            if args.local:
                print("Waiting for the local CI worker slot…", flush=True)
                one_job(local=Local(args.repository_dir, args.revision, args.local))
                return
            if args.once:
                try:
                    one_job(force=True)
                except (OSError, ValueError, RuntimeError, subprocess.SubprocessError):
                    logging.exception("runner cycle failed")
                    raise SystemExit(1)
                return
            while True:
                try:
                    with (root / "job.lock").open("w") as job_lock:
                        try:
                            fcntl.flock(job_lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
                        except BlockingIOError:
                            time.sleep(5)
                            continue
                        supervisor.recover()
                        platform = supervisor.has_work()
                        if platform:
                            supervisor.run_one(platform)
                            continue
                except (OSError, ValueError, RuntimeError, subprocess.SubprocessError):
                    # Don't log command arguments, stdin or API bodies: those can
                    # contain the short-lived runner registration configuration.
                    logging.exception("runner cycle failed")
                time.sleep(30)
        except Stopped:
            logging.info("supervisor stopped")


if __name__ == "__main__":
    main()
