# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import http.client
import json
import os
from pathlib import Path
import subprocess
import sys
import threading
import tomllib
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import runtime


MESSAGES = [{"role": "user", "content": "Review the synthetic pricing.py."}]


class HTTPTests(unittest.TestCase):
    def setUp(self):
        self.server = runtime.Server(("127.0.0.1", 0), "copilot")
        self.thread = threading.Thread(target=self.server.serve_forever)
        self.thread.start()
        self.connection = http.client.HTTPConnection(*self.server.server_address, timeout=3)

    def tearDown(self):
        self.connection.close()
        self.server.shutdown()
        self.server.server_close()
        self.thread.join()

    def request(self, method, path, body=None):
        self.connection.request(
            method, path, json.dumps(body) if body is not None else None,
            {"Content-Type": "application/json"},
        )
        response = self.connection.getresponse()
        return response.status, response.read().decode(), response.headers

    def body(self, **changes):
        return {"model": "mock/copilot", "messages": MESSAGES, **changes}

    def test_readiness_does_not_start_a_runtime(self):
        with patch.object(runtime, "run", side_effect=AssertionError("Must not run")):
            status, body, _ = self.request("GET", "/healthz")
        self.assertEqual(status, 200)
        self.assertEqual(json.loads(body), {"status": "ok"})

    def test_catalog_and_chat_for_all_three_runtimes(self):
        for name in runtime.RUNTIMES:
            with self.subTest(runtime=name):
                self.server.runtime = name
                self.server.model = f"mock/{name}"
                status, body, _ = self.request("GET", "/v1/models")
                self.assertEqual(status, 200)
                self.assertEqual(json.loads(body)["data"][0]["id"], f"mock/{name}")
                status, body, _ = self.request(
                    "POST", "/v1/chat/completions", self.body(model=f"mock/{name}")
                )
                self.assertEqual(status, 200)
                reply = json.loads(body)["choices"][0]["message"]["content"]
                self.assertIn(f"OFFLINE MOCK / {name}", reply)
                self.assertIn("pricing.py:6", reply)

    def test_sse_finishes_after_content(self):
        status, body, headers = self.request(
            "POST", "/v1/chat/completions", self.body(stream=True)
        )
        self.assertEqual(status, 200)
        self.assertEqual(headers["Content-Type"], "text/event-stream")
        events = [line[6:] for line in body.splitlines() if line.startswith("data: ")]
        self.assertEqual(events[-1], "[DONE]")
        self.assertIn("OFFLINE MOCK", json.loads(events[0])["choices"][0]["delta"]["content"])
        self.assertEqual(json.loads(events[-2])["choices"][0]["finish_reason"], "stop")

    def test_history_is_per_request(self):
        messages = [
            {"role": "user", "content": "First question."},
            {"role": "assistant", "content": "First answer."},
            *MESSAGES,
        ]
        _, body, _ = self.request("POST", "/v1/chat/completions", self.body(messages=messages))
        self.assertIn("Received 3", body)
        _, body, _ = self.request("POST", "/v1/chat/completions", self.body())
        self.assertIn("Received 1", body)

    def test_busy_request_is_not_queued_or_replayed(self):
        self.server.run_lock.acquire()
        try:
            with patch.object(runtime, "run", side_effect=AssertionError("Must not run")):
                status, body, _ = self.request("POST", "/v1/chat/completions", self.body())
            self.assertEqual(status, 409)
            self.assertEqual(json.loads(body)["error"]["code"], "runtime_busy")
        finally:
            self.server.run_lock.release()

    def test_bad_inputs_do_not_execute(self):
        cases = [
            (self.body(model="real-provider-model"), "unknown_model"),
            (self.body(stream="true"), "invalid_stream"),
            (self.body(messages=[]), "invalid_messages"),
            (self.body(messages=[{"role": "tool", "content": "x"}]), "text_messages_required"),
            (self.body(messages=[{"role": "user", "content": []}]), "text_messages_required"),
            (self.body(messages=[{"role": "assistant", "content": "x"}]),
             "last_message_must_be_user"),
            (self.body(tools=[]), "unsupported_option"),
        ]
        for request, code in cases:
            with self.subTest(code=code):
                # Errors close the connection; HTTPConnection reconnects on the next request.
                with patch.object(runtime, "run", side_effect=AssertionError("Must not run")):
                    status, body, _ = self.request("POST", "/v1/chat/completions", request)
                self.assertEqual(status, 400)
                self.assertEqual(json.loads(body)["error"]["code"], code)

    def test_large_body_is_rejected(self):
        status, body, _ = self.request(
            "POST", "/v1/chat/completions",
            self.body(messages=[{"role": "user", "content": "x" * runtime.MAX_BODY}]),
        )
        self.assertEqual(status, 413)
        self.assertIn("body_limit_exceeded", body)

    def test_malformed_json_is_rejected(self):
        self.connection.request(
            "POST", "/v1/chat/completions", "{", {"Content-Type": "application/json"}
        )
        response = self.connection.getresponse()
        self.assertEqual(response.status, 400)
        self.assertIn(b"invalid_json", response.read())

    def test_streaming_failure_is_not_reported_as_success(self):
        with patch.object(runtime, "run", side_effect=runtime.RunError("deadline_exceeded", 504)):
            status, body, _ = self.request(
                "POST", "/v1/chat/completions", self.body(stream=True)
            )
        self.assertEqual(status, 504)
        self.assertNotIn("[DONE]", body)
        self.assertFalse(self.server.run_lock.locked())


