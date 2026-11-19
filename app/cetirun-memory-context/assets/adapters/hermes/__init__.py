# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Hermes plugin: request-only context, scoped tools, and minimal events."""

import asyncio
from contextvars import ContextVar
import hashlib
import json
import os
from pathlib import Path
import subprocess
import threading
import time
import uuid

ADAPTER = "memory-adapter/v1"
COMMANDS = ("status", "query", "capture", "prepare", "commit", "propose", "check", "advance",
            "select", "retire", "revise", "record-source-check", "record-use", "trace", "lint", "source", "evaluate")
MEMORY_OFF = "CetiRun Memory is off for this conversation. Do not read or save private memory, or claim to have done so."
# The Memory view reaches the model as a final user message. A smart-routing
# model, such as the AI Gateway's complexity routers, classifies a request by
# its newest user message and leaves out blocks between these markers, so
# without them the view, not the person's request, chose the tier. A marker
# inside the view would end the block early, so it is broken up.
REMINDER_OPEN, REMINDER_CLOSE = "<system-reminder>", "</system-reminder>"


def reminder(text):
    body = text.replace(REMINDER_OPEN, "<system reminder>").replace(REMINDER_CLOSE, "</system reminder>")
    return REMINDER_OPEN + "\n" + body + "\n" + REMINDER_CLOSE


def model_revision(provider, model, base_url, api_mode):
    return "sha256:" + hashlib.sha256(json.dumps([provider, model, base_url or "", api_mode], separators=(",", ":")).encode()).hexdigest()


def owner_private_session(platform, parent_session_id, sender_id, access=None, *, chat_id="", chat_type=""):
    if parent_session_id or not platform or platform == "subagent":
        return False
    access = access or {}
    if "platforms" in access and platform not in access["platforms"]:
        return False
    if platform in ("cron", "acp") and access.get(platform) is False:
        return False
    for key, value in (("senders", sender_id), ("chats", chat_id), ("chat_types", chat_type)):
        limits = access.get(key, {})
        if platform in limits and value not in limits[platform]:
            return False
    return True


def restriction_env(name):
    value = os.environ.get(name, "").lower()
    if value not in ("", "false", "true", "0", "1"):
        raise ValueError("MemoryRestrictionsInvalid")
    return value in ("true", "1")


def owner_access(ctx):
    def valid_identifier(value, limit):
        return (isinstance(value, str) and 0 < len(value) <= limit and value.strip() == value
                and "*" not in value and not any(ord(c) < 32 for c in value))

    def valid_identifiers(values, limit):
        return isinstance(values, list) and len(values) <= 32 and all(valid_identifier(value, limit) for value in values)

    configured = ctx.get_config("owner_access", {}) if hasattr(ctx, "get_config") else {}
    try:
        environment = json.loads(os.environ.get("CETIRUN_MEMORY_OWNER_ACCESS") or "{}")
    except ValueError:
        raise ValueError("MemoryOwnerAccessInvalid") from None
    policies = (configured, environment)
    for access in policies:
        if not isinstance(access, dict) or set(access) - {"platforms", "senders", "chats", "chat_types", "subagents", "cron", "acp"}:
            raise ValueError("MemoryOwnerAccessInvalid")
        if any(type(access[key]) is not bool for key in ("subagents", "cron", "acp") if key in access):
            raise ValueError("MemoryOwnerAccessInvalid")
        if "platforms" in access and not valid_identifiers(access["platforms"], 64):
            raise ValueError("MemoryOwnerAccessInvalid")
        for key in ("senders", "chats", "chat_types"):
            limits = access.get(key, {})
            if not isinstance(limits, dict) or len(limits) > 32:
                raise ValueError("MemoryOwnerAccessInvalid")
            for platform, values in limits.items():
                if not valid_identifier(platform, 64) or not valid_identifiers(values, 256):
                    raise ValueError("MemoryOwnerAccessInvalid")
    return policies


