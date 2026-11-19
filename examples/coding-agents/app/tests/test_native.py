# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import http.client
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import threading
import tomllib
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import native
import server


KEY = "synthetic-test-key"
MESSAGES = [{"role": "user", "content": "Create and test a sum function."}]


class GatewayTests(unittest.TestCase):
    def test_partial_configuration_fails_closed(self):
        for env in ({}, {"OPENAI_BASE_URL": "https://gateway.example.com/v1"}):
            with patch.dict(os.environ, env, clear=True):
                with self.assertRaises(native.RuntimeErrorCode) as caught:
                    native.Gateway.from_environment()
                self.assertEqual(caught.exception.code, "gateway_configuration_required")

    def test_nonlocal_plaintext_and_embedded_credentials_are_rejected(self):
        for base in (
            "http://provider.example.com/v1",
            "https://user:password@gateway.example.com/v1",
            "https://gateway.example.com/v1?credential=x",
        ):
            with self.subTest(base=base), self.assertRaises(native.RuntimeErrorCode):
                native.Gateway(base, KEY)

    def test_provider_mapping_never_uses_personal_provider_environment(self):
        gateway = native.Gateway("https://gateway.example.com/v1", KEY)
        with patch.dict(os.environ, {
            "ANTHROPIC_API_KEY": "synthetic-personal-key",
            "COPILOT_PROVIDER_BASE_URL": "https://personal.example.com",
            "OPENAI_API_KEY": "synthetic-personal-key",
        }, clear=True):
            for runtime in native.RUNTIMES:
                env = gateway.child_environment(runtime, "allowed-model", Path("/home/vibe"))
                self.assertNotIn("synthetic-personal-key", env.values())
                self.assertNotIn("https://personal.example.com", env.values())
                self.assertEqual(env["CETIRUN_GATEWAY_KEY"], KEY)
                argv = native.command(runtime, gateway, "allowed-model")
                self.assertNotIn(KEY, " ".join(argv))
                if runtime == "claude":
                    self.assertEqual(env["ANTHROPIC_BASE_URL"], "https://gateway.example.com")
                if runtime == "copilot":
                    self.assertEqual(env["COPILOT_PROVIDER_WIRE_API"], "responses")

    def test_workspace_initialization_keeps_user_changes(self):
        with tempfile.TemporaryDirectory() as directory:
            home = Path(directory)
            workspace = native.prepare_workspace(home)
            profile = workspace / ".claude/agents/cetirun-coder.md"
            self.assertIn("Complete the user's task", profile.read_text())
            profile.write_text("user-owned profile")
            native.prepare_workspace(home)
            self.assertEqual(profile.read_text(), "user-owned profile")


