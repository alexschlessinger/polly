#!/usr/bin/env python3
"""Local focused, affected-package, and full verification. See docs/TESTING.md."""

import argparse
import json
import os
from pathlib import Path
import shlex
import subprocess
import sys

ROOT = Path(__file__).resolve().parent.parent


def capture(root, *command, env=None):
    return subprocess.check_output(command, cwd=root, env=env)


def git_paths(root, *args):
    return {os.fsdecode(path) for path in capture(root, "git", *args).split(b"\0") if path}


def changed_paths(root, base):
    revision = capture(root, "git", "rev-parse", "--verify", "--end-of-options", base + "^{commit}").decode().strip()
    # Disable rename detection so both old and new package directories are tested.
    return git_paths(root, "diff", "--no-renames", "--name-only", "-z", revision, "--") | git_paths(
        root, "ls-files", "--others", "--exclude-standard", "-z"
    )


def json_objects(text):
    """go list emits consecutive JSON objects, not a JSON array."""
    decoder = json.JSONDecoder()
    offset = 0
    while offset < len(text):
        if text[offset].isspace():
            offset += 1
            continue
        value, offset = decoder.raw_decode(text, offset)
        yield value


def affected_packages(root, paths, packages):
    """Return local import paths, or ./... when selection cannot be narrowed safely."""
    directories = {Path(p["Dir"]).relative_to(root).as_posix(): p["ImportPath"] for p in packages}
    selected = set()
    for path in sorted(paths):
        file = Path(path)
        if file.name in {"go.mod", "go.sum", "go.work", "go.work.sum"} or path.startswith(".github/"):
            return ["./..."], f"configuration changed: {path}"
        # Includes deleted files, ignored-by-build Go variants, embedded assets,
        # and testdata. Prefer the deepest owning package directory.
        owner = next((directories[p.as_posix()] for p in file.parents if p.as_posix() in directories), None)
        if owner is None:
            return ["./..."], f"no current package owns: {path}"
        selected.add(owner)

    reverse = {}
    for package in packages:
        for field in ("Imports", "TestImports", "XTestImports"):
            for dependency in package.get(field, []):
                reverse.setdefault(dependency, set()).add(package["ImportPath"])
    pending = list(selected)
    while pending:
        for dependent in reverse.get(pending.pop(), set()) - selected:
            selected.add(dependent)
            pending.append(dependent)
    return sorted(selected), "changed packages and transitive reverse dependencies (including test imports)"


def select_affected(root, base, env):
    paths = changed_paths(root, base)
    if not paths:
        return [], "no changed or untracked files"
    packages = list(json_objects(capture(root, "go", "list", "-json", "./...", env=env).decode()))
    return affected_packages(root, paths, packages)


def announce(command):
    print("+ " + shlex.join(command), file=sys.stderr, flush=True)


def run(root, command, env):
    announce(command)
    subprocess.run(command, cwd=root, env=env, check=True)


def check_format(root, env):
    paths = sorted(path for path in git_paths(
        root, "ls-files", "--cached", "--others", "--exclude-standard", "-z", "--", "*.go"
    ) if (root / path).is_file())
    unformatted = []
    for start in range(0, len(paths), 200):
        output = capture(root, "gofmt", "-l", *paths[start:start + 200], env=env).decode()
        if output:
            unformatted.append(output)
    if unformatted:
        raise RuntimeError("gofmt required:\n" + "".join(unformatted))
    print("gofmt: clean", flush=True)


