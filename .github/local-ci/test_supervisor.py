import json
import os
from pathlib import Path
import plistlib
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import Mock, call, patch

from supervisor import HEALTH_INTERVAL, WORKER_TIMEOUT, Supervisor, queued_local_job
from install_linux_workers import install_workers


class RecoveryTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        (self.root / "config.json").write_text(json.dumps({
            "repository": "owner/project", "gh": "gh", "tart": "tart",
            "docker": "docker", "docker_context": "orbstack",
            "platforms": {"macos": {"base_vm": "base", "label": "local-macos",
                                    "os": "macOS", "home": "/Users/admin"}}}))
        (self.root / "logs").mkdir()
        self.supervisor = Supervisor(self.root)
        self.vm = "polly-ci-job-012345abcdef"
        self.events = []
        self.supervisor.tart = Mock(side_effect=self.tart)

    def tart(self, *args, **_kwargs):
        self.events.append(args)
        return "base\n" + self.vm if args[0] == "list" else ""

    def test_offline_manual_cleanup_never_uses_github_or_deletes_base(self):
        self.supervisor.save_active({"vm": self.vm, "local": True})
        self.supervisor.api = Mock(side_effect=AssertionError("manual cleanup must work offline"))
        self.supervisor.recover()
        self.assertIn(("delete", self.vm), self.events)
        self.assertNotIn(("delete", "base"), self.events)
        self.assertFalse(self.supervisor.active_path.exists())

    def test_unknown_vm_is_not_touched(self):
        self.supervisor.save_active({"vm": "base", "local": True})
        with self.assertRaises(ValueError):
            self.supervisor.recover()
        self.assertEqual(self.events, [])

    def test_registration_survives_lost_journal_write(self):
        self.supervisor.save_active({"vm": self.vm})
        self.supervisor.api = Mock(side_effect=[
            {"total_count": 1, "runners": [{"id": 42, "name": self.vm}]},
            {"id": 42, "name": self.vm}, None])
        self.supervisor.recover()
        self.assertEqual(self.supervisor.api.call_args.args[0], "repos/owner/project/actions/runners/42")
        self.assertEqual(self.supervisor.api.call_args.kwargs, {"method": "DELETE"})
        self.assertFalse(self.supervisor.active_path.exists())

    def test_remote_failure_does_not_retain_compute_or_lose_journal(self):
        self.supervisor.save_active({"vm": self.vm, "runner_id": 42})
        self.supervisor.api = Mock(side_effect=OSError("offline"))
        with self.assertRaises(OSError):
            self.supervisor.recover()
        self.assertIn(("delete", self.vm), self.events)
        self.assertTrue(self.supervisor.active_path.exists())

    def test_different_runner_identity_is_not_deleted(self):
        self.supervisor.save_active({"vm": self.vm, "runner_id": 42})
        self.supervisor.api = Mock(return_value={"id": 42, "name": "someone-else"})
        with self.assertRaises(ValueError):
            self.supervisor.recover()
        self.assertEqual(self.supervisor.api.call_count, 1)
        self.assertTrue(self.supervisor.active_path.exists())

    def test_ephemeral_runner_already_removed_by_github(self):
        self.supervisor.save_active({"vm": self.vm, "runner_id": 42})
        self.supervisor.api = Mock(side_effect=subprocess.CalledProcessError(1, ["gh"], stderr="HTTP 404"))
        self.supervisor.recover()
        self.assertFalse(self.supervisor.active_path.exists())

    def test_vm_is_isolated_and_registration_is_only_sent_on_stdin(self):
        self.supervisor.command = Mock(return_value="")
        self.supervisor.api = Mock(return_value={"runner": {"id": 42}, "encoded_jit_config": "one-job-secret"})
        self.supervisor.recover = Mock()
        vm = Mock()
        vm.poll.return_value = None
        runner = Mock(returncode=0)
        runner.poll.return_value = 0
        with patch("supervisor.subprocess.Popen", side_effect=[vm, runner]) as start:
            self.supervisor.run_one("macos", force=True)
        vm_args, guest_args = [call.args[0] for call in start.call_args_list]
        self.assertIn("--net-softnet-block=@host", vm_args)
        self.assertIn("--no-clipboard", vm_args)
        self.assertFalse(any(arg.startswith(("--dir", "--disk", "--net-bridged")) for arg in vm_args))
        self.assertEqual(guest_args[:3], ["tart", "exec", "-i"])
        self.assertNotIn("one-job-secret", " ".join(vm_args + guest_args))
        runner.communicate.assert_called_once_with("one-job-secret\n", timeout=WORKER_TIMEOUT)

    def test_queue_matching_ignores_completed_and_hosted_jobs(self):
        self.assertFalse(queued_local_job([{"status": "completed", "labels": ["local-linux"]}], "local-linux"))
        self.assertFalse(queued_local_job([{"status": "queued", "labels": ["ubuntu-latest"]}], "local-linux"))
        self.assertTrue(queued_local_job([{"status": "queued", "labels": ["self-hosted", "local-linux"]}], "local-linux"))

    def test_both_platforms_get_turns_under_continuous_backlog(self):
        self.supervisor.config["platforms"] = {
            "macos": {"engine": "docker", "label": "local-macos"},
            "linux": {"engine": "docker", "label": "local-linux"}}
        def api(path):
            if "/jobs?" in path:
                return {"total_count": 2, "jobs": [
                    {"status": "queued", "labels": ["local-macos"]},
                    {"status": "queued", "labels": ["local-linux"]}]}
            return {"workflow_runs": [{"id": 1}]}
        self.supervisor.api = Mock(side_effect=api)
        self.supervisor.run_container = Mock()
        selected = []
        for _ in range(4):
            platform = self.supervisor.has_work()
            selected.append(platform)
            self.supervisor.run_one(platform)
        self.assertEqual(selected, ["macos", "linux", "macos", "linux"])

    def test_rotation_scans_past_newer_runs_and_checks_in_progress_runs(self):
        self.supervisor.config["platforms"] = {
            "macos": {"label": "local-macos"}, "linux": {"label": "local-linux"}}
        self.supervisor.last_platform = "macos"
        def api(path):
            if "status=queued" in path:
                return {"workflow_runs": [{"id": 1}]}
            if "status=in_progress" in path:
                return {"workflow_runs": [{"id": 2}]}
            label = "local-macos" if "/runs/1/" in path else "local-linux"
            return {"total_count": 1, "jobs": [{"status": "queued", "labels": [label]}]}
        self.supervisor.api = Mock(side_effect=api)
        self.assertEqual(self.supervisor.has_work(), "linux")
        self.assertEqual(self.supervisor.has_work("macos"), "macos")

    def test_rotation_falls_back_when_preferred_platform_has_no_work(self):
        self.supervisor.config["platforms"] = {
            "macos": {"label": "local-macos"}, "linux": {"label": "local-linux"}}
        self.supervisor.last_platform = "macos"
        self.supervisor.api = Mock(side_effect=[
            {"workflow_runs": [{"id": 1}]},
            {"total_count": 1, "jobs": [{"status": "queued", "labels": ["local-macos"]}]},
            {"workflow_runs": []}])
        self.assertEqual(self.supervisor.has_work(), "macos")

    def test_container_uses_nonroot_without_host_mounts_or_added_capabilities(self):
        config = {"engine": "docker", "image": "ci-image", "os": "Linux", "label": "local-linux"}
        args = self.supervisor.container_command(self.vm, config, "test")
        self.assertEqual(args[args.index("--user") + 1], "1000:1000")
        self.assertEqual(args[args.index("--cap-drop") + 1], "ALL")
        self.assertEqual(args[args.index("--network") + 1], "none")
        self.assertIn("no-new-privileges", args)
        self.assertIn("systempaths=unconfined", args)
        self.assertFalse(set(args) & {"--privileged", "--cap-add", "--mount", "--volume", "-v"})
        self.supervisor.api = Mock(return_value={"runner": {"id": 42}, "encoded_jit_config": "one-job-secret"})
        self.supervisor.recover = Mock()
        runner = Mock(returncode=0)
        runner.poll.return_value = 0
        with patch("supervisor.subprocess.Popen", return_value=runner) as start:
            self.supervisor.run_container(config, None)
        self.assertNotIn("one-job-secret", " ".join(start.call_args.args[0]))
        runner.communicate.assert_called_once_with("one-job-secret\n", timeout=HEALTH_INTERVAL)
        self.supervisor.tart.assert_not_called()

    def test_container_cleanup_reclaims_only_recorded_labeled_container_before_api(self):
        self.supervisor.save_active({"container": self.vm, "runner_id": 42})
        self.supervisor.command = Mock(side_effect=["redis\n" + self.vm, ""])
        self.supervisor.api = Mock(side_effect=OSError("offline"))
        with self.assertRaises(OSError):
            self.supervisor.recover()
        calls = self.supervisor.command.call_args_list
        self.assertIn("label=com.polly.ci.repository=owner/project", calls[0].args[0])
        self.assertEqual(calls[1].args[0][-3:], ["rm", "--force", self.vm])
        self.assertTrue(self.supervisor.active_path.exists())
        self.supervisor.tart.assert_not_called()

    def test_unhealthy_container_stops_docker_client_and_runs_recovery(self):
        self.supervisor.api = Mock(return_value={"runner": {"id": 42}, "encoded_jit_config": "secret"})
        self.supervisor.wait_container = Mock(side_effect=RuntimeError("container restarted"))
        self.supervisor.recover = Mock()
        process = Mock()
        process.poll.return_value = None
        config = {"image": "ci-image", "os": "Linux", "label": "local-linux"}
        with patch("supervisor.subprocess.Popen", return_value=process):
            with self.assertRaisesRegex(RuntimeError, "container restarted"):
                self.supervisor.run_container(config, None)
        process.terminate.assert_called_once()
        self.supervisor.recover.assert_called_once()

    def test_worker_resource_limits_also_bound_go_parallelism(self):
        config = {"image": "ci-image", "cpus": 3, "memory": "3g"}
        args = self.supervisor.container_command(self.vm, config, "runner")
        self.assertEqual(args[args.index("--cpus") + 1], "3")
        self.assertEqual(args[args.index("--memory") + 1], "3g")
        self.assertIn("GOMAXPROCS=3", args)
        self.assertIn("GOFLAGS=-p=3", args)
        for invalid in ({"cpus": 0}, {"cpus": True}, {"memory": "0"}, {"memory": "unlimited"}):
            with self.assertRaises(ValueError):
                self.supervisor.container_command(self.vm, dict(config, **invalid), "runner")