class LaunchTests(unittest.TestCase):
    def test_controlled_provider_and_native_context_preserve_user_files_in_both_modes(self):
        with tempfile.TemporaryDirectory() as directory:
            home = Path(directory)
            workspace = native.prepare_workspace(home)
            originals = {
                home / ".copilot/providers.json": '{"providers":[{"baseUrl":"https://old.invalid"}]}',
                home / ".copilot/config.json": '{"model":"old/model","memory":true}',
                home / ".copilot/settings.json": '{"model":"old/model","memory":true}',
                workspace / "AGENTS.md": "user AGENTS",
                workspace / "CLAUDE.md": "user CLAUDE",
                workspace / ".github/agents/cetirun-coder.agent.md": "user agent",
            }
            for path, text in originals.items():
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text(text)
            canonical = home / ".agents"
            skill = canonical / "skills/platform-marker"
            skill.mkdir(parents=True)
            (skill / "SKILL.md").write_text("---\nname: platform-marker\ndescription: platform skill\n---\n")
            marker = "Platform instructions: \u4e2d\U0001f642"
            (canonical / "AGENTS.md").write_text(marker)
            gateway = native.Gateway("https://gateway.example.com/v1", KEY)
            with patch.dict(os.environ, {
                "COPILOT_HOME": str(home / ".copilot"),
                "COPILOT_PROVIDERS_CONFIG": str(home / ".copilot/providers.json"),
            }, clear=True):
                for runtime in native.RUNTIMES:
                    for interactive in (False, True):
                        with self.subTest(runtime=runtime, interactive=interactive):
                            with native.launch(runtime, gateway, "allowed", home, interactive) as (argv, env, cwd):
                                self.assertEqual(cwd, workspace)
                                self.assertNotIn(KEY, " ".join(argv))
                                if runtime == "copilot":
                                    registry = Path(env["COPILOT_PROVIDERS_CONFIG"])
                                    provider = json.loads(registry.read_text())
                                    self.assertEqual(registry.stat().st_mode & 0o777, 0o600)
                                    self.assertEqual(provider, {"providers": [], "models": []})
                                    self.assertEqual(env["COPILOT_PROVIDER_BASE_URL"], gateway.base)
                                    self.assertEqual(env["COPILOT_PROVIDER_API_KEY"], KEY)
                                    self.assertEqual(env["COPILOT_MODEL"], "allowed")
                                    self.assertEqual(argv[argv.index("--model") + 1], "allowed")
                                    root = Path(env["COPILOT_HOME"]).parent
                                    for path in root.rglob("*"):
                                        if path.is_file():
                                            self.assertNotIn(KEY, path.read_text(), str(path))
                                    self.assertIn(marker, (Path(env["COPILOT_HOME"]) / "agents/cetirun-platform-coder.agent.md").read_text())
                                    config = json.loads((Path(env["COPILOT_HOME"]) / "settings.json").read_text())
                                    self.assertFalse(config["memory"])
                                    self.assertTrue((Path(config["skillDirectories"][0]) / "platform-marker/SKILL.md").is_file())
                                elif runtime == "claude":
                                    self.assertIn(marker, argv[argv.index("--append-system-prompt") + 1])
                                    plugin = Path(argv[argv.index("--plugin-dir") + 1])
                                    self.assertTrue((plugin / "skills/platform-marker/SKILL.md").is_file())
                                    agents = json.loads(argv[argv.index("--agents") + 1])
                                    self.assertIn("Skill", agents["cetirun-platform-coder"]["tools"])
                                else:
                                    overrides = [argv[i + 1] for i, item in enumerate(argv) if item == "-c"]
                                    config = tomllib.loads("\n".join(overrides))
                                    self.assertIn(marker, config["developer_instructions"])
                                    self.assertTrue(config["skills"]["include_instructions"])
                                    self.assertFalse(config["memories"]["generate_memories"])
                                    self.assertFalse(config["memories"]["use_memories"])
                            if runtime == "copilot":
                                self.assertFalse(registry.exists())
            for path, text in originals.items():
                self.assertEqual(path.read_text(), text)

    def test_memory_is_optional_owner_private_and_cannot_relax_existing_restrictions(self):
        with tempfile.TemporaryDirectory() as directory:
            home = Path(directory)
            assets = home / "provided-assets"
            binary = assets / "bin/cetirun-memory"
            binary.parent.mkdir(parents=True)
            binary.write_text("#!/bin/sh\nexit 1\n")
            binary.chmod(0o700)
            skill = home / ".agents/skills/cetirun-memory"
            skill.mkdir(parents=True)
            (skill / "SKILL.md").write_text("---\nname: cetirun-memory\ndescription: Memory\n---\n")
            gateway = native.Gateway("https://gateway.example.com/v1", KEY)
            cases = (
                ({}, "", False, False),
                ({"CETIRUN_MEMORY_ASSETS": str(assets), "CETIRUN_MEMORY_ROOT": str(home / "private")}, "", True, False),
                ({"CETIRUN_MEMORY_ASSETS": str(assets), "CETIRUN_MEMORY_ROOT": str(home / "private")}, "no-capture", True, True),
                ({"CETIRUN_MEMORY_ASSETS": str(assets), "CETIRUN_MEMORY_ROOT": str(home / "private")}, "off", False, True),
                ({"CETIRUN_MEMORY_ASSETS": str(assets), "CETIRUN_MEMORY_ROOT": str(home / "private"),
                  "CETIRUN_MEMORY_NO_MEMORY": "true"}, "no-capture", False, True),
                ({"CETIRUN_MEMORY_ASSETS": str(assets), "CETIRUN_MEMORY_ROOT": str(home / "private"),
                  "CETIRUN_MEMORY_NO_CAPTURE": "true"}, "", True, True),
            )
            for runtime in native.RUNTIMES:
                for env, mode, enabled, restricted in cases:
                    with self.subTest(runtime=runtime, mode=mode, env=env), patch.dict(os.environ, env, clear=True):
                        with native.launch(runtime, gateway, "allowed", home, memory_mode=mode) as (argv, child, _):
                            self.assertEqual(child["CETIRUN_MEMORY_NO_CAPTURE"], str(restricted).lower())
                            if runtime == "copilot":
                                mcp = json.loads(argv[argv.index("--additional-mcp-config") + 1])["mcpServers"] if enabled else {}
                                self.assertEqual("--additional-mcp-config" in argv, enabled)
                                skills = Path(child["COPILOT_HOME"]).parent / "skills"
                            elif runtime == "claude":
                                mcp = json.loads(argv[argv.index("--mcp-config") + 1])["mcpServers"]
                                skills = Path(argv[argv.index("--plugin-dir") + 1]) / "skills"
                            else:
                                config = tomllib.loads("\n".join(argv[i + 1] for i, item in enumerate(argv) if item == "-c"))
                                mcp = config["mcp_servers"]
                                self.assertEqual(config["skills"]["config"][0]["enabled"], enabled)
                                skills = None
                            self.assertEqual(bool(mcp), enabled)
                            if skills:
                                self.assertEqual((skills / "cetirun-memory").exists(), enabled)
                            if enabled:
                                helper = mcp["cetirun-memory"]
                                self.assertEqual(helper["command"], str(binary))
                                self.assertEqual(helper["args"], ["mcp", "--stdio", "--owner-private"])
                                self.assertEqual(helper["env"]["CETIRUN_MEMORY_NO_CAPTURE"], str(restricted).lower())
            self.assertFalse((home / "private").exists())

    def test_terminal_uses_shared_launch_without_an_http_deadline(self):
        with tempfile.TemporaryDirectory() as directory, patch.dict(os.environ, {
            "HOME": directory, "OPENAI_BASE_URL": "https://gateway.example.com/v1", "OPENAI_API_KEY": KEY,
        }, clear=True), patch.object(sys, "argv", ["server.py", "tui", "--runtime", "copilot", "--model", "allowed"]), patch.object(
            native.Gateway, "models", return_value=["allowed"]
        ), patch.object(server, "launch", wraps=native.launch) as launch, patch.object(server.subprocess, "Popen") as popen:
            popen.return_value.wait.return_value = 0
            with self.assertRaises(SystemExit) as caught:
                server.main()
            self.assertEqual(caught.exception.code, 0)
            self.assertTrue(launch.call_args.kwargs["interactive"])
            self.assertNotIn("timeout", popen.call_args.kwargs)
            self.assertNotIn("timeout", popen.return_value.wait.call_args.kwargs)
            self.assertNotIn("deadline", launch.call_args.kwargs)

    def test_terminal_termination_reaps_child_and_removes_temporary_provider_configuration(self):
        previous = signal.getsignal(signal.SIGTERM)
        with tempfile.TemporaryDirectory() as directory, patch.dict(os.environ, {
            "HOME": directory, "OPENAI_BASE_URL": "https://gateway.example.com/v1", "OPENAI_API_KEY": KEY,
        }, clear=True), patch.object(sys, "argv", ["server.py", "tui", "--runtime", "copilot", "--model", "allowed"]), patch.object(
            native.Gateway, "models", return_value=["allowed"]
        ), patch.object(server.subprocess, "Popen") as popen:
            def wait(**kwargs):
                if "timeout" not in kwargs:
                    signal.getsignal(signal.SIGTERM)(signal.SIGTERM, None)
                return 0

            popen.return_value.wait.side_effect = wait
            popen.return_value.poll.return_value = None
            with self.assertRaises(SystemExit) as caught:
                server.main()
            self.assertEqual(caught.exception.code, 128 + signal.SIGTERM)
            popen.return_value.terminate.assert_called_once()
            popen.return_value.wait.assert_called_with(timeout=2)
            provider = Path(popen.call_args.kwargs["env"]["COPILOT_PROVIDERS_CONFIG"])
            self.assertFalse(provider.exists())
        self.assertEqual(signal.getsignal(signal.SIGTERM), previous)


