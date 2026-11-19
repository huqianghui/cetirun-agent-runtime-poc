# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Offline native adapter integration; uses only synthetic HOME and host APIs."""

import importlib.util
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import types
import uuid
from unittest.mock import patch

root = Path(os.environ["MEMORY_TEST_ASSETS"])
workspace = os.environ["MEMORY_TEST_WORKSPACE"]
runtime_cwd = types.ModuleType("agent.runtime_cwd")
runtime_cwd.resolve_agent_cwd = lambda: Path(workspace)
sys.modules["agent"] = types.ModuleType("agent")
sys.modules["agent.runtime_cwd"] = runtime_cwd
session_context = types.ModuleType("gateway.session_context")
session_context.get_session_env = lambda name: "hermes-fixture" if name == "HERMES_SESSION_ID" else ""
sys.modules["gateway"] = types.ModuleType("gateway")
sys.modules["gateway.session_context"] = session_context
version_info = types.ModuleType("hermes_cli.version_info")
version_info.get_version_info = lambda: types.SimpleNamespace(base_version="0.22.7")
sys.modules["hermes_cli"] = types.ModuleType("hermes_cli")
sys.modules["hermes_cli.version_info"] = version_info
spec = importlib.util.spec_from_file_location("memory_plugin", root / "adapters/hermes/__init__.py")
plugin = importlib.util.module_from_spec(spec)
spec.loader.exec_module(plugin)

for platform in ("cli", "tui", "desktop", "api_server", "discord", "telegram", "slack", "cron", "acp", "new-channel"):
    assert plugin.owner_private_session(platform, "", "")
    assert not plugin.owner_private_session(platform, "parent", "sender")
assert not plugin.owner_private_session("", "", "sender")
assert not plugin.owner_private_session("subagent", "", "sender")


class Host:
    def __init__(self, config=None):
        self.config = {} if config is None else config
        self.hooks, self.middleware, self.tools, self.sections = {}, {}, {}, {}

    def get_config(self, key, default=None):
        return self.config.get(key, default)

    def register_hook(self, name, callback):
        self.hooks[name] = callback

    def register_middleware(self, name, callback):
        self.middleware[name] = callback

    def register_tool(self, **kwargs):
        self.tools[kwargs["name"]] = kwargs

    def register_system_prompt_section(self, name, content, **kwargs):
        self.sections[name] = content


def start_session(selected_host, sid, platform="discord", sender="owner-discord", *,
                  chat_type="dm", chat_id="fixture-chat", turn_id=None, **kwargs):
    native = {"HERMES_SESSION_ID": sid, "HERMES_SESSION_PLATFORM": platform,
              "HERMES_SESSION_CHAT_ID": chat_id, "HERMES_SESSION_CHAT_TYPE": chat_type}
    with patch.object(session_context, "get_session_env", lambda name: native.get(name, "")):
        selected_host.hooks["pre_llm_call"](session_id=sid, platform=platform, sender_id=sender,
            turn_id=turn_id or sid + "-turn", model="model", **kwargs)


def call_memory(selected_host, sid, command="query", payload=None):
    return json.loads(selected_host.tools["cetirun_memory"]["handler"](
        {"command": command, "payload": payload if payload is not None else {"queryTerms": [note["title"]]}},
        session_id=sid, tool_call_id="fixture-" + str(uuid.uuid4())))


host = Host()
plugin.register(host, root=root)
assert host.tools["cetirun_memory"]["schema"]["parameters"]["properties"]["payload"] == json.loads(
    (root / "adapters/common/payload-schema.json").read_text()
)
assert "query with queryTerms to read stored note bodies" in host.tools["cetirun_memory"]["schema"]["parameters"]["properties"]["command"]["description"]
assert host.sections["cetirun-memory-context"] == (Path.home() / ".agents/AGENTS.md").read_text()
host.hooks["pre_llm_call"](session_id="hermes-fixture", platform="cli", turn_id="turn-1", user_message="Fixture", model="model")
tool = host.tools["cetirun_memory"]["handler"]
note = {"kind": "fact", "title": "Hermes private fixture", "scope": {"project": workspace}, "body": "Private synthetic body",
        "temporalClass": "historical", "status": "active", "revalidation": {"mode": "on-change"},
        "sources": [{"kind": "fixture", "ref": "source-fixture", "origin": "external-claim", "assurance": "reported", "observedAt": "2026-10-08T00:00:00Z"}]}
