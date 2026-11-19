# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Run the actual init scripts against synthetic HOME/PVC directories only."""

import importlib.util
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import threading
import time
import unittest

import pytest
import yaml


CHART = Path(__file__).resolve().parents[1]
IMAGE = "registry.example.com/agent@sha256:" + "a" * 64


@pytest.fixture(params=["openclaw", "hermes"])
def agent(request, tmp_path):
    return make_agent(request.param, tmp_path)


def make_agent(kind, tmp_path):
    source, target, config, secrets, binaries = [tmp_path / name for name in ("source", "pvc", "config", "secrets", "bin")]
    for directory in (source, target, config, secrets, binaries):
        directory.mkdir()
    (source / ".zshrc").write_text("image shell")
    (source / "dependency").write_text("image dependency")
    (source / "dependency").chmod(0o755)
    shutil.copy(CHART / "configs/init-home-common.sh", config)
    shutil.copytree(
        CHART / "skills/connected-services",
        source / f".{kind}/skills/connected-services",
        dirs_exist_ok=True,
    )
    command_log = tmp_path / "commands"
    for command in ("git", "curl", "az", "hermes"):
        stub = binaries / command
        stub.write_text(f'#!/bin/sh\nprintf "%s\\n" "{command}" >> "$COMMAND_LOG"\nexit 1\n')
        stub.chmod(0o755)
    if kind == "openclaw":
        (source / ".openclaw/workspace").mkdir(parents=True)
        (source / ".openclaw/workspace/USER.md").write_text("image user")
        (source / ".openclaw/npm").symlink_to("/usr/local/share/openclaw-npm")
        (config / "openclaw.json").write_text(json.dumps({
            "agents": {"defaults": {"model": {"primary": "test-model"}}},
        }))
        (config / "user-md").write_text("Example User")
        script = CHART / "agents/openclaw/bootstrap/init-home.sh"
        config_file = target / ".openclaw/openclaw.json"
    else:
        (config / "hermes-config.yaml").write_text(yaml.safe_dump(GATEWAY_CONFIG))
        (config / "hermes-soul.md").write_text("Example persona")
        (config / "hermes-user.md").write_text("Example User\n")
        (config / "hermes-team.md").write_text("Example Team")
        (secrets / "hermes.env").write_text("API_SERVER_KEY=sk-dummy\nOPENAI_API_KEY=sk-dummy-model\n")
        shutil.copy(CHART / "agents/hermes/bootstrap/hermes-apply-config.py", config)
        script = CHART / "agents/hermes/bootstrap/hermes-init-home.sh"
        config_file = target / ".hermes/config.yaml"
    return {
        "kind": kind, "source": source, "target": target, "config": config,
        "secrets": secrets, "script": script, "config_file": config_file,
        "command_log": command_log, "binaries": binaries,
        "env": dict(
            os.environ,
            SOURCE_HOME=str(source), TARGET_HOME=str(target),
            CETIRUN_CONFIG_DIR=str(config), CETIRUN_SECRET_DIR=str(secrets),
            HERMES_PYTHON=sys.executable, CETIRUN_INIT_IMAGE=IMAGE,
            CETIRUN_AI_GATEWAY="true", CETIRUN_GATEWAY_KEY_ENV="CETIRUN_GATEWAY_API_KEY",
            CETIRUN_BUILTIN_SKILL_SYNC=str(CHART / "configs/builtin-skill-sync.sh"),
            AZDEVOPS_GIT_CREDENTIAL_ENABLED="false",
            COMMAND_LOG=str(command_log), PATH=str(binaries) + os.pathsep + os.environ["PATH"],
        ),
    }


def run(agent):
    return subprocess.run(
        ["sh", str(agent["script"])], env=agent["env"],
        capture_output=True, text=True, timeout=15,
    )


def read_config(agent):
    return yaml.safe_load(agent["config_file"].read_text())


def native_marker(agent):
    return agent["target"] / f'.{agent["kind"]}/.initialized'


@pytest.mark.parametrize("agent", ["hermes"], indirect=True)
def test_hermes_context_preserves_personal_home_files(agent, tmp_path):
    view = tmp_path / "context-view"
    view.mkdir()
    agent["env"]["CETIRUN_CONTEXT_VIEW"] = str(view)
    personal = agent["source"] / ".agents/skills/personal"
    personal.mkdir(parents=True)
    (personal / "SKILL.md").write_text("personal skill")
    (agent["source"] / ".agents/AGENTS.md").write_text("personal instructions")

    result = run(agent)
    assert result.returncode == 0, result.stderr
    home_skill = agent["target"] / ".agents/skills/personal/SKILL.md"
    instructions = agent["target"] / ".agents/AGENTS.md"
    assert home_skill.read_text() == "personal skill"
    assert instructions.read_text() == "personal instructions"
    home_skill.write_text("updated personal skill")
    instructions.write_text("updated personal instructions")
    result = run(agent)
    assert result.returncode == 0, result.stderr
    assert home_skill.read_text() == "updated personal skill"
    assert instructions.read_text() == "updated personal instructions"
    assert not (view / "current/skills/personal").exists()


@pytest.mark.parametrize("unsafe", ["instructions-link", "instructions-directory", "skills-link", "skills-file"])
@pytest.mark.parametrize("agent", ["hermes"], indirect=True)
def test_hermes_context_rejects_unsafe_home_mount_targets(agent, tmp_path, unsafe):
    assert run(agent).returncode == 0
    view = tmp_path / "context-view"
    view.mkdir()
    agent["env"]["CETIRUN_CONTEXT_VIEW"] = str(view)
    personal = agent["target"] / ".agents"
    personal.mkdir()
    outside = tmp_path / "outside"
    outside.mkdir()
    sentinel = outside / "sentinel"
    sentinel.write_text("retained")
    target = personal / ("AGENTS.md" if unsafe.startswith("instructions") else "skills")
    if unsafe.endswith("link"):
        target.symlink_to(sentinel if unsafe.startswith("instructions") else outside)
    elif unsafe.endswith("directory"):
        target.mkdir()
    else:
        target.write_text("retained")

    result = run(agent)
    assert result.returncode != 0
    assert sentinel.read_text() == "retained"
    assert not (view / "current").exists()


def test_native_first_start_and_repeat_preserve_user_files(agent):
    first = run(agent)
    assert first.returncode == 0, first.stderr
    assert native_marker(agent).is_file()
    assert (agent["target"] / ".cetirun-home-seeded").is_file()
    helper = agent["target"] / ".local/bin/copilot-auth"
    assert not helper.exists()
    skill_source = CHART / "skills/connected-services"
    skill_target = agent["target"] / f'.{agent["kind"]}/skills/connected-services'
    for path in skill_source.rglob("*"):
        if path.is_file():
            installed = skill_target / path.relative_to(skill_source)
            assert installed.read_bytes() == path.read_bytes()
    if agent["kind"] == "openclaw":
        user = agent["target"] / ".openclaw/workspace/USER.md"
        assert (agent["target"] / ".openclaw/npm").is_symlink()
        assert not (agent["target"] / ".acpx/config.json").exists()
    else:
        user = agent["target"] / ".hermes/memories/USER.md"
        assert "TEAM.md" in user.read_text()
    user.write_text("user edits")
    (agent["target"] / ".zshrc").unlink()
    (agent["source"] / "dependency").write_text("new dependency")
    current = read_config(agent)
    current["user_preference"] = "preserved"
    agent["config_file"].write_text(json.dumps(current))
    second = run(agent)
    assert second.returncode == 0, second.stderr
    assert user.read_text() == "user edits"
    assert not (agent["target"] / ".zshrc").exists()
    assert (agent["target"] / "dependency").read_text() == "image dependency"
    assert read_config(agent)["user_preference"] == "preserved"
    stages = ["seed_home_once", "agent_prepare_home", "agent_apply_config", "agent_prepare_integrations", "agent_verify", "complete"]
    offsets = [second.stderr.index("[agent-init] " + stage) for stage in stages]
    assert offsets == sorted(offsets)


