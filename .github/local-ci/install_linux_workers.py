#!/usr/bin/env python3
"""Install independent Linux worker LaunchAgents without starting them."""

import argparse
import json
from pathlib import Path
import sys

from install import LAUNCH_AGENTS, LINUX_PLATFORM, check_root, write_root
from supervisor import DEFAULT_ROOT, worker_limits


def install_workers(root, agents, count=3, cpus=3, memory="3g"):
    root, agents = Path(root).resolve(), Path(agents).resolve()
    if count < 1 or count > 16:
        raise ValueError("worker count must be between 1 and 16")
    base = json.loads((root / "config.json").read_text())
    linux = dict(base["platforms"].get("linux", LINUX_PLATFORM), cpus=cpus, memory=memory)
    if linux.get("engine") != "docker":
        raise ValueError("Linux worker pool requires the Docker engine")
    worker_limits(linux)
    config = {key: base[key] for key in ("repository", "gh", "docker", "docker_context")}
    config["platforms"] = {"linux": linux}
    workers = [(root / f"linux-{index}", f"com.polly.ci.linux-{index}") for index in range(1, count + 1)]
    # Validate every slot before writing any installation files.
    for worker, label in workers:
        if worker.is_symlink():
            raise ValueError(f"refusing symlink worker root: {worker}")
        check_root(worker, config, label, agents / f"{label}.plist")
    for worker, label in workers:
        write_root(worker, config, label, agents / f"{label}.plist")
    return [worker for worker, _ in workers]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=DEFAULT_ROOT)
    parser.add_argument("--workers", type=int, default=3)
    parser.add_argument("--cpus", type=int, default=3)
    parser.add_argument("--memory", default="3g")
    args = parser.parse_args()
    if sys.platform != "darwin":
        parser.error("this installer writes macOS LaunchAgents; see README.md for Linux host setup")
    try:
        workers = install_workers(args.root, LAUNCH_AGENTS, args.workers, args.cpus, args.memory)
    except (OSError, ValueError, KeyError) as error:
        parser.error(str(error))
    for worker in workers:
        print(f"Installed {worker}; not started")
    print("Before starting the pool, remove Linux from the stopped main supervisor's configuration.")


if __name__ == "__main__":
    main()