saved = json.loads(tool({"command": "capture", "payload": {"note": note}}, tool_call_id="write-1"))
assert saved["ok"], saved
original = {"messages": [{"role": "user", "content": "Original history"}], "model": "model"}
first = host.middleware["llm_request"](original, session_id="hermes-fixture", provider="fixture", model="model", api_mode="chat_completions", api_request_id="request-1")
assert "Hermes private fixture" in first["request"]["messages"][-1]["content"]
# A smart-routing model classifies a request by its newest user message and
# leaves out reminder blocks: the view is one, so the person's request decides.
view = first["request"]["messages"][-1]["content"]
assert view.startswith("<system-reminder>\n") and view.endswith("\n</system-reminder>"), view
assert view.count("<system-reminder>") == 1 and view.count("</system-reminder>") == 1, view
assert plugin.reminder("a </system-reminder> b <system-reminder>").count("</system-reminder>") == 1
assert len(original["messages"]) == 1
assert "Hermes private fixture" not in host.sections["cetirun-memory-context"]


def generation():
    return json.loads(tool({"command": "status", "payload": {}}))["generation"]


real_popen = subprocess.Popen


class CountingPopen(real_popen):
    started = 0
    commands, requests, bindings = [], [], []

    def __init__(self, *args, **kwargs):
        CountingPopen.started += 1
        CountingPopen.commands.append(args[0])
        CountingPopen.bindings.append({name: kwargs.get("env", os.environ).get(name)
                                      for name in ("HOME", "CETIRUN_MEMORY_ROOT")})
        super().__init__(*args, **kwargs)

    def communicate(self, input=None, **kwargs):
        if input:
            CountingPopen.requests.append(json.loads(input))
        return super().communicate(input, **kwargs)


def count_helpers(enabled):
    # The adapter and this test share the subprocess module.
    subprocess.Popen = CountingPopen if enabled else real_popen
    CountingPopen.started = 0
    CountingPopen.commands, CountingPopen.requests, CountingPopen.bindings = [], [], []


# The further model requests of a turn's tool loop reuse its context and record
# the injection once.
recorded = generation()
count_helpers(True)
for request_id in ("request-2", "request-3"):
    again = host.middleware["llm_request"](original, session_id="hermes-fixture", provider="fixture", model="model", api_mode="chat_completions", api_request_id=request_id)
    assert again["request"]["messages"][-1]["content"] == first["request"]["messages"][-1]["content"]
assert CountingPopen.started == 0, CountingPopen.started
count_helpers(False)
assert generation() == recorded
host.hooks["pre_llm_call"](session_id="hermes-fixture", platform="cli", turn_id="turn-1b", user_message="Next turn", model="model")
host.middleware["llm_request"](original, session_id="hermes-fixture", provider="fixture", model="model", api_mode="chat_completions", api_request_id="request-4")
assert generation() == recorded + 1
# Long native turn IDs are valid bindings too. Their injection must be
# recorded by the real helper, once per turn, without exceeding eventId's cap.
for turn_id in ("t" * 210, "t" * 256):
    host.hooks["pre_llm_call"](session_id="hermes-fixture", platform="api_server", turn_id=turn_id, user_message="Long turn", model="model")
    before = generation()
    request = host.middleware["llm_request"](original, session_id="hermes-fixture", provider="fixture", model="model", api_mode="chat_completions")
    assert "Hermes private fixture" in request["request"]["messages"][-1]["content"]
    assert generation() == before + 1, "long turn lost its injection record"
    host.middleware["llm_request"](original, session_id="hermes-fixture", provider="fixture", model="model", api_mode="chat_completions")
    assert generation() == before + 1, "same turn recorded twice"