def test_corrupt_config_is_preserved_and_not_logged(agent):
    assert run(agent).returncode == 0
    corrupt = 'not-json-or-yaml: [secret-test-sentinel'
    agent["config_file"].write_text(corrupt)
    result = run(agent)
    assert result.returncode != 0
    assert agent["config_file"].read_text() == corrupt
    assert "secret-test-sentinel" not in result.stderr + result.stdout
    assert "[agent-init] complete" not in result.stderr


@pytest.mark.parametrize("existing_home", [False, True])
def test_retired_teams_configuration_does_not_survive_initialization(agent, existing_home):
    if existing_home:
        assert run(agent).returncode == 0
        config_file = agent["config_file"]
    else:
        config_file = agent["source"] / f'.{agent["kind"]}' / agent["config_file"].name
        config_file.parent.mkdir(parents=True, exist_ok=True)
    if agent["kind"] == "openclaw":
        legacy = {
            "channels": {"msteams": {"enabled": True}, "telegram": {"enabled": False}},
            "plugins": {
                "allow": ["msteams", "home-plugin"],
                "entries": {"msteams": {"enabled": True}, "home-plugin": {"enabled": True}},
                "installs": {"msteams": {"source": "npm"}, "home-plugin": {"source": "path"}},
            },
            "user_preference": {"msteams": "unrelated user data"},
        }
        config_file.write_text(json.dumps(legacy))
    else:
        platforms = {"cetirun_teams": {"enabled": True}, "telegram": {"enabled": False}}
        # A YAML alias must not let retirement mutate an unrelated user field.
        legacy = {"platforms": platforms, "user_preference": platforms}
        config_file.write_text(yaml.safe_dump(legacy))
    for _ in range(2):
        result = run(agent)
        assert result.returncode == 0, result.stderr
        current = read_config(agent)
        assert current["user_preference"] == legacy["user_preference"]
        if agent["kind"] == "openclaw":
            assert current["channels"] == {"telegram": {"enabled": False}}
            assert current["plugins"]["entries"] == {"home-plugin": {"enabled": True}}
            assert current["plugins"]["allow"] == ["home-plugin"]
            assert current["plugins"]["installs"] == {"home-plugin": {"source": "path"}}
        else:
            assert current["platforms"] == {"telegram": {"enabled": False}}


def test_openclaw_disabling_memory_removes_only_platform_plugin_config(agent):
    if agent["kind"] != "openclaw":
        return
    source = agent["config"] / "openclaw.json"
    config = json.loads(source.read_text())
    config["plugins"] = {
        "allow": ["cetirun-memory-context", "fixture-plugin"],
        "load": {"paths": ["/opt/cetirun-memory/adapters/openclaw", "/fixture/plugin"]},
        "entries": {"cetirun-memory-context": {"enabled": True}, "fixture-plugin": {"enabled": True}},
    }
    config["skills"] = {"load": {"extraDirs": ["/home/vibe/.agents/skills", "/fixture/skills"]}}
    source.write_text(json.dumps(config))
    assert run(agent).returncode == 0
    del config["plugins"]
    del config["skills"]
    source.write_text(json.dumps(config))
    disabled = run(agent)
    assert disabled.returncode == 0, disabled.stderr
    saved = read_config(agent)
    assert saved["plugins"]["allow"] == ["fixture-plugin"]
    assert saved["plugins"]["load"]["paths"] == ["/fixture/plugin"]
    assert saved["plugins"]["entries"] == {"fixture-plugin": {"enabled": True}}
    assert saved["skills"]["load"]["extraDirs"] == ["/fixture/skills"]
    assert run(agent).returncode == 0
    assert read_config(agent) == saved


@pytest.mark.parametrize("agent", ["openclaw", "hermes"], indirect=True)
@pytest.mark.skipif(shutil.which("helm") is None, reason="requires Helm")
def test_reconciled_rendered_memory_survives_home_initialization(agent):
    helpers = CHART.parents[1] / "scripts/tests/test_native_gateway_chart.py"
    spec = importlib.util.spec_from_file_location("native_gateway_chart_fixture", helpers)
    fixture = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(fixture)
    values = fixture.native_values()
    values["agentType"] = agent["kind"]
    values["sandboxWorkload"].update(reconciled=True, executionHeld=False)
    values["memoryContext"] = {"enabled": True, "image": "registry.example.com/memory:1.2.3"}
    agent["env"].update(CETIRUN_AI_GATEWAY="true", CETIRUN_NATIVE_SANDBOX="true")
    config_name = "openclaw.json" if agent["kind"] == "openclaw" else "hermes-config.yaml"
    source = agent["config"] / config_name
    rendered = fixture.object_of_kind(fixture.render(CHART, values), "ConfigMap")["data"][config_name]
    source.write_text(rendered)
    for _ in range(2):
        result = run(agent)
        assert result.returncode == 0, result.stderr
        current = read_config(agent)
        if agent["kind"] == "openclaw":
            plugin = current.get("plugins", {}).get("entries", {}).get("cetirun-memory-context", {})
            assert plugin.get("enabled") is True
            assert plugin.get("hooks", {}).get("allowConversationAccess") is True
            assert current["plugins"]["allow"].count("cetirun-memory-context") == 1
            assert current["plugins"]["load"]["paths"].count("/opt/cetirun-memory/adapters/openclaw") == 1
            assert current["skills"]["load"]["extraDirs"].count("/home/vibe/.agents/skills") == 1
        else:
            assert current.get("plugins", {}).get("enabled", []).count("cetirun-memory-context") == 1
            assert current.get("skills", {}).get("external_dirs", []).count("/home/vibe/.agents/skills") == 1
        current["user_preference"] = "synthetic-retained-preference"
        agent["config_file"].write_text(json.dumps(current))

    values["memoryContext"]["enabled"] = False
    source.write_text(fixture.object_of_kind(fixture.render(CHART, values), "ConfigMap")["data"][config_name])
    result = run(agent)
    assert result.returncode == 0, result.stderr
    disabled = read_config(agent)
    if agent["kind"] == "openclaw":
        assert "cetirun-memory-context" not in disabled["plugins"]["entries"]
        assert "/opt/cetirun-memory/adapters/openclaw" not in disabled["plugins"]["load"]["paths"]
        assert "/home/vibe/.agents/skills" not in disabled["skills"]["load"]["extraDirs"]
    else:
        assert "cetirun-memory-context" not in disabled["plugins"]["enabled"]
        assert "/home/vibe/.agents/skills" not in disabled["skills"]["external_dirs"]
    assert disabled["user_preference"] == "synthetic-retained-preference"


