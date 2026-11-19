# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Native websockify/noVNC transport check; graphical VNC/CDP backends are synthetic."""

import configparser
import http.server
import json
from pathlib import Path
import shlex
import socket
import socketserver
import ssl
import subprocess
import sys
import threading
import time


class RFB(socketserver.BaseRequestHandler):
    def handle(self):
        self.request.sendall(b"RFB 003.008\n")
        self.request.settimeout(3)
        try:
            self.request.recv(32)
        except OSError:
            pass


class CDP(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b'{"Browser":"synthetic-local-cdp"}')

    def log_message(self, *_):
        pass


def main():
    data = json.load(sys.stdin)
    Path("run").mkdir()
    Path("tls").mkdir(mode=0o700)
    for name, content in data["material"].items():
        Path("tls", name).write_text(content)
        Path("tls", name).chmod(0o600)
    Path("envoy.json").write_text(json.dumps(data["bootstrap"]))
    # Render the real entrypoint's supervisor configuration without launching X.
    # Scratch paths are relocated and the final graphical-stack launch is
    # suppressed. The real noVNC command is executed separately below.
    source = Path("/candidate/entrypoint.sh").read_text()
    launch = "exec supervisord -n -c /tmp/supervisord.conf"
    assert source.count(launch) == 1
    Path("entrypoint.sh").write_text(source.replace(launch, "exit 0").replace("/tmp/", "/work/run/"))
    subprocess.run(["bash", "/work/entrypoint.sh"], check=True, timeout=10)
    config = configparser.RawConfigParser()
    config.read("run/supervisord.conf")
    novnc = config["program:novnc"]["command"]
    assert "127.0.0.1:6080" in novnc and "localhost:5900" in novnc
    assert "-listen 127.0.0.1 -localhost -rfbport 5900" in config["program:x11vnc"]["command"]
    assert "--remote-debugging-address=127.0.0.1" in config["program:chrome"]["command"]
    rfb = socketserver.ThreadingTCPServer(("127.0.0.1", 5900), RFB)
    rfb.daemon_threads = True
    cdp = http.server.ThreadingHTTPServer(("127.0.0.1", 18800), CDP)
    for server in (rfb, cdp):
        threading.Thread(target=server.serve_forever, daemon=True).start()
    processes = []
    log = open("run/process.log", "w+")
    try:
        processes.append(subprocess.Popen(shlex.split(novnc), stdout=log, stderr=log))
        processes.append(subprocess.Popen(["/opt/envoy", "--disable-hot-restart", "--concurrency", "1",
                                          "--log-level", "error", "-c", "/work/envoy.json"], stdout=log, stderr=log))
        context = ssl.create_default_context(cafile="/work/tls/ca.crt")
        context.minimum_version = ssl.TLSVersion.TLSv1_3
        context.load_cert_chain("/work/tls/client.crt", "/work/tls/client.key")

        def peer(ctx=context, hostname=data["serverName"]):
            return ctx.wrap_socket(socket.create_connection(("127.0.0.1", 15446), timeout=2), server_hostname=hostname)

        for _ in range(80):
            try:
                with peer() as stream:
                    stream.sendall(b"GET /desktop/ HTTP/1.1\r\nHost: example.com\r\nConnection: close\r\n\r\n")
                    response = stream.recv(4096)
                    if b" 200 " in response:
                        break
            except OSError:
                pass
            time.sleep(0.1)
        else:
            raise AssertionError("native noVNC did not serve through the TLS endpoint")
        with peer() as stream:
            stream.sendall(
                b"GET /desktop/websockify HTTP/1.1\r\nHost: example.com\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"
                b"Sec-WebSocket-Key: ZGVza3RvcC1maXh0dXJlMQ==\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Protocol: binary\r\n\r\n"
            )
            response = b""
            while b"\r\n\r\n" not in response:
                response += stream.recv(4096)
            header, payload = response.split(b"\r\n\r\n", 1)
            assert b" 101 " in header
            while b"RFB 003.008\n" not in payload:
                payload += stream.recv(4096)
        try:
            with socket.create_connection(("127.0.0.2", 6080), timeout=1):
                raise AssertionError("noVNC backend bound beyond loopback")
        except OSError:
            pass
        for scenario in ("missing-client", "wrong-client-uri", "wrong-server-san", "plaintext"):
            ctx = ssl.create_default_context(cafile="/work/tls/ca.crt")
            ctx.minimum_version = ssl.TLSVersion.TLSv1_3
            if scenario != "missing-client":
                name = "wrong" if scenario == "wrong-client-uri" else "client"
                ctx.load_cert_chain(f"/work/tls/{name}.crt", f"/work/tls/{name}.key")
            rejected = False
            try:
                stream = socket.create_connection(("127.0.0.1", 15446), timeout=2) if scenario == "plaintext" else peer(
                    ctx, "other.example.com" if scenario == "wrong-server-san" else data["serverName"])
                with stream:
                    stream.sendall(b"GET /desktop/ HTTP/1.1\r\nHost: example.com\r\n\r\n")
                    rejected = not stream.recv(4096)
            except OSError:
                rejected = True
            assert rejected, scenario
        subprocess.run(["python3", "/candidate/readiness.py"], check=True, timeout=5)
        rfb.shutdown()
        rfb.server_close()
        assert subprocess.run(["python3", "/candidate/readiness.py"], timeout=5).returncode != 0
        print(json.dumps({"ok": True, "native": "websockify/noVNC", "tls_http": 200, "tls_websocket": 101,
                          "rfb_banner": True, "loopback_only": True, "tls_negative_cases": 4,
                          "readiness_and_backend_loss": True, "synthetic_backends": ["RFB", "CDP"],
                          "not_run": ["Xvfb", "Chrome", "graphical desktop interaction", "Kata/Cilium", "browser OIDC"]}))
    finally:
        for process in processes:
            process.terminate()
        for process in processes:
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()
        rfb.server_close()
        cdp.shutdown()
        cdp.server_close()
        log.close()


if __name__ == "__main__":
    main()
