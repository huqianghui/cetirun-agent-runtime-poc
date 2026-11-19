#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Task-owned TCP/UDP witness that records only synthetic run/nonce pairs."""

import argparse
from contextlib import ExitStack
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import ipaddress
import json
import re
import socket
import socketserver
import threading
import time
import uuid


PAIR = r"([0-9a-f]{32})/([0-9a-f]{32})"
TCP_PORTS = (18080, 18082)
UDP_PORTS = (18081, 18083)
MAX_EVENTS = 4096


class Observations:
    def __init__(self):
        self.instance = uuid.uuid4().hex
        self.started = time.monotonic()
        self.lock = threading.Lock()
        self.events = []
        self.overflow = False

    def record(self, run, nonce, protocol, port):
        with self.lock:
            if len(self.events) >= MAX_EVENTS:
                self.overflow = True
                return None
            event = {"run": run, "nonce": nonce, "protocol": protocol, "port": port,
                     "instance": self.instance, "sequence": len(self.events) + 1}
            self.events.append(event)
            return event

    def snapshot(self, run=None):
        with self.lock:
            value = {"instance": self.instance, "uptime": time.monotonic() - self.started,
                     "overflow": self.overflow}
            if run is None:
                value.update(tcp=list(TCP_PORTS), udp=list(UDP_PORTS))
            else:
                value["events"] = [event for event in self.events if event["run"] == run]
            return value


class HTTPHandler(BaseHTTPRequestHandler):
    def setup(self):
        self.request.settimeout(5)
        super().setup()

    def do_GET(self):
        status = 200
        match = re.fullmatch("/witness/" + PAIR, self.path)
        events = re.fullmatch(r"/events/([0-9a-f]{32})", self.path)
        if self.path == "/healthz":
            value = self.server.observations.snapshot()
        elif events:
            value = self.server.observations.snapshot(events[1])
        elif match:
            value = self.server.observations.record(match[1], match[2], "TCP", self.server.server_port)
            if value is None:
                status, value = 503, {"error": "witness-overflow"}
        else:
            status, value = 404, {"error": "synthetic-nonce-path-required"}
        raw = json.dumps(value).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(raw)

    def log_message(self, *_):
        pass


class UDPHandler(socketserver.BaseRequestHandler):
    def handle(self):
        data, endpoint = self.request
        match = re.fullmatch(rb"([0-9a-f]{32}):([0-9a-f]{32})", data)
        if match:
            value = self.server.observations.record(
                match[1].decode(), match[2].decode(), "UDP", self.server.server_address[1])
            if value is not None:
                endpoint.sendto(json.dumps(value).encode(), self.client_address)


def allowed_peer(address, networks):
    peer = ipaddress.ip_address(address)
    if isinstance(peer, ipaddress.IPv6Address) and peer.ipv4_mapped is not None:
        peer = peer.ipv4_mapped
    return any(peer.version == network.version and peer in network for network in networks)


class PeerFilter:
    def verify_request(self, request, client_address):
        return allowed_peer(client_address[0], self.allowed_peers)


def serve(address, networks):
    observations = Observations()
    family = socket.AF_INET6 if ipaddress.ip_address(address).version == 6 else socket.AF_INET

    class HTTPServer(PeerFilter, ThreadingHTTPServer):
        address_family = family
        daemon_threads = True

    class UDPServer(PeerFilter, socketserver.UDPServer):
        address_family = family
        max_packet_size = 128

    with ExitStack() as stack:
        servers = []
        for ports, server_type, handler in (
            (TCP_PORTS, HTTPServer, HTTPHandler), (UDP_PORTS, UDPServer, UDPHandler),
        ):
            for port in ports:
                server = stack.enter_context(server_type((address, port), handler))
                server.observations = observations
                server.allowed_peers = networks
                servers.append(server)
        threads = [threading.Thread(target=server.serve_forever, daemon=True) for server in servers]
        for thread in threads:
            thread.start()
        print(json.dumps({"status": "listening", "tcp": TCP_PORTS, "udp": UDP_PORTS}), flush=True)
        try:
            threading.Event().wait()
        except KeyboardInterrupt:
            pass
        finally:
            for server in servers:
                server.shutdown()
            for thread in threads:
                thread.join(timeout=5)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--listen-address", default="127.0.0.1")
    parser.add_argument("--allow-public-listen", action="store_true",
                        help="Explicit permission to listen beyond loopback on a task-owned fixture")
    parser.add_argument("--allow-peer", action="append", default=[], metavar="CIDR",
                        help="Allowed validation client/egress CIDR; required for non-loopback listeners")
    args = parser.parse_args()
    try:
        address = ipaddress.ip_address(args.listen_address)
        if address.is_multicast or address.is_link_local or "%" in str(address):
            parser.error("a unicast or wildcard listener address is required")
        if not address.is_loopback and not args.allow_public_listen:
            parser.error("non-loopback listeners require --allow-public-listen")
        if not address.is_loopback and not args.allow_peer:
            parser.error("non-loopback listeners require explicit --allow-peer CIDRs")
        networks = [ipaddress.ip_network(value, strict=True)
                    for value in args.allow_peer or ("127.0.0.0/8", "::1/128")]
        if any(network.prefixlen == 0 for network in networks):
            parser.error("unrestricted peer ranges are not allowed")
        serve(str(address), networks)
        return 0
    except (OSError, ValueError):
        print(json.dumps({"status": "failed", "reason": "witness-listener-unavailable"}))
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