class ContainerHealthTests(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        root = Path(directory.name)
        (root / "config.json").write_text(json.dumps({
            "repository": "owner/project", "docker": "docker", "docker_context": "default"}))
        self.supervisor = Supervisor(root)
        self.supervisor.command = Mock(return_value=json.dumps({"Running": True, "StartedAt": "first"}))
        self.supervisor.api = Mock(return_value={"status": "online", "busy": True})
        self.now = 0
        clock = patch("supervisor.time.monotonic", side_effect=lambda: self.now)
        clock.start()
        self.addCleanup(clock.stop)
        self.process = Mock()
        self.process.poll.return_value = None
        self.finish_at = float("inf")
        def communicate(payload=None, timeout=None):
            self.now += timeout
            if self.now >= self.finish_at:
                self.process.returncode = 0
                return
            raise subprocess.TimeoutExpired(["docker", "run"], timeout)
        self.process.communicate.side_effect = communicate

    def wait(self):
        self.supervisor.wait_container(self.process, "polly-ci-job-012345abcdef", 42, "secret")

    def test_restart_is_detected_without_waiting_for_job_timeout(self):
        self.supervisor.command.side_effect = [
            json.dumps({"Running": True, "StartedAt": "first"}),
            json.dumps({"Running": True, "StartedAt": "restarted"})]
        with self.assertRaisesRegex(RuntimeError, "container restarted"):
            self.wait()
        self.assertEqual(self.now, 2 * HEALTH_INTERVAL)

    def test_quiet_busy_job_is_allowed_and_registration_is_sent_only_once(self):
        self.finish_at = 180
        self.wait()
        calls = self.process.communicate.call_args_list
        self.assertEqual(calls[0], call("secret\n", timeout=HEALTH_INTERVAL))
        self.assertTrue(all(c == call(None, timeout=HEALTH_INTERVAL) for c in calls[1:]))

    def test_offline_runner_is_reclaimed(self):
        self.supervisor.api.return_value = {"status": "offline", "busy": True}
        with self.assertRaisesRegex(RuntimeError, "remained offline"):
            self.wait()
        self.assertLess(self.now, 180)

    def test_idle_runner_releases_slot_when_another_host_claims_job(self):
        self.supervisor.api.return_value = {"status": "online", "busy": False}
        with self.assertRaisesRegex(RuntimeError, "claimed no job"):
            self.wait()
        self.assertLess(self.now, 180)

    def test_missing_runner_registration_gets_a_shutdown_grace_period(self):
        self.supervisor.api.side_effect = subprocess.CalledProcessError(1, ["gh"], stderr="HTTP 404")
        self.finish_at = 30
        self.wait()
        self.assertEqual(self.now, 30)

    def test_api_outage_does_not_kill_a_busy_worker(self):
        self.supervisor.api.side_effect = subprocess.CalledProcessError(1, ["gh"], stderr="HTTP 503")
        self.finish_at = 180
        with self.assertLogs(level="WARNING"):
            self.wait()

    def test_outage_resets_offline_evidence(self):
        self.supervisor.api.side_effect = [
            {"status": "offline"},
            OSError("network unavailable"),
            {"status": "online", "busy": True}]
        self.finish_at = 90
        with self.assertLogs(level="WARNING"):
            self.wait()

    def test_successful_exit_racing_with_inspect_is_not_reported_as_failure(self):
        self.supervisor.command.side_effect = subprocess.CalledProcessError(1, ["docker", "inspect"])
        self.process.poll.return_value = 0
        self.wait()

    def test_stopped_container_waits_for_docker_client_to_report_success(self):
        self.supervisor.command.return_value = json.dumps({"Running": False, "StartedAt": "first"})
        self.finish_at = 20
        self.wait()
        self.assertEqual(self.process.returncode, 0)
        self.assertEqual(self.now, 20)

    def test_stopped_container_preserves_nonzero_client_exit(self):
        self.supervisor.command.return_value = json.dumps({"Running": False, "StartedAt": "first"})
        def communicate(payload=None, timeout=None):
            self.now += timeout
            if self.now == 10:
                raise subprocess.TimeoutExpired(["docker", "run"], timeout)
            self.process.returncode = 17
        self.process.communicate.side_effect = communicate
        self.wait()
        self.assertEqual(self.process.returncode, 17)

    def test_stopped_container_with_hung_client_has_bounded_shutdown(self):
        self.supervisor.command.return_value = json.dumps({"Running": False, "StartedAt": "first"})
        with self.assertRaisesRegex(RuntimeError, "Docker client did not exit"):
            self.wait()
        self.assertEqual(self.now, 20)

    def test_unresponsive_job_still_has_an_absolute_deadline(self):
        with self.assertRaisesRegex(RuntimeError, "35-minute worker limit"):
            self.wait()
        self.assertEqual(self.now, WORKER_TIMEOUT)


class EntrypointTests(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        root = Path(directory.name)
        runner = root / "actions-runner"
        runner.mkdir()
        script = runner / "run.sh"
        script.write_text('#!/bin/bash\n[[ "$1" == --jitconfig && "$2" == fixture ]] || exit 2\necho started\n')
        script.chmod(0o700)
        self.env = dict(os.environ, HOME=str(root), POLLY_CI_CONFIG_TIMEOUT="1")
        self.command = ["/bin/bash", str(Path(__file__).with_name("entrypoint.sh")), "runner"]

    def test_missing_config_with_open_stdin_exits_instead_of_hanging(self):
        with subprocess.Popen(self.command, env=self.env, stdin=subprocess.PIPE,
                              stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True) as process:
            try:
                self.assertEqual(process.wait(timeout=5), 1)
                _, error = process.communicate()
            finally:
                if process.poll() is None:
                    process.kill()
            self.assertIn("configuration missing or timed out", error)

    def test_empty_config_fails_and_valid_config_starts_runner(self):
        for config, expected in [("", 1), ("\n", 1), ("fixture\n", 0)]:
            result = subprocess.run(self.command, env=self.env, input=config,
                                    capture_output=True, text=True, timeout=5)
            self.assertEqual(result.returncode, expected, result.stderr)
            if expected == 0:
                self.assertEqual(result.stdout.strip(), "started")


class WorkerInstallTests(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.root = Path(directory.name) / "ci"
        self.root.mkdir()
        self.agents = Path(directory.name) / "agents"
        self.config = {"repository": "owner/project", "gh": "gh", "docker": "docker",
                       "docker_context": "default", "platforms": {
                           "macos": {"base_vm": "base", "os": "macOS"},
                           "linux": {"engine": "docker", "image": "ci-image", "os": "Linux", "label": "local-linux"}}}
        (self.root / "config.json").write_text(json.dumps(self.config))

    def test_three_workers_have_independent_state_locks_logs_and_services(self):
        workers = install_workers(self.root, self.agents)
        self.assertEqual(len(workers), 3)
        for index, worker in enumerate(workers, 1):
            config = json.loads((worker / "config.json").read_text())
            self.assertEqual(list(config["platforms"]), ["linux"])
            self.assertEqual(config["platforms"]["linux"]["cpus"], 3)
            self.assertEqual(config["platforms"]["linux"]["memory"], "3g")
            self.assertEqual(config["platforms"]["linux"]["image"], "ci-image")
            definition = plistlib.loads((self.agents / f"com.polly.ci.linux-{index}.plist").read_bytes())
            self.assertEqual(definition["ProgramArguments"][-2:], ["--root", str(worker)])
            self.assertEqual(definition["StandardOutPath"], str(worker / "logs/launchd.out.log"))
            supervisor = Supervisor(worker)
            supervisor.save_active({"container": f"worker-{index}"})
        self.assertFalse((self.root / "active.json").exists())
        self.assertEqual([json.loads((w / "active.json").read_text())["container"] for w in workers],
                         ["worker-1", "worker-2", "worker-3"])
        self.assertEqual(json.loads((self.root / "config.json").read_text()), self.config)
        self.assertEqual(install_workers(self.root, self.agents), workers)

    def test_conflicting_configuration_is_rejected_before_creating_other_workers(self):
        other = self.root / "linux-2"
        other.mkdir()
        (other / "config.json").write_text('{"repository":"another/project"}')
        with self.assertRaisesRegex(ValueError, "configuration differs"):
            install_workers(self.root, self.agents)
        self.assertFalse((self.root / "linux-1").exists())
        self.assertFalse(self.agents.exists())

    def test_invalid_pool_resources_do_not_create_workers(self):
        for settings in ({"count": 0}, {"cpus": 0}, {"memory": "0"}):
            with self.assertRaises(ValueError):
                install_workers(self.root, self.agents, **settings)
        self.assertFalse((self.root / "linux-1").exists())


CROSS_TARGETS = {("linux", "amd64"), ("linux", "arm64"), ("darwin", "amd64"), ("darwin", "arm64"), ("windows", "amd64")}


def cross_targets(calls):
    return {(call["env"]["GOOS"], call["env"]["GOARCH"]) for call in calls if call["env"]["GOOS"]}


class CommandTests(unittest.TestCase):
    def run_ci(self, mode):
        """Run ci.sh with a probe in place of go and return the recorded go invocations."""
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / ".github/local-ci").mkdir(parents=True)
            (root / ".github/local-ci/test_fixture.py").write_text(
                "import unittest\nclass Fixture(unittest.TestCase):\n def test_fixture(self): pass\n")
            script = root / ".github/ci.sh"
            shutil.copyfile(Path(__file__).parents[1] / "ci.sh", script)
            script.chmod(0o700)
            (root / "bin").mkdir()
            go = root / "bin/go"
            go.write_text(f"#!{sys.executable}\n" +
                          "import json,os,sys\n" +
                          "with open(os.environ['CI_PROBE_LOG'],'a') as out:\n" +
                          " out.write(json.dumps({'args':sys.argv[1:],'env':{k:os.getenv(k) for k in ['CGO_ENABLED','GOOS','GOARCH','POLLYTOOL_REQUIRE_SANDBOX_TESTS']}})+'\\n')\n")
            go.chmod(0o700)
            logfile = root / "calls.jsonl"
            env = dict(os.environ, PATH=str(root / "bin") + os.pathsep + os.environ["PATH"], CI_PROBE_LOG=str(logfile))
            subprocess.run([str(script), mode], env=env, check=True, capture_output=True, text=True)
            calls = [json.loads(line) for line in logfile.read_text().splitlines()]
        for call in calls:
            if "-o" in call["args"]:
                output = Path(call["args"][call["args"].index("-o") + 1])
                self.assertFalse(output.parent.exists(), "cross-build temporary directory leaked")
        return calls

    def test_all_runs_sandbox_suite_once_then_race_and_every_cross_target(self):
        calls = self.run_ci("all")
        self.assertEqual(len(calls), 9)
        [suite] = [call for call in calls if call["args"] == ["test", "./..."]]
        self.assertEqual(suite["env"]["POLLYTOOL_REQUIRE_SANDBOX_TESTS"], "1")
        self.assertEqual(suite["env"]["CGO_ENABLED"], "0")
        [race] = [call for call in calls if "-race" in call["args"]]
        self.assertEqual(race["env"]["CGO_ENABLED"], "1")
        self.assertLess(calls.index(suite), calls.index(race))
        self.assertEqual(cross_targets(calls), CROSS_TARGETS)

    def test_warm_compiles_every_mode_without_running_tests(self):
        calls = self.run_ci("warm")
        tests = [call for call in calls if call["args"][0] == "test"]
        self.assertEqual(len(tests), 2)
        for call in tests:
            self.assertEqual(call["args"][call["args"].index("-run") + 1], "^$")
        [race] = [call for call in tests if "-race" in call["args"]]
        self.assertEqual(race["env"]["CGO_ENABLED"], "1")
        self.assertEqual(cross_targets(calls), CROSS_TARGETS)


if __name__ == "__main__":
    unittest.main()
