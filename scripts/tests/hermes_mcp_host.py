# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Qualify the pinned Hermes MCP transport against an isolated TLS MCP server."""

import argparse
import asyncio
import base64
import importlib.util
import json
import logging
import os
from pathlib import Path
import socket
import subprocess
import sys
import tempfile
import time
import types


ROOT = Path(__file__).resolve().parents[2]
COMPAT = ROOT / "app/cetirun-chart/agents/hermes/compat"


def load(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    sys.modules[name] = module
    spec.loader.exec_module(module)
    return module


def token(expires):
    claims = base64.urlsafe_b64encode(json.dumps({"exp": expires}).encode()).decode().rstrip("=")
    return "synthetic." + claims + ".signature"


async def qualify(source, directory):
    sys.path.insert(0, str(source))
    from mcp.server import MCPServer
    import uvicorn
    from tools import mcp_tool
    logging.disable(logging.CRITICAL)

    installer = load("cetirun_mcp_installer", COMPAT / "apply_workload_mcp.py")
    package = types.ModuleType("cetirun_compat")
    package.__path__ = [str(COMPAT)]
    sys.modules[package.__name__] = package
    auth = load("cetirun_compat.workload_mcp", COMPAT / "workload_mcp.py")
    transport = types.ModuleType("cetirun_mcp_transport")
    path = source / "tools/mcp_tool_transport.py"
    transport.__file__ = str(path)
    exec(compile(installer.prepare_patch(path.read_bytes()), str(path), "exec"), transport.__dict__)
    auth.TOKEN_PATH = directory / "token"
    auth.CA_PATH = directory / "cert.pem"
    auth.TOKEN_PATH.write_text(token(time.time() + 120))
    subprocess.run([
        "openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1",
        "-subj", "/CN=localhost", "-addext", "subjectAltName=IP:127.0.0.1",
        "-keyout", str(directory / "key.pem"), "-out", str(auth.CA_PATH),
    ], check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    server = MCPServer("synthetic-cetirun")

    @server.tool()
    def echo(text: str) -> str:
        return text

    app = server.streamable_http_app()
    seen, mode = [], {"status": 0}

    async def record(scope, receive, send):
        if scope["type"] != "http":
            return await app(scope, receive, send)
        headers = dict(scope["headers"])
        seen.append((scope["method"], scope["path"], headers.get(b"authorization")))
        status = mode["status"] or (200 if scope["method"] == "HEAD" else 0)
        if status:
            await send({"type": "http.response.start", "status": status,
                        "headers": [(b"content-type", b"application/json"), (b"location", b"/unexpected")]})
            return await send({"type": "http.response.body", "body": b"{}"})
        return await app(scope, receive, send)

    listener = socket.socket()
    listener.bind(("127.0.0.1", 0))
    endpoint = f"https://127.0.0.1:{listener.getsockname()[1]}/mcp"
    os.environ["CETIRUN_CONNECTIONS_MCP_URL"] = endpoint
    uv = uvicorn.Server(uvicorn.Config(record, log_level="critical", ssl_keyfile=str(directory / "key.pem"),
                                       ssl_certfile=str(auth.CA_PATH)))
    serving = asyncio.create_task(uv.serve(sockets=[listener]))

    attempted_sse = []

    class Task(transport.MCPServerTransportMixin, mcp_tool.MCPServerTask):
        def _sse_transport(self, *args):
            attempted_sse.append(True)
            return super()._sse_transport(*args)

        async def _serve_transport(self, connection, label, timeout):
            assert label == "HTTP", "platform MCP attempted another transport"
            async with connection as streams:
                async with mcp_tool.ClientSession(*streams[:2]) as session:
                    await session.initialize()
                    tools = await session.list_tools()
                    assert any(tool.name == "echo" for tool in tools.tools)
                    before = auth.TOKEN_PATH.read_text()
                    auth.TOKEN_PATH.write_text(token(time.time() + 240))
                    result = await session.call_tool("echo", {"text": "synthetic-roundtrip"})
                    assert result.content[0].text == "synthetic-roundtrip"
                    assert auth.TOKEN_PATH.read_text() != before
            return "complete"

    async def run(config=None):
        task = Task("cetirun-qualification")
        return await task._run_http({"url": endpoint, "connect_timeout": 2, **(config or {})})

    async def refuses(config=None):
        try:
            await asyncio.wait_for(run(config), 5)
        except (AssertionError, TimeoutError):
            raise
        except BaseExceptionGroup as error:
            if error.subgroup((AssertionError, TimeoutError)):
                raise
            return
        except Exception:
            return
        raise AssertionError("invalid platform transport was accepted")

    try:
        for _ in range(100):
            if uv.started:
                break
            if serving.done():
                await serving
            await asyncio.sleep(0.01)
        assert uv.started
        task = Task("cetirun-preflight")
        await task._preflight_content_type(endpoint, timeout=2)
        assert seen and seen[-1][0] == "HEAD" and seen[-1][2]
        assert await asyncio.wait_for(run(), 10) == "complete"
        assert mcp_tool.sdk_httpx().__name__ == "httpx2"
        assert all(path == "/mcp" and value for _, path, value in seen)
        assert len({value for _, _, value in seen}) == 2, "workload token was not re-read"
        for invalid in ({"ssl_verify": False}, {"transport": "sse"}):
            count = len(seen)
            await refuses(invalid)
            assert len(seen) == count
        for status in (307, 405):
            mode["status"] = status
            seen.clear()
            await refuses()
            assert seen and all(method == "POST" and path == "/mcp" for method, path, _ in seen)
        mode["status"] = 0
        for bad_token in (None, token(time.time() - 1), "invalid"):
            if bad_token is None:
                auth.TOKEN_PATH.unlink()
            else:
                auth.TOKEN_PATH.write_text(bad_token)
            count = len(seen)
            await refuses()
            assert len(seen) == count
        auth.TOKEN_PATH.write_text(token(time.time() + 120))
        subprocess.run([
            "openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1",
            "-subj", "/CN=untrusted.example.com", "-keyout", str(directory / "untrusted-key.pem"),
            "-out", str(directory / "untrusted.pem"),
        ], check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        auth.CA_PATH = directory / "untrusted.pem"
        count = len(seen)
        await refuses()
        assert len(seen) == count
        auth.CA_PATH = directory / "missing-ca.pem"
        count = len(seen)
        await refuses()
        assert len(seen) == count
        assert not attempted_sse, "platform MCP attempted SSE fallback"
    finally:
        uv.should_exit = True
        await serving
        listener.close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source", type=Path, required=True)
    args = parser.parse_args()
    with tempfile.TemporaryDirectory(prefix="cetirun-hermes-mcp-") as temporary:
        directory = Path(temporary)
        os.environ.update(HOME=temporary, HERMES_HOME=str(directory / ".hermes"),
                          HERMES_SKIP_AUTO_UPDATE="1", NO_PROXY="127.0.0.1,localhost")
        asyncio.run(qualify(args.source.resolve(), directory))
    print("Pinned Hermes MCP TLS, rotation, redirect and transport checks passed")


if __name__ == "__main__":
    main()
