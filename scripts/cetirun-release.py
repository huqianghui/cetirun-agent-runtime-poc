#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Build and publish the shared CetiRun inventory. Never log in or deploy."""

import argparse
from contextlib import contextmanager
import importlib.util
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile


ROOT = Path(__file__).resolve().parents[1]
INVENTORY = ROOT / "scripts/release-artifacts.json"
RUNTIME_CHART_LOCK = ROOT / "scripts/cetirun-runtime-chart-lock.py"
IMAGE_DIGEST = re.compile(r"sha256:[a-f0-9]{64}\Z")
SBOM_GENERATOR = re.compile(
    r"(?:[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?(?::[0-9]+)?/)?"
    r"[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)*"
    r"@sha256:[a-f0-9]{64}\Z"
)
VERSION_SPEC = importlib.util.spec_from_file_location("cetirun_version", ROOT / "scripts/cetirun-version.py")
VERSIONS = importlib.util.module_from_spec(VERSION_SPEC)
VERSION_SPEC.loader.exec_module(VERSIONS)


def inventory():
    data = json.loads(INVENTORY.read_text())
    if data.get("schemaVersion") != 1:
        raise ValueError("Unsupported artifact inventory schema")
    for kind in ("images", "charts"):
        names = [entry["name"] for entry in data[kind]]
        if not names or len(names) != len(set(names)):
            raise ValueError(f"Empty or duplicate {kind} inventory")
        for entry in data[kind]:
            if kind == "images" and "optional" in entry and type(entry["optional"]) is not bool:
                raise ValueError(f"Image {entry['name']} optional must be a boolean")
            if kind == "charts" and "optional" in entry:
                raise ValueError(f"Chart {entry['name']} cannot be optional")
            path = ROOT / (entry["script"] if kind == "images" else entry["path"] + "/Chart.yaml")
            if not path.is_file():
                raise ValueError(f"Missing artifact source: {path}")
    return data


def selected_inventory(data, excluded_images):
    if not isinstance(excluded_images, list) or any(not isinstance(name, str) or not name for name in excluded_images):
        raise ValueError("excludeImages must be a list of nonempty image names")
    if len(excluded_images) != len(set(excluded_images)):
        raise ValueError("excludeImages must not contain duplicate image names")
    images = {entry["name"]: entry for entry in data["images"]}
    for name in excluded_images:
        entry = images.get(name)
        if entry is None:
            raise ValueError(f"excludeImages names an unknown image: {name}")
        if not entry.get("optional", False):
            raise ValueError(f"excludeImages can only exclude optional images: {name}")
    excluded = set(excluded_images)
    return {**data, "images": [entry for entry in data["images"] if entry["name"] not in excluded]}


def run(command, *, env=None, capture=False, root=ROOT):
    result = subprocess.run(command, cwd=root, env=env, text=True, check=True,
                            stdout=subprocess.PIPE if capture else None)
    return result.stdout


def coordinates(args):
    if not re.fullmatch(r"[a-z0-9][a-z0-9.:-]*(?:/[a-z0-9][a-z0-9._-]*)*", args.registry):
        raise ValueError("REGISTRY must be an explicit registry[/repository-prefix], without a scheme")
    VERSIONS.validate_version(args.version)
    if not re.fullmatch(r"[0-9a-f]{40}", args.git_sha):
        raise ValueError("GIT_SHA must be the full source commit SHA")
    if args.chart_namespace != "helm":
        raise ValueError("CHART_NAMESPACE must be helm; use REGISTRY for a repository prefix")


def sbom_generator():
    generator = os.environ.get("SBOM_GENERATOR", "")
    if generator and not SBOM_GENERATOR.fullmatch(generator):
        raise ValueError("SBOM_GENERATOR must be a lowercase OCI image reference pinned with @sha256:<64 lowercase hexadecimal characters>")
    return generator


def runtime_chart_lock_inputs():
    openclaw = os.environ.get("OPENCLAW_IMAGE_DIGEST", "")
    hermes = os.environ.get("HERMES_IMAGE_DIGEST", "")
    if bool(openclaw) != bool(hermes):
        raise ValueError("OPENCLAW_IMAGE_DIGEST and HERMES_IMAGE_DIGEST must be supplied together")
    if not openclaw:
        return None
    for runtime, digest in (("openclaw", openclaw), ("hermes", hermes)):
        if not IMAGE_DIGEST.fullmatch(digest):
            raise ValueError(f"{runtime} image digest must be sha256 followed by 64 lowercase hexadecimal characters")
    return {"openclaw": openclaw, "hermes": hermes}