host.hooks["post_tool_call"](session_id="hermes-fixture", tool_name="fixture_tool", tool_call_id="failure-1", status="error", result="Do not persist raw outputs")
prepared = json.loads(tool({"command": "prepare", "payload": {}}))
assert prepared["data"]["observations"][0]["assurance"] == "adapter-observed"
assert "Do not persist raw outputs" not in json.dumps(prepared)
host.hooks["pre_llm_call"](session_id="hermes-fixture", platform="cli", turn_id="turn-2", user_message="/memory no-capture", model="model")
denied = json.loads(tool({"command": "capture", "payload": {"note": note}}, tool_call_id="denied-write"))
assert denied["reason"] == "CaptureProhibited", denied
host.hooks["pre_llm_call"](session_id="hermes-fixture", platform="cli", turn_id="turn-2b", user_message="/memory off", model="model")
count_helpers(True)
off = host.middleware["llm_request"](original, session_id="hermes-fixture", provider="fixture", model="model", api_mode="chat_completions", api_request_id="request-off")
assert off["request"]["messages"][-1]["content"] == plugin.reminder(plugin.MEMORY_OFF), off
assert CountingPopen.started == 0, CountingPopen.started
count_helpers(False)
host.hooks["pre_llm_call"](session_id="hermes-fixture", platform="", turn_id="turn-3", user_message="group", model="model")
# Missing native provenance still gets no Memory context or automatic events.
count_helpers(True)
shared = host.middleware["llm_request"](original, session_id="hermes-fixture", provider="fixture", model="model")
assert shared is None, shared
host.hooks["post_tool_call"](session_id="hermes-fixture", tool_name="fixture_tool", tool_call_id="shared-failure", status="error")
host.hooks["on_session_end"](session_id="hermes-fixture", turn_id="turn-3", completed=True)
assert CountingPopen.started == 0, CountingPopen.started
count_helpers(False)
host.hooks["pre_llm_call"](session_id="hermes-fixture", platform="cli", turn_id="turn-4", user_message="Fixture", model="model")
host.hooks["on_session_end"](session_id="hermes-fixture", completed=True)
assert json.loads(tool({"command": "query", "payload": {"queryTerms": ["Hermes private fixture"]}}))["reason"] == "Cancelled"

# No policy means every admitted channel, including anonymous channel posts.
for platform in ("cli", "tui", "desktop", "api_server", "discord", "telegram", "slack", "cron", "acp", "new-channel"):
    sid = "default-" + platform
    start_session(host, sid, platform, "")
    assert call_memory(host, sid)["ok"], platform
    assert call_memory(host, sid, "capture", {"note": {**note, "title": sid}})["ok"], platform
for chat_type in ("dm", "group", "channel"):
    sid = "default-discord-" + chat_type
    start_session(host, sid, chat_type=chat_type)
    assert call_memory(host, sid)["ok"], chat_type
host.hooks["subagent_start"](parent_session_id="default-cli", parent_turn_id="default-cli-turn", child_session_id="default-child")
start_session(host, "default-child", "subagent", "", parent_session_id="default-cli")
assert call_memory(host, "default-child")["ok"]

# Explicit policy narrows the default, including local entrypoints.
owner_access = {"platforms": ["discord", "telegram", "cron", "acp"],
                "senders": {"discord": ["owner-discord"], "telegram": ["owner-telegram"]},
                "subagents": True, "cron": True, "acp": True}
acl_host = Host({"owner_access": owner_access})
plugin.register(acl_host, root=root)
acl_tool = acl_host.tools["cetirun_memory"]["handler"]

def acl_start(sid, platform="discord", sender="owner-discord", **kwargs):
    start_session(acl_host, sid, platform, sender, **kwargs)

def acl_call(sid, command="query", payload=None):
    return call_memory(acl_host, sid, command, payload)

for platform, sender in (("discord", "owner-discord"), ("telegram", "owner-telegram")):
    for chat_type in ("dm", "group", "channel"):
        sid = platform + "-" + chat_type
        acl_start(sid, platform, sender, chat_type=chat_type)
        found = acl_call(sid)
        assert found["ok"], (sid, found)
        assert found["data"]["hits"][0]["note"]["body"] == note["body"]
        assert acl_call(sid, "capture", {"note": {**note, "title": "Owner " + sid}})["ok"]
