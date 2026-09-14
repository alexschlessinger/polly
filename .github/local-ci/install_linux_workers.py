#!/usr/bin/env python3
"""Install independent Linux worker LaunchAgents without starting them."""

import argparse
import json
from pathlib import Path
import plistlib
import shutil
import sys

from supervisor import Supervisor


def install_workers(root, agents, count=3, cpus=3, memory="3g"):
    root, agents = Path(root).resolve(), Path(agents).resolve()
    if count < 1 or count > 16:
        raise ValueError("worker count must be between 1 and 16")
    base = json.loads((root / "config.json").read_text())
    linux = dict(base["platforms"].get("linux", {
        "engine": "docker", "image": "polly-local-ci:go1.27",
        "label": "polly-local-linux", "os": "Linux"}))
    linux.update(cpus=cpus, memory=memory)
    if linux.get("engine") != "docker":
        raise ValueError("Linux worker pool requires the Docker engine")
    # Validate resource limits before writing any installation files.
    Supervisor(root).container_command("validation-only", linux, "runner")
    config = {key: base[key] for key in ("repository", "gh", "docker", "docker_context")}
    config["platforms"] = {"linux": linux}
    plans = []
    for index in range(1, count + 1):
        worker = root / f"linux-{index}"
        if worker.is_symlink():
            raise ValueError(f"refusing symlink worker root: {worker}")
        label = f"com.polly.ci.linux-{index}"
        definition = {
            "Label": label,
            "ProgramArguments": [sys.executable, str(worker / "supervisor.py"), "--root", str(worker)],
            "WorkingDirectory": str(worker), "RunAtLoad": True, "KeepAlive": True,
            "ThrottleInterval": 30, "ProcessType": "Background", "Nice": 10,
            "Umask": 0o077, "ExitTimeOut": 45,
            "EnvironmentVariables": {
                "PATH": "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin",
                "GH_PROMPT_DISABLED": "1", "GIT_TERMINAL_PROMPT": "0"},
            "StandardOutPath": str(worker / "logs/launchd.out.log"),
            "StandardErrorPath": str(worker / "logs/launchd.err.log"),
        }
        config_path, agent_path = worker / "config.json", agents / (label + ".plist")
        if config_path.exists() and json.loads(config_path.read_text()) != config:
            raise ValueError(f"existing worker configuration differs: {config_path}")
        if agent_path.exists() and plistlib.loads(agent_path.read_bytes()) != definition:
            raise ValueError(f"existing LaunchAgent differs: {agent_path}")
        plans.append((worker, config_path, agent_path, definition))
    agents.mkdir(parents=True, exist_ok=True)
    for worker, config_path, agent_path, definition in plans:
        worker.mkdir(mode=0o700, exist_ok=True)
        worker.chmod(0o700)
        (worker / "logs").mkdir(mode=0o700, exist_ok=True)
        config_path.write_text(json.dumps(config, indent=2) + "\n")
        config_path.chmod(0o600)
        shutil.copyfile(Path(__file__).with_name("supervisor.py"), worker / "supervisor.py")
        agent_path.write_bytes(plistlib.dumps(definition))
        agent_path.chmod(0o600)
    return [worker for worker, *_ in plans]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=Path.home() / "Library/Application Support/PollyCI")
    parser.add_argument("--workers", type=int, default=3)
    parser.add_argument("--cpus", type=int, default=3)
    parser.add_argument("--memory", default="3g")
    args = parser.parse_args()
    if sys.platform != "darwin":
        parser.error("this installer writes macOS LaunchAgents; see README.md for Linux host setup")
    try:
        workers = install_workers(args.root, Path.home() / "Library/LaunchAgents",
                                  args.workers, args.cpus, args.memory)
    except (OSError, ValueError, KeyError) as error:
        parser.error(str(error))
    for worker in workers:
        print(f"Installed {worker}; not started")
    print("Before starting the pool, remove Linux from the stopped main supervisor's configuration.")


if __name__ == "__main__":
    main()
