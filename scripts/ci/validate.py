#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Run explicit offline gates. Never install dependencies or execute deployment scripts."""

import argparse
import ast
from contextlib import contextmanager
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile


ROOT = Path(__file__).resolve().parents[2]
COMPONENTS = ("operator", "portal", "agent-broker", "memory-context")
MODULES = {"internal": "app/internal", **{name: f"app/cetirun-{name}" for name in COMPONENTS}}
MODULES["cetirun-connections-mcp"] = "app/cetirun-connections-mcp"
WORKFLOWS = (
    "memory-context-ci.yml",
    "observability-ci.yml",
    "operator-ci.yml",
    "portal-ci.yml",
    "agent-broker-ci.yml",
    "helm-lint-test.yml",
    "docker-build-validate.yml",
    "codeql.yml",
    "cetirun-validation.yml",
)
CHARTS = (
    ("app/cetirun-connections-mcp/chart", (
        "tls.secretName=tools-tls", "broker.clientTLSSecretName=broker-client-tls",
        "broker.baseURL=https://broker.example.com",
        "istio.sidecarInjection=true", "istio.revision=asm-example",
    )),
    (
        "app/cetirun-chart",
        (
            "agentType=openclaw",
            "secrets.model=lint-model",
            "aiGateway.keySecretName=lint-model-key",
            "secrets.apiBase=http://gateway.example.com/v1",
            "secrets.openclawGatewayToken=lint-token",
        ),
    ),
    ("app/cetirun-chart", ("agentType=hermes", "secrets.model=lint-model",
        "aiGateway.keySecretName=lint-model-key", "secrets.apiBase=http://gateway.example.com/v1",
        "hermes.env.HERMES_WEBAPP_PUBLIC_URL=https://agents.example.com/ui/validation")),
    ("app/cetirun-operator/chart", ("cetiadmin.enabled=false",)),
    ("app/cetirun-portal/chart", ("istio.host=portal.example.invalid",)),
    (
        "app/cetirun-agent-broker/chart",
        (
            "agentIdentity.tenantId=11111111-1111-1111-1111-111111111111",
            "agentIdentity.blueprintClientId=22222222-2222-2222-2222-222222222222",
            "agentIdentity.authorizationClientId=33333333-3333-3333-3333-333333333333",
            "agentIdentity.encryptionClientId=44444444-4444-4444-4444-444444444444",
            "agentIdentity.keyVaultKeyUrl=https://example.vault.azure.net/keys/cache/version",
            "agentIdentity.redirectUri=https://portal.example.invalid/api/v1/connections/callback",
            "agentIdentity.portalOrigin=https://portal.example.invalid",
            "agentIdentity.portalBaseUrl=https://portal.example.invalid",
            "istio.sidecarInjection=true",
            "istio.revision=asm-example",
        ),
    ),
    (
        "app/cetirun-ai-gateway/chart",
        (
            "models[0].model_name=lint-model",
            # Only models: the defaults bundle PostgreSQL and Valkey, so this
            # keeps the default path under test. ci/render-values.yaml
            # covers external dependencies in the release lint.
            "models[0].litellm_params.model=openai/lint-model",
        ),
    ),
    (
        "app/cetirun-observability/chart",
        (
            "clusterName=lint",
            "adx.endpoint=https://lint.example.invalid",
            "ingestor.clientId=66666666-6666-6666-6666-666666666666",
        ),
    ),
    (
        "infra/istio",
        (
            "clientId=lint-client",
            "tenantId=lint-tenant",
            "allowedGroups=lint-group",
            "cookieSecret=lint-cookie-secret",
        ),
    ),
    (
        "app/cetirun-platform",
        (
            "istio.enabled=true",
            "cetirun-operator.domainSuffix=platform.example.invalid",
            "cetirun-portal.istio.host=portal.example.invalid",
            # Rendered in the default namespace: the bundled AI Gateway must
            # share the operator's namespace, and the Portal reads the shared
            # models the operator publishes there.
            "cetirun-operator.namespace=default",
            "cetirun-operator.cetiadmin.enabled=false",
            "cetirun-portal.aiGateway.catalogNamespace=default",
        ),
    ),
)


def require_file(path, message=None):
    if not path.is_file():
        raise ValueError(message or f"Required file is missing: {path}")
    return path


def run(args, cwd, *, env=None, capture=False):
    """Only callers construct argument arrays; no shell commands are accepted."""
    if shutil.which(str(args[0])) is None:
        raise ValueError(f"Required tool is missing: {args[0]}; install it separately")
    print("+", " ".join(map(str, args)), flush=True)
    return subprocess.run(
        list(map(str, args)),
        cwd=cwd,
        env=env,
        check=True,
        text=True,
        stdout=subprocess.PIPE if capture else None,
    ).stdout