for sid, platform, sender in (("stranger", "discord", "stranger"), ("missing", "telegram", ""),
                              ("cross-platform", "telegram", "owner-discord"), ("unknown", "slack", "owner-discord"),
                              ("denied-cli", "cli", ""), ("denied-tui", "tui", ""),
                              ("denied-api", "api_server", ""), ("denied-desktop", "desktop", "")):
    acl_start(sid, platform, sender)
    assert not acl_call(sid)["ok"], sid
    assert acl_host.middleware["llm_request"](original, session_id=sid) is None
for platform in ("acp", "cron"):
    acl_start(platform, platform, "")
    assert acl_call(platform)["ok"]

acl_start("parent")
assert acl_host.hooks["pre_tool_call"](tool_name="delegate_task", session_id="parent") is None
assert acl_host.hooks["pre_tool_call"](tool_name="send_message", session_id="parent")["action"] == "approve"
acl_start("forged-child", "subagent", "", parent_session_id="parent")
assert not acl_call("forged-child")["ok"], "parent ID alone is not an owner grant"
spawn = acl_host.hooks["subagent_start"]
spawn(parent_session_id="parent", parent_turn_id="parent-turn", child_session_id="child")
# The child works in another directory but inherits its parent's memory scope.
setattr(runtime_cwd, "resolve_agent_cwd", lambda: Path(workspace) / "child-worktree")
acl_start("child", "subagent", "", parent_session_id="parent", user_message="/memory on")
setattr(runtime_cwd, "resolve_agent_cwd", lambda: Path(workspace))
child_result = acl_call("child")
assert child_result.get("ok"), child_result
assert child_result["data"]["hits"][0]["note"]["body"] == note["body"]
assert acl_call("child", "capture", {"note": {**note, "title": "Child synthetic note"}})["ok"]
spawn(parent_session_id="child", parent_turn_id="child-turn", child_session_id="grandchild")
acl_start("grandchild", "subagent", "", parent_session_id="child")
assert acl_call("grandchild")["ok"]
acl_start("child", "subagent", "", parent_session_id="wrong-parent")
assert not acl_call("child")["ok"], "a resumed child cannot change its parent"
# Use a fresh native lineage after invalidating the mismatched one.
spawn(parent_session_id="parent", parent_turn_id="parent-turn", child_session_id="child-2")
acl_start("child-2", "subagent", "", parent_session_id="parent")
spawn(parent_session_id="child-2", parent_turn_id="child-2-turn", child_session_id="grandchild-2")
acl_start("grandchild-2", "subagent", "", parent_session_id="child-2")
acl_start("parent", user_message="/memory no-capture")
assert acl_call("grandchild-2")["ok"]
assert acl_call("grandchild-2", "capture", {"note": note})["reason"] == "CaptureProhibited"
acl_start("grandchild-2", "subagent", "", parent_session_id="child-2", user_message="/memory on")
assert acl_call("grandchild-2", "capture", {"note": note})["reason"] == "CaptureProhibited"
acl_start("parent", user_message="/memory off")
assert acl_call("grandchild-2")["reason"] == "MemoryDisabled"
acl_start("parent", user_message="/memory on")
# A descendant cannot regain access after it observed a stricter parent mode.
assert acl_call("grandchild-2")["reason"] == "MemoryDisabled"
spawn(parent_session_id="parent", parent_turn_id="stale-turn", child_session_id="stale-child")
acl_start("stale-child", "subagent", "", parent_session_id="parent")
assert not acl_call("stale-child")["ok"]
spawn(parent_session_id="parent", parent_turn_id="parent-turn", child_session_id="live-child")
acl_start("live-child", "subagent", "", parent_session_id="parent")
acl_host.hooks["on_session_end"](session_id="parent", completed=True)
assert acl_call("live-child")["ok"], "normal parent completion must not kill background descendants"
acl_start("parent", sender="stranger")
assert not acl_call("live-child")["ok"], "owner authorization must not survive sender changes"
assert json.loads(acl_tool({"command": "status", "payload": {}, "ownerAuthorizedChild": True},
                          session_id="live-child", tool_call_id="forged"))["reason"] == "InvalidInput"