class NativeTests(unittest.TestCase):
    def fake_run(self, script, name="copilot", deadline=2, cancelled=lambda: False):
        with (
            patch.dict(os.environ, {}, clear=True),
            patch.object(runtime, "command_for", return_value=[sys.executable, "-c", script]),
        ):
            return runtime.native_result(name, MESSAGES, cancelled, deadline)

    def assert_run_error(self, code, callback):
        with self.assertRaises(runtime.RunError) as caught:
            callback()
        self.assertEqual(caught.exception.code, code)

    def test_native_output_from_child_process(self):
        self.assertEqual(self.fake_run("print('synthetic review')"), "synthetic review")

    def test_copilot_conversation_is_sent_on_stdin_not_in_argv(self):
        messages = [{"role": "user", "content": "--allow-all $(touch unexpected); review pricing.py"}]
        command = runtime.command_for("copilot")
        script = "import sys; print(sys.stdin.read())"
        with (
            patch.dict(os.environ, {}, clear=True),
            patch.object(runtime, "command_for", return_value=[sys.executable, "-c", script]) as command_for,
        ):
            output = runtime.native_result("copilot", messages)
        command_for.assert_called_once_with("copilot")
        self.assertIn(messages[0]["content"], output)
        self.assertNotIn(messages[0]["content"], " ".join(command))
        self.assertEqual(command[command.index("-p") + 1],
                         "Complete the read-only review supplied on stdin in the current workspace.")

    def test_claude_stdin_and_json_result(self):
        script = (
            "import sys,json; p=sys.stdin.read(); "
            "print(json.dumps({'subtype':'success','result':p,'is_error':False}))"
        )
        output = self.fake_run(script, name="claude")
        self.assertIn("untrusted conversation", output)
        self.assertIn("Review the synthetic pricing.py.", output)

    def test_codex_events_are_normalized(self):
        events = [
            {"type": "item.completed", "item": {"type": "agent_message", "text": "finding"}},
            {"type": "turn.completed"},
        ]
        output = "\n".join(json.dumps(event) for event in events)
        self.assertEqual(self.fake_run(f"print({output!r})", name="codex"), "finding")

    def test_protocol_failures_are_not_success(self):
        for name, text, expected in [
            ("claude", '{"subtype":"error","is_error":true}', "native_run_failed"),
            ("claude", "not json", "invalid_native_output"),
            ("codex", '{"type":"turn.failed"}', "native_run_failed"),
            ("codex", '{"type":"thread.started"}', "native_run_incomplete"),
            ("copilot", "  ", "empty_native_result"),
        ]:
            with self.subTest(runtime=name, text=text):
                self.assert_run_error(expected, lambda: runtime.decode_output(name, text))

    def test_nonzero_exit_does_not_leak_provider_logs(self):
        self.assert_run_error(
            "native_run_failed",
            lambda: self.fake_run("import sys; print('sensitive output'); sys.exit(7)"),
        )

    def test_deadline_terminates_child(self):
        self.assert_run_error(
            "deadline_exceeded",
            lambda: self.fake_run("import time; time.sleep(5)", deadline=0.1),
        )

    def test_cancellation_terminates_child(self):
        self.assert_run_error(
            "request_cancelled",
            lambda: self.fake_run("import time; time.sleep(5)", cancelled=lambda: True),
        )

    def test_large_output_is_bounded(self):
        self.assert_run_error(
            "output_limit_exceeded",
            lambda: self.fake_run(f"print('x'*{runtime.MAX_OUTPUT + 1})"),
        )

    def test_nonreading_child_cannot_block_stdin_deadline(self):
        with patch.object(runtime, "task_prompt", return_value="x" * 100000):
            self.assert_run_error(
                "deadline_exceeded",
                lambda: self.fake_run("import time; time.sleep(5)", name="claude", deadline=0.1),
            )

    def test_platform_native_fails_closed(self):
        with (
            patch.dict(os.environ, {"OPENAI_BASE_URL": "https://gateway.example.com"}, clear=True),
            patch.object(subprocess, "Popen", side_effect=AssertionError("Must not start")),
        ):
            self.assert_run_error(
                "platform_native_not_qualified",
                lambda: runtime.native_result("copilot", MESSAGES),
            )

    def test_missing_cli_is_actionable(self):
        with (
            patch.dict(os.environ, {}, clear=True),
            patch.object(runtime.shutil, "which", return_value=None),
        ):
            self.assert_run_error(
                "runtime_not_installed",
                lambda: runtime.native_result("claude", MESSAGES),
            )

    def test_native_requires_explicit_acknowledgement(self):
        result = subprocess.run(
            [sys.executable, str(runtime.ROOT / "runtime.py"), "run", "--native"],
            capture_output=True, text=True,
        )
        self.assertEqual(result.returncode, 2)
        self.assertIn("--ack-data-export", result.stderr)

    def test_codex_custom_agent_parses(self):
        path = runtime.WORKSPACE / ".codex/agents/cetirun-reviewer.toml"
        profile = tomllib.loads(path.read_text())
        self.assertIn("pricing.py", profile["developer_instructions"])
        self.assertFalse(profile["features"]["shell_tool"])


if __name__ == "__main__":
    unittest.main()
