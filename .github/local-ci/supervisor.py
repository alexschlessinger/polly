#!/usr/bin/env python3
"""Run GitHub jobs on demand in disposable macOS VMs or Linux containers."""

import argparse
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


class Stopped(Exception):
    pass


def queued_local_job(jobs, label):
    return any(job.get("status") == "queued" and label in job.get("labels", []) for job in jobs)


class Supervisor:
    def __init__(self, root):
        self.root = Path(root).resolve()
        self.config = json.loads((self.root / "config.json").read_text())
        self.repo = self.config["repository"]
        if not re.fullmatch(r"[\w.-]+/[\w.-]+", self.repo):
            raise ValueError("invalid repository")
        self.env = dict(os.environ, TART_HOME=str(self.root / "tart"))
        self.env["PATH"] = str(self.root / "bin") + ":/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin"
        self.active_path = self.root / "active.json"

    def command(self, args, **kwargs):
        return subprocess.run(args, env=self.env, check=True, text=True,
                              capture_output=True, timeout=60, **kwargs).stdout.strip()

    def api(self, path, *, method="GET", body=None):
        args = [self.config["gh"], "api", "--method", method,
                "-H", "X-GitHub-Api-Version: 2022-11-28", path]
        kwargs = {}
        if body is not None:
            args += ["--input", "-"]
            kwargs["input"] = json.dumps(body)
        result = self.command(args, **kwargs)
        return json.loads(result) if result else None

    def tart(self, *args):
        return self.command([self.config["tart"], *args])

    def docker(self, *args):
        return [self.config["docker"], "--context", self.config["docker_context"], *args]

    def container_command(self, name, config, mode):
        args = self.docker("run", "--rm", "--init", "--name", name,
            "--label", "com.polly.ci.repository=" + self.repo,
            "--cpus", "6", "--memory", "8g", "--cap-drop", "ALL", "--user", "1000:1000",
            "--security-opt", "no-new-privileges", "--security-opt", "seccomp=unconfined",
            "--security-opt", "systempaths=unconfined")
        if mode != "runner":
            args += ["--network", "none"]
        return args + ["-i", config["image"], mode]

    def has_work(self, wanted=None):
        for status in ("queued", "in_progress"):
            runs = self.api(f"repos/{self.repo}/actions/runs?status={status}&per_page=100")
            for run in runs["workflow_runs"]:
                page = 1
                while True:
                    result = self.api(f"repos/{self.repo}/actions/runs/{run['id']}/jobs?per_page=100&page={page}")
                    for platform, config in self.config["platforms"].items():
                        if wanted and wanted != platform:
                            continue
                        if queued_local_job(result["jobs"], config["label"]):
                            return platform
                    if page * 100 >= result["total_count"]:
                        break
                    page += 1
        return None

    def save_active(self, active):
        pending = self.active_path.with_suffix(".tmp")
        pending.write_text(json.dumps(active))
        os.chmod(pending, 0o600)
        pending.replace(self.active_path)

    def guest(self, vm, *args, stdin=False):
        # The VM control socket carries commands; no network access, SSH keys,
        # agent forwarding or shared filesystem is needed for host control.
        return [self.config["tart"], "exec", *(["-i"] if stdin else []), vm, *args]

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
                subprocess.run([self.config["tart"], "stop", vm], env=self.env,
                               capture_output=True, timeout=30)
                self.tart("delete", vm)
        if not active.get("local") and not active.get("runner_id"):
            # Registration can succeed just before the journal write is lost.
            page = 1
            while True:
                result = self.api(f"repos/{self.repo}/actions/runners?per_page=100&page={page}")
                matches = [r for r in result["runners"] if r["name"] == vm]
                if matches:
                    active["runner_id"] = matches[0]["id"]
                    self.save_active(active)
                    break
                if page * 100 >= result["total_count"]:
                    break
                page += 1
        if active.get("runner_id"):
            runner_id = int(active["runner_id"])
            try:
                runner = self.api(f"repos/{self.repo}/actions/runners/{runner_id}")
            except subprocess.CalledProcessError as err:
                if "HTTP 404" not in err.stderr:
                    raise
            else:
                if runner["name"] != vm:
                    raise ValueError("refusing cleanup of a different runner")
                self.api(f"repos/{self.repo}/actions/runners/{runner_id}", method="DELETE")
        self.active_path.unlink()
        logging.info("reclaimed %s", vm)

    def run_one(self, platform, force=False, local=None):
        config = self.config["platforms"][platform]
        if config.get("engine") == "docker":
            return self.run_container(config, local)
        vm = PREFIX + uuid.uuid4().hex[:12]
        active = {"vm": vm}
        if local:
            active["local"] = True
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
                    self.run_local(vm, platform, *local)
                    return
                if not force and not self.has_work(platform):
                    return
                registration = self.api(f"repos/{self.repo}/actions/runners/generate-jitconfig",
                    method="POST", body={"name": vm, "runner_group_id": 1,
                    "labels": ["self-hosted", config["os"], "ARM64", config["label"]],
                    "work_folder": "_work"})
                active["runner_id"] = registration["runner"]["id"]
                self.save_active(active)
                logging.info("registered one-job runner %s", vm)
                # Only the one-job configuration crosses into the VM. The host's
                # GitHub OAuth credential stays in its Keychain and is never sent.
                with (self.root / "logs" / "runner.log").open("w") as runner_log:
                    runner_process = subprocess.Popen(
                        self.guest(vm, "/bin/bash", config["home"] + "/actions-runner/polly-run.sh", stdin=True),
                        env=self.env, stdin=subprocess.PIPE, stdout=runner_log,
                        stderr=subprocess.STDOUT, text=True)
                    runner_process.communicate(registration["encoded_jit_config"] + "\n",
                                               timeout=35 * 60)
                    if runner_process.returncode:
                        raise RuntimeError("runner exited unsuccessfully; inspect logs/runner.log")
                logging.info("job finished on %s", vm)
        finally:
            if runner_process is not None and runner_process.poll() is None:
                runner_process.terminate()
                try:
                    runner_process.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    runner_process.kill()
                    runner_process.wait()
            # Keep recovery state until both GitHub registration and VM are gone.
            try:
                self.recover()
            finally:
                if vm_process is not None and vm_process.poll() is None:
                    vm_process.terminate()
                    try:
                        vm_process.wait(timeout=15)
                    except subprocess.TimeoutExpired:
                        vm_process.kill()
                        vm_process.wait()

    def run_local(self, vm, platform, directory, revision, check):
        """Transfer a committed tree, never a mount or the host's .git/config."""
        revision = self.command(["git", "-C", str(directory), "rev-parse", "--verify",
                                 "--end-of-options", revision + "^{commit}"])
        if not re.fullmatch(r"[a-f0-9]{40,64}", revision):
            raise ValueError("invalid committed revision")
        home = self.config["platforms"][platform]["home"]
        directory = Path(directory).resolve()
        with tempfile.TemporaryFile() as archive:
            subprocess.run(["git", "-C", str(directory), "archive", "--format=tar", revision],
                           stdout=archive, check=True, timeout=60)
            archive.seek(0)
            target = shlex.quote(home + "/polly")
            subprocess.run(self.guest(vm, "/bin/bash", "-c", f"mkdir -p {target} && tar -xf - -C {target}", stdin=True),
                           stdin=archive, env=self.env, check=True, timeout=120)
        print(f"Running {check} on {platform} at {revision}", flush=True)
        command = (f"cd {target} && export PATH=/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin && "
                   f"/bin/bash .github/ci.sh {shlex.quote(check)}")
        subprocess.run(self.guest(vm, "/bin/bash", "-c", command), env=self.env,
                       check=True, timeout=35 * 60)

    def run_container(self, config, local):
        name = PREFIX + uuid.uuid4().hex[:12]
        active = {"container": name, "local": bool(local)}
        self.save_active(active)
        process = None
        try:
            if local:
                directory, revision, mode = local
                revision = self.command(["git", "-C", str(directory), "rev-parse", "--verify",
                                         "--end-of-options", revision + "^{commit}"])
                with tempfile.TemporaryFile() as archive:
                    subprocess.run(["git", "-C", str(directory), "archive", "--format=tar", revision],
                                   stdout=archive, check=True, timeout=60)
                    archive.seek(0)
                    print(f"Running {mode} in Docker at {revision}", flush=True)
                    process = subprocess.Popen(self.container_command(name, config, mode),
                                               stdin=archive, env=self.env)
                    process.wait(timeout=35 * 60)
            else:
                registration = self.api(f"repos/{self.repo}/actions/runners/generate-jitconfig",
                    method="POST", body={"name": name, "runner_group_id": 1,
                    "labels": ["self-hosted", config["os"], "ARM64", config["label"]],
                    "work_folder": "_work"})
                active["runner_id"] = registration["runner"]["id"]
                self.save_active(active)
                logging.info("registered one-job container runner %s", name)
                with (self.root / "logs" / "runner.log").open("w") as runner_log:
                    process = subprocess.Popen(self.container_command(name, config, "runner"),
                        env=self.env, stdin=subprocess.PIPE, stdout=runner_log,
                        stderr=subprocess.STDOUT, text=True)
                    process.communicate(registration["encoded_jit_config"] + "\n", timeout=35 * 60)
            if process.returncode:
                raise RuntimeError("container exited unsuccessfully; inspect its output")
            logging.info("container finished %s", name)
        finally:
            if process is not None and process.poll() is None:
                process.terminate()
                try:
                    process.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait()
            self.recover()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", default=str(Path.home() / "Library/Application Support/PollyCI"))
    parser.add_argument("--once", choices=("macos", "linux"), help="boot one runner immediately and exit after its job")
    parser.add_argument("--local", choices=("test", "race", "cross", "all"), help="test a committed tree without GitHub")
    parser.add_argument("--platform", choices=("macos", "linux"), default="linux")
    parser.add_argument("--repository-dir", default=".")
    parser.add_argument("--revision", default="HEAD")
    args = parser.parse_args()
    if args.local and args.once:
        parser.error("--local and --once are mutually exclusive")
    root = Path(args.root)
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
        try:
            if args.local:
                with (root / "job.lock").open("w") as job_lock:
                    print("Waiting for the local CI worker slot…", flush=True)
                    fcntl.flock(job_lock, fcntl.LOCK_EX)
                    supervisor.recover()
                    supervisor.run_one(args.platform, local=(args.repository_dir, args.revision, args.local))
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
                        platform = args.once or supervisor.has_work()
                        if platform:
                            supervisor.run_one(platform, force=bool(args.once))
                        if args.once:
                            return
                        if platform:
                            continue
                except (OSError, ValueError, RuntimeError, subprocess.SubprocessError):
                    # Don't log command arguments, stdin or API bodies: those can
                    # contain the short-lived runner registration configuration.
                    logging.exception("runner cycle failed")
                    if args.once:
                        raise SystemExit(1)
                time.sleep(30)
        except Stopped:
            logging.info("supervisor stopped")


if __name__ == "__main__":
    main()