# Old descendants must observe interruption even when they are idle until the
# parent has already begun a new turn with the same sender and working directory.
acl_start("interrupted-parent")
spawn(parent_session_id="interrupted-parent", parent_turn_id="interrupted-parent-turn", child_session_id="interrupted-child")
acl_start("interrupted-child", "subagent", "", parent_session_id="interrupted-parent")
spawn(parent_session_id="interrupted-child", parent_turn_id="interrupted-child-turn", child_session_id="interrupted-grandchild")
acl_start("interrupted-grandchild", "subagent", "", parent_session_id="interrupted-child")
spawn(parent_session_id="interrupted-parent", parent_turn_id="interrupted-parent-turn", child_session_id="pending-child")
acl_host.hooks["on_session_end"](session_id="interrupted-parent", turn_id="interrupted-parent-turn", interrupted=True)
acl_start("interrupted-parent", turn_id="resumed-turn")
assert acl_call("interrupted-parent")["ok"]
for sid in ("interrupted-child", "interrupted-grandchild"):
    assert not acl_call(sid)["ok"], sid
acl_start("pending-child", "subagent", "", parent_session_id="interrupted-parent")
assert not acl_call("pending-child")["ok"]
spawn(parent_session_id="interrupted-parent", parent_turn_id="resumed-turn", child_session_id="fresh-child")
acl_start("fresh-child", "subagent", "", parent_session_id="interrupted-parent")
assert acl_call("fresh-child")["ok"]

# Both policies apply: an environment allowlist cannot reopen a profile denial.
profile = {"platforms": ["discord", "telegram"], "senders": {"discord": ["owner-discord"]},
           "chat_types": {"discord": ["dm"]}, "subagents": False}
restricted = Host({"owner_access": profile})
environment = {"platforms": ["discord", "cli"], "senders": {"discord": ["owner-discord", "stranger"]},
               "chats": {"discord": ["fixture-chat"]}, "subagents": True}
with patch.dict(os.environ, CETIRUN_MEMORY_OWNER_ACCESS=json.dumps(environment)):
    plugin.register(restricted, root=root)
for sid, platform, sender, chat_type, chat_id, allowed in (
    ("limited-owner", "discord", "owner-discord", "dm", "fixture-chat", True),
    ("limited-stranger", "discord", "stranger", "dm", "fixture-chat", False),
    ("limited-telegram", "telegram", "owner-telegram", "dm", "fixture-chat", False),
    ("limited-cli", "cli", "", "dm", "fixture-chat", False),
    ("limited-group", "discord", "owner-discord", "group", "fixture-chat", False),
    ("limited-channel", "discord", "owner-discord", "channel", "fixture-chat", False),
    ("limited-other-chat", "discord", "owner-discord", "dm", "other-chat", False),
    ("limited-unknown-chat", "discord", "owner-discord", "", "", False),
):
    start_session(restricted, sid, platform, sender, chat_type=chat_type, chat_id=chat_id)
    assert call_memory(restricted, sid)["ok"] is allowed, sid
restricted.hooks["subagent_start"](parent_session_id="limited-owner", parent_turn_id="limited-owner-turn", child_session_id="limited-child")
start_session(restricted, "limited-child", "subagent", "", parent_session_id="limited-owner")
assert not call_memory(restricted, "limited-child")["ok"]
# Neither arbitrary hook arguments nor stale context can supply a chat identity.
restricted.hooks["pre_llm_call"](session_id="wrong-context", platform="discord", sender_id="owner-discord",
    chat_id="fixture-chat", chat_type="dm", turn_id="wrong-context-turn")
assert not call_memory(restricted, "wrong-context")["ok"]