class OwnerGrant:
    """Process-local native lineage, never read from a tool payload or transcript."""
    def __init__(self, principal, cwd, private, controls, parent=None):
        self.principal, self.cwd, self.allowed = principal, cwd, private
        self.controls, self.parent = dict(controls), parent

    def refresh(self):
        if self.parent:
            self.parent.refresh()
            self.allowed &= self.parent.allowed
            for key in self.controls:
                self.controls[key] |= self.parent.controls[key]


def load_assets(root):
    manifest_path = root / "manifest.json"
    if manifest_path.is_symlink() or not manifest_path.is_file() or manifest_path.stat().st_size > 256 * 1024 or manifest_path.stat().st_nlink != 1:
        raise ValueError("MemoryManifestInvalid")
    manifest = json.loads(manifest_path.read_text())
    if manifest.get("contractVersion") != "memory-context/v1" or manifest.get("helperABI") != 1:
        raise ValueError("MemoryContractUnsupported")
    files = manifest.get("files", [])
    if not isinstance(files, list) or len(files) > 512:
        raise ValueError("MemoryAssetLimit")
    seen, total = set(), 0
    for item in files:
        relative = item["path"]
        if not isinstance(relative, str) or not relative or relative.startswith("/") or "\\" in relative or any(part in ("", ".", "..") for part in relative.split("/")) or relative in seen:
            raise ValueError("MemoryAssetUnsafe")
        seen.add(relative)
        current = root
        for part in relative.split("/"):
            current = current / part
            if current.is_symlink():
                raise ValueError("MemoryAssetUnsafe")
        stat = current.stat()
        total += stat.st_size
        if not current.is_file() or stat.st_nlink != 1 or total > 32 * 1024 * 1024 or item.get("mode") not in (0o444, 0o555):
            raise ValueError("MemoryAssetUnsafe")
        if "sha256:" + hashlib.sha256(current.read_bytes()).hexdigest() != item["digest"]:
            raise ValueError("MemoryAssetMismatch")
    if not {"AGENTS.md", "skills/cetirun-memory/SKILL.md", "bin/cetirun-memory"}.issubset(seen):
        raise ValueError("MemoryAssetMissing")
    canonical = (root / "AGENTS.md").read_text()
    if len(canonical.encode()) > 2048:
        raise ValueError("MemoryContractTooLarge")
    return canonical, next(item["digest"] for item in files if item["path"] == "bin/cetirun-memory")


