# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Exercise the pinned native host APIs with synthetic files and a loopback model.

Requires separately provisioned Hermes v0.22.7 PM sources and dependencies;
never installs packages or reads an existing profile.
"""

import importlib.util
import hashlib
import json
import os
from pathlib import Path
import shutil
import sys
import threading
from types import SimpleNamespace
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from unittest.mock import patch

source = Path(os.environ["MEMORY_HERMES_SOURCE"])
assert sys.version_info[:2] == (3, 14), "Hermes qualification requires its PM Python 3.14"
sys.path.insert(0, str(source))
root = Path(os.environ["MEMORY_TEST_ASSETS"])
workspace = os.environ["MEMORY_TEST_WORKSPACE"]
profile = Path(os.environ["HERMES_HOME"])
profile.mkdir()
plugin_name = "cetirun-memory-context"
shutil.copytree(Path(os.environ["MEMORY_TEST_PLUGIN"]).parent, profile / "plugins" / plugin_name)
binary = profile / "bin/memory-helper"
binary.parent.mkdir()
shutil.copy2(root / "bin/cetirun-memory", binary)
data_root = profile / "local-memory"
bindings = {"CETIRUN_MEMORY_ROOT": str(data_root), "CETIRUN_MEMORY_ASSETS": str(root),
            "CETIRUN_MEMORY_BINARY": str(binary), "CETIRUN_MEMORY_CONTEXT": str(root / "AGENTS.md")}
profile.joinpath(".env").write_text("".join(f"{name}={value}\n" for name, value in bindings.items()))
profile.joinpath("config.yaml").write_text(
    "plugins:\n  enabled: [cetirun-memory-context]\nskills:\n  external_dirs:\n"
    f"    - {root / 'skills'}\n")
for name in bindings:
    os.environ.pop(name, None)

from hermes_cli.env_loader import load_hermes_dotenv

load_hermes_dotenv(hermes_home=profile, load_external_secrets=False)
assert all(os.environ.get(name) == value for name, value in bindings.items())

from hermes_cli.version_info import get_version_info

identity = get_version_info()
assert identity.base_version == "0.22.7" and identity.distance == 0
assert identity.commit == "b200f5bc60e31588ba2cb60e288029c4a54ba5fa"
assert identity.source == "docker" and identity.distribution == "docker"
assert json.loads((source / "install-stamp.json").read_text())["updateMechanism"] == "external"

from hermes_cli import plugins
from hermes_cli.middleware import apply_llm_request_middleware
from agent.runtime_cwd import set_session_cwd
from tools.registry import registry

set_session_cwd(workspace)
spec = importlib.util.spec_from_file_location("memory_native_fixture", root / "adapters/hermes/__init__.py")
adapter = importlib.util.module_from_spec(spec)
spec.loader.exec_module(adapter)
manager = plugins.PluginManager()
with patch.object(plugins, "get_plugin_manager", return_value=manager):
    manager.discover_and_load()
loaded = manager._plugins[plugin_name]
assert loaded.enabled and not loaded.error, loaded.error
assert Path(loaded.manifest.path) == profile / "plugins" / plugin_name
from hermes_cli import plugins_manifest
with patch.object(plugins_manifest, "running_hermes_version", return_value="0.22.6"):
    assert plugins_manifest.requires_hermes_error(loaded.manifest)
assert registry.get_entry("cetirun_memory", scope=manager.scope_key) is not None
assert registry.get_definitions({"cetirun_memory"})[0]["function"]["name"] == "cetirun_memory"

with patch.object(plugins, "get_plugin_manager", return_value=manager):
    from agent.turn_context import _collect_pre_llm_call_context
    from gateway.owner_identity import host_owner_subject
    from gateway.session_policy import build_policy, policy_scope

    policy = build_policy({"source": "gui", "cwd": workspace, "model": "fixture"}, {})
    for name, platform, parent, sender, private in (
        ("webapp-owner", "desktop", "", host_owner_subject(), True),
        ("webapp-channel", "discord", "", "synthetic-channel-user", True),
        ("webapp-child", "desktop", "webapp-owner", host_owner_subject(), False),
        ("webapp-other-owner", "desktop", "", "uid:987654", True),
        ("api-owner", "api_server", "", "", True),
    ):
        agent = SimpleNamespace(session_id=name, platform=platform, _parent_session_id=parent,
                                _user_id=sender, model="fixture")
        with policy_scope(policy if platform == "desktop" else None):
            _collect_pre_llm_call_context(agent, effective_task_id=name, turn_id=name + "-turn",
                original_user_message="Synthetic owner question", messages=[], conversation_history=[])
        original = {"messages": [{"role": "user", "content": "Synthetic request"}]}
        result = apply_llm_request_middleware(original, session_id=name, provider="openai", model="fixture",
                                              api_mode="chat_completions")
        assert result.changed is private, name
        assert len(original["messages"]) == 1
        if private and platform == "desktop":
            with policy_scope(policy):
                _collect_pre_llm_call_context(agent, effective_task_id=name, turn_id=name + "-off",
                    original_user_message="/memory off", messages=[], conversation_history=[])
            disabled = apply_llm_request_middleware(original, session_id=name, provider="openai", model="fixture",
                                                     api_mode="chat_completions")
            assert adapter.MEMORY_OFF in json.dumps(disabled.payload)

    manager.invoke_hook("pre_llm_call", session_id="native-hermes", platform="cli", turn_id="owner-pref",
                        model="fixture", user_message="Remember my preference: use synthetic concise examples")
    request = {"messages": [{"role": "user", "content": "Native history stays unchanged"}]}
    transformed = apply_llm_request_middleware(request, session_id="native-hermes", provider="openai", model="fixture",
                                              api_mode="chat_completions", api_request_id="native-request")
    assert transformed.changed
    assert "synthetic concise examples" in json.dumps(transformed.payload)
    assert "synthetic concise examples" not in json.dumps(request)
    status = json.loads(registry.dispatch("cetirun_memory", {"command": "status", "payload": {}}, session_id="native-hermes"))
    assert status["ok"] and status["data"]["modelEvaluationSupported"]
    assert data_root.is_dir() and data_root.stat().st_mode & 0o777 == 0o700

    # Exercise the native dotenv loader, not a direct replacement of os.environ.
    profile.joinpath(".env").write_text(
        f"CETIRUN_MEMORY_BINARY=relative-helper\nCETIRUN_MEMORY_ROOT={profile / 'wrong-store'}\n")
    load_hermes_dotenv(hermes_home=profile, load_external_secrets=False)
    manager.invoke_hook("pre_llm_call", session_id="native-hermes", platform="cli", turn_id="after-dotenv",
                        model="fixture", user_message="Synthetic next turn")
    after_reload = apply_llm_request_middleware(request, session_id="native-hermes", provider="openai", model="fixture",
                                               api_mode="chat_completions")
    assert after_reload.changed and "synthetic concise examples" in json.dumps(after_reload.payload)
    assert not (profile / "wrong-store").exists()

    from model_tools import handle_function_call

    source_file = Path(workspace) / "synthetic-source.txt"
    source_file.write_text("before")
    note = {"kind": "fact", "title": "Hermes replay fixture", "scope": {"project": workspace}, "body": "Synthetic file claim",
            "temporalClass": "live", "status": "active", "revalidation": {"mode": "on-use"},
            "sources": [{"kind": "file", "ref": str(source_file), "origin": "tool-observed", "assurance": "reported",
                         "version": "sha256:" + hashlib.sha256(b"before").hexdigest()}]}

    def native_tool(command, payload, call_id):
        result = json.loads(handle_function_call("cetirun_memory", {"command": command, "payload": payload},
                           session_id="native-hermes", turn_id="owner-pref", tool_call_id=call_id,
                           enabled_tools=["cetirun_memory"]))
        assert result["ok"], result
        return result

    saved = native_tool("capture", {"note": note}, "stable-native-tool-id")
    replayed = native_tool("capture", {"note": note}, "stable-native-tool-id")
    assert replayed == saved, (saved, replayed)
    ref = saved["data"]["refs"][0]
    native_tool("source", {"ref": ref, "sourceRef": str(source_file)}, "source-before")
    matched = native_tool("query", {"queryTerms": [note["title"]]}, "query-before")
    assert matched["data"]["hits"][0]["review"]["needsRecheck"] is False
    source_file.write_text("after")
    native_tool("source", {"ref": ref, "sourceRef": str(source_file)}, "source-after")
    changed = native_tool("query", {"queryTerms": [note["title"]]}, "query-after")
    assert changed["data"]["hits"][0]["review"]["needsRecheck"] is True
    assert changed["data"]["hits"][0]["review"]["reviewReason"] == "SourceChanged"
    manager.invoke_hook("pre_llm_call", session_id="native-hermes", platform="api_server", turn_id="overlap-a", model="fixture", user_message="First turn")
    manager.invoke_hook("pre_llm_call", session_id="native-hermes", platform="api_server", turn_id="overlap-b", model="fixture", user_message="Second turn")
    manager.invoke_hook("on_session_end", session_id="native-hermes", turn_id="overlap-a", completed=True)
    surviving = json.loads(registry.dispatch("cetirun_memory", {"command": "status", "payload": {}}, session_id="native-hermes"))
    assert surviving["ok"], surviving
    manager.invoke_hook("on_session_end", session_id="native-hermes", turn_id="overlap-b", completed=True)
    closed = json.loads(registry.dispatch("cetirun_memory", {"command": "query", "payload": {"queryTerms": ["fixture"]}}, session_id="native-hermes"))
    assert closed["reason"] == "Cancelled"
    manager.invoke_hook("pre_llm_call", session_id="native-hermes", platform="api_server", turn_id="final-turn", model="fixture", user_message="Final turn")
    manager.invoke_hook("on_session_finalize", session_id="native-hermes")
    finalized = json.loads(registry.dispatch("cetirun_memory", {"command": "status", "payload": {}}, session_id="native-hermes"))
    assert finalized["reason"] == "Cancelled"

from agent.auxiliary_client import set_runtime_main, reset_runtime_main

requests = []
response_status = 200


class ModelFixture(BaseHTTPRequestHandler):
    def log_message(self, *_args):
        pass

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        requests.append(body)
        assert body.get("metadata", {}).get("tags") == ["cetirun-memory-evaluation", "artifact-contract-v1"]
        assert body.get("tools") is None and body.get("stream") is False
        assert "synthetic concise examples" not in json.dumps(body)
        self.send_response(response_status)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(json.dumps({"id": "fixture", "object": "chat.completion", "model": "fixture",
                                    "choices": [{"index": 0, "message": {"role": "assistant", "content": '{"total":9}'}, "finish_reason": "stop"}],
                                    "usage": {"total_tokens": 8}}).encode())


server = ThreadingHTTPServer(("127.0.0.1", 0), ModelFixture)
thread = threading.Thread(target=server.serve_forever, daemon=True)
thread.start()
token = set_runtime_main("openai", "fixture", base_url=f"http://127.0.0.1:{server.server_port}/v1", api_key="sk-dummy",
                         api_mode="chat_completions", session_id="native-hermes")
session = adapter.MemorySession(root / "bin/cetirun-memory", {}, {"model": "openai/fixture", "sessionId": "native-hermes",
    "modelRevision": adapter.model_revision("openai", "fixture", f"http://127.0.0.1:{server.server_port}/v1", "chat_completions")})
ticket = {"system": "Only synthetic JSON fixtures", "prompt": "Return a fixture total", "maxTokens": 512, "timeoutSeconds": 2}
try:
    result = adapter.bounded_completion(ticket, session)
    assert result["outcome"] == "completed" and result["requests"] == 1
    assert len(requests) == 1
    response_status = 500
    try:
        adapter.bounded_completion(ticket, session)
        raise AssertionError("server error was accepted")
    except Exception as error:
        assert not isinstance(error, AssertionError)
    assert len(requests) == 2, "native SDK retried a failed evaluation"
    session.binding["sessionId"] = "other-session"
    try:
        adapter.bounded_completion(ticket, session)
        raise AssertionError("cross-session model route accepted")
    except ValueError as error:
        assert str(error) == "EvaluationRouteUnavailable"
    assert len(requests) == 2
finally:
    reset_runtime_main(token)
    server.shutdown()
    server.server_close()
    thread.join()

print("Pinned Hermes host checks passed")