def require_license(images):
    if any(entry["name"] == "cetirun-vscode" for entry in images) and os.environ.get("ACCEPT_VSCODE_SERVER_LICENSE_TERMS") != "true":
        raise ValueError("Set ACCEPT_VSCODE_SERVER_LICENSE_TERMS=true after accepting the VS Code Server license; no images were built")


def image_env(args, entry, push):
    return {
        **os.environ,
        "REGISTRY": args.registry, "VERSION": args.version, "IMAGE_TAG": args.version,
        "GIT_SHA": args.git_sha, "PUSH": str(push).lower(), "IMAGE_NAME": entry["name"],
        "BASE_IMAGE": f"{args.registry}/cetirun-base:{args.version}",
        "BUILD_BASE": "false", "BUILD_IMAGE": "false", "BUILD_HERMES": "false",
        "BUILD_DESKTOP": "false", "PLATFORMS": "linux/amd64", "PLATFORM": "linux/amd64",
        "DOCKER_DEFAULT_PLATFORM": "linux/amd64",
        **entry.get("env", {}),
    }


def preflight(args, data, kinds):
    if args.force:
        print(f"WARNING: explicit overwrite of {args.registry} version {args.version}", flush=True)
        return
    for kind in kinds:
        for entry in data[kind]:
            path = entry["name"] if kind == "images" else f"{args.chart_namespace}/{entry['name']}"
            reference = f"{args.registry}/{path}:{args.version}"
            command = (["docker", "buildx", "imagetools", "inspect", reference] if kind == "images" else
                       ["helm", "show", "chart", f"oci://{args.registry}/{path}", "--version", args.version])
            result = subprocess.run(command,
                                    cwd=ROOT, text=True, capture_output=True)
            if result.returncode == 0:
                raise ValueError(f"{reference} already exists; choose a new version or explicitly use --force")
            message = result.stdout + result.stderr
            missing = re.search(r"manifest[_ ]unknown|name[_ ]unknown|no such manifest", message, re.I)
            missing = missing or re.search(re.escape(reference) + r": not found(?:\s|$)", message, re.I)
            if not missing:
                raise ValueError(f"Cannot determine whether {reference} exists; check registry access before retrying")


@contextmanager
def build_source(version, source_version):
    with tempfile.TemporaryDirectory(prefix="cetirun-build-source-") as temporary:
        root = Path(temporary) / "source"
        root.mkdir()
        # Use working-tree contents, including new nonignored source files, not
        # a checkout of HEAD that would silently discard the code under test.
        paths = run(["git", "ls-files", "--cached", "--others", "--exclude-standard", "-z"], capture=True)
        directories = {Path(".")}
        for relative in set(paths.split("\0")) - {""}:
            source, target = ROOT / relative, root / relative
            if source.is_file() or source.is_symlink():
                target.parent.mkdir(parents=True, exist_ok=True)
                shutil.copy2(source, target, follow_symlinks=False)
                directories.update(Path(relative).parents)
        # mkdir follows the caller's umask; Docker COPY must instead receive
        # the source directory modes, including under a private umask 077.
        for relative in directories:
            shutil.copystat(ROOT / relative, root / relative, follow_symlinks=False)
        if version != source_version:
            run([sys.executable, "-B", "scripts/cetirun-version.py", "set", version], root=root)
        # Images do not consume Chart.lock; package_charts rebuilds dependencies
        # in its own chart copy when chart publication is requested.
        yield root


def build_images(args, data, push, root=ROOT):
    for entry in data["images"]:
        reference = f"{args.registry}/{entry['name']}:{args.version}"
        print(f"==> {'Publish' if push else 'Build'} {reference} (source {args.git_sha})", flush=True)
        run(["bash", entry["script"]], env=image_env(args, entry, push), root=root)
        if push:
            run(["docker", "buildx", "imagetools", "inspect", reference], capture=True)
        if args.prune:
            run(["docker", "buildx", "prune", "--all", "--force"])


