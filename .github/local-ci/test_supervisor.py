import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import Mock, patch

from supervisor import Supervisor, queued_local_job


class RecoveryTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        (self.root / "config.json").write_text(json.dumps({
            "repository": "owner/project", "gh": "gh", "tart": "tart",
            "platforms": {"linux": {"base_vm": "base", "label": "local-linux",
                                      "os": "Linux", "home": "/home/admin"}}}))
        (self.root / "logs").mkdir()
        self.supervisor = Supervisor(self.root)
        self.vm = "polly-ci-job-012345abcdef"
        self.events = []
        self.supervisor.tart = Mock(side_effect=self.tart)
        self.stop = patch("supervisor.subprocess.run", side_effect=lambda *a, **k: self.events.append("stop"))
        self.stop.start()
        self.addCleanup(self.stop.stop)

    def tart(self, *args):
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
        def tart(*args):
            if args[0] == "ip":
                return "192.168.64.2"
            return ""
        self.supervisor.tart = Mock(side_effect=tart)
        self.supervisor.command = Mock(return_value="")
        self.supervisor.api = Mock(return_value={"runner": {"id": 42}, "encoded_jit_config": "one-job-secret"})
        self.supervisor.recover = Mock()
        vm = Mock()
        vm.poll.return_value = None
        runner = Mock(returncode=0)
        runner.poll.return_value = 0
        with patch("supervisor.subprocess.Popen", side_effect=[vm, runner]) as start:
            self.supervisor.run_one("linux", force=True)
        vm_args, guest_args = [call.args[0] for call in start.call_args_list]
        self.assertIn("--net-softnet-block=@host", vm_args)
        self.assertIn("--no-clipboard", vm_args)
        self.assertFalse(any(arg.startswith(("--dir", "--disk", "--net-bridged")) for arg in vm_args))
        self.assertEqual(guest_args[:3], ["tart", "exec", "-i"])
        self.assertNotIn("one-job-secret", " ".join(vm_args + guest_args))
        runner.communicate.assert_called_once_with("one-job-secret\n", timeout=35 * 60)

    def test_queue_matching_ignores_completed_and_hosted_jobs(self):
        self.assertFalse(queued_local_job([{"status": "completed", "labels": ["local-linux"]}], "local-linux"))
        self.assertFalse(queued_local_job([{"status": "queued", "labels": ["ubuntu-latest"]}], "local-linux"))
        self.assertTrue(queued_local_job([{"status": "queued", "labels": ["self-hosted", "local-linux"]}], "local-linux"))

    def test_container_uses_nonroot_without_host_mounts_or_added_capabilities(self):
        self.supervisor.config.update(docker="docker", docker_context="orbstack")
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
        runner.communicate.assert_called_once_with("one-job-secret\n", timeout=35 * 60)
        self.supervisor.tart.assert_not_called()

    def test_container_cleanup_reclaims_only_recorded_labeled_container_before_api(self):
        self.supervisor.config.update(docker="docker", docker_context="orbstack")
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


class CommandTests(unittest.TestCase):
    def test_all_runs_sandbox_suite_once_then_race_and_every_cross_target(self):
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
            subprocess.run([str(script), "all"], env=env, check=True, capture_output=True, text=True)
            calls = [json.loads(line) for line in logfile.read_text().splitlines()]
            self.assertEqual(len(calls), 9)
            self.assertEqual(calls[2]["args"], ["test", "./..."])
            self.assertEqual(calls[2]["env"]["POLLYTOOL_REQUIRE_SANDBOX_TESTS"], "1")
            self.assertEqual(calls[2]["env"]["CGO_ENABLED"], "0")
            self.assertEqual(calls[3]["args"][:2], ["test", "-race"])
            self.assertEqual(calls[3]["env"]["CGO_ENABLED"], "1")
            targets = {(c["env"]["GOOS"], c["env"]["GOARCH"]) for c in calls[4:]}
            self.assertEqual(targets, {("linux", "amd64"), ("linux", "arm64"), ("darwin", "amd64"), ("darwin", "arm64"), ("windows", "amd64")})
            for call in calls[4:]:
                output = Path(call["args"][call["args"].index("-o") + 1])
                self.assertFalse(output.parent.exists(), "cross-build temporary directory leaked")


if __name__ == "__main__":
    unittest.main()