class EventTests(unittest.TestCase):
    def test_three_native_streams_produce_real_completion_state(self):
        cases = {
            "copilot": [
                {"type": "assistant.message_delta", "data": {"deltaContent": "implemented"}},
                {"type": "assistant.message", "data": {"content": "implemented"}},
                {"type": "result", "exitCode": 0},
            ],
            "claude": [
                {"type": "stream_event", "event": {"delta": {"type": "text_delta", "text": "implemented"}}},
                {"type": "result", "subtype": "success", "is_error": False, "result": "implemented"},
            ],
            "codex": [
                {"type": "item.completed", "item": {"type": "agent_message", "text": "implemented"}},
                {"type": "turn.completed"},
            ],
        }
        for runtime, stream in cases.items():
            with self.subTest(runtime=runtime):
                output = []
                events = native.Events(runtime, output.append)
                for event in stream:
                    events.accept(json.dumps(event))
                self.assertTrue(events.complete)
                self.assertEqual(events.text.strip(), "implemented")
                self.assertEqual(len(output), 1)

    def test_failed_events_cannot_complete(self):
        for runtime, event in (
            ("copilot", {"type": "session.error", "data": {"message": "sensitive"}}),
            ("claude", {"type": "result", "is_error": True, "subtype": "error"}),
            ("codex", {"type": "turn.failed"}),
        ):
            with self.subTest(runtime=runtime):
                events = native.Events(runtime, lambda _text: None)
                with self.assertRaises(native.RuntimeErrorCode) as caught:
                    events.accept(json.dumps(event))
                self.assertEqual(str(caught.exception), "native_run_failed")
                self.assertFalse(events.complete)

    def test_invalid_event_and_output_limits(self):
        events = native.Events("codex", lambda _text: None)
        with self.assertRaises(native.RuntimeErrorCode):
            events.accept("not JSON")
        with patch.object(native, "OUTPUT_LIMIT", 8):
            with self.assertRaises(native.RuntimeErrorCode):
                events.add_text("long answer")

    def test_subagent_idle_and_aborted_root_do_not_complete_copilot_turn(self):
        events = native.Events("copilot", lambda _text: None)
        events.accept(json.dumps({"type": "session.idle", "agentId": "child", "data": {}}))
        self.assertFalse(events.complete)
        with self.assertRaises(native.RuntimeErrorCode) as caught:
            events.accept(json.dumps({"type": "session.idle", "data": {"aborted": True}}))
        self.assertEqual(caught.exception.code, "native_run_cancelled")
        self.assertFalse(events.complete)

    def test_copilot_requires_successful_root_result(self):
        events = native.Events("copilot", lambda _text: None)
        for event in (
            {"type": "session.idle", "data": {}},
            {"type": "assistant.idle", "data": {}},
            {"type": "result", "exitCode": 0, "agentId": "child"},
        ):
            events.accept(json.dumps(event))
            self.assertFalse(events.complete)
        events.accept(json.dumps({"type": "result", "exitCode": 0}))
        self.assertTrue(events.complete)

    def test_copilot_failed_or_malformed_result_is_not_success(self):
        cases = [({"type": "result"}, "invalid_native_event")]
        cases += [({"type": "result", "exitCode": value}, "invalid_native_event")
                  for value in (None, False, "0", 0.0)]
        cases += [({"type": "result", "exitCode": value}, "native_run_failed")
                  for value in (1, 130)]
        for event, code in cases:
            with self.subTest(event=event):
                events = native.Events("copilot", lambda _text: None)
                events.accept(json.dumps({"type": "assistant.message_delta", "data": {"deltaContent": "partial"}}))
                with self.assertRaises(native.RuntimeErrorCode) as caught:
                    events.accept(json.dumps(event))
                self.assertEqual(caught.exception.code, code)
                self.assertFalse(events.complete)