def selected_charts(args, data):
    return [entry for entry in data["charts"] if not args.chart or entry["name"] == args.chart]


def package_charts(args, data, destination, root=ROOT, runtime_lock=None):
    destination.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="cetirun-release-charts-") as temporary:
        source_root = Path(temporary)
        for entry in data["charts"]:
            source = args.chart_dir if args.chart_dir and entry["name"] == args.chart else root / entry["path"]
            target = source_root / entry["path"]
            if entry["name"] == "cetirun" and runtime_lock is not None:
                run([sys.executable, "-B", str(RUNTIME_CHART_LOCK),
                     "--source-chart", str(source), "--output-chart", str(target),
                     "--registry", args.registry, "--version", args.version,
                     "--openclaw-image-digest", runtime_lock["openclaw"],
                     "--hermes-image-digest", runtime_lock["hermes"]])
            else:
                shutil.copytree(source, target, ignore=shutil.ignore_patterns("charts", "__pycache__"))
        updates = VERSIONS.chart_updates(source_root, [entry["path"] for entry in data["charts"]],
                                         args.version, args.app_version)
        for path, content in updates.items():
            path.write_text(content)
        # Persist release coordinates in the package, not only in lint values.
        # Only first-party defaults change; external images and explicit digest
        # pins keep their separate ownership and versioning.
        for entry in data["charts"]:
            chart = source_root / entry["path"]
            for path in [chart / "values.yaml", chart / "values.schema.json", *(chart / "templates").rglob("*.tpl")]:
                if path.is_file():
                    content = path.read_text()
                    updated = content.replace(VERSIONS.DEFAULT_REGISTRY + "/", args.registry + "/")
                    # Memory's Helm fallback is a prefix rather than an image.
                    updated = updated.replace('"' + VERSIONS.DEFAULT_REGISTRY + '"', '"' + args.registry + '"')
                    if updated != content:
                        path.write_text(updated)
        for entry in selected_charts(args, data):
            chart = source_root / entry["path"]
            if entry["name"] == "cetirun-platform":
                run(["helm", "dependency", "update", "--skip-refresh", str(chart)])
            options = []
            if entry.get("namespace"):
                options += ["--namespace", entry["namespace"]]
            if entry.get("valuesFile"):
                options += ["--values", str(source_root / entry["valuesFile"])]
            if args.values_file:
                options += ["--values", str(args.values_file.resolve())]
            for key, value in entry.get("lintValues", {}).items():
                options += ["--set", f"{key}={value}"]
            run(["helm", "lint", "--strict", str(chart), *options])
            run(["helm", "template", entry["name"], str(chart), *options], capture=True)
            run(["helm", "package", str(chart), "--destination", str(destination),
                 "--version", args.version, "--app-version", args.app_version or args.version])
            if not (destination / f"{entry['name']}-{args.version}.tgz").is_file():
                raise ValueError(f"Missing packaged chart: {entry['name']}")


