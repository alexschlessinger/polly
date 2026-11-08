import contextlib
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import Mock, patch

spec = importlib.util.spec_from_file_location("test_runner", Path(__file__).resolve().parents[1] / "test.py")
runner = importlib.util.module_from_spec(spec)
spec.loader.exec_module(runner)


class SelectionTests(unittest.TestCase):
    root = Path("/repo")

    def packages(self):
        return [
            {"Dir": "/repo/base", "ImportPath": "m/base"},
            {"Dir": "/repo/consumer", "ImportPath": "m/consumer", "Imports": ["m/base"]},
            {"Dir": "/repo/testonly", "ImportPath": "m/testonly", "TestImports": ["m/consumer"]},
            {"Dir": "/repo/external", "ImportPath": "m/external", "XTestImports": ["m/testonly"]},
            {"Dir": "/repo/other", "ImportPath": "m/other"},
            {"Dir": "/repo/base/nested", "ImportPath": "m/nested"},
        ]

    def selected(self, *paths):
        return runner.affected_packages(self.root, set(paths), self.packages())[0]

    def test_transitive_production_and_test_imports(self):
        self.assertEqual(self.selected("base/source.go"), ["m/base", "m/consumer", "m/external", "m/testonly"])

    def test_assets_deleted_files_and_other_platforms_have_owners(self):
        for path in ("base/testdata/file with spaces.txt", "base/deleted_test.go", "base/source_windows.go"):
            with self.subTest(path=path):
                self.assertIn("m/consumer", self.selected(path))

    def test_deepest_package_owns_asset(self):
        self.assertEqual(self.selected("base/nested/assets/prompt.md"), ["m/nested"])

    def test_multiple_packages_are_deduplicated(self):
        self.assertEqual(self.selected("other/a.go", "other/b.go", "base/nested/a.go"), ["m/nested", "m/other"])

    def test_configuration_unknown_and_removed_packages_fall_back(self):
        for path in ("go.mod", "go.sum", "go.work", "go.work.sum", ".github/ci.sh", "removed/a.go", "docs/CLI.md"):
            with self.subTest(path=path):
                self.assertEqual(self.selected(path), ["./..."])

    def test_empty_changes_do_not_test_or_list_packages(self):
        with patch.object(runner, "changed_paths", return_value=set()), patch.object(runner, "capture") as capture:
            self.assertEqual(runner.select_affected(self.root, "HEAD", {})[0], [])
            capture.assert_not_called()

    def test_json_stream(self):
        self.assertEqual(list(runner.json_objects(' \n{"a": 1}\n {"b": 2}\n')), [{"a": 1}, {"b": 2}])
        with self.assertRaises(ValueError):
            list(runner.json_objects('{"a": 1}\ninvalid'))

    def test_missing_dependency_graph_is_an_error_not_an_empty_selection(self):
        with patch.object(runner, "changed_paths", return_value={"base/a.go"}), patch.object(
            runner, "capture", side_effect=subprocess.CalledProcessError(1, ["go", "list"])
        ):
            with self.assertRaises(subprocess.CalledProcessError):
                runner.select_affected(self.root, "HEAD", {})


