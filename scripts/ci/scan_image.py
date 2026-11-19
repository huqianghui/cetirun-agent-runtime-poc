#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Scan an already-built local component image; fetch Trivy/its DB, never build or push."""

import argparse
from pathlib import Path
import subprocess
import sys

from validate import run


# Reuse the scanner/digest already used by docker-build-validate.yml.
TRIVY = (
    "mcr.microsoft.com/oss/v2/aquasecurity/trivy:v0.68.2"
    "@sha256:493766e398dcdb680b4084ba396139c495466f4229698b5da7be79d61fa5f5b3"
)
IMAGES = (
    "cetirun-memory-context:validation",
    "cetirun-operator:validation",
    "cetirun-terminal:validation",
    "cetirun-portal:validation",
    "cetirun-agent-broker:validation",
    "cetirun-observability:validation",
)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image", required=True, choices=IMAGES)
    args = parser.parse_args(argv)
    try:
        # Validate the local build before any scanner image/DB download.
        run(["docker", "image", "inspect", args.image], Path.cwd(), capture=True)
        run(
            [
                "docker",
                "run",
                "--rm",
                "-v",
                "/var/run/docker.sock:/var/run/docker.sock",
                TRIVY,
                "image",
                "--scanners",
                "vuln",
                "--format",
                "table",
                "--exit-code",
                "1",
                "--severity",
                "CRITICAL,HIGH",
                "--ignore-unfixed",
                args.image,
            ],
            Path.cwd(),
        )
    except (ValueError, OSError, subprocess.CalledProcessError) as error:
        print(f"ERROR: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
