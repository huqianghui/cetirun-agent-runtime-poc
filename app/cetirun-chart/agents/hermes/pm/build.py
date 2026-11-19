# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Build the pinned Hermes tools, dependency environment and packaged identity."""

import argparse
import json
import os
from pathlib import Path
import subprocess
import sys

REQUIRED_EXTRAS = ("all", "messaging", "anthropic", "azure-identity")


def run(*args, source):
    subprocess.run([str(arg) for arg in args], cwd=source, check=True)


def prepare_tools(root, version, commit):
    import pm
    from scripts.bundles.payload import seal_pm_runtime

    for name in ("uv", "npm"):
        pm.ensure(name, explicit=True)
    binaries = root / "bin"
    binaries.mkdir(exist_ok=True)
    for command, package in (("python3", "python"), ("node", "node"), ("npm", "npm")):
        link = binaries / command
        link.unlink(missing_ok=True)
        link.symlink_to(pm.installed_package(package).binary)
    python = (binaries / "python3").resolve()
    pm.stage_manager_runtime(python=python, destination=root / "pm-runtime", project=root / "pm")
    seal_pm_runtime(root, python)
    run(python, root / "scripts/write_install_stamp.py", "--output", root / "install-stamp.json",
        "--base-version", version, "--distance", "0", "--commit", commit,
        "--distribution", "docker", "--source", "docker", "--update-mechanism", "external", source=root)
    stamp = root / "install-stamp.json"
    identity = json.loads(stamp.read_text())
    identity["pmRuntime"] = str(root / "pm-runtime")
    stamp.write_text(json.dumps(identity, indent=2) + "\n")
    (root / "cetirun-image.json").write_text(json.dumps({
        "schema": 1, "deployment_kind": "image", "manager": "docker",
        "image": "cetirun/hermes-gateway", "version": version, "revision": commit,
    }, indent=2) + "\n")


def prepare_dependencies(root):
    run(root / "bin/python3", "-m", "pm.build_env", "--source", root,
        "--python", root / "bin/python3", "--out", root / ".venv",
        "--cache", root / "uv-cache", "--no-install-project", "--sealed", "--record-selection",
        *[arg for extra in REQUIRED_EXTRAS for arg in ("--extra", extra)], source=root)


def assemble(root):
    from scripts.build.agent import assemble as assemble_agent
    from scripts.build.inputs import AgentInputs, RESOURCE_ENV, dependency_site, project_entries
    from pm.lock import Lockfile
    from pm.store import current_target

    environment = root / ".venv"
    target = current_target()
    version = Lockfile(root / "pm/lock.json").version("python")
    assemble_agent(AgentInputs(
        project=root / "pyproject.toml", code=root, repo=".", placement="fixed", target=target,
        python=(root / "bin/python3").resolve(), site_packages=dependency_site(environment, version, target),
        environment=environment, tools=root / "tools", pm_runtime=root / "pm-runtime", bin_dir="libexec",
        resources={name: root / name for name in RESOURCE_ENV},
        frontends={"tui": root / "tui-product", "webapp": root / "apps/desktop/dist-webapp"},
        stamp=root / "install-stamp.json",
    ), root)
    site = dependency_site(environment, version, target)
    with (site / "hermes-agent.pth").open("a") as binding:
        binding.write("import cetirun_pm.sqlite_runtime; cetirun_pm.sqlite_runtime.bootstrap()\n")
    for name, entry in project_entries(root / "pyproject.toml").items():
        module, function = entry.split(":")
        command = root / "libexec" / name
        command.write_text(
            "#!/bin/sh\nexec /usr/local/bin/hermes-python -c "
            f"'import sys; from {module} import {function}; sys.exit({function}())' \"$@\"\n"
        )
        command.chmod(0o755)
    (root / ".install_method").write_text("docker\n")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source", type=Path, required=True)
    parser.add_argument("--phase", choices=("tools", "dependencies", "assemble"), required=True)
    parser.add_argument("--version", default="0.22.7")
    parser.add_argument("--commit", default="b200f5bc60e31588ba2cb60e288029c4a54ba5fa")
    args = parser.parse_args()
    root = args.source.resolve()
    sys.path.insert(0, str(root))
    os.environ["HERMES_RUNTIME_DIR"] = str(root / "tools")
    os.environ["HERMES_INSTALL_DIR"] = str(root)
    if args.phase == "tools":
        prepare_tools(root, args.version, args.commit)
    elif args.phase == "dependencies":
        prepare_dependencies(root)
    else:
        assemble(root)


if __name__ == "__main__":
    main()