class GitSelectionTests(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.root = Path(directory.name).resolve()
        self.env = dict(os.environ, CGO_ENABLED="0", GOWORK="off")
        self.git("init", "-q")
        self.write("go.mod", "module example.test/fixture\n\ngo 1.27.0\n")
        self.write("base/base.go", "package base\n")
        self.write("consumer/consumer.go", 'package consumer\n\nimport _ "example.test/fixture/base"\n')
        self.write("external/external.go", "package external\n")
        self.write("external/external_test.go", 'package external_test\n\nimport _ "example.test/fixture/consumer"\n')
        self.write(".gitignore", "ignored/\n")
        self.git("add", ".")
        self.commit()

    def git(self, *args):
        return runner.capture(self.root, "git", *args).decode().strip()

    def commit(self):
        self.git("-c", "user.name=Test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false",
                 "commit", "-qm", "fixture")

    def write(self, path, text):
        file = self.root / path
        file.parent.mkdir(parents=True, exist_ok=True)
        file.write_text(text)

    def test_real_graph_untracked_files_and_clean_tree(self):
        self.assertEqual(runner.select_affected(self.root, "HEAD", self.env)[0], [])
        self.write("base/new_test.go", "package base\n")
        self.write("ignored/unused.txt", "not part of the change set")
        self.assertEqual(runner.changed_paths(self.root, "HEAD"), {"base/new_test.go"})
        packages, _ = runner.select_affected(self.root, "HEAD", self.env)
        self.assertEqual(packages, ["example.test/fixture/base", "example.test/fixture/consumer", "example.test/fixture/external"])

    def test_staged_unstaged_renamed_deleted_and_unusual_paths(self):
        self.git("mv", "base/base.go", "consumer/moved.go")
        self.write("consumer/consumer.go", "changed but not staged")
        self.git("rm", "external/external.go")
        self.write("base/space and\nnewline.go", "package base\n")
        self.assertEqual(runner.changed_paths(self.root, "HEAD"), {
            "base/base.go", "consumer/moved.go", "consumer/consumer.go", "external/external.go", "base/space and\nnewline.go"
        })

    def test_explicit_base_includes_committed_changes(self):
        base = self.git("rev-parse", "HEAD")
        self.write("base/base.go", "package base\n// changed\n")
        self.git("add", ".")
        self.commit()
        self.assertEqual(runner.changed_paths(self.root, "HEAD"), set())
        self.assertEqual(runner.changed_paths(self.root, base), {"base/base.go"})

    def test_formatting_skips_deleted_files_and_checks_untracked(self):
        (self.root / "base/base.go").unlink()
        self.write("consumer/new.go", "package consumer\nfunc f( ){ }\n")
        with self.assertRaisesRegex(RuntimeError, "consumer/new.go"):
            runner.check_format(self.root, self.env)
        self.write("consumer/new.go", "package consumer\n\nfunc f() {}\n")
        with contextlib.redirect_stdout(io.StringIO()):
            runner.check_format(self.root, self.env)


class CommandTests(unittest.TestCase):
    def setUp(self):
        stack = contextlib.ExitStack()
        self.addCleanup(stack.close)
        self.run = stack.enter_context(patch.object(runner, "run"))
        self.tests = stack.enter_context(patch.object(runner, "run_tests"))
        self.format = stack.enter_context(patch.object(runner, "check_format"))
        stack.enter_context(contextlib.redirect_stdout(io.StringIO()))
        stack.enter_context(contextlib.redirect_stderr(io.StringIO()))

    def test_focus_reruns_only_requested_packages_and_filter(self):
        self.assertEqual(runner.main(["focus", "./cmd/polly", "--run", "TestStatusRow"]), 0)
        self.run.assert_not_called()
        self.format.assert_not_called()
        self.assertEqual(self.tests.call_args.args[1], ["./cmd/polly"])
        self.assertEqual(self.tests.call_args.args[2]["CGO_ENABLED"], "0")
        self.assertEqual(self.tests.call_args.kwargs, {"fresh": True, "pattern": "TestStatusRow", "timings": False})

    def test_focus_rejects_source_files(self):
        with self.assertRaises(RuntimeError):
            runner.main(["focus", "cmd/polly/repl_status.go"])
        self.tests.assert_not_called()

    def test_full_keeps_caching_and_runs_every_gate(self):
        runner.main(["full", "--timings"])
        self.assertEqual([call.args[1][1] for call in self.run.call_args_list], ["-B", "build", "vet"])
        self.format.assert_called_once()
        self.assertEqual(self.tests.call_args.args[1], ["./..."])
        self.assertFalse(self.tests.call_args.kwargs["fresh"])
        self.assertTrue(self.tests.call_args.kwargs["timings"])

    def test_failure_stops_before_go_tests(self):
        self.run.side_effect = subprocess.CalledProcessError(1, ["go", "build"])
        with self.assertRaises(subprocess.CalledProcessError):
            runner.main(["full"])
        self.tests.assert_not_called()

    def test_affected_uses_cache_or_explicit_fresh_and_base(self):
        with patch.object(runner, "select_affected", return_value=(["m/base", "m/consumer"], "selected")) as select:
            runner.main(["affected", "--base", "main", "--fresh"])
            self.assertEqual(select.call_args.args[1], "main")
            self.assertEqual(self.tests.call_args.args[1], ["m/base", "m/consumer"])
            self.assertTrue(self.tests.call_args.kwargs["fresh"])
            self.run.assert_not_called()
            runner.main(["affected"])
            self.assertFalse(self.tests.call_args.kwargs["fresh"])

    def test_affected_fallback_runs_full_checks(self):
        with patch.object(runner, "select_affected", return_value=(["./..."], "configuration changed")):
            runner.main(["affected"])
        self.assertEqual(self.run.call_count, 3)
        self.format.assert_called_once()

    def test_list_and_no_changes_do_not_run_checks(self):
        with patch.object(runner, "select_affected", return_value=(["./..."], "configuration changed")):
            runner.main(["affected", "--list"])
        with patch.object(runner, "select_affected", return_value=([], "no changes")):
            runner.main(["affected"])
        self.run.assert_not_called()
        self.tests.assert_not_called()
        self.format.assert_not_called()


class TimingTests(unittest.TestCase):
    def test_top_level_timings_exclude_cache_replays_and_subtests(self):
        report = runner.Timings()
        for package, test, elapsed in (("fresh", "TestParent", 2), ("fresh", "TestParent/sub", 1),
                                       ("fresh", "", 3), ("cached", "TestOld", 50), ("cached", "", 0)):
            report.add({"Action": "pass", "Package": package, "Test": test, "Elapsed": elapsed})
        report.add({"Action": "output", "Package": "cached", "Output": "ok cached (cached)\n"})
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            report.report()
        self.assertIn("TestParent", out.getvalue())
        self.assertIn("Excluded 1 cached package", out.getvalue())
        self.assertNotIn("TestOld", out.getvalue())
        self.assertNotIn("TestParent/sub", out.getvalue())

    def test_report_limit(self):
        report = runner.Timings()
        for n in range(20):
            report.add({"Action": "pass", "Package": "pkg", "Test": f"Test{n}", "Elapsed": n})
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            report.report()
        self.assertEqual(out.getvalue().count(" pkg Test"), 10)
        self.assertNotIn("pkg Test0\n", out.getvalue())

    def test_timing_mode_preserves_failed_test_exit_status(self):
        process = Mock()
        process.stdout = [json.dumps({"Action": "output", "Output": "failure detail\n"}) + "\n",
                          json.dumps({"Action": "fail", "Package": "pkg", "Test": "TestBad", "Elapsed": 1}) + "\n"]
        process.wait.return_value = 7
        manager = Mock()
        manager.__enter__ = Mock(return_value=process)
        manager.__exit__ = Mock(return_value=False)
        out = io.StringIO()
        with patch.object(runner.subprocess, "Popen", return_value=manager) as start, contextlib.redirect_stdout(out), contextlib.redirect_stderr(io.StringIO()):
            with self.assertRaises(subprocess.CalledProcessError) as failure:
                runner.run_tests(Path("/repo"), ["./pkg"], {}, fresh=True, pattern="TestBad", timings=True)
        self.assertEqual(failure.exception.returncode, 7)
        self.assertEqual(start.call_args.args[0], ["go", "test", "-count=1", "-run", "TestBad", "-json", "./pkg"])
        self.assertIn("failure detail", out.getvalue())
        self.assertIn("TestBad", out.getvalue())

    def test_plain_mode_keeps_cache_by_default(self):
        with patch.object(runner, "run") as run:
            runner.run_tests(Path("/repo"), ["./pkg"], {})
        self.assertEqual(run.call_args.args[1], ["go", "test", "./pkg"])


if __name__ == "__main__":
    unittest.main()