for policy in ({"platforms": []}, {"senders": {"discord": []}}, {"chats": {"discord": []}}, {"chat_types": {"discord": []}}):
    denied_host = Host()
    with patch.dict(os.environ, CETIRUN_MEMORY_OWNER_ACCESS=json.dumps(policy)):
        plugin.register(denied_host, root=root)
    start_session(denied_host, "empty-list")
    assert not call_memory(denied_host, "empty-list")["ok"], policy
for platform in ("cron", "acp"):
    denied_host = Host()
    with patch.dict(os.environ, CETIRUN_MEMORY_OWNER_ACCESS=json.dumps({platform: False})):
        plugin.register(denied_host, root=root)
    start_session(denied_host, "disabled-" + platform, platform, "")
    assert not call_memory(denied_host, "disabled-" + platform)["ok"]

# Startup restrictions survive /memory on, later turns and child inheritance.
for variable, reason in (("CETIRUN_MEMORY_NO_CAPTURE", "CaptureProhibited"),
                         ("CETIRUN_MEMORY_NO_MEMORY", "MemoryDisabled")):
    restricted = Host()
    with patch.dict(os.environ, {variable: "true"}):
        plugin.register(restricted, root=root)
    for mode in ("on", "no-capture", "on"):
        start_session(restricted, "mode-parent", "cli", "", user_message="/memory " + mode)
        assert call_memory(restricted, "mode-parent", "capture", {"note": note})["reason"] == reason
        readback = call_memory(restricted, "mode-parent")
        assert readback["ok"] is (variable == "CETIRUN_MEMORY_NO_CAPTURE"), readback
    restricted.hooks["subagent_start"](parent_session_id="mode-parent", parent_turn_id="mode-parent-turn", child_session_id="mode-child")
    start_session(restricted, "mode-child", "subagent", "", parent_session_id="mode-parent", user_message="/memory on")
    assert call_memory(restricted, "mode-child", "capture", {"note": note})["reason"] == reason

for bad in ({"senders": {"discord": ["*"]}}, {"senders": {"discord": "owner-discord"}},
            {"subagents": "true"}, {"unknown": True}, {"platforms": "discord"},
            {"platforms": ["*"]}, {"chats": {"discord": [""]}}, {"chat_types": []}):
    bad_host = Host({"owner_access": bad})
    try:
        plugin.register(bad_host, root=root)
    except ValueError as error:
        assert str(error) == "MemoryOwnerAccessInvalid", error
    else:
        raise AssertionError("malformed owner policy accepted: " + repr(bad))
for variable, value, reason in (("CETIRUN_MEMORY_OWNER_ACCESS", "{", "MemoryOwnerAccessInvalid"),
                                ("CETIRUN_MEMORY_OWNER_ACCESS", "null", "MemoryOwnerAccessInvalid"),
                                ("CETIRUN_MEMORY_NO_CAPTURE", "yes", "MemoryRestrictionsInvalid"),
                                ("CETIRUN_MEMORY_NO_MEMORY", "tru", "MemoryRestrictionsInvalid")):
    with patch.dict(os.environ, {variable: value}):
        try:
            plugin.register(Host(), root=root)
        except ValueError as error:
            assert str(error) == reason, error
        else:
            raise AssertionError("malformed environment policy accepted")

