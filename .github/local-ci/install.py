#!/usr/bin/env python3
"""Install the local supervisor without starting it or altering the base VM."""

import json
import os
from pathlib import Path
import plistlib
import shutil
import sys

root = Path.home() / "Library/Application Support/PollyCI"
root.mkdir(parents=True, exist_ok=True)
root.chmod(0o700)
for directory in ("bin", "logs"):
    (root / directory).mkdir(exist_ok=True)

helper = Path("/Library/PrivilegedHelperTools/com.polly.ci.softnet")
if not helper.is_file() or helper.stat().st_uid != 0 or helper.stat().st_mode & 0o4022 != 0o4000:
    sys.exit("Install the root-owned Softnet helper with mode 4755 first; see README.md")
link = root / "bin/softnet"
if link.is_symlink():
    if link.resolve() != helper:
        sys.exit("Refusing to replace an unrelated Softnet link")
elif link.exists():
    sys.exit("Refusing to replace an unrelated Softnet executable")
else:
    link.symlink_to(helper)

config = {"repository": "alexschlessinger/polly", "tart": shutil.which("tart"), "gh": shutil.which("gh"),
          "docker": shutil.which("docker", path=os.environ["PATH"] + ":/usr/local/bin"),
          "docker_context": "orbstack",
          "platforms": {
              "macos": {"base_vm": "polly-ci-base", "label": "polly-local-macos", "os": "macOS", "home": "/Users/admin"},
              "linux": {"engine": "docker", "image": "polly-local-ci:go1.27", "label": "polly-local-linux", "os": "Linux"}}}
if not config["tart"] or not config["gh"] or not config["docker"]:
    sys.exit("Tart, Docker and GitHub CLI must be installed")
path = root / "config.json"
if path.exists() and json.loads(path.read_text()) != config:
    sys.exit("Existing configuration differs; inspect it before replacing")
path.write_text(json.dumps(config, indent=2) + "\n")
path.chmod(0o600)
shutil.copyfile(Path(__file__).with_name("supervisor.py"), root / "supervisor.py")

agent = Path.home() / "Library/LaunchAgents/com.polly.ci.local.plist"
agent.parent.mkdir(parents=True, exist_ok=True)
definition = {
    "Label": "com.polly.ci.local",
    "ProgramArguments": [sys.executable, str(root / "supervisor.py"), "--root", str(root)],
    "WorkingDirectory": str(root),
    "RunAtLoad": True,
    "KeepAlive": True,
    "ThrottleInterval": 30,
    "ProcessType": "Background",
    "Nice": 10,
    "Umask": 0o077,
    "ExitTimeOut": 45,
    "EnvironmentVariables": {"PATH": str(root / "bin") + ":/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin",
                             "GH_PROMPT_DISABLED": "1", "GIT_TERMINAL_PROMPT": "0"},
    "StandardOutPath": str(root / "logs/launchd.out.log"),
    "StandardErrorPath": str(root / "logs/launchd.err.log"),
}
if agent.exists() and plistlib.loads(agent.read_bytes()) != definition:
    sys.exit("Existing launch agent differs; inspect it before replacing")
agent.write_bytes(plistlib.dumps(definition))
agent.chmod(0o600)
print(f"Installed supervisor in {root}; launch agent is not started yet.")
