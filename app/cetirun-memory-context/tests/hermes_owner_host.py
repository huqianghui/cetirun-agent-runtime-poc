# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Pinned native Hermes hooks/registry, synthetic identity/data, no model call.

The native suite supplies provisioned Hermes sources, its PM interpreter and a
marked disposable profile. Never use an existing profile or Memory store.
"""
import importlib.util
import json
import os
from pathlib import Path
import sys
from types import SimpleNamespace
from unittest.mock import patch
import uuid

home = Path(os.environ["HERMES_HOME"])
assert (home / "OWNER_ACCESS_TEST_FIXTURE").is_file(), "Synthetic home required"
sys.path.insert(0, os.environ["MEMORY_HERMES_SOURCE"])

from hermes_cli import plugins
from hermes_cli.lifecycle import invoke_hook
from hermes_cli.middleware import apply_llm_request_middleware
from hermes_cli.version_info import get_version_info
from agent.turn_context import _collect_pre_llm_call_context
from agent.runtime_cwd import set_session_cwd
from gateway.session_context import clear_session_vars, set_session_vars
from tools.registry import registry

root = Path(os.environ["CETIRUN_MEMORY_ASSETS"])
workspace = os.environ["MEMORY_TEST_WORKSPACE"]
set_session_cwd(workspace)
spec = importlib.util.spec_from_file_location("memory_owner_host", root / "adapters/hermes/__init__.py")
assert spec and spec.loader
adapter = importlib.util.module_from_spec(spec)
spec.loader.exec_module(adapter)
manager = plugins.PluginManager()
manager._discovered = True
manifest = plugins.PluginManifest(name="cetirun-memory-context", version="1", source="bundled", path=str(root / "adapters/hermes"))
ctx = plugins.PluginContext(manifest, manager)
assert ctx.get_config("owner_access")["platforms"] == ["discord", "telegram"]
adapter.register(ctx, root=root)
assert registry.get_entry("cetirun_memory", scope=manager.scope_key)

note = {"kind": "fact", "title": "Native owner ACL fixture", "scope": {"project": workspace},
        "body": "Synthetic native-hook readback", "temporalClass": "historical", "status": "active",
        "revalidation": {"mode": "on-change"}, "sources": [{"kind": "fixture", "ref": "synthetic-acl",
        "origin": "tool-observed", "assurance": "reported", "observedAt": "2026-11-07T00:00:00Z"}]}
results = []

def start(sid, platform, sender="", parent="", message="Synthetic test", *, turn_id=None,
          chat_id="fixture-chat", chat_type="dm"):
    agent = SimpleNamespace(session_id=sid, platform=platform, _parent_session_id=parent, _user_id=sender, model="fixture")
    tokens = set_session_vars(platform=platform, session_id=sid, chat_id=chat_id, chat_type=chat_type, cwd=workspace)
    try:
        _collect_pre_llm_call_context(agent, effective_task_id=sid, turn_id=turn_id or sid + "-turn",
                                     original_user_message=message, messages=[], conversation_history=[])
    finally:
        clear_session_vars(tokens)

def call(sid, command="query", payload=None):
    raw = registry.dispatch("cetirun_memory", {"command": command,
        "payload": payload if payload is not None else {"queryTerms": [note["title"]]}},
        session_id=sid, tool_call_id="fixture-" + str(uuid.uuid4()))
    result = json.loads(raw) if isinstance(raw, str) else raw
    results.append({"session": sid, "command": command, "ok": result.get("ok"), "reason": result.get("reason")})
    return result

with patch.object(plugins, "get_plugin_manager", return_value=manager):
    start("owner", "discord", "fixture-owner")
    assert call("owner", "capture", {"note": note})["ok"]
    assert call("owner")["data"]["hits"][0]["note"]["body"] == note["body"]
    request = {"messages": [{"role": "user", "content": "fixture"}]}
    assert apply_llm_request_middleware(request, session_id="owner", provider="fixture", model="fixture", api_mode="chat_completions").changed
    invoke_hook("subagent_start", parent_session_id="owner", parent_turn_id="owner-turn", child_session_id="child")
    start("child", "subagent", parent="owner")
    assert call("child")["data"]["hits"][0]["note"]["body"] == note["body"]
    assert call("child", "capture", {"note": {**note, "title": "Native child ACL fixture"}})["ok"]
    start("stranger", "discord", "fixture-stranger")
    assert call("stranger")["reason"] == "AudienceUnknown"
    assert not apply_llm_request_middleware(request, session_id="stranger", provider="fixture", model="fixture").changed
    start("orphan", "subagent", parent="owner")
    assert not call("orphan")["ok"]
    start("owner", "discord", "fixture-owner", message="/memory no-capture")
    assert call("child")["ok"]
    assert call("child", "capture", {"note": note})["reason"] == "CaptureProhibited"
    start("owner", "discord", "fixture-owner", message="/memory off")
    assert call("child")["reason"] == "MemoryDisabled"
    start("telegram", "telegram", "fixture-telegram")
    assert call("telegram")["ok"]
    for sid, platform, chat_id, chat_type in (("blocked-cli", "cli", "", ""),
                                            ("blocked-group", "discord", "fixture-chat", "group"),
                                            ("blocked-chat", "discord", "other-chat", "dm"),
                                            ("missing-chat", "discord", "", "")):
        start(sid, platform, "fixture-owner", chat_id=chat_id, chat_type=chat_type)
        assert call(sid)["reason"] == "AudienceUnknown", sid
    start("interrupted-parent", "discord", "fixture-owner")
    invoke_hook("subagent_start", parent_session_id="interrupted-parent",
                parent_turn_id="interrupted-parent-turn", child_session_id="idle-child")
    start("idle-child", "subagent", parent="interrupted-parent")
    invoke_hook("on_session_end", session_id="interrupted-parent",
                turn_id="interrupted-parent-turn", interrupted=True)
    start("interrupted-parent", "discord", "fixture-owner", turn_id="next-turn")
    assert call("interrupted-parent")["ok"]
    assert call("idle-child")["reason"] == "AudienceUnknown"
    invoke_hook("subagent_start", parent_session_id="interrupted-parent",
                parent_turn_id="next-turn", child_session_id="new-child")
    start("new-child", "subagent", parent="interrupted-parent")
    assert call("new-child")["ok"]
print(json.dumps({"runtime": get_version_info().base_version, "real_hooks_registry": True,
                  "synthetic_identity_and_store": True, "model_requests": 0, "results": results}, indent=2))