# Load the shipped plugin entrypoint with local paths, including a helper outside
# the asset package. No platform context file or .cetirun data directory is needed.
local_home = Path.home() / ".hermes"
binary = local_home / "bin/memory-helper"
binary.parent.mkdir(parents=True)
shutil.copy2(root / "bin/cetirun-memory", binary)
data_root = local_home / "memories/memory"
context_file = root / "AGENTS.md"
(Path.home() / ".agents/AGENTS.md").unlink()
loader_spec = importlib.util.spec_from_file_location("local_memory_plugin", os.environ["MEMORY_TEST_PLUGIN"])
loader = importlib.util.module_from_spec(loader_spec)
loader_spec.loader.exec_module(loader)
with patch.dict(os.environ, CETIRUN_MEMORY_ROOT=str(data_root), CETIRUN_MEMORY_ASSETS=str(root),
                CETIRUN_MEMORY_BINARY=str(binary), CETIRUN_MEMORY_CONTEXT=str(context_file)):
    version_info.get_version_info = lambda: types.SimpleNamespace(base_version="0.22.8")
    local_host = Host()
    loader.register(local_host)
    assert local_host.sections["cetirun-memory-context"] == context_file.read_text()
    local_host.hooks["pre_llm_call"](session_id="local-fixture", platform="cli", turn_id="local-turn", model="model")
    local_tool = local_host.tools["cetirun_memory"]["handler"]
    count_helpers(True)
    try:
        saved = json.loads(local_tool({"command": "capture", "payload": {"note": note}},
                                      session_id="local-fixture", tool_call_id="local-write"))
        assert saved["ok"], saved
        found = json.loads(local_tool({"command": "query", "payload": {"queryTerms": [note["title"]]}},
                                      session_id="local-fixture", tool_call_id="local-query"))
        assert found["data"]["hits"][0]["note"]["body"] == note["body"], found
        assert data_root.is_dir()
        assert CountingPopen.commands and all(command == [str(binary), "native-event"] for command in CountingPopen.commands)
        assert all(request["binding"]["nativeVersion"] == "0.22.8" for request in CountingPopen.requests)
    finally:
        count_helpers(False)

    # A dotenv reload must not switch an already registered plugin's helper or store.
    changed_root = local_home / "different-store"
    count_helpers(True)
    with patch.dict(os.environ, HOME=str(local_home), CETIRUN_MEMORY_ROOT=str(changed_root),
                    CETIRUN_MEMORY_BINARY="relative-helper", CETIRUN_MEMORY_ASSETS="/missing-assets",
                    CETIRUN_MEMORY_CONTEXT="/missing-context"):
        local_host.hooks["pre_llm_call"](session_id="local-fixture", platform="cli", turn_id="after-reload", model="model")
        found = json.loads(local_tool({"command": "query", "payload": {"queryTerms": [note["title"]]}},
                                     session_id="local-fixture", tool_call_id="after-reload-query"))
        assert found["data"]["hits"][0]["note"]["body"] == note["body"], found
        assert CountingPopen.bindings and all(binding == {"HOME": str(local_home.parent), "CETIRUN_MEMORY_ROOT": str(data_root)}
                                              for binding in CountingPopen.bindings)
        assert not changed_root.exists()
        local_host.hooks["pre_llm_call"](session_id="local-fixture", platform="cli", turn_id="off-after-reload",
                                         user_message="/memory off", model="model")
        disabled = local_host.middleware["llm_request"](original, session_id="local-fixture", provider="fixture", model="model")
        assert disabled["request"]["messages"][-1]["content"] == plugin.reminder(plugin.MEMORY_OFF)
    count_helpers(False)

    other_binary = local_home / "bin/other-helper"
    other_binary.write_text("#!/bin/sh\nexit 0\n")
    other_binary.chmod(0o755)
    linked_binary = local_home / "bin/linked-helper"
    linked_binary.symlink_to(binary)
    for selected, reason in (("relative-helper", "MemoryBindingInvalid"), (str(other_binary), "MemoryAssetMismatch"),
                             (str(linked_binary), "MemoryAssetUnsafe")):
        with patch.dict(os.environ, CETIRUN_MEMORY_BINARY=selected):
            rejected = Host()
            try:
                loader.register(rejected)
            except ValueError as error:
                assert str(error) == reason, error
            else:
                raise AssertionError("unverified helper was accepted: " + selected)
            assert not rejected.tools and not rejected.hooks

    # An explicitly selected missing context must fail rather than fall back to
    # another instruction file; absence of the assets binding still disables it.
    with patch.dict(os.environ, CETIRUN_MEMORY_CONTEXT=str(local_home / "missing.md")):
        try:
            loader.register(Host())
        except ValueError as error:
            assert str(error) == "AgentContextUnavailable", error
        else:
            raise AssertionError("missing configured context was accepted")
    with patch.dict(os.environ, CETIRUN_MEMORY_ASSETS=""):
        disabled = Host()
        loader.register(disabled)
        assert not disabled.tools and not disabled.hooks

print("Hermes native adapter checks passed")