class MemorySession:
    def __init__(self, binary, environment, binding):
        self.binary, self.environment, self.binding = binary, environment, binding
        self.cancelled = threading.Event()
        self.checks, self.injected = [], []
        self.loop = self.task = None
        self.model_lock = threading.Lock()
        self.turn_text = None

    def private(self):
        return self.binding["audience"] == "owner-private"

    def writable(self):
        return self.private() and not self.binding["noCapture"] and not self.binding["noMemory"]

    def stop(self):
        self.cancelled.set()
        if self.loop and self.task and not self.loop.is_closed():
            self.loop.call_soon_threadsafe(self.task.cancel)

    def call(self, event, payload=None, command="", event_id=None):
        if self.cancelled.is_set():
            return {"ok": False, "reason": "Cancelled"}
        request = {"binding": {**self.binding, "currentSourceChecks": self.checks}, "event": event,
                   "payload": payload or {}, "command": command, "eventId": event_id or str(uuid.uuid4())}
        raw = json.dumps(request).encode()
        if len(raw) > 128 * 1024:
            return {"ok": False, "reason": "LimitReached"}
        process = None
        try:
            process = subprocess.Popen([str(self.binary), "native-event"],
                                       stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                                       env={**os.environ, **self.environment})
            deadline = time.monotonic() + 5
            first = raw
            while True:
                try:
                    output, _ = process.communicate(first, timeout=0.1)
                    break
                except subprocess.TimeoutExpired:
                    first = None
                    if self.cancelled.is_set() or time.monotonic() >= deadline:
                        process.kill()
                        process.communicate()
                        return {"ok": False, "reason": "CancelledOrTimedOut"}
            if len(output) > 256 * 1024:
                return {"ok": False, "reason": "LimitReached"}
            result = json.loads(output)
            if bool(result.get("ok")) != (process.returncode == 0):
                raise ValueError("MemoryProtocolError")
            return result
        except (OSError, ValueError):
            return {"ok": False, "reason": "MemoryUnavailable"}
        finally:
            if process and process.poll() is None:
                process.kill()
                process.wait()

    def context(self):
        result = self.call("context")
        self.injected = result.get("data", {}).get("refs", []) if result.get("ok") else []
        return result["data"]["context"] if result.get("ok") else "CetiRun Memory: " + result.get("reason", "unavailable") + ". Do not read or save private memory."

    def turn_context(self):
        """Read the view once per turn; a tool loop sends several model requests."""
        if self.turn_text is None:
            if self.binding["noMemory"]:
                self.turn_text = MEMORY_OFF
                return self.turn_text
            self.turn_text = self.context()
            refs = self.injected
            if refs and self.writable():
                key = hashlib.sha256(json.dumps([self.binding["opportunityId"], refs], sort_keys=True).encode()).hexdigest()
                self.call("injected", {"refs": refs}, event_id="injected-" + key)
            self.injected = []
        return self.turn_text

    def tool(self, args, call_id, executor):
        if set(args) - {"command", "payload"} or args.get("command") not in COMMANDS or not isinstance(args.get("payload"), dict):
            return {"ok": False, "reason": "InvalidInput"}
        command, payload = args["command"], args["payload"]
        if command == "evaluate":
            return self.evaluate(payload, call_id, executor)
        if command == "propose" and payload.get("procedure", {}).get("kind") == "playbook":
            payload = json.loads(json.dumps(payload))
            payload["procedure"]["compatibility"] = {"helperABI": "1", "adapter": ADAPTER,
                                                    "nativeVersion": self.binding["nativeVersion"], "model": self.binding["model"]}
            if self.binding.get("modelRevision"):
                payload["procedure"]["compatibility"]["modelRevision"] = self.binding["modelRevision"]
        result = self.call("tool", payload, command, call_id)
        if command == "source" and result.get("ok"):
            latest = result["data"]
            self.checks = [check for check in self.checks
                           if (check["check"]["sourceRef"], check["check"]["targetRef"]["id"]) !=
                           (latest["check"]["sourceRef"], latest["check"]["targetRef"]["id"])]
            self.checks = [*self.checks, latest][-8:]
        if command == "query" and result.get("ok") and not self.binding["noCapture"]:
            refs = [hit["ref"] for hit in result["data"].get("hits", [])]
            if refs:
                self.call("injected", {"refs": refs}, event_id=call_id + "-result")
        return result

    def evaluate(self, payload, call_id, executor):
        if not self.binding["modelEvaluation"] or not self.model_lock.acquire(blocking=False):
            return {"ok": False, "reason": "EvaluationUnavailable"}
        try:
            return self._evaluate(payload, call_id, executor)
        finally:
            self.model_lock.release()

    def _evaluate(self, payload, call_id, executor):
        reserved = self.call("evaluation-next", payload, event_id=call_id)
        if not reserved.get("ok"):
            return reserved
        ticket = reserved["data"]
        if "call" not in ticket:
            return reserved
        try:
            result = executor(ticket, self)
        except (Exception, asyncio.CancelledError):
            result = {"output": "", "outcome": "unknown", "requests": 1, "model": self.binding["model"]}
        return self.call("evaluation-result", {"id": ticket["call"]["id"], **result}, event_id=call_id + "-result")


