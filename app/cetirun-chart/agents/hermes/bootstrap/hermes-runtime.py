#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Enforce the image/Chart contract before gateway starts and observe live readiness."""

from __future__ import annotations

import argparse
import contextlib
import importlib.util
import io
import json
import logging
import os
from pathlib import Path
import re
import stat
import subprocess
import sys
import urllib.request
from urllib.parse import urlsplit

try:
    import hermes_yaml as yaml
except ImportError:
    # Repository helper tests also run with the shared initializer's PyYAML dependency.
    import yaml

ROOT = Path("/usr/local/lib/hermes-agent")
CONFIG = Path("/etc/cetirun")
MANAGED = CONFIG / "hermes-managed"
HOME = Path("/home/vibe/.hermes")
VERSION = "0.22.7"
COMMIT = "b200f5bc60e31588ba2cb60e288029c4a54ba5fa"
RESERVED = frozenset({
    "HOME", "PATH", "PYTHONPATH", "PYTHONHOME", "NODE_PATH", "NODE_OPTIONS",
    "HERMES_HOME", "HERMES_PROFILE", "HERMES_PROFILE_NAME", "HERMES_CONFIG", "HERMES_CONFIG_PATH",
    "HERMES_ENV", "HERMES_ENV_PATH", "HERMES_MANAGED_DIR", "HERMES_PYTHON",
    "HERMES_INSTALL_DIR", "HERMES_INSTALL_ROOT", "HERMES_RUNTIME_DIR", "HERMES_IGNORE_USER_CONFIG", "HERMES_CONFIG_BACKEND",
    "HERMES_GATEWAY_CONFIG", "CETIRUN_AUTH_KEY", "CETIRUN_AI_GATEWAY",
    "CETIRUN_GATEWAY_KEY_ENV", "CETIRUN_INGRESS_MTLS",
})


def require(condition: bool, code: str) -> None:
    if not condition:
        raise ValueError(code)


def document(path: Path) -> dict:
    require(path.is_file(), "ConfigurationMissing")
    value = yaml.safe_load(path.read_text(encoding="utf-8"))
    require(isinstance(value, dict), "ConfigurationTypeInvalid")
    return value


def api_port() -> int:
    value = os.environ.get("API_SERVER_PORT", "")
    require(bool(re.fullmatch(r"[0-9]+", value)), "APIListenerPortInvalid")
    port = int(value)
    require(1 <= port <= 65535 and port != 9120, "APIListenerPortInvalid")
    return port


def contract() -> dict:
    require(os.environ.get("HERMES_HOME") == str(HOME), "HomeBindingInvalid")
    require(HOME.is_dir() and not HOME.is_symlink(), "HomeBindingInvalid")
    require(os.environ.get("HERMES_MANAGED_DIR") == str(MANAGED), "ManagedPointerInvalid")
    require(os.environ.get("HERMES_PYTHON") == "/usr/local/bin/hermes-python", "PythonBindingInvalid")
    require(not any(os.environ.get(name) for name in (
        "HERMES_PROFILE", "HERMES_PROFILE_NAME", "HERMES_CONFIG", "HERMES_CONFIG_PATH", "HERMES_ENV",
        "HERMES_ENV_PATH", "HERMES_IGNORE_USER_CONFIG", "HERMES_CONFIG_BACKEND", "HERMES_GATEWAY_CONFIG", "HERMES_INSTALL_ROOT",
    )), "ProfileOverrideInvalid")
    host = "127.0.0.1" if os.environ.get("CETIRUN_INGRESS_MTLS") == "true" else "0.0.0.0"
    public_url = os.environ.get("HERMES_WEBAPP_PUBLIC_URL", "")
    parsed = urlsplit(public_url)
    require(parsed.scheme == "https" and bool(parsed.hostname) and parsed.port != 0 and not parsed.username
            and not parsed.password and not parsed.query and not parsed.fragment
            and bool(re.fullmatch(r"/ui/[a-z0-9][a-z0-9-]*", parsed.path)), "WebappPublicURLInvalid")
    expected = {"enabled": True, "host": host, "port": 9120, "public_url": public_url}
    expected_managed = {"webapp": expected}
    if host == "0.0.0.0":
        expected_managed["dashboard"] = {"trusted_proxies": ["127.0.0.6"]}
    managed = document(MANAGED / "config.yaml")
    require(managed == expected_managed, "ManagedWebappInvalid")
    require(all(type(managed["webapp"][name]) is type(value) for name, value in expected.items()),
            "ManagedWebappTypeInvalid")
    require(not ((MANAGED / "config.yaml").stat().st_mode & 0o222), "ManagedConfigWritable")
    api_port()
    for name, value in {
        "API_SERVER_ENABLED": "true", "API_SERVER_HOST": host,
        "HERMES_WEBAPP_ENABLED": "true", "HERMES_WEBAPP_HOST": host, "HERMES_WEBAPP_PORT": "9120",
        "HERMES_DASHBOARD_BASIC_AUTH_USERNAME": "cetirun", "CETIRUN_AI_GATEWAY": "true",
    }.items():
        require(os.environ.get(name) == value, "PlatformEnvironmentInvalid")
    key = os.environ.get("API_SERVER_KEY", "")
    signature = os.environ.get("CETIRUN_AUTH_KEY", "")
    require(len(key) >= 16 and key.isascii() and key.strip() == key, "APIKeyInvalid")
    require(bool(re.fullmatch(r"[A-Za-z0-9_+/=-]{32,}", signature)) and signature != key, "SigningKeyInvalid")
    require(os.environ.get("HERMES_DASHBOARD_BASIC_AUTH_PASSWORD") == key
            and os.environ.get("HERMES_DASHBOARD_BASIC_AUTH_SECRET") == signature, "AuthBindingInvalid")
    key_env = os.environ.get("CETIRUN_GATEWAY_KEY_ENV", "")
    require(bool(re.fullmatch(r"[A-Z][A-Z0-9_]*_API_KEY", key_env))
            and bool(os.environ.get(key_env)), "ModelKeyMissing")
    return expected


