# Memory access for OpenClaw and Hermes

When the Memory adapter is enabled, sessions admitted by OpenClaw or Hermes can
use the Agent's Memory by default. OpenClaw includes Control UI, TUI, CLI,
message channels, scheduled jobs and heartbeats. Hermes includes CLI, TUI, API,
Desktop, message channels, cron and ACP. Both support native descendants of an
admitted parent. These rules apply to entrypoints that run the Memory adapter;
they do not install it in an unrelated external runtime. Each host still
controls who can reach its entrypoints; the adapter does not change that
authentication.

All allowed sessions use the same Agent-private store, not separate stores per
sender or channel. Replies in a group or channel can disclose recalled material
to its participants. The controls below narrow access when that is not wanted.

## Restrict reads and writes

Set these variables on the OpenClaw or Hermes process, including the gateway
when it handles messages. Unset, `false` or `0` leaves the capability enabled;
`true` or `1` restricts it. Invalid values stop plugin registration.

```bash
CETIRUN_MEMORY_NO_CAPTURE=true hermes
CETIRUN_MEMORY_NO_MEMORY=true hermes gateway
CETIRUN_MEMORY_NO_CAPTURE=true openclaw gateway
```

`NO_CAPTURE` allows reads but prohibits capture and learning writes. `NO_MEMORY`
prohibits both reads and writes. The helper also accepts `--no-capture` and
`--no-memory` before or after its command, including `native-event` and MCP:

```bash
cetirun-memory --no-capture native-event < event.json
cetirun-memory --no-memory mcp --stdio --owner-private
```

These are helper flags, not new host CLI flags. Both hosts use the environment
variables. Flags, environment restrictions and per-request restrictions combine
by taking the stricter result. `/memory on` cannot override a startup restriction.
Changing startup controls requires restarting or reloading the affected plugin.

## Restrict platforms, senders or conversations

Configure `owner_access` in the host's Memory plugin settings, or supply the
same object as JSON in `CETIRUN_MEMORY_OWNER_ACCESS`. This Hermes profile allows
local CLI/TUI sessions and one Discord sender in one direct-message conversation:

```yaml
plugins:
  entries:
    cetirun-memory-context:
      settings:
        owner_access:
          platforms: [cli, tui, discord]
          senders:
            discord: ["example-sender-id"]
          chats:
            discord: ["example-chat-id"]
          chat_types:
            discord: [dm]
          subagents: false
```

OpenClaw uses `plugins.entries.cetirun-memory-context.config.owner_access`:

```json
{
  "plugins": {
    "entries": {
      "cetirun-memory-context": {
        "config": {
          "owner_access": {
            "platforms": ["webchat", "cli", "discord"],
            "senders": {"discord": ["example-sender-id"]},
            "chats": {"discord": ["example-chat-id"]},
            "chat_types": {"discord": ["direct"]},
            "subagents": false
          }
        }
      }
    }
  }
}
```

OpenClaw uses `webchat` for Control UI/TUI turns, `cli` for local CLI turns,
and the native channel ID for message channels. Scheduled jobs use `cron`;
heartbeats use `heartbeat` and internal Memory-flush turns use `memory`.
ACP sessions that run the adapter use `acp`. Missing source metadata cannot
satisfy an explicit restriction.

An environment-only restriction can select the permitted entrypoints:

```bash
CETIRUN_MEMORY_OWNER_ACCESS='{"platforms":["cli","tui","api_server"]}' hermes
```

- An omitted field imposes no restriction. An empty allowlist permits nothing
  for that field. `platforms` applies to top-level sessions, including local
  entrypoints; descendants inherit their parent's source permission.
- `senders`, `chats` and `chat_types` apply only to the platforms named in each
  map. Use `platforms` as well to exclude other platforms. Sender IDs are user
  IDs, not names or role IDs; chat IDs identify the conversation. Values match
  exactly, without wildcards.
- Profile and environment policies both apply. The environment cannot reopen
  a platform, identity or descendant that the profile excludes.
- `subagents`, `cron` and `acp` default to `true`; setting one to `false` disables
  Memory for that execution path. A platform allowlist can also exclude cron/ACP.
- Chat types use each host's native names: Hermes uses `dm`, `group` and
  `channel`; OpenClaw uses `direct`, `group`, `channel` and `room`.
- Hermes reads session-local chat metadata matched to the current session and
  platform. OpenClaw reads the native hook context. Missing metadata fails an
  explicit chat restriction.
  Tool arguments, message text and another session's context do not grant access.

## Descendant lifetime

Hermes requires a native `subagent_start` event for the exact parent turn and
child session. OpenClaw reads the host's current child and parent session
identities and the child's immutable creation time. A new child must have been
created through native spawn after the parent's current Memory grant. It does
not wait for `subagent_spawned`, which the host can emit after the child starts.
A parent ID in text or a tool argument cannot grant access.

Children inherit the parent's memory project scope (also from worktrees) and
may read/write within it. Restrictive modes propagate transitively, and a child
cannot undo them with `/memory on`. New children after the owner opts back in
can get a fresh grant. A normal parent turn completion does not revoke running
background children; interruption and sender/conversation/scope changes revoke
access. Resuming the parent creates a fresh grant rather than reviving the old
one, including for children that were idle or had not started at interruption.
Children cannot attest an `owner-preference` event as a direct owner statement.

The grant is process-local, bounded by the adapter's session cache, and not a
same-UID sandbox. After a gateway restart or plugin reload, existing orphaned
children fail closed; create new descendants from a fresh owner turn. Separate
profiles do not share these grants or receive automatic configuration changes.
Toolsets still follow the host's own inheritance and disabled-tool rules.
OpenClaw retains its narrower native owner-origin check for automatic
preference capture. Sending recalled material
through messaging tools still requires confirmation; native child creation
inherits access when `subagents` is enabled.

Rebuild the helper and install a new immutable asset directory together.
OpenClaw's `ownerAuthorizedChild` native binding is rejected by older helpers
that accept it only for Hermes. Update all asset/binary/context/skill paths
and reload the plugin. Never set that field in model tool arguments or bypass
a refusal with shell access.

## Verification

```bash
make -C app/cetirun-memory-context test
make -C app/cetirun-memory-context lint
```

The normal suite covers both adapters' default access, platform/sender/chat
restrictions, configuration/environment intersections, mode restrictions,
child/grandchild grants, interruption/resume, scope and malformed inputs with a
real helper and synthetic host APIs. CLI tests cover flags and environment
controls for native events, direct requests, legacy context and MCP.

With a separately provisioned OpenClaw 2026.9.6 selected by
`MEMORY_OPENCLAW_SOURCE`, the suite also loads the plugin in the real host and
checks interactive, channel and scheduled prompt injection. Without it, those
host checks are explicitly skipped. Child-lineage and restriction propagation
tests use synthetic host session metadata, not live spawned conversations.

When `MEMORY_HERMES_SOURCE` and `MEMORY_HERMES_PYTHON` select the separately
provisioned pinned host, the same native test entrypoint also runs
`tests/hermes_owner_host.py` against real hooks, session metadata, middleware
and the registry in a disposable marked profile. It makes no model requests.
Without those dependencies this check is explicitly skipped. Neither test mode
substitutes for real incoming messages or a model-driven child conversation.
