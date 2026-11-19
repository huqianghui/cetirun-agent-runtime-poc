# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Pytest outbound-network guard for offline auth and runtime helper tests."""

import ipaddress
import socket
from unittest.mock import patch


def guard_connect(original):
    def connect(sock, address):
        if sock.family in (socket.AF_INET, socket.AF_INET6):
            try:
                allowed = ipaddress.ip_address(address[0]).is_loopback
            except ValueError:
                allowed = False
            if not allowed:
                raise AssertionError(
                    f"Outbound network is forbidden in unit tests: {address[0]}"
                )
        return original(sock, address)

    return connect


def guard_resolve(original):
    def getaddrinfo(host, *args, **kwargs):
        if host != "localhost":
            try:
                allowed = ipaddress.ip_address(host).is_loopback
            except ValueError:
                allowed = False
            if not allowed:
                raise AssertionError(
                    f"External DNS resolution is forbidden in unit tests: {host}"
                )
        return original(host, *args, **kwargs)

    return getaddrinfo


def pytest_configure(config):
    # Block external requests before collection imports application code.
    patches = [
        patch("socket.socket.connect", guard_connect(socket.socket.connect)),
        patch("socket.socket.connect_ex", guard_connect(socket.socket.connect_ex)),
        patch("socket.getaddrinfo", guard_resolve(socket.getaddrinfo)),
    ]
    for item in patches:
        item.start()
        config.add_cleanup(item.stop)