def offline_env():
    return {
        **os.environ,
        "GOPROXY": "off",
        "GOSUMDB": "off",
        "GOTOOLCHAIN": "local",
        "GOWORK": "off",
        "GOFLAGS": "-mod=readonly",
        "GOTELEMETRY": "off",
        "KUBECONFIG": os.devnull,
        "npm_config_offline": "true",
        "npm_config_audit": "false",
        "npm_config_update_notifier": "false",
        "PYTHONDONTWRITEBYTECODE": "1",
    }


def files(root, suffix):
    output = run(
        ["git", "ls-files", "--cached", "--others", "--exclude-standard", "-z"],
        root,
        capture=True,
    )
    return sorted(
        {root / p for p in output.split("\0") if p.endswith(suffix) and (root / p).is_file()}
    )


@contextmanager
def go_directory(root, component):
    module = root / MODULES[component]
    require_file(module / "go.mod")
    if component != "portal" or (module / "web/dist").is_dir():
        yield module
        return
    # Go embeds the frontend. Use a disposable source copy when it is not built,
    # without writing a placeholder over a developer's frontend or source files.
    with tempfile.TemporaryDirectory(prefix="cetirun-go-") as temp:
        for name in ("operator", "portal", "internal"):
            source = root / MODULES[name]
            shutil.copytree(
                source,
                Path(temp) / source.name,
                ignore=shutil.ignore_patterns("node_modules", "dist", "bin", ".git"),
            )
        module = Path(temp) / "cetirun-portal"
        (module / "web/dist").mkdir(parents=True)
        (module / "web/dist/index.html").write_text("CI embed fixture\n")
        yield module


def go_gate(root, mode, component):
    for name in (component,) if component else MODULES:
        with go_directory(root, name) as module:
            if mode == "test":
                command = ["make", "test"] if name == "operator" else ["go", "test", "./...", "-count=1"]
                run(command, module, env=offline_env())
            else:
                # Vet is the existing component lint contract. Do not invent a
                # golangci-lint config or apply formatting to source owned elsewhere.
                run(["go", "vet", "./..."], module, env=offline_env())


def portal_gate(root, mode):
    web = root / "app/cetirun-portal/web"
    require_file(web / "package-lock.json")
    tool = web / "node_modules/.bin" / ("vitest" if mode == "test" else "tsc")
    require_file(
        tool,
        "Portal dependencies are missing; run npm ci in app/cetirun-portal/web first",
    )
    command = (
        ["npm", "test"] if mode == "test" else [str(tool), "-b", "--pretty", "false"]
    )
    run(command, web, env=offline_env())


def chart_gate(root):
    # Validate the entire inventory before executing any tool.
    for directory, _ in CHARTS:
        for filename in ("Chart.yaml", "values.yaml"):
            require_file(root / directory / filename)
    platform_chart = root / "app/cetirun-platform"
    run(
        ["helm", "dependency", "build", "--skip-refresh", str(platform_chart)],
        root,
        env=offline_env(),
    )
    for directory, values in CHARTS:
        chart = root / directory
        options = [arg for value in values for arg in (
            "--set" if value.rsplit("=", 1)[-1] in ("true", "false") else "--set-string", value
        )]
        run(["helm", "lint", "--strict", str(chart), *options], root, env=offline_env())
        run(
            ["helm", "template", "validation", str(chart), *options],
            root,
            env=offline_env(),
            capture=True,
        )
    # Retain the existing runtime-chart positive and negative render regressions.
    run(
        ["bash", str(require_file(root / "scripts/cetirun-helm-test.sh"))],
        root,
        env={**offline_env(), "CHART_DIR": str(root / "app/cetirun-chart")},
    )
    run(
        ["bash", str(require_file(root / "scripts/cetirun-platform-helm-test.sh"))],
        root,
        env={**offline_env(), "CHART_DIR": str(platform_chart)},
    )
    run(
        ["bash", str(require_file(root / "scripts/cetirun-istio-helm-test.sh"))],
        root,
        env={**offline_env(), "CHART_DIR": str(root / "infra/istio")},
    )
    run(
        ["bash", str(require_file(root / "scripts/cetirun-agent-broker-helm-test.sh"))],
        root,
        env={
            **offline_env(),
            "CHART_DIR": str(root / "app/cetirun-agent-broker/chart"),
        },
    )
    run(
        ["bash", str(require_file(root / "scripts/cetirun-ai-gateway-helm-test.sh"))],
        root,
        env={**offline_env(), "CHART_DIR": str(root / "app/cetirun-ai-gateway/chart")},
    )
    run(
        ["bash", str(require_file(root / "scripts/cetirun-observability-helm-test.sh"))],
        root,
        env={**offline_env(), "CHART_DIR": str(root / "app/cetirun-observability/chart")},
    )