def test_memory_upgrade_preserves_home_plugin_and_skill_lists(agent):
    assert run(agent).returncode == 0
    saved = read_config(agent)
    source = agent["config"] / ("openclaw.json" if agent["kind"] == "openclaw" else "hermes-config.yaml")
    desired = yaml.safe_load(source.read_text())
    if agent["kind"] == "openclaw":
        saved["plugins"] = {"allow": ["home-plugin"], "load": {"paths": ["/fixture/home-plugin"]},
                            "entries": {"home-plugin": {"enabled": True}}}
        saved["skills"] = {"load": {"extraDirs": ["/fixture/home-skills"]}}
        desired["plugins"] = {"allow": ["home-plugin", "cetirun-memory-context", "chart-plugin"],
                              "load": {"paths": ["/opt/cetirun-memory/adapters/openclaw", "/fixture/chart-plugin"]},
                              "entries": {"cetirun-memory-context": {"enabled": True}, "chart-plugin": {"enabled": True}}}
        desired["skills"] = {"load": {"extraDirs": ["/home/vibe/.agents/skills", "/fixture/chart-skills"]}}
    else:
        saved["plugins"] = {"enabled": ["home-plugin"]}
        saved["skills"] = {"external_dirs": ["/fixture/home-skills"]}
        desired["plugins"] = {"enabled": ["cetirun-memory-context", "chart-plugin"]}
        desired["skills"] = {"external_dirs": ["/home/vibe/.agents/skills", "/fixture/chart-skills"]}
    agent["config_file"].write_text(json.dumps(saved))
    source.write_text(json.dumps(desired))
    for _ in range(2):
        result = run(agent)
        assert result.returncode == 0, result.stderr
        enabled = read_config(agent)
        if agent["kind"] == "openclaw":
            assert enabled["plugins"]["allow"] == ["home-plugin", "cetirun-memory-context", "chart-plugin"]
            assert enabled["plugins"]["load"]["paths"] == ["/fixture/home-plugin", "/opt/cetirun-memory/adapters/openclaw", "/fixture/chart-plugin"]
            assert enabled["skills"]["load"]["extraDirs"] == ["/fixture/home-skills", "/home/vibe/.agents/skills", "/fixture/chart-skills"]
        else:
            assert enabled["plugins"]["enabled"] == ["home-plugin", "cetirun-memory-context", "chart-plugin"]
            assert enabled["skills"]["external_dirs"] == ["/fixture/home-skills", "/home/vibe/.agents/skills", "/fixture/chart-skills"]
    if agent["kind"] == "openclaw":
        desired["plugins"] = {"allow": ["home-plugin", "chart-plugin"], "load": {"paths": ["/fixture/chart-plugin"]}}
        desired["skills"] = {"load": {"extraDirs": ["/fixture/chart-skills"]}}
    else:
        desired["plugins"] = {"enabled": ["chart-plugin"]}
        desired["skills"] = {"external_dirs": ["/fixture/chart-skills"]}
    source.write_text(json.dumps(desired))
    for _ in range(2):
        result = run(agent)
        assert result.returncode == 0, result.stderr
        disabled = read_config(agent)
        if agent["kind"] == "openclaw":
            assert disabled["plugins"]["allow"] == ["home-plugin", "chart-plugin"]
            assert disabled["plugins"]["load"]["paths"] == ["/fixture/home-plugin", "/fixture/chart-plugin"]
            assert disabled["skills"]["load"]["extraDirs"] == ["/fixture/home-skills", "/fixture/chart-skills"]
            assert "cetirun-memory-context" not in disabled["plugins"]["entries"]
        else:
            assert disabled["plugins"]["enabled"] == ["home-plugin", "chart-plugin"]
            assert disabled["skills"]["external_dirs"] == ["/fixture/home-skills", "/fixture/chart-skills"]


def test_hermes_null_generated_sections_preserve_home_lists(agent):
    if agent["kind"] != "hermes":
        return
    assert run(agent).returncode == 0
    saved = read_config(agent)
    saved["plugins"] = {"enabled": ["home-plugin", "cetirun-memory-context"]}
    saved["skills"] = {"external_dirs": ["/fixture/home-skills", "/home/vibe/.agents/skills"]}
    agent["config_file"].write_text(json.dumps(saved))
    source = agent["config"] / "hermes-config.yaml"
    desired = yaml.safe_load(source.read_text())
    desired.update(plugins=None, skills=None)
    source.write_text(json.dumps(desired))
    result = run(agent)
    assert result.returncode == 0, result.stderr
    actual = read_config(agent)
    assert actual["plugins"]["enabled"] == ["home-plugin"]
    assert actual["skills"]["external_dirs"] == ["/fixture/home-skills"]


@pytest.mark.parametrize("admin,explicit_alias", [(True, False), (True, True), (False, False)])
@pytest.mark.parametrize("source_state", ["missing", "directory", "dangling-link", "parent-link"])
def test_hermes_cetiadmin_retires_only_unconfigured_default_skill_alias(agent, admin, explicit_alias, source_state):
    if agent["kind"] != "hermes":
        return
    assert run(agent).returncode == 0
    legacy = "/home/vibe/.aks/skills"
    builtin = "/opt/cetirun/cetiadmin/skills"
    source = agent["config"] / "hermes-config.yaml"
    desired = yaml.safe_load(source.read_text())
    paths = ([builtin] if admin else []) + ([legacy] if explicit_alias else [])
    desired["skills"] = {"external_dirs": paths}
    source.write_text(json.dumps(desired))
    saved = read_config(agent)
    saved["skills"] = {"external_dirs": [legacy, "/fixture/home-skills"]}
    agent["config_file"].write_text(json.dumps(saved))
    personal = agent["target"] / ".aks/skills/personal/SKILL.md"
    if source_state == "directory":
        personal.parent.mkdir(parents=True)
        personal.write_text("User-owned skill contents.\n")
    elif source_state == "dangling-link":
        (agent["target"] / ".aks").mkdir()
        (agent["target"] / ".aks/skills").symlink_to(agent["target"] / "unavailable")
    elif source_state == "parent-link":
        (agent["target"] / ".aks").symlink_to(agent["target"] / "unavailable")
    for _ in range(2):
        result = run(agent)
        assert result.returncode == 0, result.stderr
        current = read_config(agent)["skills"]["external_dirs"]
        assert (legacy in current) == (not admin or explicit_alias or source_state != "missing")
        assert "/fixture/home-skills" in current
        assert (builtin in current) == admin
        if source_state == "directory":
            assert personal.read_text() == "User-owned skill contents.\n"


def test_native_failure_after_seed_resumes_without_recopy(agent):
    source = agent["config"] / ("openclaw.json" if agent["kind"] == "openclaw" else "hermes-config.yaml")
    saved = source.read_text()
    source.write_text("invalid: [")
    result = run(agent)
    assert result.returncode != 0
    assert (agent["target"] / ".cetirun-home-seeded").is_file()
    assert not native_marker(agent).exists()
    (agent["source"] / "new-dependency").write_text("must not recopy")
    source.write_text(saved)
    result = run(agent)
    assert result.returncode == 0, result.stderr
    assert native_marker(agent).is_file()
    assert not (agent["target"] / "new-dependency").exists()


def test_native_config_link_is_rejected(agent, tmp_path):
    assert run(agent).returncode == 0
    outside = tmp_path / "outside"
    outside.write_text("keep")
    agent["config_file"].unlink()
    agent["config_file"].symlink_to(outside)
    result = run(agent)
    assert result.returncode != 0
    assert outside.read_text() == "keep"