class Timings:
    def __init__(self):
        self.tests = []
        self.packages = []
        self.cached = set()

    def add(self, event):
        package = event.get("Package", "")
        test = event.get("Test", "")
        if not test and event.get("Output", "").rstrip().endswith("(cached)"):
            self.cached.add(package)
        if event.get("Action") in {"pass", "fail"} and "Elapsed" in event:
            if not test:
                self.packages.append((event["Elapsed"], package, ""))
            elif "/" not in test:
                self.tests.append((event["Elapsed"], package, test))

    def report(self):
        print("\nTiming summary (top-level tests; parallel durations are not additive):", flush=True)
        if self.cached:
            print(f"Excluded {len(self.cached)} cached package(s); use --fresh to measure actual execution.")
        for title, rows in (("Packages", self.packages), ("Tests", self.tests)):
            print(title + " (slowest 10):")
            # Cached JSON replays old test durations; do not present them as new measurements.
            measured = [row for row in rows if row[1] not in self.cached]
            for elapsed, package, test in sorted(measured, reverse=True)[:10]:
                print(f"  {elapsed:8.3f}s  {package}" + (f" {test}" if test else ""))
            if not measured:
                print("  (no uncached results)")


def run_tests(root, packages, env, fresh=False, pattern=None, timings=False):
    command = ["go", "test"]
    if fresh:
        command.append("-count=1")
    if pattern is not None:
        command.extend(["-run", pattern])
    if timings:
        command.append("-json")
    command.extend(packages)
    if not timings:
        run(root, command, env)
        return
    announce(command)
    report = Timings()
    with subprocess.Popen(command, cwd=root, env=env, stdout=subprocess.PIPE, text=True) as process:
        for line in process.stdout:
            event = json.loads(line)
            report.add(event)
            if "Output" in event:
                print(event["Output"], end="", flush=True)
        code = process.wait()
    report.report()
    if code:
        raise subprocess.CalledProcessError(code, command)


def parser():
    cli = argparse.ArgumentParser(description=__doc__)
    modes = cli.add_subparsers(dest="mode", required=True)
    focus = modes.add_parser("focus", help="rerun tests in explicitly named packages")
    focus.add_argument("packages", nargs="+", help="Go package paths, relative to the repository root")
    focus.add_argument("--run", help="Go test-name regular expression")
    affected = modes.add_parser("affected", help="test working-tree changes and their reverse dependencies")
    affected.add_argument("--base", default="HEAD", help="compare the working tree to this Git revision (default: HEAD)")
    full = modes.add_parser("full", help="Python tests, build, vet, formatting, and all Go tests")
    for mode in (focus, affected, full):
        mode.add_argument("--fresh", action="store_true", help="disable test-result caching (always on for focus)")
        mode.add_argument("--timings", action="store_true", help="stream verbose test output and report uncached durations")
        mode.add_argument("--list", action="store_true", help="list selected packages without running checks")
    return cli


def main(argv=None):
    args = parser().parse_args(argv)
    env = dict(os.environ, CGO_ENABLED="0")
    full = args.mode == "full"
    if args.mode == "affected":
        packages, reason = select_affected(ROOT, args.base, env)
        print(reason, file=sys.stderr)
        full = packages == ["./..."]
    elif args.mode == "focus":
        packages = args.packages
        if any(package.startswith("-") or package.endswith(".go") for package in packages):
            raise RuntimeError("focus expects package paths, not Go flags or individual source files")
    else:
        packages = ["./..."]
    if args.list:
        for package in packages:
            print(package)
        return 0
    if not packages:
        return 0
    if full:
        run(ROOT, [sys.executable, "-B", "-m", "unittest", "discover", "-s", ".github/local-ci", "-p", "test_*.py"], env)
        run(ROOT, ["go", "build", "./..."], env)
        run(ROOT, ["go", "vet", "./..."], env)
        check_format(ROOT, env)
    run_tests(ROOT, packages, env, fresh=args.fresh or args.mode == "focus",
              pattern=getattr(args, "run", None), timings=args.timings)
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except subprocess.CalledProcessError as error:
        print(f"command failed ({error.returncode}): {shlex.join(error.cmd)}", file=sys.stderr)
        sys.exit(error.returncode if error.returncode > 0 else 1)
    except (OSError, ValueError, RuntimeError) as error:
        print(error, file=sys.stderr)
        sys.exit(1)