def bounded_completion(ticket, session):
    """Use Hermes' current route preparation, with SDK retries disabled.

    This deliberately avoids auxiliary_client.call_llm's recovery/fallback
    ladder. Other native transports are rejected before a model dispatch.
    """
    async def run():
        from agent.auxiliary_client import _plan_aux_call, _RUNTIME_MAIN_CONTEXT
        from openai import AsyncOpenAI
        session.loop, session.task = asyncio.get_running_loop(), asyncio.current_task()
        if session.cancelled.is_set():
            raise asyncio.CancelledError()
        runtime = _RUNTIME_MAIN_CONTEXT.get()
        if not isinstance(runtime, dict) or runtime.get("session_id") != session.binding["sessionId"] or runtime.get("provider", "") + "/" + runtime.get("model", "") != session.binding["model"]:
            raise ValueError("EvaluationRouteUnavailable")
        if model_revision(runtime["provider"], runtime["model"], runtime.get("base_url"), runtime.get("api_mode")) != session.binding.get("modelRevision"):
            raise ValueError("EvaluationRouteChanged")
        prepared, _, _ = _plan_aux_call(
            None, async_mode=True, provider=runtime["provider"], model=runtime["model"],
            base_url=runtime.get("base_url"), api_key=runtime.get("api_key"),
            main_runtime=runtime, messages=[{"role": "system", "content": ticket["system"]},
                                        {"role": "user", "content": ticket["prompt"]}],
            temperature=0, max_tokens=ticket["maxTokens"], tools=None, timeout=ticket["timeoutSeconds"],
            extra_body={"metadata": {"tags": ["cetirun-memory-evaluation", "artifact-contract-v1"]}},
            reasoning_config=None, extra_headers=None, api_mode=runtime.get("api_mode"), route_info=None,
        )
        model = runtime["provider"] + "/" + prepared.final_model
        if not isinstance(prepared.client, AsyncOpenAI) or model != session.binding["model"] or not runtime.get("base_url") or str(prepared.client.base_url).rstrip("/") != runtime["base_url"].rstrip("/"):
            raise ValueError("EvaluationRouteUnsupported")
        kwargs = {**prepared.kwargs, "stream": False}
        kwargs.pop("tools", None)
        kwargs.pop("tool_choice", None)
        response = await asyncio.wait_for(
            prepared.client.with_options(max_retries=0, timeout=ticket["timeoutSeconds"]).chat.completions.create(**kwargs),
            ticket["timeoutSeconds"],
        )
        choice = response.choices[0]
        return {"output": choice.message.content or "", "outcome": "completed" if choice.finish_reason == "stop" else "unknown",
                "requests": 1, "model": model, "tokens": getattr(response.usage, "total_tokens", None)}
    try:
        return asyncio.run(run())
    finally:
        session.loop = session.task = None