def test_existing_home_is_not_scanned_for_legacy_credentials(agent):
    directory = agent["target"] / f'.{agent["kind"]}'
    directory.mkdir()
    native_marker(agent).touch()
    token = agent["target"] / ".litellm/github_copilot/access-token"
    token.parent.mkdir(parents=True)
    token.write_text("legacy-test-sentinel")
    other = agent["target"] / ".openclaw/agents/main/agent/auth-profiles.json"
    other.parent.mkdir(parents=True, exist_ok=True)
    other.write_text('{"profiles":{"github-copilot:github":{"token":"legacy-test-sentinel"}}}')
    result = run(agent)
    assert result.returncode == 0, result.stderr
    assert token.read_text() == "legacy-test-sentinel"
    assert not agent["command_log"].exists()
    if agent["kind"] == "hermes":
        assert "legacy-test-sentinel" not in (directory / ".env").read_text()


@pytest.mark.parametrize("agent", ["hermes"], indirect=True)
def test_hermes_preserves_env_and_updates_team(agent):
    assert run(agent).returncode == 0
    env = agent["target"] / ".hermes/.env"
    env.write_text("API_SERVER_KEY=old-key\nOPENAI_API_KEY=retained-key\nCUSTOM_FIELD=keep\n")
    (agent["secrets"] / "hermes.env").write_text("API_SERVER_KEY=rotated-key\nOPENAI_API_KEY=\n")
    (agent["config"] / "hermes-team.md").write_text("")
    result = run(agent)
    assert result.returncode == 0, result.stderr
    assert "API_SERVER_KEY=rotated-key" in env.read_text()
    assert "OPENAI_API_KEY=retained-key" in env.read_text()
    assert "CUSTOM_FIELD=keep" in env.read_text()
    assert not (agent["target"] / ".hermes/memories/TEAM.md").exists()


@pytest.mark.parametrize("agent", ["openclaw"], indirect=True)
def test_openclaw_optional_git_failure_and_managed_skill(agent):
    assert run(agent).returncode == 0
    skill = agent["source"] / ".openclaw/skills/example"
    skill.mkdir(parents=True)
    (skill / "SKILL.md").write_text("Example task")
    agent["env"]["AZDEVOPS_GIT_CREDENTIAL_ENABLED"] = "true"
    result = run(agent)
    assert result.returncode == 0, result.stderr
    assert "WARNING" in result.stderr
    assert agent["command_log"].read_text().splitlines() == ["git", "git", "git"]
    assert (agent["target"] / ".openclaw/skills/example/SKILL.md").read_text() == "Example task"
    assert native_marker(agent).is_file()


@pytest.mark.parametrize("agent", ["openclaw"], indirect=True)
def test_duplicate_git_setting_does_not_skip_other_settings(agent):
    assert run(agent).returncode == 0
    (agent["binaries"] / "git").unlink()
    real_git = shutil.which("git")
    config = agent["target"] / ".gitconfig"
    prefix = "credential.https://dev.azure.com"
    for value in ("true", "false"):
        subprocess.run(
            [real_git, "config", "--file", str(config), "--add", prefix + ".useHttpPath", value],
            check=True, capture_output=True,
        )
    agent["env"]["AZDEVOPS_GIT_CREDENTIAL_ENABLED"] = "true"
    result = run(agent)
    assert result.returncode == 0, result.stderr
    assert "WARNING" in result.stderr
    for name, expected in (("provider", "generic"), ("helper", "/home/vibe/.git-credential-azdevops.sh")):
        value = subprocess.run(
            [real_git, "config", "--file", str(config), "--get", prefix + "." + name],
            check=True, capture_output=True, text=True,
        ).stdout.strip()
        assert value == expected


@pytest.mark.parametrize("agent", ["openclaw"], indirect=True)
def test_openclaw_replace_failure_preserves_original(agent):
    assert run(agent).returncode == 0
    original = agent["config_file"].read_bytes()
    stub = agent["binaries"] / "mv"
    real_mv = shutil.which("mv")
    stub.write_text(
        '#!/bin/sh\nfor arg in "$@"; do\n'
        'case "$arg" in */.openclaw/openclaw.json) exit 1;; esac\ndone\n'
        f'exec "{real_mv}" "$@"\n'
    )
    stub.chmod(0o755)
    result = run(agent)
    assert result.returncode != 0
    assert agent["config_file"].read_bytes() == original