def publish_charts(args, data, directory):
    destination = f"oci://{args.registry}/{args.chart_namespace}"
    with tempfile.TemporaryDirectory(prefix="cetirun-chart-verify-") as temporary:
        for entry in selected_charts(args, data):
            archive = directory / f"{entry['name']}-{args.version}.tgz"
            run(["helm", "push", str(archive), destination])
            run(["helm", "pull", f"{destination}/{entry['name']}", "--version", args.version,
                 "--destination", temporary])
            pulled = Path(temporary) / archive.name
            if not pulled.is_file() or archive.read_bytes() != pulled.read_bytes():
                raise ValueError(f"Chart pull does not match the published package: {entry['name']}")


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("list", "check", "plan", "build-images", "publish-images",
                                             "package-charts", "publish-charts", "publish"))
    parser.add_argument("kind", nargs="?", choices=("images", "charts", "chart-paths"))
    parser.add_argument("--registry", default=os.environ.get("REGISTRY", VERSIONS.DEFAULT_REGISTRY))
    parser.add_argument("--version", default=os.environ.get("VERSION"))
    parser.add_argument("--git-sha", default=os.environ.get("GIT_SHA", ""))
    parser.add_argument("--chart-namespace", default=os.environ.get("CHART_NAMESPACE", "helm"),
                        help="must be helm; put any repository prefix in REGISTRY")
    parser.add_argument("--force", action="store_true", help="explicitly permit replacing existing version tags")
    parser.add_argument("--prune", action="store_true", help="prune build cache after each image on a disposable CI agent")
    parser.add_argument("--package-dir", type=Path)
    parser.add_argument("--chart", help="package/publish one chart from the shared inventory")
    parser.add_argument("--chart-dir", type=Path, help="source override for the selected chart")
    parser.add_argument("--app-version", help="explicit application version for a standalone chart")
    parser.add_argument("--values-file", type=Path, help="additional lint/render values for a standalone chart")
    parser.add_argument("--exclude-image", action="append", default=[], dest="excluded_images",
                        help="exclude an optional image by name; repeat for each image")
    args = parser.parse_args(argv)
    try:
        data = selected_inventory(inventory(), args.excluded_images)
        if args.command == "list":
            if not args.kind:
                raise ValueError("list requires images, charts, or chart-paths")
            entries = data["charts" if args.kind == "chart-paths" else args.kind]
            print("\n".join(entry["path" if args.kind == "chart-paths" else "name"] for entry in entries))
            return 0
        if args.command == "check":
            discovered = {path.parent.relative_to(ROOT).as_posix() for pattern in
                          ("app/*/Chart.yaml", "app/*/chart/Chart.yaml", "infra/*/Chart.yaml")
                          for path in ROOT.glob(pattern)}
            if discovered != {entry["path"] for entry in data["charts"]}:
                raise ValueError("Release chart inventory differs from repository charts")
            print(f"Artifact inventory: {len(data['images'])} images, {len(data['charts'])} charts")
            return 0
        source_version = VERSIONS.top_level_yaml(ROOT / "app/cetirun-chart/Chart.yaml", "version")
        args.version = source_version if args.version is None else args.version
        args.registry = args.registry.rstrip("/")
        args.git_sha = args.git_sha or run(["git", "rev-parse", "HEAD"], capture=True).strip()
        coordinates(args)
        sbom_generator()
        runtime_lock = runtime_chart_lock_inputs()
        if runtime_lock is not None and args.force and args.command in ("publish-charts", "publish"):
            raise ValueError("Runtime Chart image locking requires a new immutable registry prefix; --force is not supported")
        if args.command.startswith("publish"):
            VERSIONS.validate_push_registry(args.registry)
        if args.app_version:
            if not args.chart or not re.fullmatch(r"[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}", args.app_version):
                raise ValueError("--app-version requires --chart and a valid image tag")
        if args.chart and (args.command not in ("package-charts", "publish-charts") or not selected_charts(args, data)):
            raise ValueError("--chart requires a known chart and package-charts or publish-charts")
        if (args.chart_dir or args.values_file) and not args.chart:
            raise ValueError("--chart-dir and --values-file require --chart")
        if args.command == "plan":
            print(json.dumps({"registry": args.registry, "version": args.version, "sourceVersion": source_version,
                              "gitSha": args.git_sha, **data}, indent=2))
            return 0
        image_work = args.command in ("build-images", "publish-images", "publish")
        if image_work:
            require_license(data["images"])
        run([sys.executable, "-B", "scripts/cetirun-version.py", "check", source_version])
        if args.command.startswith("publish"):
            kinds = ("images", "charts") if args.command == "publish" else (args.command.removeprefix("publish-"),)
            preflight(args, {**data, "charts": selected_charts(args, data)}, kinds)
        if args.command == "package-charts":
            if args.package_dir is None:
                raise ValueError("package-charts requires --package-dir")
            package_charts(args, data, args.package_dir.resolve(), runtime_lock=runtime_lock)
        elif args.command in ("publish-charts", "publish"):
            with tempfile.TemporaryDirectory(prefix="cetirun-publish-") as temporary:
                directory = Path(temporary)
                if image_work:
                    with build_source(args.version, source_version) as root:
                        package_charts(args, data, directory, root, runtime_lock)
                        build_images(args, data, push=True, root=root)
                else:
                    package_charts(args, data, directory, runtime_lock=runtime_lock)
                publish_charts(args, data, directory)
        else:
            with build_source(args.version, source_version) as root:
                build_images(args, data, push=args.command == "publish-images", root=root)
    except (ValueError, KeyError, OSError, subprocess.CalledProcessError) as error:
        print(f"ERROR: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