def bicep_gate(root):
    sources = files(root, ".bicep")
    if not sources:
        raise ValueError("No Bicep templates found")
    # --no-restore prevents registry access; all current modules are local files.
    # Build runs Bicep's linter too: errors fail, advisory warnings remain visible.
    with tempfile.TemporaryDirectory(prefix="cetirun-bicep-") as temp:
        for index, source in enumerate(sources):
            run(
                [
                    "bicep",
                    "build",
                    str(source),
                    "--no-restore",
                    "--outfile",
                    str(Path(temp) / f"{index}.json"),
                ],
                root,
            )


def shell_gate(root):
    sources = files(root, ".sh")
    if not sources:
        raise ValueError("No shell scripts found")
    for source in sources:
        run(["bash", "-n", str(source)], root)
    # Existing operational scripts are syntax/error-gated without executing them.
    run(["shellcheck", "--severity=error", "--", *map(str, sources)], root)


def python_gate(root):
    for source in files(root, ".py"):
        ast.parse(source.read_bytes(), filename=str(source))


def workflow_gate(root):
    # Publisher/E2E workflows have pre-existing actionlint diagnostics and are
    # outside this gate's scope; never globally suppress those rules.
    sources = [require_file(root / ".github/workflows" / name) for name in WORKFLOWS]
    if shutil.which("shellcheck") is None:
        raise ValueError(
            "workflow-check requires shellcheck for embedded shell validation"
        )
    run(["actionlint", "-color", *map(str, sources)], root)


def python_tests(root):
    require_file(root / "scripts/ci/python_safety.py")
    # Check the actual HTTP and configuration parsers before running the suites.
    run(
        [
            sys.executable,
            "-c",
            "import pytest, aiohttp, yaml; from dotenv.parser import parse_stream",
        ],
        root,
        env=offline_env(),
    )
    env = {
        **offline_env(),
        "PYTHONPATH": str(root / "scripts/ci"),
        "PYTEST_DISABLE_PLUGIN_AUTOLOAD": "1",
    }
    run(
        [
            sys.executable,
            "-B",
            "-m",
            "pytest",
            "-p",
            "python_safety",
            "-p",
            "no:cacheprovider",
            "-q",
            "app/cetirun-chart/tests",
        ],
        root,
        env=env,
    )


def ci_tests(root):
    for directory in ("scripts/ci/tests", "scripts/tests", "app/cetirun-operator/internal/cetiadmin/tests"):
        if not list((root / directory).glob("test_*.py")):
            raise ValueError(f"No tests found in {directory}")
        run(
            [sys.executable, "-B", "-m", "unittest", "discover", "-s", directory, "-v"],
            root,
            env=offline_env(),
        )


def license_gate(root):
    checker = require_file(
        root / "scripts/check-license-headers.py",
        "license-check requires scripts/check-license-headers.py with a --root PATH option",
    )
    run(
        [sys.executable, "-B", str(checker), "--root", str(root)],
        root,
        env=offline_env(),
    )


def docs_gate(root):
    require_file(
        root / "site/Makefile",
        "docs-check requires site/Makefile and its check target",
    )
    run(["make", "-C", "site", "check"], root, env=offline_env())


GATES = {
    "chart-check": chart_gate,
    "bicep-check": bicep_gate,
    "shell-check": shell_gate,
    "workflow-check": workflow_gate,
    "python-check": python_gate,
    "python-test": python_tests,
    "ci-test": ci_tests,
    "license-check": license_gate,
    "docs-check": docs_gate,
}


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "gate", choices=[*GATES, "go-test", "go-lint", "portal-test", "portal-lint"]
    )
    parser.add_argument("--root", type=Path, default=ROOT, help="repository root")
    parser.add_argument(
        "--component", choices=MODULES, help="limit go-test/go-lint to one module"
    )
    args = parser.parse_args(argv)
    root = args.root.resolve()
    try:
        require_file(
            root / "app/cetirun-operator/go.mod", f"Not a CetiRun root: {root}"
        )
        if args.component and not args.gate.startswith("go-"):
            raise ValueError("--component is only supported for go-test/go-lint")
        if args.gate.startswith("go-"):
            go_gate(root, args.gate.removeprefix("go-"), args.component)
        elif args.gate.startswith("portal-"):
            portal_gate(root, args.gate.removeprefix("portal-"))
        else:
            GATES[args.gate](root)
    except (ValueError, OSError, SyntaxError, subprocess.CalledProcessError) as error:
        print(f"ERROR: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
