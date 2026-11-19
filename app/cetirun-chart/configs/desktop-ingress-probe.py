#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Check only local desktop backends; Gateway client credentials are not used."""

import http.client
import json
import socket
import sys


def check():
    for port, path in ((6080, "/desktop/"), (18800, "/json/version")):
        connection = http.client.HTTPConnection("127.0.0.1", port, timeout=1)
        try:
            connection.request("GET", path)
            response = connection.getresponse()
            body = response.read(65537)
            if response.status != 200 or len(body) > 65536:
                return False
            if port == 18800 and not isinstance(json.loads(body), dict):
                return False
        finally:
            connection.close()
    with socket.create_connection(("127.0.0.1", 5900), timeout=1) as peer:
        with peer.makefile("rb") as stream:
            return stream.read(4) == b"RFB "


if __name__ == "__main__":
    try:
        ready = check()
    except (OSError, ValueError, http.client.HTTPException):
        ready = False
    sys.exit(0 if ready else 1)
