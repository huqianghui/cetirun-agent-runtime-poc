#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Check the effective AKS Kubernetes version locally before cloud writes."""

import argparse
import json
import os
from pathlib import Path
import re
import subprocess
import sys


MINIMUM_VERSION = (1, 37)
STABLE_VERSION = re.compile(r"(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:\.(0|[1-9][0-9]*))?")


def compile_bicep(*args):
    # Never install tools, restore modules, or print compiled parameter values.
    result = subprocess.run(
        ["bicep", *map(str, args), "--stdout", "--no-restore"],
        check=True, text=True, stdout=subprocess.PIPE,
    )
    return json.loads(result.stdout)


def compile_inputs(template_path, parameter_path):
    if parameter_path.suffix == ".bicepparam":
        bundle = compile_bicep("build-params", parameter_path, "--bicep-file", template_path)
        template = json.loads(bundle["templateJson"])
        parameters = json.loads(bundle["parametersJson"])
    else:
        template = compile_bicep("build", template_path)
        parameters = json.loads(parameter_path.read_text(encoding="utf-8"))
    return template, parameters


def effective_version(template, parameters):
    supplied = parameters["parameters"]
    if not isinstance(supplied, dict):
        raise ValueError("parameters must be an object")
    if "kubernetesVersion" in supplied:
        # A reference or unresolved expression cannot establish compatibility.
        return supplied["kubernetesVersion"]["value"]
    return template["parameters"]["kubernetesVersion"]["defaultValue"]


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--template", type=Path, required=True)
    parser.add_argument("--parameters", type=Path, required=True)
    parser.add_argument("--output-dir", type=Path, help="existing private directory for validated JSON inputs")
    args = parser.parse_args(argv)
    try:
        template, parameters = compile_inputs(args.template, args.parameters)
        version = effective_version(template, parameters)
        match = STABLE_VERSION.fullmatch(version) if isinstance(version, str) else None
        if match is None or tuple(map(int, match.group(1, 2))) < MINIMUM_VERSION:
            raise ValueError("unsupported Kubernetes version")
        if args.output_dir is not None:
            for name, value in (("template", template), ("parameters", parameters)):
                path = args.output_dir / f"{name}.json"
                descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
                with os.fdopen(descriptor, "w", encoding="utf-8") as output:
                    json.dump(value, output)
    except (OSError, ValueError, KeyError, TypeError, subprocess.CalledProcessError):
        print(
            "Error: could not establish a supported Kubernetes version. "
            "Sandbox and CetiAdmin admission require stable Kubernetes 1.37 or newer. "
            "Set kubernetesVersion to a supported major.minor or major.minor.patch in PARAM_FILE "
            "(or omit it to use the template default). "
            "Ensure the parameter file is valid and the Bicep CLI is installed on PATH. "
            "Confirm that the target AKS region offers the selected version; do not fall back "
            "to an older version.",
            file=sys.stderr,
        )
        return 1
    print(f"Kubernetes version preflight passed: {version}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