class ProcessTests(unittest.TestCase):
    def invoke(self, code, runtime="codex", deadline=3, cancelled=lambda: False):
        output = []
        with tempfile.TemporaryDirectory() as directory, patch.object(
            native, "command", return_value=[sys.executable, "-c", code]
        ), patch.object(native.shutil, "which", return_value=sys.executable):
            result = native.execute(
                runtime, native.Gateway("https://gateway.example.com/v1", KEY),
                "allowed-model", MESSAGES, Path(directory), output.append,
                lambda: None, cancelled, deadline,
            )
        return result, output

    def test_real_child_reads_task_edits_workspace_and_reports_success(self):
        code = """
import json,sys
from pathlib import Path
task=sys.stdin.read()
assert 'Create and test' in task
Path('sum.py').write_text('def add(a,b): return a+b\\n')
assert Path('sum.py').exists()
print(json.dumps({'type':'item.completed','item':{'type':'agent_message','text':'sum.py created; test passed'}}))
print(json.dumps({'type':'turn.completed'}))
"""
        result, output = self.invoke(code)
        self.assertIn("sum.py created", result)
        self.assertEqual(output, [result])

    def test_exit_zero_without_completion_is_incomplete(self):
        with self.assertRaises(native.RuntimeErrorCode) as caught:
            self.invoke("print('{\"type\":\"thread.started\"}')")
        self.assertEqual(caught.exception.code, "native_run_incomplete")

    def test_copilot_result_and_process_exit_must_both_succeed(self):
        stream = """
import json,sys
sys.stdin.read()
print(json.dumps({'type':'assistant.message_delta','data':{'deltaContent':'completed reply'}}))
print(json.dumps({'type':'assistant.message','data':{'content':'completed reply'}}))
print(json.dumps({'type':'result','exitCode':0}))
"""
        result, output = self.invoke(stream, runtime="copilot")
        self.assertEqual(result, "completed reply")
        self.assertEqual(output, [result])
        with self.assertRaises(native.RuntimeErrorCode) as caught:
            self.invoke(stream + "sys.exit(1)\n", runtime="copilot")
        self.assertEqual(caught.exception.code, "native_run_failed")

    def test_copilot_text_and_idle_without_result_remain_incomplete(self):
        stream = """
import json,sys
sys.stdin.read()
print(json.dumps({'type':'assistant.message_delta','data':{'deltaContent':'partial reply'}}))
print(json.dumps({'type':'session.idle','data':{}}))
"""
        with self.assertRaises(native.RuntimeErrorCode) as caught:
            self.invoke(stream, runtime="copilot")
        self.assertEqual(caught.exception.code, "native_run_incomplete")

    def test_deadline_and_cancellation_stop_child(self):
        for deadline, cancelled, code in (
            (0.1, lambda: False, "deadline_exceeded"),
            (3, lambda: True, "request_cancelled"),
        ):
            with self.subTest(code=code), self.assertRaises(native.RuntimeErrorCode) as caught:
                self.invoke("import time; time.sleep(30)", deadline=deadline, cancelled=cancelled)
            self.assertEqual(caught.exception.code, code)


