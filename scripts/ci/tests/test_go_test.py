# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Go test sharding regressions with synthetic discovery and local Go fixtures."""

from collections import Counter
from contextlib import redirect_stderr, redirect_stdout
import io
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import go_test


def discovery_output(names, package="example.com/fixture"):
    return "\n".join(
        json.dumps({"Action": "output", "Package": package, "Output": name + "\n"})
        for name in names
    )


class ShardingTests(unittest.TestCase):
    def setUp(self):
        self.names = [f"TestCase{index:03}" for index in range(100)]
        self.names += ["TestParent", "Example", "Example_widget", "FuzzInput"]
        self.go_args = [
            "-tags=envtest", "./first", "./second", "-count=1", "-v", "-timeout=40m",
        ]

    def test_discovery_includes_examples_and_fuzz_but_not_benchmarks_or_log_lines(self):
        output = discovery_output([
            *self.names, "BenchmarkNotRun", "PASS", "=== RUN   TestCase000",
            "TestParent/child", "ok\texample.com/fixture\t0.01s",
        ])
        output += "\n" + json.dumps({"Action": "pass", "Package": "example.com/fixture"})
        self.assertEqual(go_test.discover_tests(output), sorted(self.names))

    def test_discovery_handles_batched_lines_and_duplicate_names_across_packages(self):
        output = discovery_output(["TestShared\nTestOther"])
        output += "\n" + discovery_output(["TestShared"], "example.com/second")
        self.assertEqual(go_test.discover_tests(output), ["TestOther", "TestShared"])

    def test_shards_are_complete_disjoint_and_stable_when_tests_are_added(self):
        seen = []
        for index in range(1, 5):
            selected = go_test.select_tests(self.names, index, 4)
            self.assertTrue(selected)
            seen.extend(selected)
            expanded = go_test.select_tests([*self.names, "TestNewRegression"], index, 4)
            self.assertEqual([name for name in expanded if name in self.names], selected)
            self.assertEqual(
                go_test.select_tests(list(reversed(self.names)), index, 4),
                list(reversed(selected)),
            )
        self.assertEqual(Counter(seen), Counter(self.names))

    def test_default_runs_original_command_without_discovery(self):
        with patch.object(go_test.subprocess, "run") as discover:
            with patch.object(go_test.subprocess, "call", return_value=0) as run:
                self.assertEqual(go_test.main(["--", *self.go_args]), 0)
        discover.assert_not_called()
        run.assert_called_once_with(["go", "test", *self.go_args])

    def test_sharded_command_keeps_flags_and_matches_only_its_exact_top_level_tests(self):
        for index in range(1, 5):
            with self.subTest(index=index):
                with patch.object(
                    go_test.subprocess, "run",
                    return_value=subprocess.CompletedProcess([], 0, discovery_output(self.names)),
                ) as discover, patch.object(
                    go_test.subprocess, "call", return_value=0,
                ) as run, redirect_stdout(io.StringIO()) as output:
                    self.assertEqual(go_test.main([
                        "--shard-index", str(index), "--shard-count", "4", "--", *self.go_args,
                    ]), 0)
                discover.assert_called_once_with(
                    ["go", "test", *self.go_args, "-json", "-list", "."],
                    check=True, text=True, stdout=subprocess.PIPE,
                )
                command = run.call_args.args[0]
                self.assertEqual(command[:-1], ["go", "test", *self.go_args])
                pattern = command[-1].removeprefix("-run=")
                selected = go_test.select_tests(self.names, index, 4)
                self.assertEqual(
                    [name for name in self.names if re.search(pattern, name)], selected,
                )
                for name in selected:
                    self.assertIsNone(re.search(pattern, name + "Extra"))
                self.assertIn(f"Go test shard {index}/4:", output.getvalue())

    def test_invalid_shards_and_selection_flags_fail_before_go(self):
        arguments = [
            ["--shard-count", "0", "--", "./..."],
            ["--shard-count", "-1", "--", "./..."],
            ["--shard-index", "0", "--", "./..."],
            ["--shard-index", "5", "--shard-count", "4", "--", "./..."],
            ["--shard-index", "not-an-integer", "--", "./..."],
            [],
        ]
        arguments.extend(
            ["--", "./...", flag]
            for flag in ("-run=TestOne", "-test.run=TestOne", "-skip=TestOne",
                         "-list=.", "-bench=.", "-fuzz=FuzzOne", "-args")
        )
        with patch.object(go_test.subprocess, "run") as discover:
            with patch.object(go_test.subprocess, "call") as run:
                for args in arguments:
                    with self.subTest(args=args), redirect_stderr(io.StringIO()) as error:
                        with self.assertRaises(SystemExit) as result:
                            go_test.main(args)
                        self.assertEqual(result.exception.code, 2)
                        self.assertIn("error:", error.getvalue())
        discover.assert_not_called()
        run.assert_not_called()

    def test_discovery_failure_preserves_diagnostics_and_never_runs_partial_selection(self):
        with patch.object(
            go_test.subprocess, "run",
            side_effect=subprocess.CalledProcessError(7, "go", output="fixture build failed\n"),
        ), patch.object(go_test.subprocess, "call") as run:
            with redirect_stdout(io.StringIO()) as output, redirect_stderr(io.StringIO()) as error:
                self.assertEqual(go_test.main(["--shard-count", "4", "--", "./..."]), 7)
        run.assert_not_called()
        self.assertIn("fixture build failed", output.getvalue())
        self.assertIn("discovery failed (exit 7)", error.getvalue())

    def test_empty_or_malformed_discovery_fails_closed(self):
        outputs = ("", "not JSON", "[]", discovery_output(["BenchmarkOnly", "PASS"]))
        for output in outputs:
            with self.subTest(output=output):
                with patch.object(
                    go_test.subprocess, "run",
                    return_value=subprocess.CompletedProcess([], 0, output),
                ), patch.object(go_test.subprocess, "call") as run:
                    with redirect_stderr(io.StringIO()) as error:
                        self.assertEqual(go_test.main(["--shard-count", "4", "--", "./..."]), 1)
                run.assert_not_called()
                self.assertIn("ERROR:", error.getvalue())

    def test_empty_shard_fails_instead_of_passing_without_tests(self):
        empty_index = next(
            index for index in range(1, 5) if not go_test.select_tests(["TestOnly"], index, 4)
        )
        with patch.object(
            go_test.subprocess, "run",
            return_value=subprocess.CompletedProcess([], 0, discovery_output(["TestOnly"])),
        ), patch.object(go_test.subprocess, "call") as run:
            with redirect_stderr(io.StringIO()) as error:
                self.assertEqual(go_test.main([
                    "--shard-count", "4", "--shard-index", str(empty_index), "--", "./...",
                ]), 1)
        run.assert_not_called()
        self.assertIn("contains no runnable tests", error.getvalue())

    def test_test_failure_propagates_for_both_full_and_sharded_runs(self):
        for count in (1, 4):
            with self.subTest(count=count):
                with patch.object(
                    go_test.subprocess, "run",
                    return_value=subprocess.CompletedProcess([], 0, discovery_output(self.names)),
                ), patch.object(go_test.subprocess, "call", return_value=1):
                    with redirect_stdout(io.StringIO()):
                        self.assertEqual(go_test.main([
                            "--shard-count", str(count), "--", *self.go_args,
                        ]), 1)

    def test_missing_go_is_an_explicit_failure(self):
        with patch.object(go_test.subprocess, "call", side_effect=FileNotFoundError("go missing")):
            with redirect_stderr(io.StringIO()) as error:
                self.assertEqual(go_test.main(["--", "./..."]), 1)
        self.assertIn("ERROR: go missing", error.getvalue())

    @unittest.skipUnless(shutil.which("go"), "Go is required for the local runner integration test")
    def test_real_go_runs_each_test_subtest_example_and_fuzz_seed_exactly_once(self):
        with tempfile.TemporaryDirectory(prefix="cetirun-go-shards-") as temp:
            root = Path(temp)
            (root / "go.mod").write_text("module example.com/shardfixture\n\ngo 1.20\n")
            (root / "fixture_test.go").write_text(
                'package fixture\nimport ("fmt"; "testing")\n'
                + "\n".join(
                    f"func TestCase{index:03}(t *testing.T) {{}}" for index in range(20)
                )
                + '\nfunc TestShared(t *testing.T) {}\n'
                'func TestParent(t *testing.T) { t.Run("child", func(t *testing.T) {}) }\n'
                'func Example() { fmt.Println("fixture")\n// Output: fixture\n}\n'
                'func FuzzInput(f *testing.F) { f.Add("seed"); f.Fuzz(func(t *testing.T, s string) {}) }\n'
                'func BenchmarkNotRun(b *testing.B) { b.Fatal("benchmarks must not run") }\n'
            )
            second = root / "second"
            second.mkdir()
            (second / "fixture_test.go").write_text(
                'package second\nimport "testing"\nfunc TestShared(t *testing.T) {}\n'
            )
            env = {
                **os.environ, "GOPROXY": "off", "GOSUMDB": "off", "GOTOOLCHAIN": "local",
                "GOWORK": "off", "GOFLAGS": "-mod=readonly", "GOTELEMETRY": "off",
            }
            go_args = ["./...", "-json", "-count=1", "-timeout=2m"]

            def passed(command):
                result = subprocess.run(
                    command, cwd=root, env=env, text=True, capture_output=True, timeout=180,
                )
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                events = [json.loads(line) for line in result.stdout.splitlines() if line.startswith("{")]
                return Counter(
                    (event["Package"], event["Test"])
                    for event in events if event["Action"] == "pass" and "Test" in event
                )

            expected = passed(["go", "test", *go_args])
            self.assertIn(("example.com/shardfixture", "TestParent/child"), expected)
            self.assertIn(("example.com/shardfixture", "Example"), expected)
            self.assertIn(("example.com/shardfixture", "FuzzInput/seed#0"), expected)
            self.assertIn(("example.com/shardfixture/second", "TestShared"), expected)
            actual = Counter()
            for index in range(1, 5):
                actual.update(passed([
                    sys.executable, "-B", str(Path(go_test.__file__).resolve()),
                    "--shard-index", str(index), "--shard-count", "4", "--", *go_args,
                ]))
            self.assertEqual(actual, expected)


if __name__ == "__main__":
    unittest.main()
