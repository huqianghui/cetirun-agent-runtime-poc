# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Request-time workload authentication for the pinned native MCP client."""

import base64
import json
import os
from pathlib import Path
import ssl
import time
from urllib.parse import urlsplit


TOKEN_PATH = Path("/var/run/cetirun/connections/token")
CA_PATH = Path("/var/run/cetirun/connections-ca/ca.crt")


def workload_tls(endpoint, existing=True):
    if endpoint != os.environ.get("CETIRUN_CONNECTIONS_MCP_URL", ""):
        return existing
    if existing is not True:
        raise ValueError("Platform MCP requires verified TLS")
    try:
        context = ssl.create_default_context()
        context.load_verify_locations(cafile=str(CA_PATH))
    except (OSError, ssl.SSLError):
        raise ValueError("Platform MCP CA trust is unavailable") from None
    return context


def workload_auth(httpx, endpoint, existing=None):
    target = os.environ.get("CETIRUN_CONNECTIONS_MCP_URL", "")
    if not target or endpoint != target:
        return existing
    parsed = urlsplit(target)
    if parsed.scheme != "https" or not parsed.hostname or parsed.path != "/mcp" or parsed.username or parsed.password or parsed.query or parsed.fragment:
        raise ValueError("Invalid platform MCP endpoint")

    class WorkloadAuth(httpx.Auth):
        is_cetirun_workload = True

        def auth_flow(self, request):
            if str(request.url) != target:
                raise ValueError("Workload identity cannot leave the platform MCP endpoint")
            try:
                with TOKEN_PATH.open("rb") as source:
                    raw = source.read(65537)
                if len(raw) > 65536:
                    raise ValueError
                token = raw.decode("ascii").strip()
                parts = token.split(".")
                if len(parts) != 3 or any(character.isspace() for character in token):
                    raise ValueError
                claims = json.loads(base64.urlsafe_b64decode(parts[1] + "=" * (-len(parts[1]) % 4)))
                if not isinstance(claims.get("exp"), (int, float)) or claims["exp"] <= time.time():
                    raise ValueError
            except (OSError, UnicodeError, ValueError, KeyError, TypeError):
                raise ValueError("Workload identity is unavailable") from None
            request.headers["Authorization"] = "Bearer " + token
            yield request

    return WorkloadAuth()