def register(ctx, *, root=None, session_factory=MemorySession, executor=bounded_completion):
    selected = os.environ.get("CETIRUN_MEMORY_ASSETS")
    if root is None and not selected:
        return
    root = Path(root or selected)
    if not root.is_absolute():
        raise ValueError("MemoryBindingInvalid")
    memory_contract, helper_digest = load_assets(root)
    binary = Path(os.environ.get("CETIRUN_MEMORY_BINARY") or root / "bin/cetirun-memory")
    if not binary.is_absolute():
        raise ValueError("MemoryBindingInvalid")
    if binary.is_symlink() or not binary.is_file() or binary.stat().st_nlink != 1 or binary.stat().st_size > 32 * 1024 * 1024:
        raise ValueError("MemoryAssetUnsafe")
    if "sha256:" + hashlib.sha256(binary.read_bytes()).hexdigest() != helper_digest:
        raise ValueError("MemoryAssetMismatch")
    environment = {"HOME": str(Path.home()), "CETIRUN_MEMORY_ROOT": os.environ.get("CETIRUN_MEMORY_ROOT", "")}
    data_root = environment["CETIRUN_MEMORY_ROOT"]
    if data_root and (not os.path.isabs(data_root) or os.path.normpath(data_root) != data_root or data_root == "/"):
        raise ValueError("MemoryBindingInvalid")
    payload_schema = json.loads((root / "adapters/common/payload-schema.json").read_text())
    context_file = Path(os.environ.get("CETIRUN_MEMORY_CONTEXT") or Path.home() / ".agents/AGENTS.md")
    if not context_file.is_absolute():
        raise ValueError("MemoryBindingInvalid")
    context_dir = context_file.parent
    if context_dir.is_symlink() or context_file.is_symlink() or not context_file.is_file() or context_file.stat().st_size > 32768 or context_file.stat().st_nlink != 1:
        raise ValueError("AgentContextUnavailable")
    canonical = context_file.read_text()
    if len(canonical) > 4000:
        raise ValueError("AgentContextTooLarge")
    from hermes_cli.version_info import get_version_info
    native_version = get_version_info().base_version
    access = owner_access(ctx)
    allow_children = all(policy.get("subagents", True) for policy in access)
    restrictions = {"noCapture": restriction_env("CETIRUN_MEMORY_NO_CAPTURE"),
                    "noMemory": restriction_env("CETIRUN_MEMORY_NO_MEMORY")}
    sessions, grants, spawns, guard = {}, {}, {}, threading.RLock()
    tool_identity = ContextVar("cetirun_memory_tool_identity", default=None)

    def state(session_id=None):
        if not session_id:
            from gateway.session_context import get_session_env
            session_id = get_session_env("HERMES_SESSION_ID")
        with guard:
            current = sessions.get(session_id)
            grant = grants.get(session_id)
            if current and grant:
                grant.refresh()
                changed = any(current.binding[key] != value for key, value in grant.controls.items())
                current.binding.update(grant.controls)
                current.binding["audience"] = "owner-private" if grant.allowed else "unknown"
                # A tightened parent restriction invalidates the cached private view.
                if changed or not grant.allowed or current.binding["noMemory"]:
                    current.turn_text = None
            return current

    def spawn(parent_session_id="", parent_turn_id="", child_session_id="", **_kwargs):
        if not allow_children or not child_session_id or child_session_id == parent_session_id:
            return
        with guard:
            parent = state(parent_session_id)
            if (not parent or parent.cancelled.is_set() or not parent.private()
                    or not parent_turn_id or parent.binding["opportunityId"] != parent_turn_id
                    or child_session_id in grants or child_session_id in spawns):
                return
            grant = grants[parent_session_id]
            spawns[child_session_id] = (parent_session_id, OwnerGrant(
                ("subagent", parent_session_id), grant.cwd, True, grant.controls, grant))
            if len(spawns) > 128:
                spawns.pop(next(iter(spawns)))

    def start(session_id="", platform="", parent_session_id="", sender_id="", user_message="", turn_id="", model="", conversation_history=None, **kwargs):
        from agent.runtime_cwd import resolve_agent_cwd
        if not session_id:
            return
        from gateway.session_context import get_session_env
        chat_id = chat_type = ""
        if get_session_env("HERMES_SESSION_ID") == session_id and get_session_env("HERMES_SESSION_PLATFORM") == platform:
            chat_id = get_session_env("HERMES_SESSION_CHAT_ID")
            chat_type = get_session_env("HERMES_SESSION_CHAT_TYPE")
        private = all(owner_private_session(platform, parent_session_id, sender_id, policy,
                                           chat_id=chat_id, chat_type=chat_type) for policy in access)
        with guard:
            inherited = spawns.pop(session_id, None)
            old_grant = grants.get(session_id)
            if platform == "subagent" and parent_session_id:
                grant = inherited[1] if inherited and inherited[0] == parent_session_id else old_grant
                if not grant or grant.principal != ("subagent", parent_session_id) or not grant.parent:
                    grant = None
                if grant:
                    grant.refresh()
                    private = grant.allowed
            else:
                grant = None
            previous = sessions.get(session_id)
            controls = {"noCapture": False, "noMemory": False}
            if previous:
                controls = {key: previous.binding[key] for key in controls}
                previous.stop()
            else:
                for message in conversation_history or []:
                    if not isinstance(message, dict) or message.get("role") != "user":
                        continue
                    text = message.get("content")
                    if isinstance(text, str) and text.strip() in ("/memory off", "/memory no-capture"):
                        controls["noCapture"] = True
                        controls["noMemory"] |= text.strip() == "/memory off"
            if private and not parent_session_id and isinstance(user_message, str) and user_message.strip() in ("/memory off", "/memory no-capture", "/memory on"):
                mode = user_message.strip().split()[1]
                controls = {"noCapture": mode != "on", "noMemory": mode == "off"}
            controls["noCapture"] |= restrictions["noCapture"] or kwargs.get("noCapture") is True
            controls["noMemory"] |= restrictions["noMemory"] or kwargs.get("noMemory") is True
            cwd = str(resolve_agent_cwd())
            if grant:
                cwd = grant.cwd
                controls = {key: controls[key] or grant.controls[key] for key in controls}
                grant.controls = controls
            else:
                principal = (platform, sender_id, chat_id, chat_type)
                if old_grant and old_grant.allowed and old_grant.principal == principal and old_grant.cwd == cwd and not old_grant.parent:
                    grant = old_grant
                    grant.allowed, grant.controls = private, controls
                else:
                    if old_grant:
                        old_grant.allowed = False
                    grant = OwnerGrant(principal, cwd, private, controls)
            grants[session_id] = grant
            # Apply restrictions to descendants now, even if they are between turns.
            for item in [*grants.values(), *(value[1] for value in spawns.values())]:
                item.refresh()
            provider = kwargs.get("provider", "")
            # The effective route is resolved by native request middleware.
            binding = {"adapter": ADAPTER, "entrypoint": "hermes", "nativeVersion": native_version,
                       "sessionId": session_id, "opportunityId": turn_id or str(uuid.uuid4()),
                       "cwd": cwd, "audience": "owner-private" if private else "unknown",
                       "destination": {"kind": "owner-private" if private else "unknown"},
                       "parentSessionId": parent_session_id or "", "evaluation": False, **controls,
                       "model": provider + "/" + model, "modelEvaluation": False}
            if private and grant.parent:
                binding["ownerAuthorizedChild"] = True
            sessions[session_id] = session_factory(binary, environment, binding)
            if len(sessions) > 128:
                evicted = next(iter(sessions))
                sessions.pop(evicted).stop()
                grants.pop(evicted).allowed = False
        if not parent_session_id and sessions[session_id].writable() and isinstance(user_message, str) and user_message.lower().startswith(("remember my preference:", "记住我的偏好：")):
            sessions[session_id].call("owner-preference", {"message": user_message}, event_id=binding["opportunityId"] + "-preference")

    def request(request, session_id="", provider="", model="", api_request_id="", base_url="", **kwargs):
        current = state(session_id)
        if not current:
            return None
        current.binding["model"] = provider + "/" + model
        current.binding["modelRevision"] = model_revision(provider, model, base_url, kwargs.get("api_mode"))
        current.binding["modelEvaluation"] = kwargs.get("api_mode") in ("chat_completions", "chat", "openai")
        # Explicitly excluded sessions and unqualified children get no private view.
        if not current.private():
            return None
        messages = request.get("messages")
        if not isinstance(messages, list):
            # Do not guess another transport's message schema.
            return None
        text = current.turn_context()
        if text.startswith(memory_contract):
            text = text[len(memory_contract):].lstrip()
        next_request = dict(request)
        next_request["messages"] = [*messages, {"role": "user", "content": reminder(text)}]
        return {"request": next_request, "source": "cetirun-memory-context", "reason": "current private Memory view"}

    def tool(args, **kwargs):
        current = state(kwargs.get("session_id"))
        if not current:
            return json.dumps({"ok": False, "reason": "NativeContextUnavailable"})
        if current.cancelled.is_set():
            return json.dumps({"ok": False, "reason": "Cancelled"})
        identity = tool_identity.get()
        call_id = kwargs.get("tool_call_id") or (identity[1] if identity and identity[0] == current.binding["sessionId"] else None)
        if not call_id:
            if args.get("command") not in ("status", "prepare", "lint", "trace", "source"):
                return json.dumps({"ok": False, "reason": "NativeCallIdentityUnavailable"})
            call_id = str(uuid.uuid4())
        return json.dumps(current.tool(args, call_id, executor))

    def execute(tool_name, args, next_call, session_id="", tool_call_id="", **_kwargs):
        if tool_name != "cetirun_memory":
            return next_call(args)
        token = tool_identity.set((session_id, tool_call_id))
        try:
            return next_call(args)
        finally:
            tool_identity.reset(token)

    def feedback(tool_name="", status="", session_id="", tool_call_id="", **_kwargs):
        current = state(session_id)
        if current and tool_name == "cetirun_memory" and status == "cancelled":
            current.stop()
        elif current and current.writable() and tool_name != "cetirun_memory" and status in ("error", "blocked"):
            current.call("feedback", {"tool": tool_name, "category": "failure" if status == "error" else "permission-denied"}, event_id=tool_call_id or str(uuid.uuid4()))

    def outbound(tool_name="", session_id="", **_kwargs):
        current = state(session_id)
        if current and current.binding["audience"] == "owner-private" and not current.binding["noMemory"] and tool_name in ("send_message", "delegate_task"):
            if tool_name == "delegate_task" and allow_children:
                return None
            return {"action": "approve", "message": "This task may contain Agent-private memory. Confirm the exact recipient and minimum outgoing content; a child receives no automatic Memory access."}

    def finish(session_id="", turn_id="", completed=False, interrupted=False, **_kwargs):
        current = state(session_id)
        if current and (not turn_id or current.binding["opportunityId"] == turn_id):
            if interrupted and session_id in grants:
                grants[session_id].allowed = False
            if completed and not interrupted and current.writable():
                current.call("boundary")
            current.stop()

    def finalize(session_id="", **_kwargs):
        current = state(session_id)
        if current:
            current.stop()

    ctx.register_system_prompt_section("cetirun-memory-context", canonical, max_chars=4000)
    ctx.register_hook("pre_llm_call", start)
    ctx.register_hook("subagent_start", spawn)
    ctx.register_middleware("llm_request", request)
    ctx.register_middleware("tool_execution", execute)
    ctx.register_hook("post_tool_call", feedback)
    ctx.register_hook("pre_tool_call", outbound)
    ctx.register_hook("on_session_end", finish)
    ctx.register_hook("on_session_finalize", finalize)
    ctx.register_tool(name="cetirun_memory", toolset="cetirun_memory", handler=tool, schema={
        "name": "cetirun_memory", "description": "Use this Agent's private Memory: look up earlier conversations before answering, save facts, preferences, decisions and your own recommendations worth keeping, correct notes, and propose scoped method candidates. Read the cetirun-memory skill for payload formats. This tool cannot grant authority, change request restrictions or shared skills.",
        "parameters": {"type": "object", "additionalProperties": False, "properties": {
            "command": {"type": "string", "description": "Use query with queryTerms to read stored note bodies. source only checks a recorded workspace file against its digest; it requires ref and sourceRef and does not read a note. Do not repeat a source check that returns SourceAssuranceUnavailable.", "enum": list(COMMANDS)}, "payload": payload_schema}, "required": ["command", "payload"]},
    })
