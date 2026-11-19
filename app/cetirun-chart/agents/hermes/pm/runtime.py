# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Select an already prepared PM environment before importing Hermes."""

import json
import os
from pathlib import Path
import runpy
import sys

REQUIRED_EXTRAS = ("all", "messaging", "anthropic", "azure-identity")
ROOT = Path(__file__).resolve().parents[1]


def validate_selection(root):
    import pm
    from pm.environments import runtime_facts_path, selected_venv
    from pm.lock import Facts

    path = runtime_facts_path(root)
    if path.exists():
        fact = Facts(path, strict=True).get("venv")
        if not isinstance(fact, dict) or not fact.get("environment"):
            raise RuntimeError("persisted dependency selection has no prepared generation")
        extras = fact.get("extras")
        if not isinstance(extras, list) or not set(REQUIRED_EXTRAS).issubset(extras):
            raise RuntimeError("persisted dependency selection is missing platform extras")
    environment = selected_venv(root)
    if not (environment / "pyvenv.cfg").is_file():
        raise RuntimeError("selected dependency environment is missing")
    if not pm.venv_is_current(extras=list(REQUIRED_EXTRAS), project_root=root):
        raise RuntimeError("dependencies do not match this image; prepare them with hermes-prepare-dependencies")
    return environment


def activate(root):
    from pm.environments import activate_dependencies, selected_venv
    import pm

    selected = validate_selection(root)
    activate_dependencies(root)
    if selected_venv(root) != selected:
        raise RuntimeError("dependency selection changed during launch; retry startup")
    # env_for only reads installed packages. Explicit plugin installs still use
    # PM's writable HOME store and independent manager runtime.
    env = pm.env_for("python", "npm")
    os.environ["PATH"] = os.pathsep.join([str(root / "libexec"), env["PATH"]])
    os.environ["HERMES_PYTHON"] = "/usr/local/bin/hermes-python"


def validate_identity(root):
    from hermes_cli.version_info import get_version_info
    import pm

    provenance = json.loads((root / "cetirun-image.json").read_text())
    stamp = json.loads((root / "install-stamp.json").read_text())
    info = get_version_info()
    if (info.base_version != provenance["version"] or info.commit != provenance["revision"]
            or stamp.get("updateMechanism") != "external" or stamp.get("distribution") != "docker"):
        raise RuntimeError("Hermes install identity does not match the image")
    if sys.version_info[:2] != (3, 14):
        raise RuntimeError("Hermes requires its pinned Python 3.14")
    if not pm.is_installed("uv"):
        raise RuntimeError("the pinned PM uv tool is missing")
    for name in ("python", "node", "npm"):
        if not pm.is_installed(name):
            raise RuntimeError(f"pinned Hermes {name} is missing or does not match pm/lock.json")
        binary = pm.installed_package(name).binary.resolve()
        if not binary.is_relative_to((root / "tools").resolve()):
            raise RuntimeError(f"Hermes {name} must come from the image toolchain")


def execute(args):
    while args and args[0] in ("-B", "-I", "-u"):
        option = args.pop(0)
        if option == "-B":
            sys.dont_write_bytecode = True
        elif option == "-u":
            sys.stdout.reconfigure(line_buffering=True, write_through=True)
            sys.stderr.reconfigure(line_buffering=True, write_through=True)
    if not args:
        raise RuntimeError("hermes-python requires a script, -m module or -c command")
    mode = args.pop(0)
    if mode in ("-V", "--version"):
        print(f"Python {sys.version.split()[0]}")
    elif mode == "-":
        sys.argv = ["-", *args]
        exec(compile(sys.stdin.read(), "<stdin>", "exec"), {"__name__": "__main__"})
    elif mode == "-m" and args:
        module = args.pop(0)
        sys.argv = [module, *args]
        runpy.run_module(module, run_name="__main__", alter_sys=True)
    elif mode == "-c" and args:
        code = args.pop(0)
        sys.argv = ["-c", *args]
        exec(compile(code, "<string>", "exec"), {"__name__": "__main__"})
    elif not mode.startswith("-"):
        sys.argv = [mode, *args]
        runpy.run_path(mode, run_name="__main__")
    else:
        raise RuntimeError(f"unsupported hermes-python argument: {mode}")


def main():
    sys.path.insert(0, str(ROOT))
    os.environ["HERMES_RUNTIME_DIR"] = str(ROOT / "tools")
    os.environ["HERMES_DISABLE_LAZY_INSTALLS"] = "1"
    from cetirun_pm.sqlite_runtime import install

    install()
    validate_identity(ROOT)
    args = sys.argv[1:]
    if args == ["--prepare"]:
        import pm
        from pm.recovery import validate_environment
        from pm.environments import venv_command

        pm.sync_venv(list(REQUIRED_EXTRAS), explicit=True, project_root=ROOT)
        selected = validate_selection(ROOT)
        validate_environment(venv_command(ROOT, selected, ("-I",)), env=dict(os.environ), cwd=ROOT)
        print("Hermes dependency generation prepared and validated")
        return
    activate(ROOT)
    execute(args)


if __name__ == "__main__":
    try:
        main()
    except (OSError, RuntimeError, ValueError) as exc:
        print(f"Hermes runtime: {exc}", file=sys.stderr)
        raise SystemExit(1)