@pytest.mark.parametrize("helper", ["hermes-apply-config", "hermes-runtime"])
def test_hermes_yaml_round_trip_preserves_owner_values(helper):
    spec = importlib.util.spec_from_file_location(helper, CHART / f"agents/hermes/bootstrap/{helper}.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    owner = {"mode": "off", "answer": "yes", "flag": "on", "quiet_from": "22:00"}
    original = yaml.safe_dump(owner, sort_keys=False)
    rewritten = module.yaml.safe_dump(module.yaml.safe_load(original), sort_keys=False)
    actual = yaml.safe_load(rewritten)
    assert actual == owner
    assert list(actual) == list(owner)


def test_hermes_atomic_replace_failure(tmp_path, monkeypatch):
    spec = importlib.util.spec_from_file_location("hermes_apply", CHART / "agents/hermes/bootstrap/hermes-apply-config.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    dest = tmp_path / "config.yaml"
    dest.write_text("original")

    def reject(*_):
        raise OSError("synthetic failure")

    monkeypatch.setattr(module.os, "replace", reject)
    with pytest.raises(OSError):
        module.replace_file(dest, "new")
    assert dest.read_text() == "original"
    assert sorted(path.name for path in tmp_path.iterdir()) == ["config.yaml"]


def openai_config(models, provider=None):
    # The configuration both an Agent on the AI Gateway and a deployment with
    # its own openai endpoint render: an openai/ default and that provider.
    return {
        "agents": {"defaults": {"model": {"primary": "openai/model-a"}, "models": models}},
        "models": {"providers": {"openai": provider or {"baseUrl": "https://models.example.com/v1", "models": []}}},
    }


# The Gateway provider as the chart renders it: bound to the Agent's virtual
# key in OPENAI_API_KEY.
GATEWAY_PROVIDER = {
    "baseUrl": "http://cetirun-ai-gateway.cetirun-system.svc.cluster.local:4000/v1",
    "models": [{"id": "model-a", "name": "model-a"}],
    "auth": "api-key",
    "apiKey": {"source": "env", "provider": "default", "id": "OPENAI_API_KEY"},
}


def saved_openclaw_config(agent, provider=None):
    # A HOME the Agent has used: its own model settings, a sign-in profile,
    # and the owner's own keys, for openai and for another provider.
    assert run(agent).returncode == 0
    saved = read_config(agent)
    saved["agents"]["defaults"]["models"] = {
        "openai/model-a": {"alias": "my-alias", "params": {"temperature": 0.2}},
        "openai/model-old": {},
    }
    saved["auth"] = {"profiles": {"github-copilot:github": {"provider": "github-copilot"}}}
    saved["models"] = {"providers": {
        "openai": {
            "baseUrl": "https://api.openai.com/v1",
            "apiKey": "owner-test-key",
            "request": {"auth": {"mode": "authorization-bearer", "token": "owner-test-key"}},
        },
        "anthropic": {"apiKey": "owner-anthropic-test-key"},
    }}
    agent["config_file"].write_text(json.dumps(saved))
    (agent["config"] / "openclaw.json").write_text(
        json.dumps(openai_config({"openai/model-a": {}, "openai/model-b": {}}, provider)))


@pytest.mark.parametrize("agent", ["openclaw"], indirect=True)
def test_openclaw_on_the_gateway_takes_the_gateway_model_list(agent):
    agent["env"]["CETIRUN_AI_GATEWAY"] = "true"
    saved_openclaw_config(agent, GATEWAY_PROVIDER)
    saved = read_config(agent)
    saved["agents"]["list"] = [{"id": "research", "name": "Research", "model": "anthropic/old-model"}]
    agent["config_file"].write_text(json.dumps(saved))
    result = run(agent)
    assert result.returncode == 0, result.stderr
    config = read_config(agent)
    assert config["agents"]["defaults"]["models"] == {"openai/model-a": {}, "openai/model-b": {}}
    assert "github-copilot:github" not in config["auth"]["profiles"]
    # The Gateway provider is CetiRun's, bound to the Agent's key: a key or a
    # request override saved on it would be sent to the Gateway instead.
    assert config["models"]["providers"]["openai"] == GATEWAY_PROVIDER
    # Saved direct providers cannot survive the platform Gateway configuration.
    assert set(config["models"]["providers"]) == {"openai"}
    # Existing named Agents retain their identity and inherit the Gateway model.
    assert config["agents"]["list"] == [{"id": "research", "name": "Research"}]


def native_openclaw_source(agent):
    saved_openclaw_config(agent, GATEWAY_PROVIDER)
    source_file = agent["config"] / "openclaw.json"
    source = json.loads(source_file.read_text())
    source["auth"] = {"profiles": {}}
    source["gateway"] = {"auth": {"mode": "token"}, "controlUi": {"basePath": "/ui/example-agent"}}
    source["agents"]["defaults"].update({
        "model": {"primary": "openai/model-a"},
        "modelPolicy": {"allow": ["openai/model-a", "openai/model-b"]},
        "utilityModel": "openai/model-a",
    })
    source_file.write_text(json.dumps(source))
    agent["env"].update(CETIRUN_AI_GATEWAY="true", CETIRUN_NATIVE_SANDBOX="true")
    return source


@pytest.mark.parametrize("agent", ["openclaw"], indirect=True)
def test_native_openclaw_replaces_saved_model_and_gateway_overrides(agent):
    source = native_openclaw_source(agent)
    saved = read_config(agent)
    saved["user_preference"] = "preserved"
    saved["gateway"] = {"auth": {"mode": "none"}, "controlUi": {"allowInsecureAuth": True}}
    saved["agents"]["list"] = [{"id": "example-extra", "model": "anthropic/ungranted"}]
    saved["agents"]["defaults"]["model"]["fallbacks"] = ["anthropic/ungranted"]
    agent["config_file"].write_text(json.dumps(saved))
    for _ in range(2):
        result = run(agent)
        assert result.returncode == 0, result.stderr
        config = read_config(agent)
        assert config["models"] == source["models"]
        assert config["auth"] == source["auth"]
        assert config["gateway"] == source["gateway"]
        assert config["agents"]["defaults"]["model"] == source["agents"]["defaults"]["model"]
        assert config["agents"]["defaults"]["modelPolicy"] == source["agents"]["defaults"]["modelPolicy"]
        assert config["agents"]["defaults"]["utilityModel"] == "openai/model-a"
        assert "list" not in config["agents"]
        assert config["user_preference"] == "preserved"


@pytest.mark.parametrize("agent", ["openclaw"], indirect=True)
def test_native_openclaw_requires_a_gateway_binding(agent):
    agent["env"]["CETIRUN_NATIVE_SANDBOX"] = "true"
    agent["env"].pop("CETIRUN_AI_GATEWAY")
    result = run(agent)
    assert result.returncode != 0
    assert "AIGatewayRequired" in result.stderr
    assert not (agent["target"] / ".cetirun-home-seeded").exists()


@pytest.mark.parametrize("agent", ["openclaw"], indirect=True)
@pytest.mark.parametrize("invalid", ["key", "named-agent"])
def test_native_openclaw_refuses_unbound_configuration(agent, invalid):
    native_openclaw_source(agent)
    source_file = agent["config"] / "openclaw.json"
    source = json.loads(source_file.read_text())
    if invalid == "key":
        source["models"]["providers"]["openai"]["apiKey"] = "sk-dummy-unbound"
    else:
        source["agents"]["list"] = [{"id": "example-extra", "model": "anthropic/ungranted"}]
    source_file.write_text(json.dumps(source))
    saved = agent["config_file"].read_text()
    result = run(agent)
    assert result.returncode != 0
    assert "OpenClawNativeModelConfigInvalid" in result.stderr
    assert "sk-dummy-unbound" not in result.stdout + result.stderr
    assert agent["config_file"].read_text() == saved


@pytest.mark.parametrize("agent", ["openclaw"], indirect=True)
@pytest.mark.parametrize("marker", [None, "false"])
def test_openclaw_refuses_direct_model_mode(agent, marker):
    assert run(agent).returncode == 0
    before = agent["config_file"].read_bytes()
    if marker is None:
        agent["env"].pop("CETIRUN_AI_GATEWAY", None)
    else:
        agent["env"]["CETIRUN_AI_GATEWAY"] = marker
    result = run(agent)
    assert result.returncode != 0
    assert "AIGatewayRequired" in result.stderr
    assert agent["config_file"].read_bytes() == before


GATEWAY_CONFIG = {
    "model": {"default": "gpt-6-sol", "provider": "cetirun-gateway"},
    "providers": {"cetirun-gateway": {
        "name": "CetiRun AI Gateway", "api": "http://gateway.example.svc.cluster.local:4000/v1",
        "key_env": "CETIRUN_GATEWAY_API_KEY", "transport": "chat_completions", "default_model": "gpt-6-sol",
        "discover_models": False, "models": {"gpt-6-sol": {"context_length": 272000}},
    }},
}


def on_gateway(agent):
    """Render the agent's next start as the chart does on the AI Gateway."""
    (agent["config"] / "hermes-config.yaml").write_text(yaml.safe_dump(GATEWAY_CONFIG))
    (agent["config"] / "hermes-auth.sh").unlink(missing_ok=True)
    agent["env"].update(CETIRUN_AI_GATEWAY="true", CETIRUN_GATEWAY_KEY_ENV="CETIRUN_GATEWAY_API_KEY")


def env_names(agent):
    return [line.split("=", 1)[0] for line in (agent["target"] / ".hermes/.env").read_text().splitlines() if line]


@pytest.mark.parametrize("agent", ["hermes"], indirect=True)
def test_hermes_replaces_platform_model_configuration(agent):
    assert run(agent).returncode == 0
    (agent["target"] / ".hermes/.env").write_text(
        "API_SERVER_KEY=old\nGITHUB_TOKEN=ghp_example_own\nCETIRUN_GATEWAY_API_KEY=stale\n")
    config = read_config(agent)
    config["providers"] = {
        "cetirun-gateway": {"api": "http://old.example/v1", "models": {"gone-model": {}}},
        "mine": {"api": "https://models.example.com/v1", "key_env": "MY_KEY"},
    }
    config["model"]["base_url"] = "https://models.example.com/v1"
    config["user_preference"] = "preserved"
    agent["config_file"].write_text(yaml.safe_dump(config))
    on_gateway(agent)

    result = run(agent)
    assert result.returncode == 0, result.stderr
    config = read_config(agent)
    # The Gateway owns the model choice and its provider: replaced, not merged.
    assert config["model"] == GATEWAY_CONFIG["model"]
    assert config["providers"]["cetirun-gateway"] == GATEWAY_CONFIG["providers"]["cetirun-gateway"]
    assert set(config["providers"]) == {"cetirun-gateway"}
    assert config["user_preference"] == "preserved"
    # The Gateway key cannot be overridden from .env.
    names = env_names(agent)
    assert "CETIRUN_GATEWAY_API_KEY" not in names
    assert {"GITHUB_TOKEN", "API_SERVER_KEY"} <= set(names)
    assert native_marker(agent).is_file()

    # Model configuration remains platform-owned on subsequent starts.
    env = agent["target"] / ".hermes/.env"
    env.write_text(env.read_text() + "CETIRUN_GATEWAY_API_KEY=again\n")
    result = run(agent)
    assert result.returncode == 0, result.stderr
    names = env_names(agent)
    assert "CETIRUN_GATEWAY_API_KEY" not in names


@pytest.mark.parametrize("agent", ["hermes"], indirect=True)
def test_hermes_refuses_gateway_disable_without_modifying_home(agent):
    on_gateway(agent)
    assert run(agent).returncode == 0
    before = agent["config_file"].read_bytes()
    agent["env"].pop("CETIRUN_AI_GATEWAY")
    result = run(agent)
    assert result.returncode != 0
    assert "AI Gateway is required" in result.stderr
    assert agent["config_file"].read_bytes() == before


@pytest.mark.parametrize("agent", ["hermes"], indirect=True)
def test_hermes_on_the_gateway_with_another_key_variable(agent):
    # Standalone Helm can bind the key to another variable, such as
    # OPENAI_API_KEY, which the chart also writes into the generated
    # hermes.env: empty without secrets.apiKey. Hermes loads .env over its
    # environment, where the key is, so no assignment of the variable may
    # stay in .env, generated or saved.
    on_gateway(agent)
    agent["env"]["CETIRUN_GATEWAY_KEY_ENV"] = "OPENAI_API_KEY"
    config = json.loads(json.dumps(GATEWAY_CONFIG))
    config["providers"]["cetirun-gateway"]["key_env"] = "OPENAI_API_KEY"
    (agent["config"] / "hermes-config.yaml").write_text(yaml.safe_dump(config))
    (agent["secrets"] / "hermes.env").write_text("API_SERVER_KEY=sk-dummy\nOPENAI_API_KEY=\nBRAVE_API_KEY=\n")
    result = run(agent)
    assert result.returncode == 0, result.stderr
    names = env_names(agent)
    assert "OPENAI_API_KEY" not in names
    assert {"API_SERVER_KEY", "BRAVE_API_KEY"} <= set(names)

    env = agent["target"] / ".hermes/.env"
    env.write_text(env.read_text() + "OPENAI_API_KEY=sk-example-saved\n")
    (agent["secrets"] / "hermes.env").write_text("API_SERVER_KEY=sk-dummy\nOPENAI_API_KEY=sk-example-generated\n")
    result = run(agent)
    assert result.returncode == 0, result.stderr
    assert "OPENAI_API_KEY" not in env_names(agent)
    assert read_config(agent)["providers"]["cetirun-gateway"]["key_env"] == "OPENAI_API_KEY"

PORTAL_SIGN_IN = {
    "mode": "trusted-proxy",
    "trustedProxy": {"userHeader": "x-auth-request-email", "allowLoopback": True, "deviceAutoApprove": {"enabled": True}},
}


@pytest.mark.parametrize("agent", ["openclaw"], indirect=True)
@pytest.mark.parametrize("remote_token_source", ["source", "home"])
def test_native_openclaw_portal_sign_in_replaces_retained_auth_and_can_be_disabled(agent, remote_token_source):
    source = native_openclaw_source(agent)
    saved = read_config(agent)
    saved["gateway"] = {
        "auth": {"mode": "token", "token": "saved-test-token"},
        "trustedProxies": ["10.244.0.0/16"], "bind": "lan",
        "remote": {"token": "sk-dummy-saved-remote"},
    }
    saved["user_preference"] = "preserved"
    agent["config_file"].write_text(json.dumps(saved))
    source["gateway"].update({
        "bind": "loopback", "trustedProxies": ["127.0.0.1"], "auth": PORTAL_SIGN_IN,
        "mode": "local", "remote": {"url": "wss://gateway.example.com"},
    })
    if remote_token_source == "source":
        source["gateway"]["remote"]["token"] = "sk-dummy-source-remote"
    expected_gateway = {**source["gateway"], "remote": {"url": "wss://gateway.example.com"}}
    source_file = agent["config"] / "openclaw.json"
    source_file.write_text(json.dumps(source))
    for _ in range(2):
        result = run(agent)
        assert result.returncode == 0, result.stderr
        current = read_config(agent)
        assert current["gateway"] == expected_gateway
        assert current["user_preference"] == "preserved"
    del source["gateway"]["auth"]
    source_file.write_text(json.dumps(source))
    result = run(agent)
    assert result.returncode == 0, result.stderr
    assert read_config(agent)["gateway"] == source["gateway"]
    assert "auth" not in read_config(agent)["gateway"]


@pytest.mark.parametrize("agent", ["openclaw"], indirect=True)
@pytest.mark.parametrize("remote_token_source", ["source", "home"])
def test_openclaw_portal_sign_in_replaces_and_then_leaves_the_saved_gateway_auth(agent, remote_token_source):
    # A HOME that saved a token: OpenClaw refuses a token in trusted-proxy mode.
    assert run(agent).returncode == 0
    saved = read_config(agent)
    saved["gateway"] = {
        "port": 18789, "auth": {"mode": "token", "token": "saved-test-token"},
        "remote": {"token": "sk-dummy-saved-remote"},
    }
    agent["config_file"].write_text(json.dumps(saved))
    source_file = agent["config"] / "openclaw.json"
    source = json.loads(source_file.read_text())
    remote = {"url": "wss://gateway.example.com", "password": "sk-dummy-password"}
    source["gateway"] = {
        "trustedProxies": ["127.0.0.6"], "auth": PORTAL_SIGN_IN, "mode": "local", "remote": dict(remote),
    }
    if remote_token_source == "source":
        source["gateway"]["remote"]["token"] = "sk-dummy-source-remote"
    source_file.write_text(json.dumps(source))
    for _ in range(2):
        result = run(agent)
        assert result.returncode == 0, result.stderr
        config = read_config(agent)
        assert config["gateway"]["auth"] == PORTAL_SIGN_IN
        assert config["gateway"]["port"] == 18789
        assert config["gateway"]["remote"] == remote
    # Turned off again: the chart's mode goes, and OpenClaw falls back to the
    # token from its environment.
    del source["gateway"]["auth"]
    source_file.write_text(json.dumps(source))
    result = run(agent)
    assert result.returncode == 0, result.stderr
    config = read_config(agent)
    assert "auth" not in config["gateway"]
    assert config["gateway"]["trustedProxies"] == ["127.0.0.6"]
    assert config["gateway"]["remote"] == source["gateway"]["remote"]
    # An owner's own non-proxy auth settings are not the chart's to remove.
    config["gateway"]["auth"] = {"mode": "password"}
    agent["config_file"].write_text(json.dumps(config))
    result = run(agent)
    assert result.returncode == 0, result.stderr
    assert read_config(agent)["gateway"]["auth"] == {"mode": "password"}


# Skill Marketplace mode: CETIRUN_SHARED_SKILLS_DIR is the Team's subtree, and
# the initializer copies the release that CURRENT names into the context
# view's team-skills, which native Agents mount at skills.mountPath. Plain
# unittest cases: pytest collects them, and python3 -m unittest runs them too
# where pytest can be imported for the fixtures above.
SELECTION = "sha256:" + "0123456789abcdef" * 4
RELEASE = "0123456789abcdef0123"
NEXT_RELEASE = "fedcba9876543210fedc"


def snapshot(root):
    """Every path below root with its link target or file content."""
    return {
        str(path.relative_to(root)): os.readlink(path) if path.is_symlink()
        else path.read_bytes() if path.is_file() else None
        for path in root.rglob("*")
    }


class SkillsReleaseTests(unittest.TestCase):
    def setUp(self):
        self.use("hermes")

    def use(self, kind):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        self.root = Path(temp.name)
        self.agent = make_agent(kind, self.root)
        self.team, self.view = self.root / "team", self.root / "view"
        self.team.mkdir()
        self.view.mkdir()
        self.agent["env"].update(
            CETIRUN_CONTEXT_VIEW=str(self.view),
            CETIRUN_SHARED_SKILLS_DIR=str(self.team),
            CETIRUN_SKILLS_RELEASE_SELECTION=SELECTION,
        )

    def release(self, release_id=RELEASE, selection=SELECTION, skills=("team-skill",)):
        """A release directory as the marketplace publishes it."""
        directory = self.team / "releases" / release_id
        directory.mkdir(parents=True)
        for name in skills:
            (directory / name).mkdir()
            (directory / name / "SKILL.md").write_text(f"Synthetic {name} instructions.\n")
        (directory / ".cetirun-selection").write_text(selection + "\n")
        (directory / ".cetirun-release.json").write_text(json.dumps({"skills": list(skills)}) + "\n")
        return directory

    def publish(self, release_id=RELEASE):
        """Name a release in CURRENT: written aside, then renamed into place."""
        written = self.root / "CURRENT.new"
        written.write_text(release_id + "\n")
        os.replace(written, self.team / "CURRENT")

    def run_init(self, **env):
        self.agent["env"].update(env)
        return run(self.agent)

    def published_skills(self):
        return sorted(path.name for path in (self.view / "current/team-skills").iterdir())

    def assert_refused(self, result, error):
        self.assertNotEqual(result.returncode, 0, result.stderr)
        self.assertIn(error, result.stderr)
        self.assertNotIn("[agent-init] complete", result.stderr)
        # Neither a view nor its staging copy is left behind.
        self.assertEqual(list(self.view.iterdir()), [])

    def stub_release_copy(self, script):
        """Run script whenever the initializer copies a released skill."""
        real = shutil.which("cp")
        stub = self.agent["binaries"] / "cp"
        stub.write_text(
            f'#!/bin/sh\ncase "$1 $2" in "-RP {self.team}/releases/"*)\n{script}\n;;\nesac\n'
            f'exec "{real}" "$@"\n'
        )
        stub.chmod(0o755)

    def test_only_skills_of_the_current_release_are_copied(self):
        for kind in ("openclaw", "hermes"):
            with self.subTest(kind=kind):
                self.use(kind)
                self.release(NEXT_RELEASE, skills=("older-skill",))
                release = self.release(skills=("team-skill", "second-skill"))
                (release / "team-skill/references").mkdir()
                (release / "team-skill/references/guide.md").write_text("Synthetic reference.\n")
                (release / "team-skill/run.sh").write_text("#!/bin/sh\n")
                (release / "team-skill/run.sh").chmod(0o755)
                (release / "README.md").write_text("Synthetic release notes.\n")
                for name in (".hidden-skill", "Upper-skill", "double--dash", "-leading", "trailing-"):
                    (release / name).mkdir()
                    (release / name / "SKILL.md").write_text("Not a skill name.\n")
                (release / "Latest").symlink_to("team-skill")
                self.publish()
                sources = snapshot(self.team)
                result = run(self.agent)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(self.published_skills(), ["second-skill", "team-skill"])
                skill = self.view / "current/team-skills/team-skill"
                self.assertEqual((skill / "SKILL.md").read_text(), "Synthetic team-skill instructions.\n")
                self.assertEqual((skill / "references/guide.md").read_text(), "Synthetic reference.\n")
                self.assertEqual((skill / "run.sh").stat().st_mode & 0o777, 0o755)
                self.assertEqual(sorted(os.listdir(self.view / "current")),
                                 [".ready", ".skills-release", "AGENTS.md", "skills", "team-skills"])
                self.assertEqual((self.view / "current/.skills-release").read_text(), RELEASE + "\n")
                # Team skills stay apart from the composed platform skills.
                self.assertEqual(os.listdir(self.view / "current/skills"), [])
                self.assertEqual(snapshot(self.team), sources)

    def test_waits_for_current_to_name_the_release(self):
        self.release()
        self.assertFalse((self.team / "CURRENT").exists())
        writer = threading.Timer(3, self.publish)
        writer.start()
        self.addCleanup(writer.join)
        self.addCleanup(writer.cancel)
        result = self.run_init(CETIRUN_SKILLS_RELEASE_TIMEOUT="10")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.published_skills(), ["team-skill"])

    def test_release_that_never_arrives_is_pending_after_the_timeout(self):
        self.release()  # Published, but CURRENT never names it.
        started = time.monotonic()
        result = self.run_init(CETIRUN_SKILLS_RELEASE_TIMEOUT="3")
        self.assert_refused(result, "SkillsReleasePending")
        self.assertGreaterEqual(time.monotonic() - started, 3)

    def test_release_of_another_selection_is_pending(self):
        release = self.release(selection="sha256:" + "f" * 64)
        self.publish()
        started = time.monotonic()
        result = self.run_init(CETIRUN_SKILLS_RELEASE_TIMEOUT="2")
        self.assert_refused(result, "SkillsReleasePending")
        self.assertGreaterEqual(time.monotonic() - started, 2)
        # Only the first line counts, and only exactly.
        self.agent["env"]["CETIRUN_SKILLS_RELEASE_TIMEOUT"] = "0"
        for content in (f"\n{SELECTION}\n", f"{SELECTION} \n", ""):
            with self.subTest(selection=content):
                (release / ".cetirun-selection").write_text(content)
                self.assert_refused(run(self.agent), "SkillsReleasePending")
        (release / ".cetirun-selection").write_text(f"{SELECTION}\nsynthetic note\n")
        result = run(self.agent)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.published_skills(), ["team-skill"])

    def test_invalid_current_is_pending(self):
        self.agent["env"]["CETIRUN_SKILLS_RELEASE_TIMEOUT"] = "0"
        self.release()
        # Each malformed id names a release that would otherwise be accepted.
        malformed = [RELEASE.upper(), RELEASE[:19], RELEASE + "4", "./" + RELEASE[:18]]
        for value in malformed:
            self.release(value)
        current = self.team / "CURRENT"
        for content in [value + "\n" for value in malformed] + ["", f" {RELEASE}\n", f"{RELEASE}\n{RELEASE}\n"]:
            with self.subTest(current=content):
                current.write_text(content)
                self.assert_refused(run(self.agent), "SkillsReleasePending")
        current.unlink()
        pointer = self.root / "pointer"
        pointer.write_text(RELEASE + "\n")
        current.symlink_to(pointer)
        with self.subTest(current="link"):
            self.assert_refused(run(self.agent), "SkillsReleasePending")
        current.unlink()
        current.mkdir()
        with self.subTest(current="directory"):
            self.assert_refused(run(self.agent), "SkillsReleasePending")
        current.rmdir()
        current.write_text(RELEASE)  # One line needs no final newline.
        result = run(self.agent)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.published_skills(), ["team-skill"])

    def test_release_that_is_not_in_place_is_pending(self):
        self.agent["env"]["CETIRUN_SKILLS_RELEASE_TIMEOUT"] = "0"
        other = self.release(NEXT_RELEASE)
        self.publish()
        releases = self.team / "releases"
        with self.subTest(release="missing"):
            self.assert_refused(run(self.agent), "SkillsReleasePending")
        (releases / RELEASE).symlink_to(NEXT_RELEASE)
        with self.subTest(release="link"):
            self.assert_refused(run(self.agent), "SkillsReleasePending")
        (releases / RELEASE).unlink()
        marker = self.release() / ".cetirun-selection"
        marker.unlink()
        with self.subTest(selection="missing"):
            self.assert_refused(run(self.agent), "SkillsReleasePending")
        marker.symlink_to(other / ".cetirun-selection")
        with self.subTest(selection="link"):
            self.assert_refused(run(self.agent), "SkillsReleasePending")
        marker.unlink()
        marker.write_text(SELECTION + "\n")
        moved = self.root / "releases"
        releases.rename(moved)
        releases.symlink_to(moved)
        with self.subTest(releases="link"):
            self.assert_refused(run(self.agent), "SkillsReleasePending")
        releases.unlink()
        moved.rename(releases)
        result = run(self.agent)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.published_skills(), ["team-skill"])

    def test_links_and_special_files_in_a_released_skill_are_refused(self):
        outside = self.root / "outside-skill"
        outside.mkdir()
        (outside / "SKILL.md").write_text("Synthetic skill outside the release.\n")
        for case in ("file", "dangling", "nested", "skill", "fifo"):
            with self.subTest(case=case):
                shutil.rmtree(self.team / "releases", ignore_errors=True)
                skill = self.release() / "team-skill"
                if case == "file":
                    (skill / "copy.md").symlink_to("SKILL.md")
                elif case == "dangling":
                    (skill / "absent").symlink_to("/nonexistent/synthetic")
                elif case == "nested":
                    (skill / "references").mkdir()
                    (skill / "references/parent").symlink_to("..")
                elif case == "skill":
                    shutil.rmtree(skill)
                    skill.symlink_to(outside)
                else:
                    os.mkfifo(skill / "pipe")
                self.publish()
                self.assert_refused(run(self.agent), "UnsupportedSkillFile")

    def test_released_skill_cannot_replace_the_memory_skill(self):
        # These tests mount no Memory assets; the shared conflict check
        # reserves the Memory skill's name in any case.
        self.release(skills=("team-skill", "cetirun-memory"))
        self.publish()
        self.assert_refused(run(self.agent), "SharedSkillConflictsWithCanonicalContext")

    def test_current_moving_during_the_copy_copies_the_new_release(self):
        self.release(skills=("first-skill",))
        self.release(NEXT_RELEASE, skills=("second-skill",))
        self.publish()
        moved = self.root / "moved"
        self.stub_release_copy(
            f'if [ ! -e "{moved}" ]; then\n'
            f'  : > "{moved}"\n'
            f'  printf "%s\\n" {NEXT_RELEASE} > "{self.root}/CURRENT.new"\n'
            f'  mv "{self.root}/CURRENT.new" "{self.team}/CURRENT"\n'
            "fi"
        )
        result = run(self.agent)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(moved.exists())
        # The copy of the release CURRENT no longer names is removed.
        self.assertEqual(self.published_skills(), ["second-skill"])
        self.assertEqual((self.view / "current/.skills-release").read_text(), NEXT_RELEASE + "\n")

    def test_current_that_keeps_moving_is_unstable(self):
        self.release(skills=("first-skill",))
        self.release(NEXT_RELEASE, skills=("second-skill",))
        self.publish()
        copies = self.root / "copies"
        self.stub_release_copy(
            f'printf "copy\\n" >> "{copies}"\n'
            f'IFS= read -r id < "{self.team}/CURRENT"\n'
            f"next={NEXT_RELEASE}\n"
            f'[ "$id" = {RELEASE} ] || next={RELEASE}\n'
            f'printf "%s\\n" "$next" > "{self.root}/CURRENT.new"\n'
            f'mv "{self.root}/CURRENT.new" "{self.team}/CURRENT"'
        )
        self.assert_refused(run(self.agent), "SkillsReleaseUnstable")
        self.assertEqual(copies.read_text().splitlines(), ["copy"] * 5)

    def test_without_a_selection_the_shared_directory_is_copied_as_before(self):
        del self.agent["env"]["CETIRUN_SKILLS_RELEASE_SELECTION"]
        self.release()
        self.publish()
        (self.team / ".team-notes").write_text("Synthetic notes.\n")
        (self.team / "notes-link").symlink_to(".team-notes")
        log = self.root / "termination-log"
        log.write_text("")
        self.agent["env"]["CETIRUN_TERMINATION_LOG"] = str(log)
        result = run(self.agent)
        self.assertEqual(result.returncode, 0, result.stderr)
        skills = self.view / "current/skills"
        self.assertEqual(sorted(path.name for path in skills.iterdir()), [".team-notes", "CURRENT", "notes-link", "releases"])
        self.assertFalse((self.view / "current/team-skills").exists())
        self.assertEqual(os.readlink(skills / "notes-link"), ".team-notes")
        self.assertEqual((skills / "releases" / RELEASE / ".cetirun-selection").read_text(), SELECTION + "\n")
        # Only a Skill Marketplace release is reported.
        self.assertFalse((self.view / "current/.skills-release").exists())
        self.assertEqual(log.read_text(), "")

    def test_the_copied_release_is_reported_in_the_termination_message(self):
        # The Portal compares the report with the Team's current release.
        self.release(skills=("first-skill",))
        self.publish()
        log = self.root / "termination-log"
        log.write_text("")
        self.agent["env"]["CETIRUN_TERMINATION_LOG"] = str(log)
        result = run(self.agent)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(log.read_text(), f"cetirun.skills.release={RELEASE}\n")
        # A later release does not reach a Pod that has its view: an
        # initializer that runs again in the same Pod reports the copy again.
        self.release(NEXT_RELEASE, skills=("second-skill",))
        self.publish(NEXT_RELEASE)
        log.write_text("")
        result = run(self.agent)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.published_skills(), ["first-skill"])
        self.assertEqual(log.read_text(), f"cetirun.skills.release={RELEASE}\n")

    def test_an_unavailable_termination_message_does_not_stop_the_initializer(self):
        self.release()
        self.publish()
        for case in ("missing", "directory", "read-only"):
            with self.subTest(case=case):
                shutil.rmtree(self.view)
                self.view.mkdir()
                log = self.root / f"termination-{case}"
                if case == "directory":
                    log.mkdir()
                elif case == "read-only":
                    log.write_text("")
                    log.chmod(0o444)
                result = self.run_init(CETIRUN_TERMINATION_LOG=str(log))
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(self.published_skills(), ["team-skill"])
                if case == "missing":
                    # Outside Kubernetes, no file appears at the path.
                    self.assertFalse(log.exists())

    def test_release_mode_configuration_errors(self):
        self.release()
        self.publish()
        for timeout in ("3m", "-1", "1.5"):
            with self.subTest(timeout=timeout):
                result = self.run_init(CETIRUN_SKILLS_RELEASE_TIMEOUT=timeout)
                self.assert_refused(result, "InvalidSkillsReleaseTimeout")
        del self.agent["env"]["CETIRUN_SKILLS_RELEASE_TIMEOUT"]
        del self.agent["env"]["CETIRUN_SHARED_SKILLS_DIR"]
        self.assert_refused(run(self.agent), "SharedSkillsUnavailable")
