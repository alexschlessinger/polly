#!/usr/bin/env python3
"""Install the local supervisor without starting it or altering the base VM."""

import json
import os
from pathlib import Path
import plistlib
import shutil
import sys

from supervisor import DEFAULT_ROOT, host_path

LAUNCH_AGENTS = Path.home() / "Library/LaunchAgents"
LINUX_PLATFORM = {"engine": "docker", "image": "polly-local-ci:go1.27", "label": "polly-local-linux", "os": "Linux"}
MACOS_PLATFORM = {"base_vm": "polly-ci-base", "label": "polly-local-macos", "os": "macOS", "home": "/Users/admin"}


def launch_agent(label, root):
    """LaunchAgent definition that keeps the supervisor installed at root running."""
    root = Path(root)
    return {
        "Label": label,
        "ProgramArguments": [sys.executable, str(root / "supervisor.py"), "--root", str(root)],
        "WorkingDirectory": str(root),
        "RunAtLoad": True,
        "KeepAlive": True,
        "ThrottleInterval": 30,
        "ProcessType": "Background",
        "Nice": 10,
        "Umask": 0o077,
        "ExitTimeOut": 45,
        "EnvironmentVariables": {"PATH": host_path(root), "GH_PROMPT_DISABLED": "1", "GIT_TERMINAL_PROMPT": "0"},
        "StandardOutPath": str(root / "logs/launchd.out.log"),
        "StandardErrorPath": str(root / "logs/launchd.err.log"),
    }


def check_root(root, config, label, agent):
    """Refuse to replace a differing configuration or LaunchAgent; run before any write."""
    for path, expected, loads, what in ((root / "config.json", config, json.loads, "configuration"),
                                        (agent, launch_agent(label, root), plistlib.loads, "LaunchAgent")):
        if path.exists() and loads(path.read_bytes()) != expected:
            raise ValueError(f"existing {what} differs; inspect it before replacing: {path}")


def write_root(root, config, label, agent):
    """Write a supervisor root, its private files and its LaunchAgent."""
    root.mkdir(mode=0o700, parents=True, exist_ok=True)
    root.chmod(0o700)
    (root / "logs").mkdir(mode=0o700, exist_ok=True)
    for path, data in ((root / "config.json", (json.dumps(config, indent=2) + "\n").encode()),
                       (agent, plistlib.dumps(launch_agent(label, root)))):
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(data)
        path.chmod(0o600)
    shutil.copyfile(Path(__file__).with_name("supervisor.py"), root / "supervisor.py")


def main():
    helper = Path("/Library/PrivilegedHelperTools/com.polly.ci.softnet")
    status = helper.stat() if helper.is_file() else None
    if status is None or status.st_uid != 0 or status.st_mode & 0o4022 != 0o4000:
        sys.exit("Install the root-owned Softnet helper with mode 4755 first; see README.md")
    link = DEFAULT_ROOT / "bin/softnet"
    if link.is_symlink():
        if link.resolve() != helper:
            sys.exit("Refusing to replace an unrelated Softnet link")
    elif link.exists():
        sys.exit("Refusing to replace an unrelated Softnet executable")

    config = {"repository": "alexschlessinger/polly", "tart": shutil.which("tart"), "gh": shutil.which("gh"),
              "docker": shutil.which("docker", path=os.environ["PATH"] + ":/usr/local/bin"),
              "docker_context": "orbstack",
              "platforms": {"macos": MACOS_PLATFORM, "linux": LINUX_PLATFORM}}
    if not config["tart"] or not config["gh"] or not config["docker"]:
        sys.exit("Tart, Docker and GitHub CLI must be installed")
    agent = LAUNCH_AGENTS / "com.polly.ci.local.plist"
    try:
        check_root(DEFAULT_ROOT, config, "com.polly.ci.local", agent)
    except ValueError as error:
        sys.exit(str(error))
    write_root(DEFAULT_ROOT, config, "com.polly.ci.local", agent)
    link.parent.mkdir(exist_ok=True)
    if not link.is_symlink():
        link.symlink_to(helper)
    print(f"Installed supervisor in {DEFAULT_ROOT}; launch agent is not started yet.")


if __name__ == "__main__":
    main()