def reserved(name: str) -> bool:
    return (name in RESERVED or name == os.environ.get("CETIRUN_GATEWAY_KEY_ENV")
            or name.startswith(("API_SERVER_", "HERMES_WEBAPP_", "HERMES_DASHBOARD_", "CETIRUN_",
                                "HERMES_CONFIG", "HERMES_ENV", "HERMES_MANAGED", "HERMES_INSTALL_", "HERMES_PROFILE")))


def validate_secret_sources(config: dict, profile: str = "") -> None:
    sources = config.get("secrets") or {}
    require(isinstance(sources, dict), "SecretSourcesInvalid")
    requested = sources.get("sources") or []
    require(isinstance(requested, list) and all(isinstance(name, str) for name in requested),
            "SecretSourcesInvalid")
    for name, settings in sources.items():
        if name in {"sources", "preserve_existing", "profile_alias"}:
            continue
        require(isinstance(settings, dict), "SecretSourcesInvalid")
        if not settings.get("enabled"):
            continue
        # Bulk sources do not declare their output names before they execute. The
        # pinned OnePassword source is mapped: only its explicit env keys are applied.
        require(name == "onepassword", "SecretSourceOutputsUnbounded")
        bindings = settings.get("env") or {}
        require(isinstance(bindings, dict), "SecretSourceBindingsInvalid")
        token_env = settings.get("service_account_token_env") or "OP_SERVICE_ACCOUNT_TOKEN"
        require(isinstance(token_env, str) and not reserved(token_env), "SecretSourcePlatformCredentialInvalid")
        for variable in bindings:
            require(isinstance(variable, str) and bool(re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", variable)),
                    "SecretSourceBindingsInvalid")
            require(not reserved(variable), "SecretSourcePlatformOverride")
            if profile and sources.get("profile_alias", True):
                from agent.secret_sources.registry import _profile_alias_target

                alias = _profile_alias_target(variable, profile)
                require(alias is None or not reserved(alias), "SecretSourceProfileOverride")


def validate_registered_secret_sources(config: dict, home: Path) -> None:
    from agent.secret_sources.onepassword import OnePasswordSource
    from agent.secret_sources.registry import _ordered_enabled_sources
    from hermes_constants import hermes_home_key

    for source in _ordered_enabled_sources(config.get("secrets") or {}, scope=hermes_home_key(home)):
        require(type(source) is OnePasswordSource, "SecretSourceOutputsUnbounded")


def validate_home_secret_sources() -> None:
    validate_secret_sources(document(HOME / "config.yaml"))
    profiles = HOME / "profiles"
    require(not profiles.is_symlink(), "ProfileDirectoryInvalid")
    if not profiles.exists():
        return
    require(profiles.is_dir(), "ProfileDirectoryInvalid")
    for home in profiles.iterdir():
        require(not home.is_symlink(), "ProfileDirectoryInvalid")
        if home.is_dir() and (home / "config.yaml").exists():
            validate_secret_sources(document(home / "config.yaml"), home.name)


def remove_home_overrides() -> None:
    from dotenv.parser import parse_stream

    spec = importlib.util.spec_from_file_location("cetirun_apply", CONFIG / "hermes-apply-config.py")
    apply = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(apply)
    for relative in ("config.yaml", "gateway.json"):
        path = HOME / relative
        require(not path.is_symlink(), "ConfigurationSymlinkInvalid")
        if not path.exists():
            require(relative != "config.yaml", "ConfigurationMissing")
            continue
        current = document(path)
        before = json.dumps(current, sort_keys=True)
        if relative == "config.yaml":
            apply.apply_gateway(current, document(CONFIG / "hermes-config.yaml"))
        apply.reserve_ingress_listeners(current, {})
        dashboard = current.get("dashboard")
        if isinstance(dashboard, dict):
            current["dashboard"] = {name: value for name, value in dashboard.items() if name != "basic_auth"}
        if before != json.dumps(current, sort_keys=True):
            apply.replace_file(path, yaml.safe_dump(current, sort_keys=False) if relative.endswith("yaml")
                               else json.dumps(current, indent=2) + "\n")
    for relative in (".env", ".op.env"):
        path = HOME / relative
        require(not path.is_symlink(), "EnvironmentSymlinkInvalid")
        if not path.exists():
            require(relative != ".env", "EnvironmentMissing")
            continue
        before = path.read_text(encoding="utf-8-sig")
        bindings = list(parse_stream(io.StringIO(before)))
        require(not any(item.error for item in bindings), "EnvironmentSyntaxInvalid")
        after = "".join(item.original.string for item in bindings if not item.key or not reserved(item.key))
        if before != after:
            apply.replace_file(path, after)


def bundle(full: bool = False) -> None:
    dist = ROOT / "apps/desktop/dist-webapp"
    require(os.environ.get("HERMES_WEBAPP_DIST") == str(dist), "WebappBundlePointerInvalid")
    require(all((dist / name).is_file() for name in ("index.html", "hermes-build.json", "renderer-manifest.json"))
            and not (dist / ".hermes-stale").exists(), "WebappBundleMissing")
    if full:
        from hermes_cli.source_build import source_product_current
        require(source_product_current(ROOT, "webapp", dist), "WebappReceiptInvalid")


def effective(expected: dict) -> None:
    validate_home_secret_sources()
    from hermes_cli.env_loader import load_hermes_dotenv

    protected = {name: value for name, value in os.environ.items() if reserved(name)}
    load_hermes_dotenv(hermes_home=HOME, project_env=ROOT / ".env", load_external_secrets=False)
    require(all(os.environ.get(name) == value for name, value in protected.items()), "EnvironmentAuthorityLost")
    from hermes_constants import get_hermes_home
    from hermes_cli.config import load_config_readonly
    from gateway import config_loader
    from gateway.config import Platform, load_gateway_config
    from gateway.platforms.api_server import listen_address
    from gateway.webapp_listener import webapp_settings

    require(get_hermes_home() == HOME, "ProfileAuthorityLost")
    # This raises instead of accepting load_gateway_config's fallback on a bad YAML layer.
    config_loader.load_yaml_layer(HOME, config_loader.load_legacy_gateway_json(HOME))
    api = load_gateway_config().platforms.get(Platform.API_SERVER)
    require(api is not None and api.enabled and listen_address(api.extra) == (expected["host"], api_port())
            and api.extra.get("key") == os.environ["API_SERVER_KEY"], "EffectiveAPIInvalid")
    web = webapp_settings(HOME)
    require(web.enabled is True and web.reason is None and web.host == expected["host"]
            and web.port == 9120 and web.public_url == expected["public_url"], "EffectiveWebappInvalid")
    config = load_config_readonly()
    validate_secret_sources(config)
    generated = document(CONFIG / "hermes-config.yaml")
    require(all(config.get(name) == generated.get(name) for name in ("model", "providers")), "EffectiveModelInvalid")
    from hermes_cli.dashboard_auth.registry import get_provider
    from hermes_cli.plugins import discover_plugins

    discover_plugins()
    validate_registered_secret_sources(config, HOME)
    provider = get_provider("basic")
    require(provider is not None and provider.supports_password, "PasswordProviderMissing")
    require(provider.complete_password_login(username="cetirun", password=os.environ["API_SERVER_KEY"]) is not None,
            "PasswordProviderInvalid")


def check() -> None:
    expected = contract()
    remove_home_overrides()
    validate_home_secret_sources()
    require(sys.version_info[:2] == (3, 14), "PythonVersionInvalid")
    from hermes_cli.version_info import get_version_info

    version = get_version_info()
    require(version.base_version == VERSION and version.commit == COMMIT and version.distance == 0
            and version.distribution == "docker" and version.source == "docker", "ImageVersionInvalid")
    stamp = json.loads((ROOT / "install-stamp.json").read_text())
    require(stamp.get("updateMechanism") == "external", "ImageUpdateOwnerInvalid")
    bundle(full=True)
    effective(expected)


def get_json(port: int, path: str, timeout: float, public_url: str = "") -> dict:
    headers = {}
    if public_url:
        public = urlsplit(public_url)
        headers = {"Host": public.netloc, "X-Forwarded-Proto": "https", "X-Forwarded-Prefix": public.path}
    request = urllib.request.Request(f"http://127.0.0.1:{port}{path}", headers=headers)
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    with opener.open(request, timeout=timeout) as response:
        require(response.status == 200, "HTTPProbeFailed")
        return json.load(response)


def helper_health(timeout: float) -> None:
    for flag, filename, code in (
        ("CETIRUN_EGRESS_SNI", "proxy-health.cjs", "EgressProxyNotReady"),
        ("CETIRUN_INGRESS_MTLS", "ingress-health.cjs", "IngressTLSNotReady"),
    ):
        if os.environ.get(flag) != "true":
            continue
        script = "require(process.argv[1])((ready) => process.exit(ready ? 0 : 1))"
        try:
            result = subprocess.run(["node", "-e", script, str(CONFIG / filename)],
                                    stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                                    stderr=subprocess.DEVNULL, timeout=timeout, check=False)
        except (OSError, subprocess.TimeoutExpired):
            raise ValueError(code) from None
        require(result.returncode == 0, code)


def readiness(timeout: float) -> None:
    expected = contract()
    bundle()
    token = HOME / "webapp/session-token"
    for path, mode in ((token.parent, 0o700), (token, 0o600)):
        info = path.lstat()
        require(not stat.S_ISLNK(info.st_mode) and stat.S_IMODE(info.st_mode) == mode
                and info.st_uid == os.geteuid(), "BrowserCredentialPermissionsInvalid")
    require(bool(re.fullmatch(r"[A-Za-z0-9_-]{43}\n?", token.read_text())), "BrowserCredentialInvalid")
    from hermes_cli.gateway_client import private_control_query

    request = json.dumps({"protocol": 1, "id": 1, "verb": "webapp-status", "params": {}}).encode() + b"\n"
    budget = max(0.1, timeout / 5)
    response = json.loads(private_control_query(HOME, request, timeout=budget).split(b"\n", 1)[0])
    status = response.get("result", {})
    require(response.get("ok") is True and status.get("state") == "serving"
            and status.get("url") == "http://127.0.0.1:9120", "WebappNotServing")
    get_json(api_port(), "/health", budget)
    auth = get_json(9120, "/api/auth/providers", budget, expected["public_url"])
    require(any(p.get("name") == "basic" and p.get("supports_password") is True
                for p in auth.get("providers", [])), "LivePasswordProviderMissing")
    helper_health(budget)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("mode", choices=("check", "readiness", "liveness"))
    parser.add_argument("--timeout", type=float, default=3)
    args = parser.parse_args()
    logging.disable(logging.CRITICAL)
    sys.path.insert(0, str(ROOT))
    try:
        # Upstream parser/provider diagnostics can include user configuration or secrets.
        with contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
            if args.mode == "check":
                check()
            elif args.mode == "readiness":
                readiness(args.timeout)
            else:
                get_json(api_port(), "/health", max(0.1, args.timeout / 3))
                helper_health(max(0.1, args.timeout / 3))
        return 0
    except Exception as exc:
        code = str(exc) if type(exc) is ValueError and re.fullmatch(r"[A-Z][A-Za-z]+", str(exc)) else "RuntimeCheckFailed"
        print(f"[hermes-runtime] {code}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