class HTTPTests(unittest.TestCase):
    def setUp(self):
        self.home = tempfile.TemporaryDirectory()
        self.gateway = native.Gateway("https://gateway.example.com/v1", KEY)
        self.instance = server.Server(("127.0.0.1", 0), "codex", self.gateway, Path(self.home.name))
        self.thread = threading.Thread(target=self.instance.serve_forever)
        self.thread.start()
        self.client = http.client.HTTPConnection(*self.instance.server_address, timeout=3)

    def tearDown(self):
        self.client.close()
        self.instance.stopping.set()
        self.instance.shutdown()
        self.instance.server_close()
        self.thread.join()
        self.home.cleanup()

    def request(self, method, path, body=None, headers=None):
        raw = body if isinstance(body, bytes) else json.dumps(body, ensure_ascii=False).encode() if body is not None else None
        self.client.request(method, path, raw, {"Content-Type": "application/json", **(headers or {})})
        response = self.client.getresponse()
        return response.status, response.read().decode()

    def test_readiness_does_not_call_gateway_or_model(self):
        with patch.object(self.gateway, "models", side_effect=AssertionError("No model calls")):
            status, body = self.request("GET", "/healthz")
        self.assertEqual(status, 200)
        self.assertEqual(json.loads(body), {"status": "ok"})

    def test_model_entitlement_failure_does_not_invoke_cli(self):
        with patch.object(self.gateway, "models", return_value=["allowed"]), patch.object(
            server, "execute", side_effect=AssertionError("Must not run")
        ):
            status, body = self.request(
                "POST", "/v1/chat/completions", {"model": "other", "messages": MESSAGES}
            )
        self.assertEqual(status, 403)
        self.assertIn("model_not_authorized", body)

    def test_json_and_sse_success_use_native_result(self):
        def execute(*args, **_kwargs):
            args[5]("native result")
            return "native result"

        for streaming in (False, True):
            with self.subTest(stream=streaming), patch.object(
                self.gateway, "models", return_value=["allowed"]
            ), patch.object(server, "execute", side_effect=execute):
                status, body = self.request(
                    "POST", "/v1/chat/completions",
                    {"model": "allowed", "messages": MESSAGES, "stream": streaming},
                )
            self.assertEqual(status, 200)
            self.assertIn("native result", body)
            self.assertEqual("[DONE]" in body, streaming)

    def test_failed_stream_never_emits_done(self):
        with patch.object(self.gateway, "models", return_value=["allowed"]), patch.object(
            server, "execute", side_effect=native.RuntimeErrorCode("native_run_failed")
        ):
            status, body = self.request(
                "POST", "/v1/chat/completions",
                {"model": "allowed", "messages": MESSAGES, "stream": True},
            )
        self.assertEqual(status, 200)
        self.assertIn("native_run_failed", body)
        self.assertNotIn("[DONE]", body)
        self.assertNotIn('"finish_reason": "stop"', body)

    def test_unicode_and_escaped_output_is_lossless_and_every_encoded_sse_frame_fits(self):
        text = "\x00" * 20000 + ("\u4e2d\U0001f642\\\"\n\x00" * 17000)
        for runtime, event in (
            ("copilot", {"type": "assistant.message", "data": {"content": text}}),
            ("claude", {"type": "result", "subtype": "success", "result": text}),
            ("codex", {"type": "item.completed", "item": {"type": "agent_message", "text": text}}),
        ):
            def execute(*args, **_kwargs):
                events = native.Events(runtime, args[5])
                events.accept(json.dumps(event))
                return events.text

            with self.subTest(runtime=runtime), patch.object(self.gateway, "models", return_value=["allowed"]), patch.object(
                server, "execute", side_effect=execute
            ):
                status, body = self.request("POST", "/v1/chat/completions", {
                    "model": "allowed", "messages": MESSAGES, "stream": True,
                })
            self.assertEqual(status, 200)
            frames = [frame + "\n\n" for frame in body.split("\n\n") if frame]
            self.assertGreater(len(frames), 3)
            self.assertTrue(all(len(frame.encode("utf-8")) <= 65536 for frame in frames))
            payloads = [json.loads(frame[6:]) for frame in frames[:-1]]
            reconstructed = "".join(item["choices"][0]["delta"].get("content", "") for item in payloads)
            self.assertEqual(reconstructed, text + ("\n" if runtime == "codex" else ""))
            self.assertEqual(payloads[-1]["choices"][0]["finish_reason"], "stop")
            self.assertEqual(frames[-1], "data: [DONE]\n\n")

    def test_output_failure_after_partial_delivery_is_explicit_and_never_completes(self):
        def execute(*args, **_kwargs):
            args[5]("partial result")
            raise OSError("synthetic I/O failure")

        with patch.object(self.gateway, "models", return_value=["allowed"]), patch.object(server, "execute", side_effect=execute):
            _, body = self.request("POST", "/v1/chat/completions", {"model": "allowed", "messages": MESSAGES, "stream": True})
        self.assertIn("partial result", body)
        self.assertIn("native_io_failed", body)
        self.assertNotIn("[DONE]", body)
        self.assertNotIn('"finish_reason": "stop"', body)

    def test_sse_write_failure_never_marks_a_native_run_complete(self):
        class FailOnce:
            def __init__(self, wrapped):
                self.wrapped = wrapped
                self.failed = False

            def __getattr__(self, name):
                return getattr(self.wrapped, name)

            def write(self, data):
                if b'"content":' in data and not self.failed:
                    self.failed = True
                    raise OSError("synthetic transport write failure")
                return self.wrapped.write(data)

        original = server.Handler.setup

        def setup(handler):
            original(handler)
            handler.wfile = FailOnce(handler.wfile)

        def execute(*args, **_kwargs):
            args[5]("result cannot be delivered")
            return "must never finish"

        with patch.object(server.Handler, "setup", setup), patch.object(
            self.gateway, "models", return_value=["allowed"]
        ), patch.object(server, "execute", side_effect=execute):
            _, body = self.request("POST", "/v1/chat/completions", {"model": "allowed", "messages": MESSAGES, "stream": True})
        self.assertIn("native_io_failed", body)
        self.assertNotIn("[DONE]", body)
        self.assertNotIn('"finish_reason": "stop"', body)

    def test_portal_limits_keep_all_history_and_pass_http_deadline_and_memory_mode(self):
        history = [{"role": "assistant", "content": "history " + str(i)} for i in range(99)] + MESSAGES
        exact_input = "\U0001f642" * 16384
        history[-1] = {"role": "user", "content": exact_input}
        payload = {"model": "allowed", "messages": history}
        large = {"model": "allowed", "messages": [{"role": "assistant", "content": ""}] + MESSAGES}
        overhead = len(json.dumps(large, ensure_ascii=False).encode())
        large["messages"][0]["content"] = "h" * (1048576 - overhead)
        with patch.object(self.gateway, "models", return_value=["allowed"]), patch.object(server, "execute", return_value="ok") as execute:
            for body in (payload, large):
                status, _ = self.request("POST", "/v1/chat/completions", body,
                                         {"X-CetiRun-Memory-Mode": "no-capture"})
                self.assertEqual(status, 200)
                self.assertEqual(execute.call_args.args[3], body["messages"])
                self.assertEqual(execute.call_args.kwargs, {"deadline": 600, "memory_mode": "no-capture"})
            execute.reset_mock()
            for body, headers, expected in (
                ({"model": "allowed", "messages": history + MESSAGES}, {}, 400),
                ({"model": "allowed", "messages": [{"role": "user", "content": exact_input + "x"}]}, {}, 413),
                (b"{}", {"Content-Length": "1048577"}, 413),
                ({"model": "allowed", "messages": MESSAGES}, {"X-CetiRun-Memory-Mode": "invalid"}, 400),
            ):
                status, _ = self.request("POST", "/v1/chat/completions", body, headers)
                self.assertEqual(status, expected)
            execute.assert_not_called()

    def test_busy_and_invalid_requests_do_not_start_cli(self):
        with patch.object(self.gateway, "models", return_value=["allowed"]), patch.object(
            server, "execute", side_effect=AssertionError("Must not run")
        ):
            self.instance.busy.acquire()
            try:
                status, body = self.request(
                    "POST", "/v1/chat/completions", {"model": "allowed", "messages": MESSAGES}
                )
                self.assertEqual(status, 409)
            finally:
                self.instance.busy.release()
            status, body = self.request(
                "POST", "/v1/chat/completions",
                {"model": "allowed", "messages": [{"role": "tool", "content": "x"}]},
            )
            self.assertEqual(status, 400)


if __name__ == "__main__":
    unittest.main()
